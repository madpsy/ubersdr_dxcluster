package main

import (
	"bufio"
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// clientEntry holds per-connection tracking data for the status tooltip.
type clientEntry struct {
	MaskedIP    string    `json:"ip"`
	Callsign    string    `json:"callsign"`     // empty until login completes
	ConnectedAt time.Time `json:"connected_at"` // when the connection was established
}

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
	rxCallsign   string
	rxName       string
	rxLocation   string
	rxLat        float64
	rxLon        float64
	ubersdrURL   string // base URL for /api/lookup (QRZ) and /api/cty calls
	clients      atomic.Int32
	version      string
	requireLogin bool
	spotPassword string // SPOT_PASSWORD env var; empty = spot submission disabled
	startTime    time.Time

	// connected client tracking (one entry per active connection)
	clientsMu  sync.RWMutex
	clientMap  map[uint64]*clientEntry // connID → entry
	nextConnID atomic.Uint64
}

func NewTelnetServer(addr string, hub *Hub, store *SpotStore, spotterCall string, rx ReceiverInfo, ubersdrURL string, requireLogin bool, spotPassword string) *TelnetServer {
	return &TelnetServer{
		addr:         addr,
		hub:          hub,
		store:        store,
		spotterCall:  spotterCall,
		rxCallsign:   rx.Callsign,
		rxName:       rx.Name,
		rxLocation:   rx.Location,
		rxLat:        rx.Lat,
		rxLon:        rx.Lon,
		ubersdrURL:   ubersdrURL,
		version:      "ubersdr_dxcluster/1.0",
		requireLogin: requireLogin,
		spotPassword: spotPassword,
		startTime:    time.Now(),
		clientMap:    make(map[uint64]*clientEntry),
	}
}

// ClientCount returns the number of currently connected telnet clients.
func (t *TelnetServer) ClientCount() int {
	return int(t.clients.Load())
}

// ConnectedClients returns a snapshot of all currently connected clients.
func (t *TelnetServer) ConnectedClients() []clientEntry {
	t.clientsMu.RLock()
	defer t.clientsMu.RUnlock()
	out := make([]clientEntry, 0, len(t.clientMap))
	for _, e := range t.clientMap {
		out = append(out, *e)
	}
	return out
}

// maskIP replaces the identifying octets of an IP with "xxx" for privacy.
// IPv4 a.b.c.d  → xxx.xxx.c.d
// IPv6           → xxxx:xxxx:…:last (all groups but last two replaced)
func maskIP(ip string) string {
	// Try IPv4
	parts := strings.Split(ip, ".")
	if len(parts) == 4 {
		return "xxx.xxx." + parts[2] + "." + parts[3]
	}
	// IPv6 — expand and mask all but last two groups
	groups := strings.Split(ip, ":")
	if len(groups) >= 2 {
		masked := make([]string, len(groups))
		for i := range groups {
			if i < len(groups)-2 {
				masked[i] = "xxxx"
			} else {
				masked[i] = groups[i]
			}
		}
		return strings.Join(masked, ":")
	}
	// Fallback: return as-is
	return ip
}

// registerConn adds a new connection entry and returns its unique ID.
func (t *TelnetServer) registerConn(ip string) uint64 {
	id := t.nextConnID.Add(1)
	entry := &clientEntry{MaskedIP: maskIP(ip), ConnectedAt: time.Now()}
	t.clientsMu.Lock()
	t.clientMap[id] = entry
	t.clientsMu.Unlock()
	return id
}

// formatDuration formats a duration as "Xh Ym Zs", omitting leading zero units.
// e.g. 3723s → "1h 2m 3s", 65s → "1m 5s", 45s → "45s".
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// setConnCallsign updates the callsign for a connection after login.
func (t *TelnetServer) setConnCallsign(id uint64, callsign string) {
	t.clientsMu.Lock()
	if e, ok := t.clientMap[id]; ok {
		e.Callsign = callsign
	}
	t.clientsMu.Unlock()
}

// unregisterConn removes a connection entry.
func (t *TelnetServer) unregisterConn(id uint64) {
	t.clientsMu.Lock()
	delete(t.clientMap, id)
	t.clientsMu.Unlock()
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
		go t.handleConn(conn, conn.RemoteAddr().String())
	}
}

// ── Connection handler ─────────────────────────────────────────────────────

// handleConn handles a single telnet client connection.
// remoteAddr is the client's address string — passed explicitly so that the
// WebSocket proxy can supply the real browser IP rather than 127.0.0.1.
func (t *TelnetServer) handleConn(conn net.Conn, remoteAddr string) {
	remote := remoteAddr
	t.clients.Add(1)

	// Extract bare IP for tracking (strip port if present)
	clientIP := remote
	if host, _, err := net.SplitHostPort(remote); err == nil {
		clientIP = host
	}
	connID := t.registerConn(clientIP)

	log.Printf("[telnet] client connected: %s (total: %d)", remote, t.clients.Load())
	defer func() {
		t.clients.Add(-1)
		t.unregisterConn(connID)
		conn.Close()
		log.Printf("[telnet] client disconnected: %s (total: %d)", remote, t.clients.Load())
	}()

	state := newClientState()

	// ── Welcome + login prompt ─────────────────────────────────────────────
	clientNum := int(t.clients.Load())
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
		// Start a 15-second login timer. If the client does not supply a valid
		// callsign within that window the timer goroutine sends a timeout
		// message and closes the connection, which causes loginScanner.Scan()
		// to return false and handleConn to return.
		// The timer is stopped immediately on successful login so it never
		// fires for well-behaved clients.
		loginDone := make(chan struct{})
		loginTimer := time.AfterFunc(15*time.Second, func() {
			select {
			case <-loginDone:
				return // login already completed
			default:
			}
			log.Printf("[telnet] login timeout from %s", remote)
			fmt.Fprintf(conn, "\r\nLogin timeout. Disconnecting.\r\n")
			conn.Close()
		})

		fmt.Fprintf(conn, "Please enter your callsign: ")
		loginScanner := bufio.NewScanner(conn)
		loggedIn := loginScanner.Scan()
		loginTimer.Stop()
		close(loginDone)

		if !loggedIn {
			return // connection closed (by timeout or client)
		}
		input := strings.TrimSpace(loginScanner.Text())
		// Support "CALLSIGN PASSWORD" on a single login line so that logging
		// software can authenticate in one step (e.g. "MM3NDH mysecret").
		// The callsign is always the first token; the optional second token is
		// treated as a spot-submission password attempt.
		loginTokens := strings.Fields(input)
		call := strings.ToUpper(loginTokens[0])
		if !isValidCallsign(call) {
			fmt.Fprintf(conn, "Sorry %s is an invalid callsign\r\n", loginTokens[0])
			log.Printf("[telnet] rejected invalid callsign %q from %s", loginTokens[0], remote)
			return
		}
		state.Name = call
		t.setConnCallsign(connID, call)

		// If a second token was supplied and spot submission is enabled,
		// attempt password authentication silently. A wrong password is not
		// an error — the user still connects, just without spot-submission rights.
		if len(loginTokens) >= 2 && t.spotPassword != "" {
			given := []byte(loginTokens[1])
			want := []byte(t.spotPassword)
			if subtle.ConstantTimeCompare(given, want) == 1 {
				state.CanSpot = true
				log.Printf("[telnet] login: %s from %s (spot submission authenticated)", call, remote)
			} else {
				log.Printf("[telnet] login: %s from %s (spot password incorrect)", call, remote)
			}
		} else {
			log.Printf("[telnet] login: %s from %s", call, remote)
		}
	}

	// ── Banner ─────────────────────────────────────────────────────────────
	fmt.Fprintf(conn, "Hello de %s DX Cluster\r\n", t.spotterCall)
	fmt.Fprintf(conn, "Streaming live spots from UberSDR (Digital / CW / Voice / DX Cluster)\r\n")
	fmt.Fprintf(conn, "Type HELP for a full list of commands, or BYE to disconnect.\r\n")
	fmt.Fprintf(conn, "Digital decoder and upstream DX cluster spots are disabled by default.\r\n")
	fmt.Fprintf(conn, "To enable: SET/DIGITAL and/or SET/DXCLUSTER\r\n")
	// If spot submission is enabled and the user is logged in, show an appropriate hint.
	if t.spotPassword != "" && state.Name != "" {
		if state.CanSpot {
			// Already authenticated via the login line password — confirm it.
			fmt.Fprintf(conn, "Spot submission enabled. Use: DX <freq_kHz> <callsign> [comment]\r\n")
		} else {
			// Not yet authenticated — tell them how to enable it.
			fmt.Fprintf(conn, "To submit spots: SET/SPOTPASS <password> then DX <freq_kHz> <callsign> [comment]\r\n")
		}
	}
	fmt.Fprintf(conn, "\r\n")

	// Subscribe to hub
	ch := t.hub.Subscribe()
	defer t.hub.Unsubscribe(ch)

	// ── QRZ welcome lookup (concurrent, non-blocking) ──────────────────────
	// Fire the lookup immediately after the banner so the client can send
	// commands freely while the lookup is in flight. The result is injected
	// into the output stream as a fourth select case when it arrives (≤2s).
	// qrzCh is nilled after first use so the case never fires again.
	var qrzCh <-chan *qrzLookupResult
	if t.ubersdrURL != "" && state.Name != "" {
		c := make(chan *qrzLookupResult, 1)
		qrzCh = c
		go func() {
			r, _ := lookupQRZ(t.ubersdrURL, state.Name)
			c <- r // nil on error / not-found
		}()
		// Hard 2-second timeout: send nil if the lookup goroutine hasn't fired yet.
		go func() {
			time.Sleep(2 * time.Second)
			select {
			case c <- nil: // only sends if lookup goroutine hasn't already
			default:
			}
		}()
	}

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

		case r := <-qrzCh:
			// Fire exactly once: nil the channel so this case never triggers again.
			qrzCh = nil
			if r != nil {
				fmt.Fprintf(conn, "%s\r\n", formatLoginWelcome(r))
			}

		case line := <-cmdCh:
			// Some clients re-send the callsign as a command after login
			// (e.g. logging software that always sends the callsign on connect).
			// Silently ignore it rather than returning "unknown command".
			if state.Name != "" && strings.EqualFold(strings.TrimSpace(line), state.Name) {
				continue
			}
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
