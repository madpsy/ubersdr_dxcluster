package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ctyLookupResult mirrors the "data" block of UberSDR's /api/cty/lookup response.
type ctyLookupResult struct {
	Callsign    string  `json:"callsign"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	CQZone      int     `json:"cq_zone"`
	ITUZone     int     `json:"itu_zone"`
	Continent   string  `json:"continent"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	TimeOffset  float64 `json:"time_offset"`
}

// ctyAPIResponse is the envelope returned by all /api/cty/* endpoints.
type ctyAPIResponse struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// lookupCTY calls UberSDR's /api/cty/lookup endpoint for a callsign or prefix.
// Returns:
//   - result, nil   on success
//   - nil, nil       when not found (404)
//   - nil, error     on network error / service unavailable
func lookupCTY(baseURL, callsign string) (*ctyLookupResult, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/api/cty/lookup?callsign=" + url.QueryEscape(callsign)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		return nil, fmt.Errorf("cty lookup request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var env ctyAPIResponse
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return nil, fmt.Errorf("decode cty response: %w", err)
		}
		if !env.Success {
			return nil, fmt.Errorf("%s", env.Error)
		}
		var result ctyLookupResult
		if err := json.Unmarshal(env.Data, &result); err != nil {
			return nil, fmt.Errorf("decode cty data: %w", err)
		}
		return &result, nil
	case http.StatusNotFound:
		return nil, nil
	case http.StatusServiceUnavailable:
		return nil, fmt.Errorf("CTY database is not loaded on this receiver")
	default:
		return nil, fmt.Errorf("cty lookup failed (HTTP %d)", resp.StatusCode)
	}
}

// ── Great-circle bearing & distance ─────────────────────────────────────────

const earthRadiusKM = 6371.0

// bearingDistance returns the initial bearing (degrees from north, 0–360) and
// great-circle distance (km) from (lat1,lon1) to (lat2,lon2).
func bearingDistance(lat1, lon1, lat2, lon2 float64) (bearing, distanceKM float64) {
	φ1 := lat1 * math.Pi / 180
	φ2 := lat2 * math.Pi / 180
	Δφ := (lat2 - lat1) * math.Pi / 180
	Δλ := (lon2 - lon1) * math.Pi / 180

	// Haversine distance
	a := math.Sin(Δφ/2)*math.Sin(Δφ/2) +
		math.Cos(φ1)*math.Cos(φ2)*math.Sin(Δλ/2)*math.Sin(Δλ/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	distanceKM = earthRadiusKM * c

	// Initial bearing
	y := math.Sin(Δλ) * math.Cos(φ2)
	x := math.Cos(φ1)*math.Sin(φ2) - math.Sin(φ1)*math.Cos(φ2)*math.Cos(Δλ)
	θ := math.Atan2(y, x)
	bearing = math.Mod(θ*180/math.Pi+360, 360)
	return
}

// slat formats a latitude as "NN N/S" (degrees + hemisphere), Spider-style.
func slat(lat float64) string {
	h := "N"
	if lat < 0 {
		h = "S"
		lat = -lat
	}
	return fmt.Sprintf("%.0f %s", lat, h)
}

// slong formats a longitude as "NN E/W", Spider-style.
func slong(lon float64) string {
	h := "E"
	if lon < 0 {
		h = "W"
		lon = -lon
	}
	return fmt.Sprintf("%.0f %s", lon, h)
}
