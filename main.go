package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// descriptionResponse is the subset of /api/description we care about.
type descriptionResponse struct {
	Receiver struct {
		Callsign string `json:"callsign"`
		Name     string `json:"name"`
		Location string `json:"location"`
		GPS      struct {
			Lat float64 `json:"lat"`
			Lon float64 `json:"lon"`
		} `json:"gps"`
		// Timezone is an IANA name ("Europe/London"); TimezoneOffset is the
		// offset in minutes as it stands right now. The name is preferred —
		// a fixed offset would be wrong for spots either side of a DST change.
		Timezone       string `json:"timezone"`
		TimezoneOffset int    `json:"timezone_offset"`
	} `json:"receiver"`

	// TuningRange is the hardware tuning range of the receiver, in Hz. Not
	// every UberSDR version reports it, so it is a pointer: nil means "not
	// reported" and the caller falls back to its own default spot limits.
	TuningRange *struct {
		MinFrequency float64 `json:"min_frequency"`
		MaxFrequency float64 `json:"max_frequency"`
	} `json:"tuning_range"`

	// Addons names the addon proxies enabled on the receiver.
	Addons []string `json:"addons"`
}

// CountryEntry is one entry from /api/cty/countries.
type CountryEntry struct {
	Name        string `json:"name"`
	CountryCode string `json:"country_code"`
}

// fetchCountries fetches the country list from /api/cty/countries.
// Returns a sorted slice of CountryEntry. Falls back to empty on error.
func fetchCountries(baseURL string) []CountryEntry {
	url := strings.TrimRight(baseURL, "/") + "/api/cty/countries"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("fetchCountries: %v (country filter will be unavailable)", err)
		return nil
	}
	defer resp.Body.Close()

	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Countries []CountryEntry `json:"countries"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("fetchCountries decode: %v", err)
		return nil
	}

	// Deduplicate by country_code (CTY has multiple entries per code)
	seen := make(map[string]bool)
	var out []CountryEntry
	for _, c := range result.Data.Countries {
		if c.CountryCode == "" || seen[c.CountryCode] {
			continue
		}
		seen[c.CountryCode] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	log.Printf("fetchCountries: loaded %d countries", len(out))
	return out
}

// descriptionRetryDelay is how long to wait between /api/description attempts
// while the UberSDR instance is unreachable.
const descriptionRetryDelay = 5 * time.Second

// fetchDescription calls /api/description on the UberSDR instance and returns
// the receiver callsign, name, location, and GPS coordinates.
//
// The receiver callsign is the spotter shown on every spot line, so a partial
// or defaulted result is not acceptable — an error (or a response without a
// callsign) is reported to the caller rather than papered over.
func fetchDescription(baseURL string) (ReceiverInfo, error) {
	var rx ReceiverInfo

	url := strings.TrimRight(baseURL, "/") + "/api/description"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return rx, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return rx, fmt.Errorf("HTTP %s", resp.Status)
	}

	var d descriptionResponse
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return rx, fmt.Errorf("decode: %w", err)
	}

	if d.Receiver.Callsign == "" {
		return rx, fmt.Errorf("no receiver callsign in response")
	}

	rx.Callsign = d.Receiver.Callsign
	rx.Name = d.Receiver.Name
	if rx.Name == "" {
		rx.Name = "UberSDR DX Cluster"
	}
	rx.Location = d.Receiver.Location
	rx.Lat = d.Receiver.GPS.Lat
	rx.Lon = d.Receiver.GPS.Lon
	rx.Timezone = d.Receiver.Timezone
	rx.TimezoneOffset = d.Receiver.TimezoneOffset

	// A tuning range is only usable if the upstream actually reported one and
	// it makes sense. Anything else leaves the zero value, which downstream
	// reads as "not reported" and substitutes the built-in default range.
	if tr := d.TuningRange; tr != nil && tr.MaxFrequency > tr.MinFrequency && tr.MaxFrequency > 0 {
		rx.TuneMinHz = tr.MinFrequency
		rx.TuneMaxHz = tr.MaxFrequency
	}
	rx.Addons = d.Addons
	return rx, nil
}

// mustFetchDescription retries fetchDescription every descriptionRetryDelay
// until it succeeds. Startup does not continue without it: the receiver
// callsign is baked into every spot line and the telnet greeting, and there is
// no later opportunity to correct it, so serving spots under a placeholder
// callsign is worse than waiting for the upstream to come up.
func mustFetchDescription(baseURL string) ReceiverInfo {
	for attempt := 1; ; attempt++ {
		rx, err := fetchDescription(baseURL)
		if err == nil {
			if attempt > 1 {
				log.Printf("fetchDescription: succeeded after %d attempts", attempt)
			}
			return rx
		}
		log.Printf("fetchDescription: %v — retrying in %s (attempt %d)",
			err, descriptionRetryDelay, attempt)
		time.Sleep(descriptionRetryDelay)
	}
}

func main() {
	ubersdrURL := flag.String("url", "http://ubersdr:8080", "Base URL of UberSDR instance")
	webListen := flag.String("listen", ":6087", "Web UI listen address")
	telnetListen := flag.String("telnet", ":7300", "DX cluster telnet listen address")
	spotterCall := flag.String("spotter", "", "Callsign shown as spotter (default: fetched from /api/description, retried until it succeeds)")
	requireLogin := flag.Bool("require-login", true, "Require a valid callsign login on telnet connect (default: true)")
	dataDir := flag.String("data-dir", "", "Directory for persistent data (SQLite DB). Defaults to DATA_DIR env var or /data")
	retentionDays := flag.Int("retention-days", 0, "Days of spot history to retain. Defaults to RETENTION_DAYS env var or 30")
	sstvURL := flag.String("sstv-url", "http://sstv:6091", "Base URL of the SSTV addon, used when the receiver lists it as enabled")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[dxcluster] ")

	// Resolve data directory: flag > env > default
	dir := *dataDir
	if dir == "" {
		dir = os.Getenv("DATA_DIR")
	}
	if dir == "" {
		dir = "/data"
	}

	// Resolve retention days: flag > env > default (30)
	retain := *retentionDays
	if retain == 0 {
		if v := os.Getenv("RETENTION_DAYS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				retain = n
			} else {
				log.Printf("invalid RETENTION_DAYS=%q — using default 30", v)
			}
		}
	}
	if retain <= 0 {
		retain = 30
	}

	// Resolve voice dedup window: VOICE_DEDUP_MINS env var > default (10)
	voiceDedupMins := 10
	if v := os.Getenv("VOICE_DEDUP_MINS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			voiceDedupMins = n
		} else {
			log.Printf("invalid VOICE_DEDUP_MINS=%q — using default 10", v)
		}
	}

	// Resolve decoder (digital) dedup window: DECODER_DEDUP_MINS env var > default (5)
	decoderDedupMins := 5
	if v := os.Getenv("DECODER_DEDUP_MINS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			decoderDedupMins = n
		} else {
			log.Printf("invalid DECODER_DEDUP_MINS=%q — using default 5", v)
		}
	}

	// Resolve SSTV spot dedup window: SSTV_DEDUP_MINS env var > default (10)
	sstvDedupMins := 10
	if v := os.Getenv("SSTV_DEDUP_MINS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			sstvDedupMins = n
		} else {
			log.Printf("invalid SSTV_DEDUP_MINS=%q — using default 10", v)
		}
	}

	// Resolve spot submission password: SPOT_PASSWORD env var
	// If empty (default), the DX command is disabled entirely.
	spotPassword := os.Getenv("SPOT_PASSWORD")
	if spotPassword != "" {
		log.Printf("  spot submit: enabled (SPOT_PASSWORD set)")
	} else {
		log.Printf("  spot submit: disabled (set SPOT_PASSWORD env var to enable)")
	}

	// Resolve WebSocket terminal limits: WS_MAX_CONNS / WS_MAX_CONNS_PER_IP env vars
	wsMaxConns := 25
	if v := os.Getenv("WS_MAX_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			wsMaxConns = n
		} else {
			log.Printf("invalid WS_MAX_CONNS=%q — using default 25", v)
		}
	}
	wsMaxConnsPerIP := 2
	if v := os.Getenv("WS_MAX_CONNS_PER_IP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			wsMaxConnsPerIP = n
		} else {
			log.Printf("invalid WS_MAX_CONNS_PER_IP=%q — using default 2", v)
		}
	}

	// Open persistent spot store
	dbPath := dir + "/spots.db"
	store, err := OpenStore(dbPath)
	if err != nil {
		log.Fatalf("open spot store %s: %v", dbPath, err)
	}
	if voiceDedupMins > 0 {
		store.SetVoiceDedupWindow(time.Duration(voiceDedupMins) * time.Minute)
		log.Printf("  voice dedup: %d min window", voiceDedupMins)
	} else {
		log.Printf("  voice dedup: disabled")
	}
	if decoderDedupMins > 0 {
		store.SetDecoderDedupWindow(time.Duration(decoderDedupMins) * time.Minute)
		log.Printf("  decoder dedup: %d min window", decoderDedupMins)
	} else {
		log.Printf("  decoder dedup: disabled")
	}
	go store.Run()
	go store.RunPurge(retain)
	log.Printf("  store    : %s (%d spots, %d-day retention)", dbPath, store.Count(), retain)

	// Fetch receiver info from UberSDR — blocks until the upstream answers
	log.Printf("fetching receiver description from %s", *ubersdrURL)
	rx := mustFetchDescription(*ubersdrURL)
	if *spotterCall != "" {
		rx.Callsign = *spotterCall
	}
	callsign := rx.Callsign

	log.Printf("starting ubersdr_dxcluster")
	log.Printf("  upstream : %s", *ubersdrURL)
	log.Printf("  web      : %s", *webListen)
	log.Printf("  telnet   : %s", *telnetListen)
	log.Printf("  spotter  : %s", callsign)
	log.Printf("  receiver : %s — %s (%.4f, %.4f)", rx.Name, rx.Location, rx.Lat, rx.Lon)
	if rx.Timezone != "" {
		log.Printf("  timezone : %s (UTC%+d min)", rx.Timezone, rx.TimezoneOffset)
	} else {
		log.Printf("  timezone : not reported — stats page will offer UTC and local only")
	}
	spotMin, spotMax := spotRangeKHz(rx)
	if rx.TuneMaxHz > 0 {
		log.Printf("  spot rng : %s – %s (receiver tuning range)",
			formatKHz(spotMin), formatKHz(spotMax))
	} else {
		log.Printf("  spot rng : %s – %s (tuning range not reported — using default)",
			formatKHz(spotMin), formatKHz(spotMax))
	}

	// SSTV spots come from the SSTV addon, which is optional. Its feed is only
	// consumed when the receiver lists the addon as enabled; the consumer then
	// reconnects like the others, so it does not matter which starts first.
	sstvFeed := ""
	switch {
	case !rx.HasAddon("sstv"):
		log.Printf("  sstv     : SSTV addon not enabled on this receiver — SSTV spots disabled")
	case *sstvURL == "":
		log.Printf("  sstv     : disabled (-sstv-url is empty)")
	default:
		sstvFeed = *sstvURL
		if sstvDedupMins > 0 {
			log.Printf("  sstv     : %s (one spot per callsign per band per %d min)", sstvFeed, sstvDedupMins)
		} else {
			log.Printf("  sstv     : %s (dedup disabled)", sstvFeed)
		}
	}

	hub := NewHub(store)
	go hub.Run()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start upstream SSE consumers
	StartConsumers(ctx, *ubersdrURL, sstvFeed, time.Duration(sstvDedupMins)*time.Minute, hub)

	// Start telnet DX cluster server
	telnet := NewTelnetServer(*telnetListen, hub, store, callsign, rx,
		*ubersdrURL, *requireLogin, spotPassword)
	go func() {
		if err := telnet.ListenAndServe(); err != nil {
			log.Fatalf("telnet server: %v", err)
		}
	}()

	// Fetch country list for web UI filter
	countries := fetchCountries(*ubersdrURL)

	log.Printf("  ws limits: max %d global, %d per-IP", wsMaxConns, wsMaxConnsPerIP)

	// Start web server
	web, err := NewWebServer(*webListen, *telnetListen, rx,
		countries, telnet, hub, store, wsMaxConns, wsMaxConnsPerIP)
	if err != nil {
		log.Fatalf("web server init: %v", err)
	}
	go func() {
		if err := web.ListenAndServe(); err != nil {
			log.Fatalf("web server: %v", err)
		}
	}()

	// Wait for signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down")
	cancel()
}
