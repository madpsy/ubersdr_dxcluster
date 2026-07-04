package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"strings"
	"sync/atomic"
)

// TelnetServer listens for DX cluster client connections and streams spots
// in standard AR-Cluster format.
type TelnetServer struct {
	addr        string
	hub         *Hub
	spotterCall string
	clients     atomic.Int32
}

func NewTelnetServer(addr string, hub *Hub, spotterCall string) *TelnetServer {
	return &TelnetServer{addr: addr, hub: hub, spotterCall: spotterCall}
}

// ClientCount returns the number of currently connected telnet clients.
func (t *TelnetServer) ClientCount() int {
	return int(t.clients.Load())
}

// ListenAndServe starts the telnet listener. Blocks until the listener fails.
func (t *TelnetServer) ListenAndServe() error {
	ln, err := net.Listen("tcp", t.addr)
	if err != nil {
		return fmt.Errorf("telnet listen %s: %w", t.addr, err)
	}
	log.Printf("[telnet] listening on %s", t.addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("[telnet] accept error: %v", err)
			continue
		}
		go t.handleConn(conn)
	}
}

func (t *TelnetServer) handleConn(conn net.Conn) {
	remote := conn.RemoteAddr().String()
	t.clients.Add(1)
	log.Printf("[telnet] client connected: %s (total: %d)", remote, t.clients.Load())
	defer func() {
		t.clients.Add(-1)
		conn.Close()
		log.Printf("[telnet] client disconnected: %s (total: %d)", remote, t.clients.Load())
	}()

	// Send login banner
	fmt.Fprintf(conn, "Hello %s de UberSDR DX Cluster\r\n", t.spotterCall)
	fmt.Fprintf(conn, "Streaming live spots from UberSDR (Digital / CW / Voice)\r\n")
	fmt.Fprintf(conn, "Type BYE or QUIT to disconnect.\r\n\r\n")

	// Subscribe to hub
	ch := t.hub.Subscribe()
	defer t.hub.Unsubscribe(ch)

	// Read input in a separate goroutine so we can detect BYE/QUIT
	quit := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			cmd := strings.TrimSpace(strings.ToUpper(scanner.Text()))
			if cmd == "BYE" || cmd == "QUIT" {
				fmt.Fprintf(conn, "73 de UberSDR\r\n")
				close(quit)
				return
			}
		}
		close(quit)
	}()

	// Stream spots to client
	for {
		select {
		case <-quit:
			return
		case spot, ok := <-ch:
			if !ok {
				return
			}
			line := spot.FormatDXCluster(t.spotterCall)
			if _, err := fmt.Fprintf(conn, "%s\r\n", line); err != nil {
				return
			}
		}
	}
}
