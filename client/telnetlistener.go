package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
)

// TelnetListener accepts plain TCP/telnet clients on 0.0.0.0:<port> and
// bridges them to the active DX cluster WebSocket session:
//
//   - every chunk of server output is broadcast to all connected clients, and
//   - each line a client types is forwarded to the WebSocket.
//
// This lets any standard DX cluster telnet consumer (logging software, a
// terminal, etc.) read the instance's spot stream over the local network.
type TelnetListener struct {
	port    int
	forward func(string) // forward a client's command line to the WebSocket
	onCount func(int)    // called whenever the connected-client count changes

	mu      sync.Mutex
	ln      net.Listener
	clients map[net.Conn]struct{}
	running bool
}

// NewTelnetListener creates a listener for the given port. forward is called
// with each command line received from a client; onCount is called with the
// current client count whenever a client connects or disconnects.
func NewTelnetListener(port int, forward func(string), onCount func(int)) *TelnetListener {
	return &TelnetListener{
		port:    port,
		forward: forward,
		onCount: onCount,
		clients: make(map[net.Conn]struct{}),
	}
}

// Start binds the listening socket and begins accepting clients. It returns an
// error immediately if the port cannot be bound.
func (t *TelnetListener) Start() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running {
		return nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", t.port))
	if err != nil {
		return err
	}
	t.ln = ln
	t.running = true
	go t.acceptLoop(ln)
	return nil
}

// Stop closes the listener and disconnects all clients.
func (t *TelnetListener) Stop() {
	t.mu.Lock()
	if !t.running {
		t.mu.Unlock()
		return
	}
	t.running = false
	ln := t.ln
	t.ln = nil
	conns := make([]net.Conn, 0, len(t.clients))
	for c := range t.clients {
		conns = append(conns, c)
	}
	t.clients = make(map[net.Conn]struct{})
	t.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	t.notifyCount(0)
}

// Broadcast sends text to every connected client. Server output already
// carries CR/LF line endings, so it is forwarded verbatim.
func (t *TelnetListener) Broadcast(text string) {
	data := []byte(text)
	t.mu.Lock()
	conns := make([]net.Conn, 0, len(t.clients))
	for c := range t.clients {
		conns = append(conns, c)
	}
	t.mu.Unlock()
	for _, c := range conns {
		_, _ = c.Write(data)
	}
}

func (t *TelnetListener) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed
		}

		t.mu.Lock()
		if !t.running {
			t.mu.Unlock()
			_ = conn.Close()
			return
		}
		t.clients[conn] = struct{}{}
		n := len(t.clients)
		t.mu.Unlock()

		t.notifyCount(n)
		go t.handleClient(conn)
	}
}

func (t *TelnetListener) handleClient(conn net.Conn) {
	defer func() {
		_ = conn.Close()
		t.mu.Lock()
		delete(t.clients, conn)
		n := len(t.clients)
		t.mu.Unlock()
		t.notifyCount(n)
	}()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Local echo to this client, then forward upstream.
		fmt.Fprintf(conn, "> %s\r\n", line)
		if t.forward != nil {
			t.forward(line)
		}
	}
}

func (t *TelnetListener) notifyCount(n int) {
	if t.onCount != nil {
		t.onCount(n)
	}
}
