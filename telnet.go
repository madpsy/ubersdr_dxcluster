package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"regexp"
	"strconv"
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

// TelnetServer listens for DX cluster client connections and streams spots
// in standard AR-Cluster / RBN format.
type TelnetServer struct {
	addr         string
	hub          *Hub
	spotterCall  string
	rxName       string
	rxLocation   string
	clients      atomic.Int32
	version      string
	requireLogin bool
}

func NewTelnetServer(addr string, hub *Hub, spotterCall string, rx ReceiverInfo, requireLogin bool) *TelnetServer {
	return &TelnetServer{
		addr:         addr,
		hub:          hub,
		spotterCall:  spotterCall,
		rxName:       rx.Name,
		rxLocation:   rx.Location,
		version:      "ubersdr_dxcluster/1.0",
		requireLogin: requireLogin,
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

// ── Per-connection state ───────────────────────────────────────────────────

// ClientState holds per-connection state including filters and toggles.
type ClientState struct {
	Filter       ClientFilter
	WantAll      bool // receive any spots at all (set/spots / unset/spots)
	WantDigital  bool // receive digital decoder spots
	WantRBN      bool // receive CW/RBN spots (set/rbn / unset/rbn)
	WantVoice    bool // receive voice activity spots
	WantDXCluster bool // receive DX cluster spots
	Name         string
}

func newClientState() *ClientState {
	return &ClientState{
		WantAll:       true,
		WantDigital:   true,
		WantRBN:       true,
		WantVoice:     true,
		WantDXCluster: true,
	}
}

// ShouldSend returns true if the spot should be sent to this client.
func (s *ClientState) ShouldSend(spot Spot) bool {
	if !s.WantAll {
		return false
	}
	switch spot.Stream {
	case StreamDecoder:
		if !s.WantDigital {
			return false
		}
	case StreamCWSkimmer:
		if !s.WantRBN {
			return false
		}
	case StreamVoiceActivity:
		if !s.WantVoice {
			return false
		}
	case StreamDXCluster:
		if !s.WantDXCluster {
			return false
		}
	}
	return s.Filter.Match(spot)
}

// ── Per-connection filter ──────────────────────────────────────────────────

// ClientFilter holds the active filters for one telnet connection.
// All non-empty slices are OR-matched within the field; all fields are AND-matched.
type ClientFilter struct {
	Bands     []string     // band labels, e.g. ["40m","20m"]
	Modes     []string     // mode strings, e.g. ["FT8","FT4"] or ["CW"] or ["USB"]
	Types     []StreamType // stream types, e.g. [StreamCWSkimmer]
	Conts     []string     // continent codes, e.g. ["EU","NA"]
	Countries []string     // ISO 3166-1 alpha-2, e.g. ["DE","G"]
	CallPfx   []string     // callsign prefixes, e.g. ["DL","VK"]
	MinSNR    *float64
	MaxSNR    *float64
}

// Match returns true if the spot passes all active filters.
func (f *ClientFilter) Match(s Spot) bool {
	if len(f.Bands) > 0 && !containsCI(f.Bands, s.Band) {
		return false
	}
	if len(f.Modes) > 0 {
		spotMode := s.Mode
		if s.Stream == StreamCWSkimmer {
			spotMode = "CW"
		} else if s.Stream == StreamVoiceActivity {
			spotMode = s.VoiceMode
		}
		if !containsCI(f.Modes, spotMode) {
			return false
		}
	}
	if len(f.Types) > 0 {
		found := false
		for _, t := range f.Types {
			if s.Stream == t {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(f.Conts) > 0 && !containsCI(f.Conts, s.Continent) {
		return false
	}
	if len(f.Countries) > 0 && !containsCI(f.Countries, s.CountryCode) {
		return false
	}
	if len(f.CallPfx) > 0 {
		matched := false
		for _, pfx := range f.CallPfx {
			if strings.HasPrefix(strings.ToUpper(s.Callsign), strings.ToUpper(pfx)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if f.MinSNR != nil && s.SNR < *f.MinSNR {
		return false
	}
	if f.MaxSNR != nil && s.SNR > *f.MaxSNR {
		return false
	}
	return true
}

func (f *ClientFilter) Summary() string {
	var lines []string
	if len(f.Bands) > 0 {
		lines = append(lines, fmt.Sprintf("  band    : %s", strings.Join(f.Bands, ", ")))
	}
	if len(f.Modes) > 0 {
		lines = append(lines, fmt.Sprintf("  mode    : %s", strings.Join(f.Modes, ", ")))
	}
	if len(f.Types) > 0 {
		ts := make([]string, len(f.Types))
		for i, t := range f.Types {
			ts[i] = string(t)
		}
		lines = append(lines, fmt.Sprintf("  type    : %s", strings.Join(ts, ", ")))
	}
	if len(f.Conts) > 0 {
		lines = append(lines, fmt.Sprintf("  cont    : %s", strings.Join(f.Conts, ", ")))
	}
	if len(f.Countries) > 0 {
		lines = append(lines, fmt.Sprintf("  country : %s", strings.Join(f.Countries, ", ")))
	}
	if len(f.CallPfx) > 0 {
		lines = append(lines, fmt.Sprintf("  call    : %s", strings.Join(f.CallPfx, ", ")))
	}
	if f.MinSNR != nil {
		lines = append(lines, fmt.Sprintf("  snr     : >= %.1f dB", *f.MinSNR))
	}
	if f.MaxSNR != nil {
		lines = append(lines, fmt.Sprintf("  maxsnr  : <= %.1f dB", *f.MaxSNR))
	}
	if len(lines) == 0 {
		return "No active filters — receiving all spots."
	}
	return "Active filters:\r\n" + strings.Join(lines, "\r\n")
}

// ── Help text ──────────────────────────────────────────────────────────────

const helpText = `
UberSDR DX Cluster — Command Reference
=======================================

Commands can be abbreviated: SET/FILTER → SET/F, SHOW/DX → SH/DX, etc.

FILTERING  (filters are AND-combined; multiple values within a field are OR-combined)
  set/filter band <bands>       Filter by band (comma-separated)
                                  e.g. set/filter band 20m
                                       set/filter band 40m,20m,15m
                                  Bands: 160m 80m 60m 40m 30m 20m 17m 15m 12m 10m 6m

  set/filter mode <modes>       Filter by mode (comma-separated)
                                  Digital: FT8 FT4 WSPR JS8 FT2
                                  CW:      CW
                                  Voice:   USB LSB
                                  e.g. set/filter mode FT8
                                       set/filter mode FT8,FT4,WSPR

  set/filter type <types>       Filter by activity type (comma-separated)
                                  Types: digital  cw  voice
                                  e.g. set/filter type cw
                                       set/filter type digital,cw

  set/filter cont <conts>       Filter by continent (comma-separated)
                                  Codes: EU NA SA AF AS OC AN
                                  e.g. set/filter cont EU
                                       set/filter cont EU,NA

  set/filter country <codes>    Filter by country ISO 3166-1 alpha-2 (comma-separated)
                                  e.g. set/filter country DE
                                       set/filter country DE,PA,ON

  set/filter call <prefixes>    Filter by callsign prefix (comma-separated)
                                  e.g. set/filter call DL
                                       set/filter call DL,VK,ZL

  set/filter snr <dB>           Minimum SNR threshold
                                  e.g. set/filter snr 10

  set/filter maxsnr <dB>        Maximum SNR threshold
                                  e.g. set/filter maxsnr 30

CLEARING FILTERS
  clear/filter                  Clear ALL active filters
  clear/filter band             Clear band filter only
  clear/filter mode             Clear mode filter only
  clear/filter type             Clear type filter only
  clear/filter cont             Clear continent filter only
  clear/filter country          Clear country filter only
  clear/filter call             Clear callsign prefix filter only
  clear/filter snr              Clear minimum SNR filter
  clear/filter maxsnr           Clear maximum SNR filter

SPOT STREAM TOGGLES  (each stream can be enabled/disabled independently)
  set/dx                        Enable ALL spots (DX Spider compat, default: on)
  unset/dx                      Disable ALL spots

  set/digital                   Enable digital decoder spots (FT8/FT4/WSPR/JS8, default: on)
  unset/digital                 Disable digital decoder spots

  set/rbn                       Enable CW/RBN skimmer spots (default: on)
  unset/rbn                     Disable CW/RBN skimmer spots
  set/skimmer                   Alias for set/rbn
  unset/skimmer                 Alias for unset/rbn

  set/voice                     Enable voice activity spots (default: on)
  unset/voice                   Disable voice activity spots

  set/dxcluster                 Enable DX cluster spots from DX Spider (default: on)
  unset/dxcluster               Disable DX cluster spots
  set/cluster                   Alias for set/dxcluster
  unset/cluster                 Alias for unset/dxcluster

DX SPIDER COMPATIBILITY (mapped to set/filter)
  accept/spots on <band>        Accept spots on band  → set/filter band <band>
  accept/spots call <prefix>    Accept spots by call  → set/filter call <prefix>
  accept/spots cont <cont>      Accept spots by cont  → set/filter cont <cont>
  accept/rbn on <band>          Accept RBN on band    → set/filter band + type cw
  reject/spots ...              Silently accepted (reject filters not supported)

INFORMATION
  show/filter                   Show all currently active filters
  show/dx [N]                   Show last N spots from history (default: 20)
  show/time                     Show current UTC time
  show/version                  Show cluster software version
  help [<command>]              Show this help text

SESSION
  bye / quit                    Disconnect from the cluster

NOTES
  - Multiple values in a filter are OR-matched (e.g. band 40m,20m = 40m OR 20m)
  - Different filter fields are AND-matched (band AND mode AND country etc.)
  - Callsign prefix matching is case-insensitive prefix match (DL matches DL1ABC)
  - Country codes are exact ISO 3166-1 alpha-2 match (case-insensitive)
  - Filters persist for the duration of your connection only

`

// ── Abbreviation expansion ─────────────────────────────────────────────────

// expandAbbrev expands common DX Spider command abbreviations to their full form.
func expandAbbrev(cmd string) string {
	// Normalise to lower case for matching
	lc := strings.ToLower(cmd)

	// Exact abbreviations
	abbrevs := map[string]string{
		"sh/dx":      "show/dx",
		"sh/f":       "show/filter",
		"sh/filter":  "show/filter",
		"sh/time":    "show/time",
		"sh/ver":     "show/version",
		"sh/version": "show/version",
		"set/f":      "set/filter",
		"clr/f":      "clear/filter",
		"clr/filter": "clear/filter",
		"acc/spots":  "accept/spots",
		"acc/rbn":    "accept/rbn",
		"rej/spots":  "reject/spots",
		"rej/rbn":    "reject/rbn",
	}
	if full, ok := abbrevs[lc]; ok {
		return full
	}
	return lc
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
	fmt.Fprintf(conn, "\r\nWelcome to %s UberSDR DX Cluster. You are client #%d.\r\n", t.spotterCall, clientNum)
	if t.rxName != "" {
		fmt.Fprintf(conn, "Receiver  : %s\r\n", t.rxName)
	}
	if t.rxLocation != "" {
		fmt.Fprintf(conn, "Location  : %s\r\n", t.rxLocation)
	}
	fmt.Fprintf(conn, "Streaming live Digital, CW and Voice spots from UberSDR.\r\n\r\n")

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
	fmt.Fprintf(conn, "Streaming live spots from UberSDR (Digital / CW / Voice)\r\n")
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

// handleCommand parses and executes a single command line, returning the response string.
func (t *TelnetServer) handleCommand(line string, state *ClientState) string {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return ""
	}

	// Expand abbreviations on the command word
	cmd := expandAbbrev(parts[0])

	switch cmd {
	// ── Help ──────────────────────────────────────────────────────────────
	case "help", "?":
		return strings.ReplaceAll(helpText, "\n", "\r\n")

	// ── Show commands ──────────────────────────────────────────────────────
	case "show/filter":
		return state.Filter.Summary()

	case "show/time":
		return fmt.Sprintf("UTC: %s", time.Now().UTC().Format("2006-01-02 15:04:05"))

	case "show/version":
		return fmt.Sprintf("UberSDR DX Cluster — %s", t.version)

	case "show/dx":
		n := 20
		if len(parts) >= 2 {
			if v, err := strconv.Atoi(parts[1]); err == nil && v > 0 {
				if v > 200 {
					v = 200
				}
				n = v
			}
		}
		history := t.hub.History("")
		var lines []string
		for _, s := range history {
			if state.ShouldSend(s) {
				lines = append(lines, s.FormatDXCluster(t.spotterCall))
				if len(lines) >= n {
					break
				}
			}
		}
		if len(lines) == 0 {
			return "No spots in history matching current filters."
		}
		return strings.Join(lines, "\r\n")

	// ── Stream toggles ─────────────────────────────────────────────────────
	case "set/dx":
		// DX Spider compat: set/dx means "enable all spots"
		state.WantAll = true
		return "All spots enabled."
	case "unset/dx":
		state.WantAll = false
		return "All spots disabled."

	case "set/digital":
		state.WantDigital = true
		return "Digital decoder spots enabled."
	case "unset/digital":
		state.WantDigital = false
		return "Digital decoder spots disabled."

	case "set/rbn", "set/skimmer":
		state.WantRBN = true
		return "CW/RBN spots enabled."
	case "unset/rbn", "unset/skimmer":
		state.WantRBN = false
		return "CW/RBN spots disabled."

	case "set/voice":
		state.WantVoice = true
		return "Voice activity spots enabled."
	case "unset/voice":
		state.WantVoice = false
		return "Voice activity spots disabled."

	case "set/dxcluster", "set/cluster":
		state.WantDXCluster = true
		return "DX cluster spots enabled."
	case "unset/dxcluster", "unset/cluster":
		state.WantDXCluster = false
		return "DX cluster spots disabled."

	// ── DX Spider accept/reject compatibility ──────────────────────────────
	// accept/spots on <band>  → set/filter band <band>
	// accept/spots call <pfx> → set/filter call <pfx>
	// accept/spots cont <c>   → set/filter cont <c>
	// accept/rbn on <band>    → set/filter band <band> + type cw
	// reject/spots ...        → silently accepted (not supported)
	// reject/rbn ...          → silently accepted
	case "accept/spots", "accept/rbn":
		if len(parts) < 3 {
			return "OK" // silently accept malformed
		}
		field := strings.ToLower(parts[1])
		val := parts[2]
		vals := splitComma(val)
		switch field {
		case "on", "freq":
			state.Filter.Bands = vals
			if cmd == "accept/rbn" {
				state.Filter.Types = []StreamType{StreamCWSkimmer}
			}
			return fmt.Sprintf("Filter set: band = %s", strings.Join(vals, ", "))
		case "call":
			state.Filter.CallPfx = upperAll(vals)
			return fmt.Sprintf("Filter set: call = %s", strings.Join(state.Filter.CallPfx, ", "))
		case "cont":
			state.Filter.Conts = upperAll(vals)
			return fmt.Sprintf("Filter set: cont = %s", strings.Join(state.Filter.Conts, ", "))
		case "all":
			// accept/spots all → clear filters
			state.Filter = ClientFilter{}
			return "All filters cleared."
		default:
			return "OK"
		}

	case "reject/spots", "reject/rbn":
		// We don't support reject filters — silently accept to avoid logging software errors
		return "OK"

	// ── set/filter ─────────────────────────────────────────────────────────
	case "set/filter":
		if len(parts) < 3 {
			return "Usage: set/filter <field> <value[,value...]>"
		}
		field := strings.ToLower(parts[1])
		val := parts[2]
		vals := splitComma(val)
		switch field {
		case "band":
			state.Filter.Bands = vals
			return fmt.Sprintf("Filter set: band = %s", strings.Join(vals, ", "))
		case "mode":
			state.Filter.Modes = upperAll(vals)
			return fmt.Sprintf("Filter set: mode = %s", strings.Join(state.Filter.Modes, ", "))
		case "type":
			state.Filter.Types = parseTypes(vals)
			ts := make([]string, len(state.Filter.Types))
			for i, t := range state.Filter.Types {
				ts[i] = string(t)
			}
			return fmt.Sprintf("Filter set: type = %s", strings.Join(ts, ", "))
		case "cont":
			state.Filter.Conts = upperAll(vals)
			return fmt.Sprintf("Filter set: cont = %s", strings.Join(state.Filter.Conts, ", "))
		case "country":
			state.Filter.Countries = upperAll(vals)
			return fmt.Sprintf("Filter set: country = %s", strings.Join(state.Filter.Countries, ", "))
		case "call":
			state.Filter.CallPfx = upperAll(vals)
			return fmt.Sprintf("Filter set: call = %s", strings.Join(state.Filter.CallPfx, ", "))
		case "snr":
			v, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return "Invalid SNR value — use a number, e.g. set/filter snr 10"
			}
			state.Filter.MinSNR = &v
			return fmt.Sprintf("Filter set: snr >= %.1f dB", v)
		case "maxsnr":
			v, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return "Invalid SNR value — use a number, e.g. set/filter maxsnr 30"
			}
			state.Filter.MaxSNR = &v
			return fmt.Sprintf("Filter set: maxsnr <= %.1f dB", v)
		default:
			return fmt.Sprintf("Unknown filter field %q — type HELP for usage", field)
		}

	// ── clear/filter ───────────────────────────────────────────────────────
	case "clear/filter":
		if len(parts) == 1 {
			state.Filter = ClientFilter{}
			return "All filters cleared."
		}
		field := strings.ToLower(parts[1])
		switch field {
		case "band":
			state.Filter.Bands = nil
			return "Filter cleared: band"
		case "mode":
			state.Filter.Modes = nil
			return "Filter cleared: mode"
		case "type":
			state.Filter.Types = nil
			return "Filter cleared: type"
		case "cont":
			state.Filter.Conts = nil
			return "Filter cleared: cont"
		case "country":
			state.Filter.Countries = nil
			return "Filter cleared: country"
		case "call":
			state.Filter.CallPfx = nil
			return "Filter cleared: call"
		case "snr":
			state.Filter.MinSNR = nil
			return "Filter cleared: snr"
		case "maxsnr":
			state.Filter.MaxSNR = nil
			return "Filter cleared: maxsnr"
		default:
			return fmt.Sprintf("Unknown filter field %q — type HELP for usage", field)
		}

	default:
		return fmt.Sprintf("Unknown command %q — type HELP for a list of commands", parts[0])
	}
}

// ── Helpers ────────────────────────────────────────────────────────────────

func containsCI(slice []string, val string) bool {
	v := strings.ToUpper(val)
	for _, s := range slice {
		if strings.ToUpper(s) == v {
			return true
		}
	}
	return false
}

func splitComma(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func upperAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToUpper(s)
	}
	return out
}

func parseTypes(vals []string) []StreamType {
	var out []StreamType
	for _, v := range vals {
		switch strings.ToLower(v) {
		case "digital", "decoder":
			out = append(out, StreamDecoder)
		case "cw", "cwskimmer", "rbn":
			out = append(out, StreamCWSkimmer)
		case "voice", "voiceactivity":
			out = append(out, StreamVoiceActivity)
		case "dx", "dxcluster", "cluster":
			out = append(out, StreamDXCluster)
		}
	}
	return out
}
