package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultClientsDir is where the desktop client binaries live in the container
// (see Dockerfile). Overridable via the CLIENTS_DIR env var.
const defaultClientsDir = "/usr/local/share/dxcluster/clients"

// clientDownloads maps the public download path to the on-disk filename.
// Restricting to known names avoids directory listing and path traversal.
var clientDownloads = map[string]string{
	"dxcluster":     "dxcluster",     // Linux amd64
	"dxcluster.exe": "dxcluster.exe", // Windows amd64
}

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

func NewWebServer(addr, telnetAddr string, rx ReceiverInfo, countries []CountryEntry, telnet *TelnetServer, hub *Hub, wsMaxConns, wsMaxConnsPerIP int) (*WebServer, error) {
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
		terminal:   NewTerminalProxy(telnet, wsMaxConns, wsMaxConnsPerIP),
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

	// Desktop client binary downloads
	mux.HandleFunc("/clients/", w.handleClients)               // fixed names
	mux.HandleFunc("/client/download", w.handleClientDownload) // OS-detected, callsign-named

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

// clientsDir returns the directory holding the desktop client binaries.
func (w *WebServer) clientsDir() string {
	if dir := os.Getenv("CLIENTS_DIR"); dir != "" {
		return dir
	}
	return defaultClientsDir
}

// serveClientFile writes the given on-disk client binary as a download using
// downloadName as the filename presented to the browser.
func (w *WebServer) serveClientFile(rw http.ResponseWriter, r *http.Request, srcFile, downloadName string) {
	f, err := os.Open(filepath.Join(w.clientsDir(), srcFile))
	if err != nil {
		http.Error(rw, "client binary not available", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "application/octet-stream")
	rw.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", downloadName))
	http.ServeContent(rw, r, downloadName, info.ModTime(), f)
}

// handleClients serves the desktop client binaries under their fixed names.
// Only the known filenames are exposed; anything else is a 404.
func (w *WebServer) handleClients(rw http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/clients/")
	fname, ok := clientDownloads[name]
	if !ok {
		http.NotFound(rw, r)
		return
	}
	w.serveClientFile(rw, r, fname, fname)
}

// handleClientDownload serves the desktop client for the requesting OS, named
// after this instance's callsign, e.g. dxcluster_m9psy or dxcluster_m9psy.exe.
// The downloaded name is what the client parses at startup to auto-target this
// instance's callsign.
func (w *WebServer) handleClientDownload(rw http.ResponseWriter, r *http.Request) {
	srcFile, ext := "dxcluster", ""
	if detectClientOS(r) == "windows" {
		srcFile, ext = "dxcluster.exe", ".exe"
	}

	call := sanitizeCallsign(w.rxCallsign)
	if call == "" {
		call = "client"
	}
	w.serveClientFile(rw, r, srcFile, "dxcluster_"+call+ext)
}

// detectClientOS returns "windows" or "linux" for the requesting browser.
// An explicit ?os= override wins; otherwise the Sec-CH-UA-Platform client hint
// (sent by Chromium browsers) is preferred, falling back to the User-Agent.
// Non-Windows platforms map to the Linux build.
func detectClientOS(r *http.Request) string {
	if v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("os"))); v == "windows" || v == "linux" {
		return v
	}
	if p := strings.Trim(r.Header.Get("Sec-CH-UA-Platform"), `"`); p != "" {
		if strings.EqualFold(p, "Windows") {
			return "windows"
		}
		return "linux"
	}
	if strings.Contains(r.UserAgent(), "Windows") {
		return "windows"
	}
	return "linux"
}

// sanitizeCallsign lowercases a callsign and keeps only filename-safe chars
// ([a-z0-9-]), so it can be embedded in a download filename.
func sanitizeCallsign(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// handleStatus returns a simple health/status JSON including telnet client count.
func (w *WebServer) handleStatus(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Access-Control-Allow-Origin", "*")

	telnetClients := 0
	var clients []clientEntry
	if w.telnet != nil {
		telnetClients = w.telnet.ClientCount()
		clients = w.telnet.ConnectedClients()
	}
	if clients == nil {
		clients = []clientEntry{}
	}

	_ = json.NewEncoder(rw).Encode(map[string]interface{}{
		"status":             "ok",
		"ts":                 time.Now().UTC().Format(time.RFC3339),
		"telnet_addr":        w.telnetAddr,
		"telnet_clients":     telnetClients,
		"telnet_client_list": clients,
		"streams": []string{
			string(StreamDecoder),
			string(StreamCWSkimmer),
			string(StreamVoiceActivity),
			string(StreamDXCluster),
		},
	})
}
