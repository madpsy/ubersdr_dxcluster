package main

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestStore builds an in-memory store holding a small, hand-placed set of
// spots so the aggregate results below can be asserted exactly.
func newTestStore(t *testing.T) *SpotStore {
	t.Helper()
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.db.Close() })

	// A fixed reference day, so hour-of-day assertions don't drift.
	base := time.Now().UTC().Truncate(24 * time.Hour).Add(-12 * time.Hour)
	at := func(hour int) time.Time { return base.Add(time.Duration(hour) * time.Hour) }

	spots := []Spot{
		// 40m, Germany: three spots clustered at 20:00 UTC, one at 08:00.
		{Stream: StreamDecoder, Timestamp: at(20), Band: "40m", Callsign: "DL1AAA", FreqHz: 7074000, SNR: -5, Country: "Germany", CountryCode: "DE", Continent: "EU", Mode: "FT8", DistanceKM: 800},
		{Stream: StreamDecoder, Timestamp: at(20), Band: "40m", Callsign: "DL2BBB", FreqHz: 7074000, SNR: -9, Country: "Germany", CountryCode: "DE", Continent: "EU", Mode: "FT8", DistanceKM: 900},
		{Stream: StreamCWSkimmer, Timestamp: at(20), Band: "40m", Callsign: "DL3CCC", FreqHz: 7030000, SNR: 12, Country: "Germany", CountryCode: "DE", Continent: "EU", Mode: "CW", WPM: 25, Spotter: "G3ABC", DistanceKM: 850},
		{Stream: StreamDecoder, Timestamp: at(8), Band: "40m", Callsign: "DL4DDD", FreqHz: 7074000, SNR: -18, Country: "Germany", CountryCode: "DE", Continent: "EU", Mode: "FT8", DistanceKM: 820},
		// 20m, Japan: one spot at 08:00.
		{Stream: StreamDecoder, Timestamp: at(8), Band: "20m", Callsign: "JA1XYZ", FreqHz: 14074000, SNR: -3, Country: "Japan", CountryCode: "JP", Continent: "AS", Mode: "FT8", DistanceKM: 9500},
		// A voice spot, whose mode lives in voice_mode rather than mode.
		{Stream: StreamVoiceActivity, Timestamp: at(20), Band: "20m", Callsign: "N0CALL", FreqHz: 14200000, SNR: 6, VoiceMode: "USB", Country: "Italy", CountryCode: "IT", Continent: "EU"},
	}
	if err := s.insertBatch(spots); err != nil {
		t.Fatalf("insertBatch: %v", err)
	}
	return s
}

// dayFilter is a filter spanning the whole test window.
func dayFilter(t *testing.T, extra url.Values) StatsFilter {
	t.Helper()
	q := url.Values{"days": {"2"}}
	for k, v := range extra {
		q[k] = v
	}
	f, err := ParseStatsFilter(q)
	if err != nil {
		t.Fatalf("ParseStatsFilter(%v): %v", q, err)
	}
	return f
}

// TestParseStatsFilterWindow covers the three ways of expressing a time window
// and the guard against an empty one.
func TestParseStatsFilterWindow(t *testing.T) {
	f, err := ParseStatsFilter(url.Values{})
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if d := f.To.Sub(f.From); d < 6*24*time.Hour || d > 8*24*time.Hour {
		t.Errorf("default window = %v, want ~7 days", d)
	}

	f, err = ParseStatsFilter(url.Values{"hours": {"6"}})
	if err != nil {
		t.Fatalf("hours: %v", err)
	}
	if d := f.To.Sub(f.From); d < 5*time.Hour || d > 7*time.Hour {
		t.Errorf("hours=6 window = %v, want ~6h", d)
	}

	f, err = ParseStatsFilter(url.Values{"from": {"2026-01-01"}, "to": {"2026-01-03"}})
	if err != nil {
		t.Fatalf("absolute: %v", err)
	}
	if f.From.Format("2006-01-02") != "2026-01-01" || f.To.Format("2006-01-02") != "2026-01-03" {
		t.Errorf("absolute window = %v..%v", f.From, f.To)
	}

	if _, err := ParseStatsFilter(url.Values{"from": {"2026-01-05"}, "to": {"2026-01-01"}}); err == nil {
		t.Error("reversed window: want error, got nil")
	}
	if _, err := ParseStatsFilter(url.Values{"hour_min": {"25"}}); err == nil {
		t.Error("hour_min=25: want error, got nil")
	}
	if _, err := ParseStatsFilter(url.Values{"days": {"nope"}}); err == nil {
		t.Error("days=nope: want error, got nil")
	}
}

// TestStatsFilterCSV checks that repeated and comma-separated params both
// produce multi-value OR filters, with case normalised per column.
func TestStatsFilterCSV(t *testing.T) {
	f, err := ParseStatsFilter(url.Values{
		"band":         {"40M,20m"},
		"continent":    {"eu", "as"},
		"country_code": {"de"},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := strings.Join(f.Bands, ","); got != "40m,20m" {
		t.Errorf("bands = %q, want %q (lowercased)", got, "40m,20m")
	}
	if got := strings.Join(f.Continents, ","); got != "EU,AS" {
		t.Errorf("continents = %q, want %q (uppercased)", got, "EU,AS")
	}
	if got := strings.Join(f.CountryCodes, ","); got != "DE" {
		t.Errorf("country_code = %q, want %q", got, "DE")
	}
}

// TestBestHourForCountryOnBand is the headline question this API exists to
// answer: "when is the best time to hear Germany on 40m?"
func TestBestHourForCountryOnBand(t *testing.T) {
	s := newTestStore(t)
	f := dayFilter(t, url.Values{"band": {"40m"}, "country_code": {"DE"}})

	rows, err := s.Breakdown(f, "hour", "count", 24)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d hour groups, want 2", len(rows))
	}
	if rows[0].Count != 3 {
		t.Errorf("busiest hour count = %d, want 3", rows[0].Count)
	}
	if rows[0].Calls != 3 {
		t.Errorf("busiest hour unique calls = %d, want 3", rows[0].Calls)
	}
	// (-5 + -9 + 12) / 3 = -0.666… → -0.7
	if rows[0].AvgSNR == nil || *rows[0].AvgSNR != -0.7 {
		t.Errorf("busiest hour avg SNR = %v, want -0.7", rows[0].AvgSNR)
	}

	sum, err := s.Summary(f)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum["spots"].(int64) != 4 {
		t.Errorf("summary spots = %v, want 4", sum["spots"])
	}
	best, ok := sum["best_hour"].(map[string]any)
	if !ok || best["key"] != rows[0].Key {
		t.Errorf("summary best_hour = %v, want key %q", sum["best_hour"], rows[0].Key)
	}
}

// TestModeFallsBackToVoiceMode checks the unified mode dimension: voice spots
// carry USB/LSB in voice_mode and must still group and filter as a mode.
func TestModeFallsBackToVoiceMode(t *testing.T) {
	s := newTestStore(t)

	rows, err := s.Breakdown(dayFilter(t, nil), "mode", "count", 10)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	seen := map[string]int64{}
	for _, r := range rows {
		seen[r.Key] = r.Count
	}
	if seen["FT8"] != 4 || seen["CW"] != 1 || seen["USB"] != 1 {
		t.Errorf("mode counts = %v, want FT8:4 CW:1 USB:1", seen)
	}

	sum, err := s.Summary(dayFilter(t, url.Values{"mode": {"USB"}}))
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum["spots"].(int64) != 1 {
		t.Errorf("mode=USB spots = %v, want 1", sum["spots"])
	}
}

// TestHourWindowWraps checks that an hour window crossing midnight selects the
// night, not its complement.
func TestHourWindowWraps(t *testing.T) {
	f := dayFilter(t, url.Values{"hour_min": {"22"}, "hour_max": {"2"}})
	where, _ := f.where()
	if !strings.Contains(where, " OR ") {
		t.Errorf("wrapped hour window did not produce an OR clause: %s", where)
	}

	f2 := dayFilter(t, url.Values{"hour_min": {"2"}, "hour_max": {"6"}})
	where2, _ := f2.where()
	if !strings.Contains(where2, "BETWEEN") {
		t.Errorf("ordinary hour window did not produce a BETWEEN clause: %s", where2)
	}
}

// TestPrefixFiltersEscapeWildcards makes sure LIKE metacharacters typed by a
// user are matched literally rather than acting as wildcards.
func TestPrefixFiltersEscapeWildcards(t *testing.T) {
	s := newTestStore(t)

	sum, err := s.Summary(dayFilter(t, url.Values{"callsign": {"DL"}}))
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum["spots"].(int64) != 4 {
		t.Errorf("callsign=DL spots = %v, want 4", sum["spots"])
	}

	// "D%" must mean a literal percent sign, which nothing matches.
	sum, err = s.Summary(dayFilter(t, url.Values{"callsign": {"D%"}}))
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum["spots"].(int64) != 0 {
		t.Errorf("callsign=D%% spots = %v, want 0 (%% must be literal)", sum["spots"])
	}

	// A trailing * is accepted and stripped, not treated as a literal.
	sum, err = s.Summary(dayFilter(t, url.Values{"callsign": {"DL*"}}))
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum["spots"].(int64) != 4 {
		t.Errorf("callsign=DL* spots = %v, want 4", sum["spots"])
	}
}

// TestMatrixAxesAndTrim checks the cross-tab shape, and that a band axis is
// ordered by frequency rather than alphabetically.
func TestMatrixAxesAndTrim(t *testing.T) {
	s := newTestStore(t)
	cells, xKeys, yKeys, err := s.Matrix(dayFilter(t, nil), "hour", "band", "count", 25)
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}
	if len(xKeys) != 2 {
		t.Errorf("x keys = %v, want 2 hours", xKeys)
	}
	if strings.Join(yKeys, ",") != "40m,20m" {
		t.Errorf("band axis = %v, want 40m before 20m (by frequency)", yKeys)
	}
	var total int64
	for _, c := range cells {
		total += c.N
	}
	if total != 6 {
		t.Errorf("cell total = %d, want 6", total)
	}
}

// TestUnknownDimensionsRejected confirms the whitelists are what stop arbitrary
// identifiers reaching the SQL.
func TestUnknownDimensionsRejected(t *testing.T) {
	s := newTestStore(t)
	f := dayFilter(t, nil)

	if _, err := s.Breakdown(f, "callsign; DROP TABLE spots", "count", 10); err == nil {
		t.Error("Breakdown accepted an unknown dimension")
	}
	if _, _, _, err := s.Matrix(f, "hour", "band", "evil", 10); err == nil {
		t.Error("Matrix accepted an unknown metric")
	}
	if _, _, err := s.TimeSeries(f, "fortnight", "count", "", 8); err == nil {
		t.Error("TimeSeries accepted an unknown bucket")
	}
	if _, _, err := s.TimeSeries(f, "day", "count", "evil", 8); err == nil {
		t.Error("TimeSeries accepted an unknown split_by")
	}
}

// TestTimeSeriesSplitAndCap checks series splitting and the cap that keeps a
// legend readable.
func TestTimeSeriesSplitAndCap(t *testing.T) {
	s := newTestStore(t)
	series, names, err := s.TimeSeries(dayFilter(t, nil), "day", "count", "band", 8)
	if err != nil {
		t.Fatalf("TimeSeries: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("series names = %v, want 2 bands", names)
	}
	if names[0] != "40m" {
		t.Errorf("series order = %v, want the busiest band (40m) first", names)
	}
	var total int64
	for _, n := range names {
		for _, p := range series[n] {
			total += p.N
		}
	}
	if total != 6 {
		t.Errorf("series total = %d, want 6", total)
	}

	// A cap of 1 must drop the quieter band entirely.
	_, names, err = s.TimeSeries(dayFilter(t, nil), "day", "count", "band", 1)
	if err != nil {
		t.Fatalf("TimeSeries capped: %v", err)
	}
	if len(names) != 1 || names[0] != "40m" {
		t.Errorf("capped series = %v, want [40m]", names)
	}
}

// TestTimeSeriesFillsGaps covers the 24-hour-chart case: a quiet slice must
// come back with a point for every hour, so a line drawn through it doesn't
// imply activity across the silent hours.
func TestTimeSeriesFillsGaps(t *testing.T) {
	s := newTestStore(t)
	f := dayFilter(t, url.Values{"band": {"40m"}, "country_code": {"DE"}, "mode": {"FT8"}})

	series, names, err := s.TimeSeries(f, "hour", "count", "", 8)
	if err != nil {
		t.Fatalf("TimeSeries: %v", err)
	}
	pts := series[names[0]]
	if len(pts) < 47 || len(pts) > 49 {
		t.Fatalf("got %d hourly buckets over a 2-day window, want ~48", len(pts))
	}

	// Counting metrics fill empty buckets with a real zero, never a gap.
	var zeros, nonzero int
	for _, p := range pts {
		if p.V == nil {
			t.Fatalf("count series has a nil bucket at %s; counts must zero-fill", p.Label)
		}
		if *p.V == 0 {
			zeros++
		} else {
			nonzero++
		}
	}
	if nonzero != 2 {
		t.Errorf("non-empty buckets = %d, want 2 (the 08:00 and 20:00 FT8 spots)", nonzero)
	}
	if zeros == 0 {
		t.Error("no zero-filled buckets — gap filling did not run")
	}

	// Buckets must be in ascending time order for a time axis to be meaningful.
	for i := 1; i < len(pts); i++ {
		if pts[i].TS <= pts[i-1].TS {
			t.Fatalf("bucket %d (%s) is not after %s", i, pts[i].Label, pts[i-1].Label)
		}
	}
}

// TestTimeSeriesGapsStayNullForAverages checks the other half of the contract:
// an hour with no spots has no average, and must not be drawn as 0 dB.
func TestTimeSeriesGapsStayNullForAverages(t *testing.T) {
	s := newTestStore(t)
	f := dayFilter(t, url.Values{"band": {"40m"}, "country_code": {"DE"}, "mode": {"FT8"}})

	series, names, err := s.TimeSeries(f, "hour", "avg_snr", "", 8)
	if err != nil {
		t.Fatalf("TimeSeries: %v", err)
	}
	var withValue int
	for _, p := range series[names[0]] {
		if p.V != nil {
			withValue++
		}
	}
	if withValue != 2 {
		t.Errorf("buckets with an average = %d, want 2 (the rest must stay nil)", withValue)
	}
}

// TestBucketLabelsCap keeps gap filling from materialising an unbounded axis.
func TestBucketLabelsCap(t *testing.T) {
	f := dayFilter(t, nil)
	if got := len(f.bucketLabels("hour")); got < 47 || got > 49 {
		t.Errorf("2-day hourly labels = %d, want ~48", got)
	}
	if got := f.bucketLabels("week"); got != nil {
		t.Errorf("week labels = %v, want nil (weeks aren't enumerable here)", got)
	}

	// A year of hourly buckets is past the cap, so filling is skipped entirely.
	wide, err := ParseStatsFilter(url.Values{"days": {"365"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := wide.bucketLabels("hour"); got != nil {
		t.Errorf("365-day hourly labels = %d entries, want nil (over the cap)", len(got))
	}
	if got := len(wide.bucketLabels("day")); got < 364 || got > 366 {
		t.Errorf("365-day daily labels = %d, want ~365", got)
	}
}

// TestModeGroupsCoverEveryStream guards the single source of truth behind the
// mode pickers: every stream that carries a mode must be represented, and the
// grouping must stay in a stable presentation order.
func TestModeGroupsCoverEveryStream(t *testing.T) {
	groups := ModeGroups()
	if len(groups) != 3 {
		t.Fatalf("got %d mode groups, want 3 (digital, CW, voice)", len(groups))
	}
	if groups[0]["stream"] != string(StreamDecoder) ||
		groups[1]["stream"] != string(StreamCWSkimmer) ||
		groups[2]["stream"] != string(StreamVoiceActivity) {
		t.Errorf("group order = %v, want decoder, cwskimmer, voice", groups)
	}

	digital, _ := groups[0]["modes"].([]string)
	want := map[string]bool{"FT8": true, "FT4": true, "WSPR": true, "JS8": true, "FT2": true}
	if len(digital) != len(want) {
		t.Errorf("digital modes = %v, want %d entries", digital, len(want))
	}
	for _, m := range digital {
		if !want[m] {
			t.Errorf("unexpected digital mode %q", m)
		}
	}

	// Every stream must have an entry, even the ones with no mode field, so a
	// new stream type can't be added without deciding what it emits.
	for _, s := range streamOrder {
		if _, ok := StreamModes[s]; !ok {
			t.Errorf("stream %q has no StreamModes entry", s)
		}
		if StreamLabels[s] == "" {
			t.Errorf("stream %q has no label", s)
		}
	}
}

// TestKnownModesAreQueryable checks that every canonical mode round-trips
// through the filter — a mode offered in the picker must be a valid filter
// value, whether or not the database currently holds any.
func TestKnownModesAreQueryable(t *testing.T) {
	s := newTestStore(t)
	for _, g := range ModeGroups() {
		for _, mode := range g["modes"].([]string) {
			sum, err := s.Summary(dayFilter(t, url.Values{"mode": {mode}}))
			if err != nil {
				t.Fatalf("mode=%s: %v", mode, err)
			}
			if _, ok := sum["spots"].(int64); !ok {
				t.Errorf("mode=%s returned no spot count", mode)
			}
		}
	}
	// FT8 and CW are in the fixture; WSPR is a known mode with no spots, and
	// must return zero rather than an error.
	sum, err := s.Summary(dayFilter(t, url.Values{"mode": {"WSPR"}}))
	if err != nil {
		t.Fatalf("WSPR: %v", err)
	}
	if sum["spots"].(int64) != 0 {
		t.Errorf("WSPR spots = %v, want 0", sum["spots"])
	}
}

// TestDistinctCountsIgnoreEmpty is a regression guard: the streams that don't
// populate a field store an empty string, and SQLite counts ” as a distinct
// value. Only the CW spot in the fixture has a spotter, so a naive
// COUNT(DISTINCT spotter) would report 2.
func TestDistinctCountsIgnoreEmpty(t *testing.T) {
	s := newTestStore(t)
	sum, err := s.Summary(dayFilter(t, nil))
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got := sum["spotters"].(int64); got != 1 {
		t.Errorf("spotters = %d, want 1 — the empty spotter must not count", got)
	}
	// All six fixture spots have a country code, so this one is unaffected;
	// assert it anyway so a future NULLIF regression is caught here too.
	if got := sum["countries"].(int64); got != 3 {
		t.Errorf("countries = %d, want 3 (DE, JP, IT)", got)
	}
	if got := sum["bands"].(int64); got != 2 {
		t.Errorf("bands = %d, want 2 (40m, 20m)", got)
	}

	// The same must hold per group in a breakdown.
	rows, err := s.Breakdown(dayFilter(t, nil), "band", "count", 10)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	for _, r := range rows {
		if r.Key == "40m" && r.Spotters != 1 {
			t.Errorf("40m spotters = %d, want 1", r.Spotters)
		}
		if r.Key == "20m" && r.Spotters != 0 {
			t.Errorf("20m spotters = %d, want 0 (neither 20m spot has one)", r.Spotters)
		}
	}
}

// TestSpotterLeaderboard covers "how many spots did each spotter submit".
func TestSpotterLeaderboard(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.Breakdown(dayFilter(t, nil), "spotter", "count", 50)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	// Spots with no spotter must be dropped entirely, not grouped under "".
	if len(rows) != 1 {
		t.Fatalf("got %d spotter rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].Key != "G3ABC" || rows[0].Count != 1 {
		t.Errorf("spotter row = %q/%d, want G3ABC/1", rows[0].Key, rows[0].Count)
	}
	if rows[0].Bands != 1 {
		t.Errorf("G3ABC bands = %d, want 1", rows[0].Bands)
	}
	if rows[0].FirstTS == 0 || rows[0].LastTS == 0 {
		t.Error("spotter row is missing first/last seen timestamps")
	}
}

// TestExactMatchFilters checks that the lookup filters are exact: asking who
// spotted DL1AAA must not also return DL1AAAX.
func TestExactMatchFilters(t *testing.T) {
	s := newTestStore(t)

	sum, err := s.Summary(dayFilter(t, url.Values{"callsign_exact": {"DL1AAA"}}))
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got := sum["spots"].(int64); got != 1 {
		t.Errorf("callsign_exact=DL1AAA spots = %d, want 1", got)
	}

	// A prefix that matches four callsigns must match none when exact.
	sum, err = s.Summary(dayFilter(t, url.Values{"callsign_exact": {"DL"}}))
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got := sum["spots"].(int64); got != 0 {
		t.Errorf("callsign_exact=DL spots = %d, want 0 (exact, not prefix)", got)
	}
	sum, err = s.Summary(dayFilter(t, url.Values{"callsign": {"DL"}}))
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got := sum["spots"].(int64); got != 4 {
		t.Errorf("callsign=DL spots = %d, want 4 (prefix)", got)
	}

	// Exact matching is case-insensitive on input, since callsigns are stored
	// uppercase and users type either.
	sum, err = s.Summary(dayFilter(t, url.Values{"spotter_exact": {"g3abc"}}))
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got := sum["spots"].(int64); got != 1 {
		t.Errorf("spotter_exact=g3abc spots = %d, want 1", got)
	}
}

// TestWhoSpottedCallsign is the lookup the Spotters tab performs: given a
// callsign, who spotted it and with what detail.
func TestWhoSpottedCallsign(t *testing.T) {
	s := newTestStore(t)
	f := dayFilter(t, url.Values{"callsign_exact": {"DL3CCC"}})

	spots, total, err := s.ListSpots(f, 100, 0)
	if err != nil {
		t.Fatalf("ListSpots: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	got := spots[0]
	if got.Spotter != "G3ABC" {
		t.Errorf("spotter = %q, want G3ABC", got.Spotter)
	}
	if got.FreqHz != 7030000 {
		t.Errorf("freq = %v, want 7030000", got.FreqHz)
	}
	if got.Band != "40m" || got.Mode != "CW" || got.WPM != 25 {
		t.Errorf("band/mode/wpm = %s/%s/%d, want 40m/CW/25", got.Band, got.Mode, got.WPM)
	}
	if got.Timestamp.IsZero() {
		t.Error("spot has no timestamp")
	}

	rows, err := s.Breakdown(f, "spotter", "count", 50)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if len(rows) != 1 || rows[0].Key != "G3ABC" {
		t.Errorf("spotters of DL3CCC = %+v, want just G3ABC", rows)
	}
}

// TestListSpotsPaging checks the drill-down's total/offset contract.
func TestListSpotsPaging(t *testing.T) {
	s := newTestStore(t)
	f := dayFilter(t, nil)

	spots, total, err := s.ListSpots(f, 2, 0)
	if err != nil {
		t.Fatalf("ListSpots: %v", err)
	}
	if total != 6 {
		t.Errorf("total = %d, want 6", total)
	}
	if len(spots) != 2 {
		t.Errorf("page size = %d, want 2", len(spots))
	}
	if spots[0].Timestamp.Before(spots[1].Timestamp) {
		t.Error("spots are not newest-first")
	}

	spots, _, err = s.ListSpots(f, 100, 5)
	if err != nil {
		t.Fatalf("ListSpots offset: %v", err)
	}
	if len(spots) != 1 {
		t.Errorf("last page size = %d, want 1", len(spots))
	}
}

// TestFacetsOnlyOfferPresentValues confirms the pickers can't offer a dead end.
func TestFacetsOnlyOfferPresentValues(t *testing.T) {
	s := newTestStore(t)
	facets, err := s.Facets(dayFilter(t, url.Values{"band": {"20m"}}))
	if err != nil {
		t.Fatalf("Facets: %v", err)
	}
	bands, _ := facets["band"].([]map[string]any)
	if len(bands) != 1 || bands[0]["key"] != "20m" {
		t.Errorf("band facet = %v, want only 20m", bands)
	}
	countries, _ := facets["country"].([]map[string]any)
	if len(countries) != 2 {
		t.Errorf("country facet = %v, want 2 (JP, IT)", countries)
	}
}
