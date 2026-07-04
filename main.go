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
// the receiver callsign, name, and location. Falls back to defaults on error.
func fetchDescription(baseURL string) (callsign, name, location string) {
	callsign = "UBERSDR"
	name     = "UberSDR DX Cluster"
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
	return
}

func main() {
	ubersdrURL    := flag.String("url",           "http://ubersdr:8080", "Base URL of UberSDR instance")
	webListen     := flag.String("listen",        ":6087",               "Web UI listen address")
	telnetListen  := flag.String("telnet",        ":7300",               "DX cluster telnet listen address")
	spotterCall   := flag.String("spotter",       "",                    "Callsign shown as spotter (default: fetched from /api/description)")
	requireLogin  := flag.Bool("require-login",   true,                  "Require a valid callsign login on telnet connect (default: true)")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[dxcluster] ")

	// Fetch receiver info from UberSDR
	callsign, rxName, rxLocation := fetchDescription(*ubersdrURL)
	if *spotterCall != "" {
		callsign = *spotterCall
	}

	log.Printf("starting ubersdr_dxcluster")
	log.Printf("  upstream : %s", *ubersdrURL)
	log.Printf("  web      : %s", *webListen)
	log.Printf("  telnet   : %s", *telnetListen)
	log.Printf("  spotter  : %s", callsign)
	log.Printf("  receiver : %s — %s", rxName, rxLocation)

	hub := NewHub()
	go hub.Run()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start upstream SSE consumers
	StartConsumers(ctx, *ubersdrURL, hub)

	// Start telnet DX cluster server
	telnet := NewTelnetServer(*telnetListen, hub, callsign, ReceiverInfo{
		Callsign: callsign,
		Name:     rxName,
		Location: rxLocation,
	}, *requireLogin)
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
