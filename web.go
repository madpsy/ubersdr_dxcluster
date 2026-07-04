package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"time"
)

//go:embed static
var staticFiles embed.FS

// WebServer serves the web UI and SSE relay endpoint.
type WebServer struct {
	addr       string
	hub        *Hub
	basePath   string
	rxCallsign string
	rxName     string
	rxLocation string
	tmpl       *template.Template
}

// ReceiverInfo holds static data fetched from /api/description at startup.
type ReceiverInfo struct {
	Callsign string
	Name     string
	Location string
}

func NewWebServer(addr, basePath string, rx ReceiverInfo, hub *Hub) (*WebServer, error) {
	// Parse index.html as a template (for {{.BasePath}} etc. injection)
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
		basePath:   basePath,
		rxCallsign: rx.Callsign,
		rxName:     rx.Name,
		rxLocation: rx.Location,
		tmpl:       tmpl,
	}, nil
}

func (w *WebServer) ListenAndServe() error {
	mux := http.NewServeMux()

	// Root → serve index.html with BasePath injected
	mux.HandleFunc("/", w.handleIndex)

	// SSE relay
	mux.HandleFunc("/api/events", w.handleEvents)

	// REST history endpoints
	mux.HandleFunc("/api/spots", w.handleSpots)
	mux.HandleFunc("/api/status", w.handleStatus)

	// Static assets (js, css)
	mux.Handle("/static/", http.FileServer(http.FS(staticFiles)))

	log.Printf("[web] listening on %s (base path: %q)", w.addr, w.basePath)
	return http.ListenAndServe(w.addr, mux)
}

func (w *WebServer) handleIndex(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "" {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = w.tmpl.Execute(rw, struct {
		BasePath   string
		Callsign   string
		Name       string
		Location   string
	}{
		BasePath: w.basePath,
		Callsign: w.rxCallsign,
		Name:     w.rxName,
		Location: w.rxLocation,
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

	// Send connected comment + retry hint
	fmt.Fprintf(rw, ": connected to ubersdr_dxcluster\n")
	fmt.Fprintf(rw, "retry: 3000\n\n")
	flusher.Flush()

	ch := w.hub.Subscribe()
	defer w.hub.Unsubscribe(ch)

	// Heartbeat ticker
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

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
	_ = json.NewEncoder(rw).Encode(spots)
}

// handleStatus returns a simple health/status JSON.
func (w *WebServer) handleStatus(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(map[string]interface{}{
		"status": "ok",
		"ts":     time.Now().UTC().Format(time.RFC3339),
		"streams": []string{
			string(StreamDecoder),
			string(StreamCWSkimmer),
			string(StreamVoiceActivity),
		},
	})
}

