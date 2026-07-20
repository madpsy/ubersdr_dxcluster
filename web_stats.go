package main

// web_stats.go — the /api/stats/* endpoints.
//
// Every endpoint takes the same filter query string (see ParseStatsFilter) and
// echoes the resolved filter back in its response, so a chart and its
// drill-down table can be driven from one URL.

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
)

// registerStatsRoutes wires the analytics endpoints onto a mux.
func (w *WebServer) registerStatsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/stats/meta", w.handleStatsMeta)
	mux.HandleFunc("/api/stats/facets", w.handleStatsFacets)
	mux.HandleFunc("/api/stats/summary", w.handleStatsSummary)
	mux.HandleFunc("/api/stats/breakdown", w.handleStatsBreakdown)
	mux.HandleFunc("/api/stats/matrix", w.handleStatsMatrix)
	mux.HandleFunc("/api/stats/series", w.handleStatsSeries)
	mux.HandleFunc("/api/stats/spots", w.handleStatsSpots)
}

// writeJSON sends v as JSON with the CORS header the other endpoints use.
func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Access-Control-Allow-Origin", "*")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

// statsError reports a client or server error as JSON. Query-string mistakes
// are the common case and are worth spelling out; internal failures are logged
// server-side and reported generically.
func statsError(rw http.ResponseWriter, status int, err error) {
	if status >= 500 {
		log.Printf("[stats] %v", err)
		writeJSON(rw, status, map[string]string{"error": "query failed"})
		return
	}
	writeJSON(rw, status, map[string]string{"error": err.Error()})
}

// statsPrep parses the shared filter and reports the store, or writes an error
// response and returns ok=false.
func (w *WebServer) statsPrep(rw http.ResponseWriter, r *http.Request) (StatsFilter, bool) {
	if w.store == nil {
		statsError(rw, http.StatusServiceUnavailable, errNoStore)
		return StatsFilter{}, false
	}
	f, err := ParseStatsFilter(r.URL.Query())
	if err != nil {
		statsError(rw, http.StatusBadRequest, err)
		return StatsFilter{}, false
	}
	return f, true
}

// errNoStore is returned when the process is running without a spot database.
var errNoStore = errors.New("spot database unavailable")

// queryInt reads an integer query param, falling back to def when absent or
// unparseable — a bad limit shouldn't fail the whole request.
func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// queryOr reads a string query param with a default.
func queryOr(r *http.Request, key, def string) string {
	if v := r.URL.Query().Get(key); v != "" {
		return v
	}
	return def
}

// handleStatsMeta describes the available dimensions, metrics and buckets.
// The UI builds its pickers from this rather than hardcoding the lists.
func (w *WebServer) handleStatsMeta(rw http.ResponseWriter, r *http.Request) {
	writeJSON(rw, http.StatusOK, map[string]any{
		"dimensions": StatsDimsList(),
		"metrics":    StatsMetricsList(),
		"buckets":    []string{"hour", "day", "week"},
		"streams": []string{
			string(StreamDecoder), string(StreamCWSkimmer),
			string(StreamVoiceActivity), string(StreamDXCluster), string(StreamLocalSpot),
		},
		// The modes this cluster can produce, grouped by source. The UI offers
		// these regardless of whether the current window happens to contain
		// any, so a filter never hides a mode that simply had a quiet week.
		"mode_groups": ModeGroups(),
		"countries":   w.countries,
	})
}

// handleStatsFacets lists the values actually present under the current
// filter, so the dropdowns never offer a dead end.
func (w *WebServer) handleStatsFacets(rw http.ResponseWriter, r *http.Request) {
	f, ok := w.statsPrep(rw, r)
	if !ok {
		return
	}
	facets, err := w.store.Facets(f)
	if err != nil {
		statsError(rw, http.StatusInternalServerError, err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"filter": f.Describe(), "facets": facets})
}

// handleStatsSummary returns the headline figures for the filter.
func (w *WebServer) handleStatsSummary(rw http.ResponseWriter, r *http.Request) {
	f, ok := w.statsPrep(rw, r)
	if !ok {
		return
	}
	sum, err := w.store.Summary(f)
	if err != nil {
		statsError(rw, http.StatusInternalServerError, err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"filter": f.Describe(), "summary": sum})
}

// handleStatsBreakdown groups by one dimension.
// e.g. /api/stats/breakdown?group_by=hour&band=40m&country_code=DE
func (w *WebServer) handleStatsBreakdown(rw http.ResponseWriter, r *http.Request) {
	f, ok := w.statsPrep(rw, r)
	if !ok {
		return
	}
	dim := queryOr(r, "group_by", "band")
	sortBy := queryOr(r, "sort", "count")
	rows, err := w.store.Breakdown(f, dim, sortBy, queryInt(r, "limit", 50))
	if err != nil {
		statsError(rw, http.StatusBadRequest, err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{
		"filter": f.Describe(), "group_by": dim, "sort": sortBy, "rows": rows,
	})
}

// handleStatsMatrix cross-tabulates two dimensions.
// e.g. /api/stats/matrix?x=hour&y=band&metric=count&country_code=DE
func (w *WebServer) handleStatsMatrix(rw http.ResponseWriter, r *http.Request) {
	f, ok := w.statsPrep(rw, r)
	if !ok {
		return
	}
	x := queryOr(r, "x", "hour")
	y := queryOr(r, "y", "band")
	metric := queryOr(r, "metric", "count")
	cells, xKeys, yKeys, err := w.store.Matrix(f, x, y, metric, queryInt(r, "limit_y", 25))
	if err != nil {
		statsError(rw, http.StatusBadRequest, err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{
		"filter": f.Describe(), "x": x, "y": y, "metric": metric,
		"x_keys": xKeys, "y_keys": yKeys, "cells": cells,
	})
}

// handleStatsSeries returns a bucketed time series, optionally split into one
// series per value of split_by.
// e.g. /api/stats/series?bucket=day&split_by=band&continent=EU
func (w *WebServer) handleStatsSeries(rw http.ResponseWriter, r *http.Request) {
	f, ok := w.statsPrep(rw, r)
	if !ok {
		return
	}
	bucket := queryOr(r, "bucket", "day")
	metric := queryOr(r, "metric", "count")
	splitBy := r.URL.Query().Get("split_by")
	series, names, err := w.store.TimeSeries(f, bucket, metric, splitBy, queryInt(r, "max_series", 8))
	if err != nil {
		statsError(rw, http.StatusBadRequest, err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{
		"filter": f.Describe(), "bucket": bucket, "metric": metric,
		"split_by": splitBy, "names": names, "series": series,
	})
}

// handleStatsSpots is the drill-down: the raw rows behind any aggregate.
func (w *WebServer) handleStatsSpots(rw http.ResponseWriter, r *http.Request) {
	f, ok := w.statsPrep(rw, r)
	if !ok {
		return
	}
	limit := queryInt(r, "limit", 100)
	offset := queryInt(r, "offset", 0)
	spots, total, err := w.store.ListSpots(f, limit, offset)
	if err != nil {
		statsError(rw, http.StatusInternalServerError, err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{
		"filter": f.Describe(), "total": total,
		"limit": limit, "offset": offset, "spots": spots,
	})
}
