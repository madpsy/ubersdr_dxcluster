package main

import (
	"context"
	"encoding/json"
	"flag"
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
	} `json:"receiver"`
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

// fetchDescription calls /api/description on the UberSDR instance and returns
// the receiver callsign, name, location, and GPS coordinates.
// Falls back to defaults on error.
func fetchDescription(baseURL string) (callsign, name, location string, lat, lon float64) {
	callsign = "UBERSDR"
	name = "UberSDR DX Cluster"
	location = ""

	url := strings.TrimRight(baseURL, "/") + "/api/description"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("fetchDescription: %v (using defaults)", err)
		return
	}
	defer resp.Body.Close()

	var d descriptionResponse
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		log.Printf("fetchDescription decode: %v (using defaults)", err)
		return
	}

	if d.Receiver.Callsign != "" {
		callsign = d.Receiver.Callsign
	}
	if d.Receiver.Name != "" {
		name = d.Receiver.Name
	}
	location = d.Receiver.Location
	lat = d.Receiver.GPS.Lat
	lon = d.Receiver.GPS.Lon
	return
}

func main() {
	ubersdrURL := flag.String("url", "http://ubersdr:8080", "Base URL of UberSDR instance")
	webListen := flag.String("listen", ":6087", "Web UI listen address")
	telnetListen := flag.String("telnet", ":7300", "DX cluster telnet listen address")
	spotterCall := flag.String("spotter", "", "Callsign shown as spotter (default: fetched from /api/description)")
	requireLogin := flag.Bool("require-login", true, "Require a valid callsign login on telnet connect (default: true)")
	dataDir := flag.String("data-dir", "", "Directory for persistent data (SQLite DB). Defaults to DATA_DIR env var or /data")
	retentionDays := flag.Int("retention-days", 0, "Days of spot history to retain. Defaults to RETENTION_DAYS env var or 30")
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
	go store.Run()
	go store.RunPurge(retain)
	log.Printf("  store    : %s (%d spots, %d-day retention)", dbPath, store.Count(), retain)

	// Fetch receiver info from UberSDR
	callsign, rxName, rxLocation, rxLat, rxLon := fetchDescription(*ubersdrURL)
	if *spotterCall != "" {
		callsign = *spotterCall
	}

	log.Printf("starting ubersdr_dxcluster")
	log.Printf("  upstream : %s", *ubersdrURL)
	log.Printf("  web      : %s", *webListen)
	log.Printf("  telnet   : %s", *telnetListen)
	log.Printf("  spotter  : %s", callsign)
	log.Printf("  receiver : %s — %s (%.4f, %.4f)", rxName, rxLocation, rxLat, rxLon)

	hub := NewHub(store)
	go hub.Run()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start upstream SSE consumers
	StartConsumers(ctx, *ubersdrURL, hub)

	// Start telnet DX cluster server
	telnet := NewTelnetServer(*telnetListen, hub, store, callsign, ReceiverInfo{
		Callsign: callsign,
		Name:     rxName,
		Location: rxLocation,
		Lat:      rxLat,
		Lon:      rxLon,
	}, *ubersdrURL, *requireLogin)
	go func() {
		if err := telnet.ListenAndServe(); err != nil {
			log.Fatalf("telnet server: %v", err)
		}
	}()

	// Fetch country list for web UI filter
	countries := fetchCountries(*ubersdrURL)

	// Start web server
	web, err := NewWebServer(*webListen, *telnetListen, ReceiverInfo{
		Callsign: callsign,
		Name:     rxName,
		Location: rxLocation,
	}, countries, telnet, hub)
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
