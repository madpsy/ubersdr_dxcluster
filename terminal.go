package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

// ── Security constants ─────────────────────────────────────────────────────

const (
	// terminalTarget is the only TCP address the proxy will ever connect to.
	// Hardcoded — never derived from user input — to prevent SSRF.
	terminalTarget = "localhost:7300"

	// terminalMaxConns is the maximum number of simultaneous WebSocket
	// terminal sessions. Prevents connection-flood DoS.
	terminalMaxConns = 10

	// terminalMaxMsgBytes is the maximum size of a single WebSocket message
	// from the browser (i.e. a single command line). Commands are short;
	// 4 KB is generous.
	terminalMaxMsgBytes = 4 * 1024

	// terminalIdleTimeout is the maximum time a connection may be idle
	// (no data in either direction) before it is closed.
	terminalIdleTimeout = 5 * time.Minute

	// terminalWriteTimeout is the deadline for a single write to the TCP
	// telnet connection. Prevents slow-write stalls from blocking the proxy.
	terminalWriteTimeout = 10 * time.Second
)

// ── TerminalProxy ──────────────────────────────────────────────────────────

// TerminalProxy handles WebSocket → TCP proxying to the local telnet server.
// It is safe for concurrent use.
type TerminalProxy struct {
	conns atomic.Int32 // current active WebSocket sessions
}

// NewTerminalProxy creates a ready-to-use TerminalProxy.
func NewTerminalProxy() *TerminalProxy {
	return &TerminalProxy{}
}

// ServeHTTP upgrades the HTTP connection to WebSocket and proxies it to the
// local telnet server at terminalTarget.
//
// Security measures applied:
//   - Origin check: nhooyr.io/websocket rejects cross-origin requests by
//     default unless OriginPatterns is set. We allow same-origin only.
//   - Connection limit: at most terminalMaxConns concurrent sessions.
//   - Message size limit: terminalMaxMsgBytes per browser message.
//   - Idle timeout: terminalIdleTimeout closes stale connections.
//   - Write deadline: terminalWriteTimeout on every TCP write.
//   - Fixed target: always connects to localhost:7300, never user-supplied.
func (tp *TerminalProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// ── 1. Connection limit ────────────────────────────────────────────────
	if tp.conns.Load() >= terminalMaxConns {
		http.Error(w, "too many terminal sessions", http.StatusServiceUnavailable)
		return
	}

	// ── 2. WebSocket upgrade (same-origin only) ────────────────────────────
	// nhooyr.io/websocket checks the Origin header against the Host header
	// by default, rejecting cross-origin requests. We do not set
	// OriginPatterns, so only same-origin connections are accepted.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Subprotocols: none required for plain-text terminal
		// OriginPatterns: not set → same-origin only (default behaviour)
		InsecureSkipVerify: false, // enforce origin check
	})
	if err != nil {
		// Accept already wrote the HTTP error response
		log.Printf("[terminal] WebSocket upgrade failed from %s: %v", r.RemoteAddr, err)
		return
	}
	ws.SetReadLimit(terminalMaxMsgBytes)

	// ── 3. Connect to local telnet server ─────────────────────────────────
	// Target is hardcoded — never derived from user input.
	tcpConn, err := net.DialTimeout("tcp", terminalTarget, 5*time.Second)
	if err != nil {
		log.Printf("[terminal] cannot connect to telnet server: %v", err)
		ws.Close(websocket.StatusInternalError, "telnet server unavailable")
		return
	}

	tp.conns.Add(1)
	remote := r.RemoteAddr
	log.Printf("[terminal] session opened from %s (active: %d)", remote, tp.conns.Load())
	defer func() {
		tp.conns.Add(-1)
		tcpConn.Close()
		log.Printf("[terminal] session closed from %s (active: %d)", remote, tp.conns.Load())
	}()

	// ── 4. Bidirectional proxy ─────────────────────────────────────────────
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// TCP → WebSocket: forward telnet server output to the browser.
	// Runs in a goroutine; cancels the context on error/EOF.
	go func() {
		defer cancel()
		buf := make([]byte, 4096)
		for {
			// Reset idle deadline on each read
			_ = tcpConn.SetReadDeadline(time.Now().Add(terminalIdleTimeout))
			n, err := tcpConn.Read(buf)
			if n > 0 {
				if werr := ws.Write(ctx, websocket.MessageText, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("[terminal] TCP read error from %s: %v", remote, err)
				}
				return
			}
		}
	}()

	// WebSocket → TCP: forward browser input to the telnet server.
	// Runs in the main goroutine; cancels the context on error/close.
	for {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			// Normal close or context cancelled — not an error worth logging
			return
		}
		// Enforce write deadline to prevent slow-write stalls
		_ = tcpConn.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
		if _, err := tcpConn.Write(msg); err != nil {
			log.Printf("[terminal] TCP write error to %s: %v", remote, err)
			return
		}
	}
}

// ActiveSessions returns the number of currently active terminal sessions.
func (tp *TerminalProxy) ActiveSessions() int {
	return int(tp.conns.Load())
}
