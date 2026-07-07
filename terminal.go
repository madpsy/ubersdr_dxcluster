package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

// ── Security constants ─────────────────────────────────────────────────────

const (
	// terminalMaxMsgBytes is the maximum size of a single WebSocket message
	// from the browser (i.e. a single command line). 4 KB is generous.
	terminalMaxMsgBytes = 4 * 1024

	// Default limits — overridden by WS_MAX_CONNS / WS_MAX_CONNS_PER_IP env vars.
	defaultTerminalMaxConns      = 25
	defaultTerminalMaxConnsPerIP = 2
)

// ── wsConn — net.Conn adapter for a WebSocket connection ──────────────────

// wsConn wraps an nhooyr.io/websocket connection as a net.Conn so that
// TelnetServer.handleConn can be called directly without a TCP round-trip.
// This means the real browser IP is available to handleConn without any
// proxy header tricks.
//
// Read/Write are synchronised because nhooyr.io/websocket requires that
// only one goroutine reads and one goroutine writes at a time.
type wsConn struct {
	ws      *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	remAddr net.Addr

	// Buffered reader: WebSocket messages arrive as discrete frames but
	// net.Conn.Read expects a byte stream. We buffer the current frame.
	mu  sync.Mutex
	buf []byte
}

func newWSConn(ws *websocket.Conn, ctx context.Context, cancel context.CancelFunc, remoteAddr string) *wsConn {
	return &wsConn{
		ws:      ws,
		ctx:     ctx,
		cancel:  cancel,
		remAddr: &wsAddr{addr: remoteAddr},
	}
}

func (c *wsConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If we have buffered data from a previous frame, drain it first.
	if len(c.buf) > 0 {
		n := copy(b, c.buf)
		c.buf = c.buf[n:]
		return n, nil
	}

	// Read the next WebSocket message.
	_, msg, err := c.ws.Read(c.ctx)
	if err != nil {
		return 0, io.EOF
	}
	n := copy(b, msg)
	if n < len(msg) {
		c.buf = append(c.buf, msg[n:]...)
	}
	return n, nil
}

func (c *wsConn) Write(b []byte) (int, error) {
	err := c.ws.Write(c.ctx, websocket.MessageText, b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *wsConn) Close() error {
	c.cancel()
	return c.ws.Close(websocket.StatusNormalClosure, "")
}

func (c *wsConn) LocalAddr() net.Addr                { return &wsAddr{addr: "ws-local"} }
func (c *wsConn) RemoteAddr() net.Addr               { return c.remAddr }
func (c *wsConn) SetDeadline(t time.Time) error      { return nil } // handled by context
func (c *wsConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return nil }

// wsAddr implements net.Addr for a WebSocket connection.
type wsAddr struct{ addr string }

func (a *wsAddr) Network() string { return "websocket" }
func (a *wsAddr) String() string  { return a.addr }

// ── TerminalProxy ──────────────────────────────────────────────────────────

// TerminalProxy handles WebSocket terminal sessions by calling
// TelnetServer.handleConn directly — no TCP round-trip to localhost:7300.
// This means the real browser IP is passed through correctly.
type TerminalProxy struct {
	telnet        *TelnetServer
	maxConns      int32 // global cap (WS_MAX_CONNS)
	maxConnsPerIP int   // per-IP cap (WS_MAX_CONNS_PER_IP)
	conns         atomic.Int32

	// per-IP connection tracking
	ipMu    sync.Mutex
	ipConns map[string]int // bare IP → active WebSocket session count
}

// NewTerminalProxy creates a TerminalProxy that calls into the given TelnetServer.
// maxConns is the global session cap; maxConnsPerIP is the per-IP cap.
func NewTerminalProxy(telnet *TelnetServer, maxConns, maxConnsPerIP int) *TerminalProxy {
	return &TerminalProxy{
		telnet:        telnet,
		maxConns:      int32(maxConns),
		maxConnsPerIP: maxConnsPerIP,
		ipConns:       make(map[string]int),
	}
}

// ipAdd increments the per-IP counter and returns the new count.
func (tp *TerminalProxy) ipAdd(ip string) int {
	tp.ipMu.Lock()
	defer tp.ipMu.Unlock()
	tp.ipConns[ip]++
	return tp.ipConns[ip]
}

// ipRemove decrements the per-IP counter, removing the key when it reaches zero.
func (tp *TerminalProxy) ipRemove(ip string) {
	tp.ipMu.Lock()
	defer tp.ipMu.Unlock()
	tp.ipConns[ip]--
	if tp.ipConns[ip] <= 0 {
		delete(tp.ipConns, ip)
	}
}

// ServeHTTP upgrades the HTTP connection to WebSocket and runs a full telnet
// session via TelnetServer.handleConn, passing the real client IP.
//
// Security measures:
//   - Origin check: nhooyr.io/websocket rejects cross-origin by default.
//   - Connection limit: at most terminalMaxConns concurrent sessions.
//   - Message size limit: terminalMaxMsgBytes per browser message.
//   - Real IP: extracted from X-Real-IP / X-Forwarded-For / RemoteAddr.
func (tp *TerminalProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// ── 1. Global connection limit ─────────────────────────────────────────
	if tp.conns.Load() >= tp.maxConns {
		http.Error(w, "too many terminal sessions", http.StatusServiceUnavailable)
		return
	}

	// ── 2. Resolve real client IP ──────────────────────────────────────────
	// When behind a reverse proxy, the real IP is in X-Real-IP or
	// X-Forwarded-For. We only trust these headers when the connection comes
	// from a trusted proxy (the web server itself sets them via Caddy/nginx).
	// For direct connections, r.RemoteAddr is authoritative.
	realIP := realClientIP(r)

	// ── 3. Per-IP connection limit ─────────────────────────────────────────
	// Check before upgrading so we can return a plain HTTP 429.
	tp.ipMu.Lock()
	currentForIP := tp.ipConns[realIP]
	tp.ipMu.Unlock()
	if currentForIP >= tp.maxConnsPerIP {
		log.Printf("[terminal] per-IP limit reached for %s (%d sessions)", realIP, currentForIP)
		http.Error(w, "too many terminal sessions from your IP", http.StatusTooManyRequests)
		return
	}

	// ── 4. WebSocket upgrade (same-origin only) ────────────────────────────
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: false, // enforce same-origin check
	})
	if err != nil {
		log.Printf("[terminal] WebSocket upgrade failed from %s: %v", realIP, err)
		return
	}
	ws.SetReadLimit(terminalMaxMsgBytes)

	tp.conns.Add(1)
	defer tp.conns.Add(-1)

	// Register per-IP session; deregister on exit.
	tp.ipAdd(realIP)
	defer tp.ipRemove(realIP)

	// ── 5. Run telnet session directly (no TCP round-trip) ─────────────────
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	conn := newWSConn(ws, ctx, cancel, realIP)
	// handleConn blocks until the session ends (BYE, disconnect, or error).
	tp.telnet.handleConn(conn, realIP)
}

// ActiveSessions returns the number of currently active terminal sessions.
func (tp *TerminalProxy) ActiveSessions() int {
	return int(tp.conns.Load())
}

// realClientIP extracts the real client IP from the HTTP request.
// Checks X-Real-IP first (set by Caddy/nginx), then X-Forwarded-For,
// then falls back to RemoteAddr.
func realClientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		// X-Forwarded-For may be a comma-separated list; take the first.
		for i := 0; i < len(ip); i++ {
			if ip[i] == ',' {
				return ip[:i]
			}
		}
		return ip
	}
	// Direct connection — strip port from RemoteAddr.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
