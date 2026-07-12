package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// qrzLookupResult mirrors the JSON returned by UberSDR's /api/lookup endpoint.
// It combines QRZ.com fields with an optional CTY augmentation block.
// All fields are optional — only populated fields are returned.
type qrzLookupResult struct {
	// Core identification
	Call    string `json:"call"`
	Aliases string `json:"aliases,omitempty"`
	DXCC    int    `json:"dxcc,omitempty"` // ADIF DXCC entity number

	// Operator name
	FName   string `json:"fname,omitempty"`
	Name    string `json:"name,omitempty"`
	NameFmt string `json:"name_fmt,omitempty"`

	// Address / location
	Addr2   string `json:"addr2,omitempty"`
	State   string `json:"state,omitempty"`
	County  string `json:"county,omitempty"`
	Country string `json:"country,omitempty"`

	// Geography
	Lat  float64 `json:"lat,omitempty"`
	Lon  float64 `json:"lon,omitempty"`
	Grid string  `json:"grid,omitempty"`

	// Licence / profile
	Class   string `json:"class,omitempty"`
	QSLMgr  string `json:"qslmgr,omitempty"`
	ModDate string `json:"moddate,omitempty"`

	// QSL / awards
	EqSL string `json:"eqsl,omitempty"`
	MqSL string `json:"mqsl,omitempty"`
	LoTW string `json:"lotw,omitempty"`

	// Zones
	CQZone  int    `json:"cqzone,omitempty"`
	ITUZone int    `json:"ituzone,omitempty"`
	IOTA    string `json:"iota,omitempty"`

	// CTY augmentation block (present when the receiver has a CTY database loaded)
	CTY *struct {
		Country     string `json:"country,omitempty"`
		CountryCode string `json:"country_code,omitempty"`
		Continent   string `json:"continent,omitempty"`
		CQZone      int    `json:"cq_zone,omitempty"`
		ITUZone     int    `json:"itu_zone,omitempty"`
		PrimaryPfx  string `json:"primary_prefix,omitempty"`
	} `json:"cty,omitempty"`
}

// qrzErrorResult is the JSON body returned by /api/lookup on error.
type qrzErrorResult struct {
	Error string `json:"error"`
}

// lookupQRZ calls UberSDR's /api/lookup endpoint for the given callsign.
// Our container ("dxcluster") is whitelisted for UUID-free lookups, so no
// session UUID is required when running inside the Docker network.
//
// Returns:
//   - result, nil          on success
//   - nil, nil             when the callsign is not found (404)
//   - nil, error           on network error, service disabled, or other failure
func lookupQRZ(baseURL, callsign string) (*qrzLookupResult, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/api/lookup?callsign=" + url.QueryEscape(callsign)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		return nil, fmt.Errorf("lookup request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var result qrzLookupResult
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("decode lookup response: %w", err)
		}
		return &result, nil
	case http.StatusNotFound:
		return nil, nil // callsign not found
	case http.StatusServiceUnavailable:
		return nil, fmt.Errorf("QRZ lookup is not enabled on this receiver")
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("rate limit exceeded - please slow down")
	default:
		var e qrzErrorResult
		if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s", e.Error)
		}
		return nil, fmt.Errorf("lookup failed (HTTP %d)", resp.StatusCode)
	}
}

// formatQRZ renders a lookup result in DX Spider SHOW/QRZ style.
//
// DX Spider outputs these fields (see cmd/show/qrz.pl):
//
//	call fname name addr2 state country lat lon county moddate qslmgr grid ADIF(dxcc)
//
// each as "qrz> %-10s: value", finishing with a data-source footer.
// We match that field set and add a few useful extras that UberSDR provides
// (CQ/ITU zone, IOTA, LoTW/eQSL) when present.
func formatQRZ(r *qrzLookupResult) string {
	var b strings.Builder

	line := func(tag, val string) {
		if val != "" {
			fmt.Fprintf(&b, "qrz> %-10s: %s\r\n", tag, val)
		}
	}
	lineInt := func(tag string, val int) {
		if val != 0 {
			fmt.Fprintf(&b, "qrz> %-10s: %d\r\n", tag, val)
		}
	}
	yesNo := func(v string) string {
		switch v {
		case "1":
			return "Yes"
		case "0":
			return "No"
		}
		return ""
	}

	// Prefer QRZ country; fall back to CTY country.
	country := r.Country
	continent := ""
	if r.CTY != nil {
		if country == "" {
			country = r.CTY.Country
		}
		continent = r.CTY.Continent
	}

	// Name: prefer QRZ pre-formatted name_fmt, else assemble fname + name.
	name := r.NameFmt
	if name == "" {
		name = strings.TrimSpace(r.FName + " " + r.Name)
	}

	// Spider-order fields
	line("Callsign", r.Call)
	line("Name", name)
	line("Addr", r.Addr2)
	line("State", r.State)
	line("County", r.County)
	line("Country", country)
	line("Continent", continent)
	line("Grid", r.Grid)
	if r.Lat != 0 || r.Lon != 0 {
		fmt.Fprintf(&b, "qrz> %-10s: %.4f, %.4f\r\n", "Lat/Lon", r.Lat, r.Lon)
	}
	line("Class", r.Class)
	lineInt("CQ Zone", zoneOr(r.CQZone, ctyCQ(r)))
	lineInt("ITU Zone", zoneOr(r.ITUZone, ctyITU(r)))
	line("IOTA", r.IOTA)
	line("QSL Mgr", r.QSLMgr)
	line("LoTW", yesNo(r.LoTW))
	line("eQSL", yesNo(r.EqSL))
	line("Buro QSL", yesNo(r.MqSL))
	lineInt("ADIF", r.DXCC)
	line("Modified", r.ModDate)

	b.WriteString("Data provided by www.qrz.com")
	return b.String()
}

// formatLoginWelcome builds a personalised greeting from a QRZ lookup result.
// It degrades gracefully: name+location → name only → location only → callsign only.
// Location is assembled from city (Addr2), state, and country, skipping empty parts.
// Falls back to the CTY country when QRZ has no country field.
func formatLoginWelcome(r *qrzLookupResult) string {
	// Prefer pre-formatted name_fmt; fall back to fname + name.
	name := r.NameFmt
	if name == "" {
		name = strings.TrimSpace(r.FName + " " + r.Name)
	}

	// Build location from available address fields.
	var locParts []string
	if r.Addr2 != "" {
		locParts = append(locParts, r.Addr2)
	}
	if r.State != "" {
		locParts = append(locParts, r.State)
	}
	if r.Country != "" {
		locParts = append(locParts, r.Country)
	}
	// Fall back to CTY country when QRZ has no country field.
	if len(locParts) == 0 && r.CTY != nil && r.CTY.Country != "" {
		locParts = append(locParts, r.CTY.Country)
	}
	loc := strings.Join(locParts, ", ")

	switch {
	case name != "" && loc != "":
		return fmt.Sprintf("Welcome, %s (%s) from %s", name, r.Call, loc)
	case name != "":
		return fmt.Sprintf("Welcome, %s (%s)", name, r.Call)
	case loc != "":
		return fmt.Sprintf("Welcome, %s from %s", r.Call, loc)
	default:
		return fmt.Sprintf("Welcome, %s", r.Call)
	}
}

// zoneOr returns primary if non-zero, else fallback.
func zoneOr(primary, fallback int) int {
	if primary != 0 {
		return primary
	}
	return fallback
}

func ctyCQ(r *qrzLookupResult) int {
	if r.CTY != nil {
		return r.CTY.CQZone
	}
	return 0
}

func ctyITU(r *qrzLookupResult) int {
	if r.CTY != nil {
		return r.CTY.ITUZone
	}
	return 0
}
