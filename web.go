package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"
)

//go:embed static
var staticFiles embed.FS

// WebServer serves the web UI and SSE relay endpoint.
type WebServer struct {
	addr       string
	hub        *Hub
	telnet     *TelnetServer
	terminal   *TerminalProxy
	rxCallsign string
	rxName     string
	rxLocation string
	telnetAddr string
	countries  []CountryEntry
	tmpl       *template.Template
}

// ReceiverInfo holds static data fetched from /api/description at startup.
type ReceiverInfo struct {
	Callsign string
	Name     string
	Location string
	Lat      float64
	Lon      float64
}

func NewWebServer(addr, telnetAddr string, rx ReceiverInfo, countries []CountryEntry, telnet *TelnetServer, hub *Hub) (*WebServer, error) {
	tmplData, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		return nil, fmt.Errorf("read index.html: %w", err)
	}
	tmpl, err := template.New("index").Parse(string(tmplData))
	if err != nil {
		return nil, fmt.Errorf("parse index.html template: %w", err)
	}
	return &WebServer{
		addr:       addr,
		hub:        hub,
		telnet:     telnet,
		terminal:   NewTerminalProxy(),
		rxCallsign: rx.Callsign,
		rxName:     rx.Name,
		rxLocation: rx.Location,
		telnetAddr: telnetAddr,
		countries:  countries,
		tmpl:       tmpl,
	}, nil
}

// basePath extracts the proxy prefix from the X-Forwarded-Prefix header.
// UberSDR's addon proxy sets this when strip_prefix is true.
// Returns "" when running standalone (direct access).
func basePath(r *http.Request) string {
	return strings.TrimRight(r.Header.Get("X-Forwarded-Prefix"), "/")
}

func (w *WebServer) ListenAndServe() error {
	mux := http.NewServeMux()

	// Static files — strip the /static/ prefix and serve from embedded FS sub-tree
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("embed sub: %w", err)
	}
	staticHandler := http.FileServer(http.FS(sub))
	mux.Handle("/static/", http.StripPrefix("/static/", staticHandler))

	// Root → index.html rendered with BasePath from X-Forwarded-Prefix
	mux.HandleFunc("/", w.handleIndex)

	// SSE relay
	mux.HandleFunc("/api/events", w.handleEvents)

	// WebSocket terminal proxy (bidirectional telnet-over-WebSocket)
	mux.Handle("/api/terminal", w.terminal)

	// REST history endpoints
	mux.HandleFunc("/api/spots", w.handleSpots)
	mux.HandleFunc("/api/status", w.handleStatus)
	mux.HandleFunc("/api/help", w.handleHelp)
	mux.HandleFunc("/api/countries", w.handleCountries)

	log.Printf("[web] listening on %s", w.addr)
	return http.ListenAndServe(w.addr, mux)
}

func (w *WebServer) handleIndex(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(rw, r)
		return
	}
	bp := basePath(r)
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = w.tmpl.Execute(rw, struct {
		BasePath   string
		Callsign   string
		Name       string
		Location   string
		TelnetAddr string
	}{
		BasePath:   bp,
		Callsign:   w.rxCallsign,
		Name:       w.rxName,
		Location:   w.rxLocation,
		TelnetAddr: w.telnetAddr,
	})
}

// handleEvents is the SSE relay: subscribes to the hub and streams JSON spots.
func (w *WebServer) handleEvents(rw http.ResponseWriter, r *http.Request) {
	flusher, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "streaming not supported", http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("Connection", "keep-alive")
	rw.Header().Set("X-Accel-Buffering", "no")
	rw.Header().Set("Access-Control-Allow-Origin", "*")

	// Send connected event + retry hint
	fmt.Fprintf(rw, "event: connected\ndata: {}\n\n")
	fmt.Fprintf(rw, "retry: 3000\n\n")
	flusher.Flush()

	ch := w.hub.Subscribe()
	defer w.hub.Unsubscribe(ch)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	log.Printf("[sse] client connected: %s", r.RemoteAddr)
	defer log.Printf("[sse] client disconnected: %s", r.RemoteAddr)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fmt.Fprintf(rw, "event: heartbeat\ndata: {\"ts\":\"%s\"}\n\n",
				time.Now().UTC().Format(time.RFC3339))
			flusher.Flush()
		case spot, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(spot)
			if err != nil {
				continue
			}
			fmt.Fprintf(rw, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

// handleSpots returns history from the ring buffer.
// Query params: stream=decoder|cwskimmer|voice  (optional)
func (w *WebServer) handleSpots(rw http.ResponseWriter, r *http.Request) {
	stream := StreamType(r.URL.Query().Get("stream"))
	spots := w.hub.History(stream)
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(rw).Encode(spots)
}

// handleHelp returns the telnet help text as plain text — single source of truth.
func (w *WebServer) handleHelp(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprint(rw, helpText)
}

// handleCountries returns the country list as JSON for the web UI filter.
func (w *WebServer) handleCountries(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(rw).Encode(w.countries)
}

// handleStatus returns a simple health/status JSON including telnet client count.
func (w *WebServer) handleStatus(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Access-Control-Allow-Origin", "*")

	telnetClients := 0
	if w.telnet != nil {
		telnetClients = w.telnet.ClientCount()
	}

	_ = json.NewEncoder(rw).Encode(map[string]interface{}{
		"status":         "ok",
		"ts":             time.Now().UTC().Format(time.RFC3339),
		"telnet_addr":    w.telnetAddr,
		"telnet_clients": telnetClients,
		"streams": []string{
			string(StreamDecoder),
			string(StreamCWSkimmer),
			string(StreamVoiceActivity),
			string(StreamDXCluster),
		},
	})
}
