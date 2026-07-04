package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
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
	ubersdrURL   := flag.String("url",       "http://ubersdr:8080", "Base URL of UberSDR instance")
	webListen    := flag.String("listen",    ":6087",               "Web UI listen address")
	telnetListen := flag.String("telnet",    ":7300",               "DX cluster telnet listen address")
	spotterCall  := flag.String("spotter",   "",                    "Callsign shown as spotter (default: fetched from /api/description)")
	basePath     := flag.String("base-path", "",                    "URL base path (for reverse-proxy addon prefix)")
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
	telnet := NewTelnetServer(*telnetListen, hub, callsign)
	go func() {
		if err := telnet.ListenAndServe(); err != nil {
			log.Fatalf("telnet server: %v", err)
		}
	}()

	// Start web server
	web, err := NewWebServer(*webListen, *basePath, ReceiverInfo{
		Callsign: callsign,
		Name:     rxName,
		Location: rxLocation,
	}, hub)
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
