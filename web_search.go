package main

// web_search.go — the public /api/search endpoints.
//
// /api/search is the parametric read API over the spot database: one endpoint
// that takes every filter the stats API understands, plus ordering, paging,
// field projection and a CSV rendering, and returns the matching rows.
//
// /api/search/meta describes itself — every filter, sort key, field and limit —
// so a client (including this project's own search modal) can build its pickers
// from the server rather than hardcoding a copy that drifts.

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// searchMaxConcurrent bounds how many searches run at once. A search is the one
// public endpoint that can ask the database for real work, and SQLite serves
// readers from a shared file — letting an unbounded number of them in would let
// a burst of wide queries starve the live spot writes. Excess callers wait
// briefly, then get a 503 telling them to retry, which is a better answer than
// a request that hangs until it times out.
const (
	searchMaxConcurrent = 8
	searchQueueWait     = 2 * time.Second
)

// registerSearchRoutes wires the search endpoints onto a mux.
func (w *WebServer) registerSearchRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/search", w.handleSearch)
	mux.HandleFunc("/api/search/meta", w.handleSearchMeta)
	// Facets answer "what values are actually present under this filter", which
	// a search UI needs just as much as the stats dashboard does. Same handler,
	// same filter parameters — exposed here so a search client never has to
	// know the analytics API exists.
	mux.HandleFunc("/api/search/facets", w.handleStatsFacets)
}

// corsPreflight answers an OPTIONS request for the public API. Returns true
// when it handled the request.
func corsPreflight(rw http.ResponseWriter, r *http.Request) bool {
	rw.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodOptions {
		return false
	}
	rw.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	rw.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	rw.Header().Set("Access-Control-Max-Age", "86400")
	rw.WriteHeader(http.StatusNoContent)
	return true
}

// handleSearch runs one parametric search and renders it as JSON or CSV.
func (w *WebServer) handleSearch(rw http.ResponseWriter, r *http.Request) {
	if corsPreflight(rw, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		statsError(rw, http.StatusMethodNotAllowed, errors.New("use GET"))
		return
	}
	if w.store == nil {
		statsError(rw, http.StatusServiceUnavailable, errNoStore)
		return
	}

	p, err := ParseSearchParams(r.URL.Query())
	if err != nil {
		statsError(rw, http.StatusBadRequest, err)
		return
	}

	// Admission control, then a hard deadline on the query itself.
	release, ok := w.acquireSearchSlot(r.Context())
	if !ok {
		rw.Header().Set("Retry-After", "2")
		statsError(rw, http.StatusServiceUnavailable,
			errors.New("search is busy — retry in a moment"))
		return
	}
	defer release()

	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()

	res, err := w.store.Search(ctx, p)
	if err != nil {
		switch {
		case errors.Is(r.Context().Err(), context.Canceled):
			return // caller went away mid-query; nothing to report
		case errors.Is(err, context.DeadlineExceeded):
			statsError(rw, http.StatusGatewayTimeout, errors.New(
				"search timed out — narrow the time window, add a filter, or sort by ts"))
		default:
			statsError(rw, http.StatusInternalServerError, err)
		}
		return
	}

	if p.Format == "csv" {
		w.writeSearchCSV(rw, p, res)
		return
	}

	out := map[string]any{
		"filter":   p.Filter.Describe(),
		"search":   p.Describe(),
		"took_ms":  res.TookMS,
		"returned": len(res.Spots),
		"has_more": res.HasMore,
	}
	if res.Total >= 0 {
		out["total"] = res.Total
		out["total_capped"] = res.TotalCapped
	}
	if res.NextCursor != "" {
		out["next_cursor"] = res.NextCursor
	}
	if res.HasMore {
		out["next_offset"] = res.NextOffset
	}
	if len(p.Warnings) > 0 {
		out["warnings"] = p.Warnings
	}
	// Without a field selection the rows are whole spots, which keeps
	// /api/search interchangeable with /api/spots and /api/stats/spots.
	if len(p.Fields) == 0 {
		out["spots"] = res.Spots
	} else {
		rows := make([]map[string]any, len(res.Spots))
		for i, sp := range res.Spots {
			rows[i] = projectSpot(sp, p.Fields)
		}
		out["spots"] = rows
	}
	writeJSON(rw, http.StatusOK, out)
}

// writeSearchCSV renders the page as CSV, one column per selected field
// (all of them when none were named).
func (w *WebServer) writeSearchCSV(rw http.ResponseWriter, p SearchParams, res *SearchResult) {
	fields := p.Fields
	if len(fields) == 0 {
		fields = searchFields
	}

	name := fmt.Sprintf("dxcluster-search-%s.csv", time.Now().UTC().Format("20060102-150405"))
	rw.Header().Set("Content-Type", "text/csv; charset=utf-8")
	rw.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	rw.Header().Set("Access-Control-Allow-Origin", "*")

	cw := csv.NewWriter(rw)
	defer cw.Flush()

	header := make([]string, len(fields))
	for i, f := range fields {
		header[i] = f.Key
	}
	if err := cw.Write(header); err != nil {
		return
	}
	rec := make([]string, len(fields))
	for _, sp := range res.Spots {
		for i, f := range fields {
			rec[i] = csvValue(f.Get(sp))
		}
		if err := cw.Write(rec); err != nil {
			log.Printf("[search] csv write: %v", err)
			return
		}
	}
}

// acquireSearchSlot takes one of the concurrent-search slots, waiting up to
// searchQueueWait. The returned func releases it; ok is false when the wait
// expired or the caller disconnected.
func (w *WebServer) acquireSearchSlot(ctx context.Context) (func(), bool) {
	if w.searchSem == nil {
		return func() {}, true
	}
	select {
	case w.searchSem <- struct{}{}:
		return func() { <-w.searchSem }, true
	default:
	}
	timer := time.NewTimer(searchQueueWait)
	defer timer.Stop()
	select {
	case w.searchSem <- struct{}{}:
		return func() { <-w.searchSem }, true
	case <-timer.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// ── Meta ───────────────────────────────────────────────────────────────────

// searchFilterDoc is one documented filter parameter. The list is what makes
// /api/search discoverable without the README.
type searchFilterDoc struct {
	Key      string `json:"key"`
	Type     string `json:"type"`  // int | float | string | csv | time | enum
	Group    string `json:"group"` // how a UI should group it
	Desc     string `json:"desc"`
	Min      *int   `json:"min,omitempty"`
	Max      *int   `json:"max,omitempty"`
	Multi    bool   `json:"multi,omitempty"`
	Unit     string `json:"unit,omitempty"`
	Examples string `json:"example,omitempty"`
}

func intp(v int) *int { return &v }

// searchFilterDocs describes every filter parameter ParseStatsFilter accepts.
// It is documentation, not validation — the parser remains the authority — but
// the two are meant to be read together, so anything added there belongs here.
var searchFilterDocs = []searchFilterDoc{
	{Key: "days", Type: "int", Group: "time", Desc: "Relative lookback in days", Min: intp(1), Max: intp(maxWindowDays), Unit: "days", Examples: "7"},
	{Key: "hours", Type: "int", Group: "time", Desc: "Relative lookback in hours", Min: intp(1), Unit: "hours", Examples: "24"},
	{Key: "from", Type: "time", Group: "time", Desc: "Window start — RFC3339, YYYY-MM-DD or Unix seconds", Examples: "2026-08-01"},
	{Key: "to", Type: "time", Group: "time", Desc: "Window end — same formats as from", Examples: "2026-08-31"},
	{Key: "hour_min", Type: "int", Group: "time", Desc: "UTC hour-of-day lower bound; wraps midnight when hour_min > hour_max", Min: intp(0), Max: intp(23)},
	{Key: "hour_max", Type: "int", Group: "time", Desc: "UTC hour-of-day upper bound", Min: intp(0), Max: intp(23)},

	{Key: "callsign", Type: "string", Group: "station", Desc: "Callsign prefix (a trailing * is accepted and ignored)", Examples: "DL"},
	{Key: "callsign_exact", Type: "string", Group: "station", Desc: "Exact callsign — G3ABC does not also match G3ABCD", Examples: "G3ABC"},
	{Key: "callsign_exclude", Type: "csv", Group: "station", Desc: "Callsigns to exclude", Multi: true},
	{Key: "spotter", Type: "string", Group: "station", Desc: "Spotter callsign prefix", Examples: "G3"},
	{Key: "spotter_exact", Type: "string", Group: "station", Desc: "Exact spotter callsign"},
	{Key: "spotter_exclude", Type: "csv", Group: "station", Desc: "Spotters to exclude; spots with no spotter are kept", Multi: true},
	{Key: "locator", Type: "string", Group: "station", Desc: "Maidenhead grid prefix", Examples: "IO91"},

	{Key: "band", Type: "csv", Group: "signal", Desc: "Band labels; OR within, AND across parameters", Multi: true, Examples: "20m,40m"},
	{Key: "mode", Type: "csv", Group: "signal", Desc: "Modes, including CW and voice USB/LSB", Multi: true, Examples: "FT8,FT4"},
	{Key: "stream", Type: "csv", Group: "signal", Desc: "Source streams", Multi: true, Examples: "decoder,cwskimmer"},
	{Key: "freq_min", Type: "float", Group: "signal", Desc: "Lower frequency bound", Unit: "kHz", Examples: "14000"},
	{Key: "freq_max", Type: "float", Group: "signal", Desc: "Upper frequency bound", Unit: "kHz", Examples: "14350"},
	{Key: "snr_min", Type: "float", Group: "signal", Desc: "Lower SNR bound", Unit: "dB"},
	{Key: "snr_max", Type: "float", Group: "signal", Desc: "Upper SNR bound", Unit: "dB"},
	{Key: "wpm_min", Type: "int", Group: "signal", Desc: "Lower CW speed bound", Unit: "wpm"},
	{Key: "wpm_max", Type: "int", Group: "signal", Desc: "Upper CW speed bound", Unit: "wpm"},
	{Key: "conf_min", Type: "float", Group: "signal", Desc: "Minimum voice-detection confidence, 0–1"},

	{Key: "country_code", Type: "csv", Group: "location", Desc: "ISO 3166-1 alpha-2 country codes", Multi: true, Examples: "DE,FR"},
	{Key: "country", Type: "csv", Group: "location", Desc: "Country names — for DXCC entities that have no ISO code", Multi: true},
	{Key: "continent", Type: "csv", Group: "location", Desc: "Continent codes", Multi: true, Examples: "EU,NA"},
	{Key: "cq_zone", Type: "csv", Group: "location", Desc: "CQ zone numbers", Multi: true, Examples: "14,15"},
	{Key: "dist_min", Type: "float", Group: "location", Desc: "Lower distance bound from the receiver", Unit: "km"},
	{Key: "dist_max", Type: "float", Group: "location", Desc: "Upper distance bound from the receiver", Unit: "km"},

	{Key: "q", Type: "string", Group: "text", Desc: "Substring of the spot comment or message"},
}

// searchOptionDocs describes the parameters that shape the result rather than
// select them.
var searchOptionDocs = []searchFilterDoc{
	{Key: "sort", Type: "enum", Group: "result", Desc: "Sort key — see `sorts`", Examples: "ts"},
	{Key: "order", Type: "enum", Group: "result", Desc: "asc or desc; defaults to the sort key's natural direction"},
	{Key: "limit", Type: "int", Group: "result", Desc: "Rows per page", Min: intp(1), Max: intp(maxSearchLimit)},
	{Key: "offset", Type: "int", Group: "result", Desc: "Rows to skip; prefer cursor past a few pages", Min: intp(0), Max: intp(maxSearchOffset)},
	{Key: "cursor", Type: "string", Group: "result", Desc: "next_cursor from the previous page — constant-cost paging, sort=ts only"},
	{Key: "count", Type: "enum", Group: "result", Desc: "capped (default), exact, or none — none is the cheapest"},
	{Key: "fields", Type: "csv", Group: "result", Desc: "Subset of fields to return; omit for the whole spot", Multi: true},
	{Key: "format", Type: "enum", Group: "result", Desc: "json (default) or csv"},
}

// SearchUIMeta is everything a search UI needs to draw its pickers, and every
// bit of it is a compile-time constant of this program: the band table, the
// modes each stream can produce, the sort whitelist, the field whitelist, the
// limits.
//
// It is built once at startup and injected straight into the page, so the
// search modal opens with its dropdowns already populated rather than empty
// while a fetch lands. Only the country list is left out — it is an order of
// magnitude larger than everything else here, and the page already loads it
// asynchronously for the live filter bar.
func SearchUIMeta() map[string]any {
	bands := make([]string, len(bandRanges))
	for i, b := range bandRanges {
		bands[i] = b.Name
	}
	return map[string]any{
		"endpoint": "/api/search",
		"sorts":    SearchSortsList(),
		"fields":   SearchFieldsList(),
		"limits": map[string]any{
			"default_limit":   defaultSearchLimit,
			"max_limit":       maxSearchLimit,
			"max_offset":      maxSearchOffset,
			"max_window_days": maxWindowDays,
			"count_cap":       searchCountCap,
			"timeout_seconds": int(searchTimeout / time.Second),
		},
		"bands": bands,
		"streams": []string{
			string(StreamDecoder), string(StreamCWSkimmer),
			string(StreamVoiceActivity), string(StreamSSTV), string(StreamDXCluster), string(StreamLocalSpot),
		},
		"stream_labels": StreamLabelMap(),
		"mode_groups":   ModeGroups(),
		"continents":    searchContinents,
	}
}

// searchContinents is the fixed continent list, in the order the pickers show
// it — busiest first for this receiver's hemisphere rather than alphabetical.
var searchContinents = []map[string]string{
	{"code": "EU", "name": "Europe"},
	{"code": "NA", "name": "North America"},
	{"code": "SA", "name": "South America"},
	{"code": "AF", "name": "Africa"},
	{"code": "AS", "name": "Asia"},
	{"code": "OC", "name": "Oceania"},
	{"code": "AN", "name": "Antarctica"},
}

// handleSearchMeta describes the search API: its parameters, its whitelists and
// its limits, plus the option lists a UI needs to build pickers.
func (w *WebServer) handleSearchMeta(rw http.ResponseWriter, r *http.Request) {
	if corsPreflight(rw, r) {
		return
	}
	out := SearchUIMeta()
	out["filters"] = searchFilterDocs
	out["options"] = searchOptionDocs
	out["countries"] = w.countries
	out["examples"] = searchExamples
	writeJSON(rw, http.StatusOK, out)
}

// searchExamples are worked queries, returned by meta so the endpoint teaches
// its own use.
var searchExamples = []map[string]string{
	{
		"desc":  "Every FT8 spot of a German station on 40m in the last week, strongest first",
		"query": "/api/search?days=7&band=40m&mode=FT8&country_code=DE&sort=snr&order=desc",
	},
	{
		"desc":  "Everything heard from one callsign this month, oldest first, as CSV",
		"query": "/api/search?days=30&callsign_exact=G3ABC&sort=ts&order=asc&format=csv",
	},
	{
		"desc":  "Long-haul CW between 03:00 and 06:00 UTC, over 5000 km",
		"query": "/api/search?days=30&mode=CW&dist_min=5000&hour_min=3&hour_max=6&sort=distance",
	},
	{
		"desc":  "Page through a large result set at constant cost",
		"query": "/api/search?days=30&band=20m&limit=500&count=none → then &cursor=<next_cursor>",
	},
	{
		"desc":  "Just the columns needed for a spreadsheet",
		"query": "/api/search?hours=6&fields=timestamp,callsign,freq_hz,mode,snr,country&format=csv",
	},
}

// ── Search page ────────────────────────────────────────────────────────────

// handleSearchDocs serves the human-readable parameter reference that the
// search modal's "API" tab links to: the same meta document, pretty-printed.
func (w *WebServer) handleSearchDocs(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.Header().Set("Access-Control-Allow-Origin", "*")

	var b strings.Builder
	b.WriteString("UberSDR DX Cluster — /api/search\n\n")
	b.WriteString("Filter parameters\n")
	for _, d := range searchFilterDocs {
		fmt.Fprintf(&b, "  %-18s %-6s %s\n", d.Key, d.Type, d.Desc)
	}
	b.WriteString("\nResult parameters\n")
	for _, d := range searchOptionDocs {
		fmt.Fprintf(&b, "  %-18s %-6s %s\n", d.Key, d.Type, d.Desc)
	}
	b.WriteString("\nSort keys\n  ")
	b.WriteString(strings.Join(sortedMapKeys(searchSorts), ", "))
	b.WriteString("\n\nFields\n  ")
	keys := make([]string, len(searchFields))
	for i, f := range searchFields {
		keys[i] = f.Key
	}
	b.WriteString(strings.Join(keys, ", "))
	b.WriteString("\n\nExamples\n")
	for _, e := range searchExamples {
		fmt.Fprintf(&b, "  %s\n    %s\n", e["desc"], e["query"])
	}
	_, _ = rw.Write([]byte(b.String()))
}
