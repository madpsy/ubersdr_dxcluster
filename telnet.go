package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// callsignRe is the DX Spider is_callsign() regex translated to Go.
// Matches valid amateur callsigns including portable/mobile suffixes and SSIDs.
var callsignRe = regexp.MustCompile(`(?i)^` +
	`(?:\d?[A-Z]{1,2}\d{0,2}/)?` + // optional out-of-area prefix/
	`(?:\d?[A-Z]{1,2}\d{1,5})` + // main prefix (required)
	`[A-Z]{1,8}` + // callsign letters (required)
	`(?:-\d{1,2})?` + // optional -nn SSID
	`(?:/[0-9A-Z]{1,7})?` + // optional /suffix
	`(?:/(?:AM?|MM?|P))?` + // optional /A /AM /M /MM /P
	`$`)

// isValidCallsign returns true if s looks like a valid amateur callsign.
// Pure digit strings are rejected (matching DX Spider behaviour).
func isValidCallsign(s string) bool {
	if regexp.MustCompile(`^\d+$`).MatchString(s) {
		return false
	}
	return callsignRe.MatchString(strings.ToUpper(s))
}

// ── TelnetServer ───────────────────────────────────────────────────────────

// TelnetServer listens for DX cluster client connections and streams spots
// in standard AR-Cluster / RBN format.
type TelnetServer struct {
	addr         string
	hub          *Hub
	store        *SpotStore
	spotterCall  string
	rxName       string
	rxLocation   string
	rxLat        float64
	rxLon        float64
	ubersdrURL   string // base URL for /api/lookup (QRZ) and /api/cty calls
	clients      atomic.Int32
	version      string
	requireLogin bool
	startTime    time.Time
}

func NewTelnetServer(addr string, hub *Hub, store *SpotStore, spotterCall string, rx ReceiverInfo, ubersdrURL string, requireLogin bool) *TelnetServer {
	return &TelnetServer{
		addr:         addr,
		hub:          hub,
		store:        store,
		spotterCall:  spotterCall,
		rxName:       rx.Name,
		rxLocation:   rx.Location,
		rxLat:        rx.Lat,
		rxLon:        rx.Lon,
		ubersdrURL:   ubersdrURL,
		version:      "ubersdr_dxcluster/1.0",
		requireLogin: requireLogin,
		startTime:    time.Now(),
	}
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

// ── Connection handler ─────────────────────────────────────────────────────

func (t *TelnetServer) handleConn(conn net.Conn) {
	remote := conn.RemoteAddr().String()
	t.clients.Add(1)
	log.Printf("[telnet] client connected: %s (total: %d)", remote, t.clients.Load())
	defer func() {
		t.clients.Add(-1)
		conn.Close()
		log.Printf("[telnet] client disconnected: %s (total: %d)", remote, t.clients.Load())
	}()

	state := newClientState()

	// ── Welcome + login prompt ─────────────────────────────────────────────
	clientNum := int(t.clients.Load())
	// Extract just the IP (strip port) for display — informational only.
	clientIP := remote
	if host, _, err := net.SplitHostPort(remote); err == nil {
		clientIP = host
	}
	fmt.Fprintf(conn, "\r\nWelcome to %s UberSDR DX Cluster. You are client #%d.\r\n", t.spotterCall, clientNum)
	fmt.Fprintf(conn, "Your IP   : %s\r\n", clientIP)
	if t.rxName != "" {
		fmt.Fprintf(conn, "Receiver  : %s\r\n", t.rxName)
	}
	if t.rxLocation != "" {
		fmt.Fprintf(conn, "Location  : %s\r\n", t.rxLocation)
	}
	fmt.Fprintf(conn, "Streaming live Digital, CW, Voice and DX Cluster spots from UberSDR.\r\n\r\n")

	if t.requireLogin {
		fmt.Fprintf(conn, "Please enter your callsign: ")
		loginScanner := bufio.NewScanner(conn)
		if !loginScanner.Scan() {
			return // connection closed before login
		}
		input := strings.TrimSpace(loginScanner.Text())
		call := strings.ToUpper(input)
		if !isValidCallsign(call) {
			fmt.Fprintf(conn, "Sorry %s is an invalid callsign\r\n", input)
			log.Printf("[telnet] rejected invalid callsign %q from %s", input, remote)
			return
		}
		state.Name = call
		log.Printf("[telnet] login: %s from %s", call, remote)
	}

	// ── Banner ─────────────────────────────────────────────────────────────
	fmt.Fprintf(conn, "Hello de %s DX Cluster\r\n", t.spotterCall)
	fmt.Fprintf(conn, "Streaming live spots from UberSDR (Digital / CW / Voice / DX Cluster)\r\n")
	fmt.Fprintf(conn, "Type HELP for a full list of commands, or BYE to disconnect.\r\n\r\n")

	// Subscribe to hub
	ch := t.hub.Subscribe()
	defer t.hub.Unsubscribe(ch)

	// Read input in a separate goroutine
	quit := make(chan struct{})
	cmdCh := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			upper := strings.ToUpper(line)
			if upper == "BYE" || upper == "QUIT" {
				fmt.Fprintf(conn, "73 de UberSDR\r\n")
				close(quit)
				return
			}
			select {
			case cmdCh <- line:
			default:
			}
		}
		close(quit)
	}()

	// Stream spots and handle commands
	for {
		select {
		case <-quit:
			return

		case line := <-cmdCh:
			resp := t.handleCommand(line, state)
			if resp != "" {
				fmt.Fprintf(conn, "%s\r\n", resp)
			}

		case spot, ok := <-ch:
			if !ok {
				return
			}
			if !state.ShouldSend(spot) {
				continue
			}
			line := spot.FormatDXCluster(t.spotterCall)
			if _, err := fmt.Fprintf(conn, "%s\r\n", line); err != nil {
				return
			}
		}
	}
}
