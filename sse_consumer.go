package main

import (
	"bufio"
	"context"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	backoffMin = 1 * time.Second
	backoffMax = 60 * time.Second
)

// StreamConfig describes one upstream SSE endpoint.
type StreamConfig struct {
	Name   string
	Path   string // e.g. /api/decoder/stream
	Parse  func([]byte) (*Spot, error)
	Stream StreamType
	// BaseURL overrides the UberSDR base URL, for streams served by another
	// addon (the SSTV addon's gallery feed).
	BaseURL string
	// Event, when set, keeps only SSE events with this "event:" name. Streams
	// that send just "data:" lines leave it empty.
	Event string
}

// StartConsumers launches one goroutine per upstream SSE stream.
// Each goroutine reconnects with exponential back-off on failure.
// sstvURL is the SSTV addon's base URL; empty disables the SSTV stream. The
// caller passes it only when the receiver lists the addon as enabled, so a
// receiver without it never has a consumer retrying a host that isn't there.
func StartConsumers(ctx context.Context, baseURL, sstvURL string, sstvDedup time.Duration, hub *Hub) {
	streams := []StreamConfig{
		{
			Name:   "decoder",
			Path:   "/api/decoder/stream",
			Parse:  parseDecoder,
			Stream: StreamDecoder,
		},
		{
			Name:   "cwskimmer",
			Path:   "/api/cwskimmer/stream",
			Parse:  parseCWSpot,
			Stream: StreamCWSkimmer,
		},
		{
			Name:   "voice-activity",
			Path:   "/api/voice-activity/stream",
			Parse:  parseVoiceActivity,
			Stream: StreamVoiceActivity,
		},
		{
			Name:   "dxcluster",
			Path:   "/api/dxcluster/stream",
			Parse:  parseDXSpot,
			Stream: StreamDXCluster,
		},
	}
	if sstvURL != "" {
		streams = append(streams, StreamConfig{
			Name:    "sstv",
			Path:    "/api/live",
			Parse:   newSSTVParser(baseURL, sstvDedup),
			Stream:  StreamSSTV,
			BaseURL: sstvURL,
			Event:   "image",
		})
	}

	for _, sc := range streams {
		go consumeStream(ctx, baseURL, sc, hub)
	}
}

func consumeStream(ctx context.Context, baseURL string, sc StreamConfig, hub *Hub) {
	backoff := backoffMin
	if sc.BaseURL != "" {
		baseURL = sc.BaseURL
	}
	url := strings.TrimRight(baseURL, "/") + sc.Path

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Printf("[%s] connecting to %s", sc.Name, url)
		err := readStream(ctx, url, sc, hub)
		if err != nil {
			log.Printf("[%s] stream error: %v — retrying in %s", sc.Name, err, backoff)
		} else {
			log.Printf("[%s] stream closed — retrying in %s", sc.Name, backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Exponential back-off, capped at backoffMax
		backoff *= 2
		if backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

func readStream(ctx context.Context, url string, sc StreamConfig, hub *Hub) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	client := &http.Client{Timeout: 0} // no timeout — SSE is long-lived
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		log.Printf("[%s] upstream returned 503 — subsystem not enabled", sc.Name)
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	// Reset back-off on first successful connection
	log.Printf("[%s] connected (HTTP %d)", sc.Name, resp.StatusCode)

	scanner := bufio.NewScanner(resp.Body)
	// The SSTV feed's image records carry a per-image SNR series, which can
	// run past bufio's 64 KiB default line limit.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var dataLine, eventName string

	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case strings.HasPrefix(line, "data:"):
			dataLine = strings.TrimSpace(strings.TrimPrefix(line, "data:"))

		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))

		case line == "":
			// Blank line = end of event; process accumulated data
			if dataLine != "" && (sc.Event == "" || eventName == sc.Event) {
				spot, err := sc.Parse([]byte(dataLine))
				if err == nil && spot != nil {
					hub.Publish(*spot)
				}
			}
			dataLine, eventName = "", ""

		// Ignore id:, retry:, comment lines
		default:
			dataLine = ""
		}
	}

	return scanner.Err()
}
