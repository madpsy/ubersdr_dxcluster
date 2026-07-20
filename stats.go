package main

// stats.go — the analytics query layer over the spots table.
//
// Everything here is driven by two whitelists: statsDims (what you can group
// by) and statsMetrics (what you can measure). Filters are shared by every
// endpoint, so "count spots per UTC hour" and "average SNR per country" take
// exactly the same query string. Values are always bound as parameters; only
// whitelisted identifiers are ever interpolated into SQL.

import (
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── Filter ─────────────────────────────────────────────────────────────────

// StatsFilter is the shared filter applied by every stats endpoint.
// Slice fields are OR-matched within themselves and AND-matched against each
// other, so bands=20m,40m&continent=EU means "20m or 40m, in Europe".
type StatsFilter struct {
	From time.Time
	To   time.Time

	Bands        []string
	Modes        []string
	Streams      []string
	Continents   []string
	CountryCodes []string
	Countries    []string
	CQZones      []int

	Callsign string // prefix match
	Spotter  string // prefix match
	Locator  string // grid-square prefix match
	Text     string // substring of comment or message

	// Exact variants, for looking a station up rather than browsing a prefix:
	// "who spotted G3ABC" must not also return G3ABCD.
	CallsignExact string
	SpotterExact  string

	// Exclusions. The receiver's own skimmer callsign submits most of the
	// spots on its own cluster, so it swamps any ranking unless it can be
	// taken out.
	CallsignExclude []string
	SpotterExclude  []string

	SNRMin, SNRMax   *float64
	DistMin, DistMax *float64
	FreqMin, FreqMax *float64 // kHz
	WPMMin, WPMMax   *int
	ConfMin          *float64
	HourMin, HourMax *int // UTC hour-of-day window, 0–23 inclusive
}

// modeExpr is the unified mode column, and it has to reconstruct the mode for
// two of the four streams:
//
//   - digital spots carry it in `mode` (FT8, FT4, …);
//   - voice spots carry USB/LSB in `voice_mode` and leave `mode` empty;
//   - CW skimmer spots leave BOTH empty — parseCWSpot never set a mode, because
//     the stream itself is the mode. The telnet filter has always compensated
//     in memory (see condMode in filter.go); this does the same in SQL, which
//     is what makes the thousands of already-stored CW spots findable.
//
// parseCWSpot now records "CW" too, so new rows don't rely on this fallback —
// but historical rows do, and retention runs to a month or more.
const modeExpr = `COALESCE(NULLIF(mode,''), NULLIF(voice_mode,''),
	CASE WHEN stream = 'cwskimmer' THEN 'CW' END)`

// hourExpr is the UTC hour of day, 0–23 — the axis behind "best time to work X".
const hourExpr = `CAST(strftime('%H', ts, 'unixepoch') AS INTEGER)`

// maxWindowDays caps how far back a query may reach, so a malformed or hostile
// `from` can't turn into a full-table scan of every retained spot.
const maxWindowDays = 400

// ParseStatsFilter builds a StatsFilter from a query string.
//
// The time window accepts either absolute bounds (`from`/`to`, RFC3339 or Unix
// seconds) or a relative lookback (`days`, or `hours`). It defaults to the last
// 24 hours, matching the dashboard's default period and keeping the common case
// cheap.
func ParseStatsFilter(q url.Values) (StatsFilter, error) {
	var f StatsFilter
	now := time.Now().UTC()

	// Absolute bounds win; otherwise fall back to a relative lookback.
	var err error
	if v := strings.TrimSpace(q.Get("from")); v != "" {
		if f.From, err = parseTimeParam(v); err != nil {
			return f, fmt.Errorf("bad from: %w", err)
		}
	}
	if v := strings.TrimSpace(q.Get("to")); v != "" {
		if f.To, err = parseTimeParam(v); err != nil {
			return f, fmt.Errorf("bad to: %w", err)
		}
	}
	if f.From.IsZero() {
		switch {
		case q.Get("hours") != "":
			n, e := strconv.Atoi(q.Get("hours"))
			if e != nil || n <= 0 {
				return f, fmt.Errorf("bad hours: %q", q.Get("hours"))
			}
			f.From = now.Add(-time.Duration(n) * time.Hour)
		case q.Get("days") != "":
			n, e := strconv.Atoi(q.Get("days"))
			if e != nil || n <= 0 {
				return f, fmt.Errorf("bad days: %q", q.Get("days"))
			}
			f.From = now.AddDate(0, 0, -n)
		default:
			f.From = now.Add(-24 * time.Hour)
		}
	}
	if f.To.IsZero() {
		f.To = now
	}
	if !f.To.After(f.From) {
		return f, fmt.Errorf("time window is empty: from >= to")
	}
	if oldest := now.AddDate(0, 0, -maxWindowDays); f.From.Before(oldest) {
		f.From = oldest
	}

	f.Bands = csvUpper(q, "band")
	f.Modes = csvUpper(q, "mode")
	f.Streams = csvLower(q, "stream")
	f.Continents = csvUpper(q, "continent")
	f.CountryCodes = csvUpper(q, "country_code")
	f.Countries = csvList(q, "country")
	for _, s := range csvList(q, "cq_zone") {
		n, e := strconv.Atoi(s)
		if e != nil {
			return f, fmt.Errorf("bad cq_zone: %q", s)
		}
		f.CQZones = append(f.CQZones, n)
	}
	// Band labels are lowercase in the DB ("20m"), unlike the other codes.
	for i, b := range f.Bands {
		f.Bands[i] = strings.ToLower(b)
	}

	f.Callsign = strings.ToUpper(strings.TrimSpace(q.Get("callsign")))
	f.Spotter = strings.ToUpper(strings.TrimSpace(q.Get("spotter")))
	f.Locator = strings.ToUpper(strings.TrimSpace(q.Get("locator")))
	f.Text = strings.TrimSpace(q.Get("q"))
	f.CallsignExact = strings.ToUpper(strings.TrimSpace(q.Get("callsign_exact")))
	f.SpotterExact = strings.ToUpper(strings.TrimSpace(q.Get("spotter_exact")))
	f.CallsignExclude = csvUpper(q, "callsign_exclude")
	f.SpotterExclude = csvUpper(q, "spotter_exclude")

	for _, spec := range []struct {
		key string
		dst **float64
	}{
		{"snr_min", &f.SNRMin}, {"snr_max", &f.SNRMax},
		{"dist_min", &f.DistMin}, {"dist_max", &f.DistMax},
		{"freq_min", &f.FreqMin}, {"freq_max", &f.FreqMax},
		{"conf_min", &f.ConfMin},
	} {
		v, e := optFloat(q, spec.key)
		if e != nil {
			return f, e
		}
		*spec.dst = v
	}
	for _, spec := range []struct {
		key string
		dst **int
	}{
		{"wpm_min", &f.WPMMin}, {"wpm_max", &f.WPMMax},
		{"hour_min", &f.HourMin}, {"hour_max", &f.HourMax},
	} {
		v, e := optInt(q, spec.key)
		if e != nil {
			return f, e
		}
		*spec.dst = v
	}
	for _, h := range []*int{f.HourMin, f.HourMax} {
		if h != nil && (*h < 0 || *h > 23) {
			return f, fmt.Errorf("hour_min/hour_max must be 0–23")
		}
	}

	return f, nil
}

// where renders the filter as a SQL WHERE body plus its bound arguments.
// The returned clause is never empty — the time window is always present.
func (f StatsFilter) where() (string, []any) {
	var cl []string
	var args []any

	cl = append(cl, "ts >= ? AND ts < ?")
	args = append(args, f.From.Unix(), f.To.Unix())

	inClause := func(expr string, vals []string) {
		if len(vals) == 0 {
			return
		}
		ph := make([]string, len(vals))
		for i, v := range vals {
			ph[i] = "?"
			args = append(args, v)
		}
		cl = append(cl, expr+" IN ("+strings.Join(ph, ",")+")")
	}
	inClause("band", f.Bands)
	inClause(modeExpr, f.Modes)
	inClause("stream", f.Streams)
	inClause("continent", f.Continents)
	inClause("country_code", f.CountryCodes)
	inClause("country", f.Countries)

	if len(f.CQZones) > 0 {
		ph := make([]string, len(f.CQZones))
		for i, v := range f.CQZones {
			ph[i] = "?"
			args = append(args, v)
		}
		cl = append(cl, "cq_zone IN ("+strings.Join(ph, ",")+")")
	}

	prefix := func(col, val string) {
		if val == "" {
			return
		}
		// A trailing * is redundant — prefix matching is the default — but it
		// is the natural thing for a user to type, so accept and drop it.
		cl = append(cl, col+` LIKE ? ESCAPE '\'`)
		args = append(args, escapeLike(strings.TrimSuffix(val, "*"))+"%")
	}
	prefix("callsign", f.Callsign)
	prefix("spotter", f.Spotter)
	prefix("locator", f.Locator)

	if f.CallsignExact != "" {
		cl = append(cl, "callsign = ?")
		args = append(args, f.CallsignExact)
	}
	if f.SpotterExact != "" {
		cl = append(cl, "spotter = ?")
		args = append(args, f.SpotterExact)
	}

	// COALESCE matters here: `col NOT IN (…)` is NULL — and so excludes the
	// row — when col is NULL, which would silently drop every spot that has
	// no spotter at all the moment one exclusion is set.
	notIn := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		ph := make([]string, len(vals))
		for i, v := range vals {
			ph[i] = "?"
			args = append(args, v)
		}
		cl = append(cl, "COALESCE("+col+",'') NOT IN ("+strings.Join(ph, ",")+")")
	}
	notIn("callsign", f.CallsignExclude)
	notIn("spotter", f.SpotterExclude)

	if f.Text != "" {
		cl = append(cl, `(comment LIKE ? ESCAPE '\' OR message LIKE ? ESCAPE '\')`)
		pat := "%" + escapeLike(f.Text) + "%"
		args = append(args, pat, pat)
	}

	rangeF := func(col string, lo, hi *float64, scale float64) {
		if lo != nil {
			cl = append(cl, col+" >= ?")
			args = append(args, *lo*scale)
		}
		if hi != nil {
			cl = append(cl, col+" <= ?")
			args = append(args, *hi*scale)
		}
	}
	rangeF("snr", f.SNRMin, f.SNRMax, 1)
	rangeF("distance_km", f.DistMin, f.DistMax, 1)
	rangeF("freq_hz", f.FreqMin, f.FreqMax, 1000) // params are kHz
	if f.ConfMin != nil {
		cl = append(cl, "confidence >= ?")
		args = append(args, *f.ConfMin)
	}
	if f.WPMMin != nil {
		cl = append(cl, "wpm >= ?")
		args = append(args, *f.WPMMin)
	}
	if f.WPMMax != nil {
		cl = append(cl, "wpm <= ?")
		args = append(args, *f.WPMMax)
	}

	// An hour window may wrap midnight (hour_min=22&hour_max=4 → the night
	// path), so the wrapped case becomes an OR rather than a BETWEEN.
	if f.HourMin != nil && f.HourMax != nil {
		if *f.HourMin <= *f.HourMax {
			cl = append(cl, hourExpr+" BETWEEN ? AND ?")
			args = append(args, *f.HourMin, *f.HourMax)
		} else {
			cl = append(cl, "("+hourExpr+" >= ? OR "+hourExpr+" <= ?)")
			args = append(args, *f.HourMin, *f.HourMax)
		}
	} else if f.HourMin != nil {
		cl = append(cl, hourExpr+" >= ?")
		args = append(args, *f.HourMin)
	} else if f.HourMax != nil {
		cl = append(cl, hourExpr+" <= ?")
		args = append(args, *f.HourMax)
	}

	return strings.Join(cl, " AND "), args
}

// Describe returns the filter as a JSON-friendly map, echoed back on every
// response so a client can render "what am I looking at" without re-parsing
// its own query string.
func (f StatsFilter) Describe() map[string]any {
	m := map[string]any{
		"from": f.From.UTC().Format(time.RFC3339),
		"to":   f.To.UTC().Format(time.RFC3339),
	}
	put := func(k string, v []string) {
		if len(v) > 0 {
			m[k] = v
		}
	}
	put("band", f.Bands)
	put("mode", f.Modes)
	put("stream", f.Streams)
	put("continent", f.Continents)
	put("country_code", f.CountryCodes)
	put("country", f.Countries)
	put("callsign_exclude", f.CallsignExclude)
	put("spotter_exclude", f.SpotterExclude)
	if len(f.CQZones) > 0 {
		m["cq_zone"] = f.CQZones
	}
	for k, v := range map[string]string{
		"callsign": f.Callsign, "spotter": f.Spotter,
		"locator": f.Locator, "q": f.Text,
		"callsign_exact": f.CallsignExact, "spotter_exact": f.SpotterExact,
	} {
		if v != "" {
			m[k] = v
		}
	}
	for k, v := range map[string]*float64{
		"snr_min": f.SNRMin, "snr_max": f.SNRMax,
		"dist_min": f.DistMin, "dist_max": f.DistMax,
		"freq_min": f.FreqMin, "freq_max": f.FreqMax,
		"conf_min": f.ConfMin,
	} {
		if v != nil {
			m[k] = *v
		}
	}
	for k, v := range map[string]*int{
		"wpm_min": f.WPMMin, "wpm_max": f.WPMMax,
		"hour_min": f.HourMin, "hour_max": f.HourMax,
	} {
		if v != nil {
			m[k] = *v
		}
	}
	return m
}

// ── Dimensions & metrics ───────────────────────────────────────────────────

// statsDim is one groupable axis: the SQL expression that produces its key,
// plus how that key should be presented.
type statsDim struct {
	Expr    string // SQL expression producing the group key
	Label   string // human-readable axis name
	Numeric bool   // key is a number (affects client-side sorting/axis type)
	// KeepEmpty allows rows whose key is NULL/'' through. Most dimensions drop
	// them — an unlabelled "" bar is noise, not a finding.
	KeepEmpty bool
	// Rank overrides axis ordering for keys that have a natural sequence which
	// is neither alphabetical nor numeric — bands, most obviously, where "160m"
	// belongs beside "80m" rather than between "15m" and "17m".
	Rank map[string]int
	// Extra is an optional aggregate returning a companion value for each group,
	// surfaced as `meta`. Grouping by country name yields the ISO code, which is
	// what lets the UI draw a flag beside it; grouping by code yields the name.
	Extra string
}

// bandRank orders band labels by frequency, using the same table that assigns
// them, so a band axis always reads 160m → 6m rather than alphabetically.
var bandRank = func() map[string]int {
	m := make(map[string]int, len(bandRanges))
	for i, b := range bandRanges {
		m[b.Name] = i
	}
	return m
}()

// statsDims is the whitelist of group-by axes. Nothing outside this map can
// reach the SQL, which is what makes group_by safe to take from a query string.
var statsDims = map[string]statsDim{
	"hour":       {Expr: hourExpr, Label: "UTC hour", Numeric: true, KeepEmpty: true},
	"weekday":    {Expr: `CAST(strftime('%w', ts, 'unixepoch') AS INTEGER)`, Label: "Weekday", Numeric: true, KeepEmpty: true},
	"date":       {Expr: `date(ts, 'unixepoch')`, Label: "Date"},
	"month":      {Expr: `strftime('%Y-%m', ts, 'unixepoch')`, Label: "Month"},
	"band":       {Expr: `band`, Label: "Band", Rank: bandRank},
	"mode":       {Expr: modeExpr, Label: "Mode"},
	"stream":     {Expr: `stream`, Label: "Source"},
	"callsign":   {Expr: `callsign`, Label: "Callsign"},
	"spotter":    {Expr: `spotter`, Label: "Spotter"},
	"country":    {Expr: `country`, Label: "Country", Extra: `MAX(country_code)`},
	"cc":         {Expr: `country_code`, Label: "Country code", Extra: `MAX(country)`},
	"continent":  {Expr: `continent`, Label: "Continent"},
	"cq_zone":    {Expr: `cq_zone`, Label: "CQ zone", Numeric: true},
	"locator":    {Expr: `substr(locator,1,4)`, Label: "Grid square"},
	"field":      {Expr: `substr(locator,1,2)`, Label: "Grid field"},
	"wpm":        {Expr: `wpm`, Label: "WPM", Numeric: true},
	"voice_mode": {Expr: `voice_mode`, Label: "Voice mode"},
	// Bucketed continuous values. FLOOR keeps negative SNR bucketing correct —
	// a plain CAST truncates toward zero and would fold -4 and +4 together.
	"snr_bucket":  {Expr: `CAST(FLOOR(snr/5.0) AS INTEGER)*5`, Label: "SNR (5 dB bins)", Numeric: true},
	"dist_bucket": {Expr: `CAST(FLOOR(distance_km/500.0) AS INTEGER)*500`, Label: "Distance (500 km bins)", Numeric: true},
	"freq_bucket": {Expr: `CAST(FLOOR(freq_hz/1000.0) AS INTEGER)`, Label: "Frequency (kHz)", Numeric: true},
}

// statsMetrics is the whitelist of measures. Each is an aggregate over the
// filtered rows in a group.
type statsMetric struct {
	Expr  string
	Label string
	// Zero says an empty bucket means zero rather than "no data". It is true
	// for counting metrics — an hour with no spots really did see zero spots —
	// and false for averages, where an empty bucket has no value at all and
	// must stay a gap rather than being drawn as a plunge to 0 dB.
	Zero bool
	// Summable says values from different series can be added together, which
	// is what lets the tail beyond the series cap fold into one "Other" line.
	// Only a plain row count qualifies: distinct counts would double-count a
	// callsign heard on two bands, and averages cannot be added at all.
	Summable bool
}

// Distinct counts wrap the column in NULLIF(…,”): the streams that don't
// populate a field store an empty string rather than NULL, and SQLite counts ”
// as a value of its own. Without this, any window containing digital or voice
// spots reports one more spotter (and one more country) than it has.
var statsMetrics = map[string]statsMetric{
	"count":     {`COUNT(*)`, "Spots", true, true},
	"calls":     {`COUNT(DISTINCT NULLIF(callsign,''))`, "Unique callsigns", true, false},
	"countries": {`COUNT(DISTINCT NULLIF(country_code,''))`, "Unique countries", true, false},
	"spotters":  {`COUNT(DISTINCT NULLIF(spotter,''))`, "Unique spotters", true, false},
	"bands":     {`COUNT(DISTINCT NULLIF(band,''))`, "Unique bands", true, false},
	"avg_snr":   {`AVG(snr)`, "Average SNR (dB)", false, false},
	"max_snr":   {`MAX(snr)`, "Peak SNR (dB)", false, false},
	"avg_dist":  {`AVG(distance_km)`, "Average distance (km)", false, false},
	"max_dist":  {`MAX(distance_km)`, "Max distance (km)", false, false},
	"avg_wpm":   {`AVG(wpm)`, "Average WPM", false, false},
}

// StatsDimsList returns the group-by whitelist for the UI's dimension pickers.
func StatsDimsList() []map[string]any {
	out := make([]map[string]any, 0, len(statsDims))
	for k, d := range statsDims {
		out = append(out, map[string]any{"key": k, "label": d.Label, "numeric": d.Numeric})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["label"].(string) < out[j]["label"].(string) })
	return out
}

// StatsMetricsList returns the metric whitelist for the UI's metric pickers.
func StatsMetricsList() []map[string]any {
	out := make([]map[string]any, 0, len(statsMetrics))
	for k, m := range statsMetrics {
		out = append(out, map[string]any{"key": k, "label": m.Label})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["label"].(string) < out[j]["label"].(string) })
	return out
}

// ── Breakdown ──────────────────────────────────────────────────────────────

// BreakdownRow is one group: its key plus the full metric set. Metrics that
// have no data in the group (no SNR on voice-only groups, say) stay nil so the
// client can render "—" rather than a misleading zero.
type BreakdownRow struct {
	Key string `json:"key"`
	// Meta is the dimension's companion value (see statsDim.Extra); empty for
	// dimensions that don't define one.
	Meta      string   `json:"meta,omitempty"`
	Count     int64    `json:"count"`
	Calls     int64    `json:"calls"`
	Countries int64    `json:"countries"`
	Spotters  int64    `json:"spotters"`
	Bands     int64    `json:"bands"`
	AvgSNR    *float64 `json:"avg_snr"`
	MaxSNR    *float64 `json:"max_snr"`
	AvgDist   *float64 `json:"avg_dist"`
	MaxDist   *float64 `json:"max_dist"`
	AvgWPM    *float64 `json:"avg_wpm"`
	FirstTS   int64    `json:"first_ts"`
	LastTS    int64    `json:"last_ts"`
}

// Breakdown groups the filtered spots by one dimension and returns the full
// metric set per group, ordered by `sortBy` (a metric key) descending.
//
// This is the workhorse behind every ranking, bar chart and hour-of-day plot:
// group_by=country answers "who is loudest", group_by=hour answers "when".
func (s *SpotStore) Breakdown(f StatsFilter, dim, sortBy string, limit int) ([]BreakdownRow, error) {
	d, ok := statsDims[dim]
	if !ok {
		return nil, fmt.Errorf("unknown group_by %q", dim)
	}
	if _, ok := statsMetrics[sortBy]; !ok {
		sortBy = "count"
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	whereSQL, args := f.where()
	if !d.KeepEmpty {
		whereSQL += " AND " + d.Expr + " IS NOT NULL AND " + d.Expr + " != ''"
	}

	// Ordering is by the requested metric, with count as a stable tiebreak so
	// equal averages don't shuffle between requests.
	extra := `''`
	if d.Extra != "" {
		extra = d.Extra
	}
	q := fmt.Sprintf(`
		SELECT %s AS k, %s,
		       COUNT(*), COUNT(DISTINCT NULLIF(callsign,'')),
		       COUNT(DISTINCT NULLIF(country_code,'')),
		       COUNT(DISTINCT NULLIF(spotter,'')), COUNT(DISTINCT NULLIF(band,'')),
		       AVG(snr), MAX(snr), AVG(distance_km), MAX(distance_km), AVG(wpm),
		       MIN(ts), MAX(ts)
		FROM spots
		WHERE %s
		GROUP BY k
		ORDER BY %s DESC, COUNT(*) DESC
		LIMIT ?`,
		d.Expr, extra, whereSQL, statsMetrics[sortBy].Expr)
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []BreakdownRow{}
	for rows.Next() {
		var r BreakdownRow
		var key, meta sql.NullString
		var avgSNR, maxSNR, avgDist, maxDist, avgWPM sql.NullFloat64
		var first, last sql.NullInt64
		if err := rows.Scan(&key, &meta, &r.Count, &r.Calls, &r.Countries, &r.Spotters, &r.Bands,
			&avgSNR, &maxSNR, &avgDist, &maxDist, &avgWPM, &first, &last); err != nil {
			return nil, err
		}
		r.Key, r.Meta = key.String, meta.String
		r.AvgSNR, r.MaxSNR = round1(avgSNR), round1(maxSNR)
		r.AvgDist, r.MaxDist, r.AvgWPM = round1(avgDist), round1(maxDist), round1(avgWPM)
		r.FirstTS, r.LastTS = first.Int64, last.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── Matrix ─────────────────────────────────────────────────────────────────

// MatrixCell is one cell of a two-dimensional pivot: the x/y keys, the chosen
// metric, and the underlying sample size behind it.
type MatrixCell struct {
	X string   `json:"x"`
	Y string   `json:"y"`
	V *float64 `json:"v"`
	N int64    `json:"n"`
}

// Matrix cross-tabulates two dimensions — the heatmap behind questions like
// "best time to hear Germany on 40m", which is x=hour, y=band, country_code=DE.
//
// It returns the cells plus the sorted axis keys, so the client renders a full
// grid (including empty cells) rather than only the populated ones.
// MatrixAxes carries the sorted keys for each axis plus, where the dimension
// defines one, a key → companion value map (country name → ISO code) so the
// client can label the axis with a flag.
type MatrixAxes struct {
	XKeys, YKeys []string
	XMeta, YMeta map[string]string
}

func (s *SpotStore) Matrix(f StatsFilter, xDim, yDim, metric string, limitY int) ([]MatrixCell, MatrixAxes, error) {
	var axes MatrixAxes
	x, ok := statsDims[xDim]
	if !ok {
		return nil, axes, fmt.Errorf("unknown x %q", xDim)
	}
	y, ok := statsDims[yDim]
	if !ok {
		return nil, axes, fmt.Errorf("unknown y %q", yDim)
	}
	m, ok := statsMetrics[metric]
	if !ok {
		return nil, axes, fmt.Errorf("unknown metric %q", metric)
	}
	if limitY <= 0 || limitY > 100 {
		limitY = 25
	}

	whereSQL, args := f.where()
	for _, d := range []statsDim{x, y} {
		if !d.KeepEmpty {
			whereSQL += " AND " + d.Expr + " IS NOT NULL AND " + d.Expr + " != ''"
		}
	}

	xExtra, yExtra := `''`, `''`
	if x.Extra != "" {
		xExtra = x.Extra
	}
	if y.Extra != "" {
		yExtra = y.Extra
	}
	q := fmt.Sprintf(`
		SELECT %s AS xk, %s AS yk, %s, %s, %s, COUNT(*)
		FROM spots
		WHERE %s
		GROUP BY xk, yk`, x.Expr, y.Expr, xExtra, yExtra, m.Expr, whereSQL)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, axes, err
	}
	defer func() { _ = rows.Close() }()

	cells := []MatrixCell{}
	xSeen, ySeen := map[string]bool{}, map[string]bool{}
	xMeta, yMeta := map[string]string{}, map[string]string{}
	yWeight := map[string]int64{} // total spots per y, for trimming to top rows
	for rows.Next() {
		var xk, yk, xm, ym sql.NullString
		var v sql.NullFloat64
		var n int64
		if err := rows.Scan(&xk, &yk, &xm, &ym, &v, &n); err != nil {
			return nil, axes, err
		}
		c := MatrixCell{X: xk.String, Y: yk.String, V: round1(v), N: n}
		cells = append(cells, c)
		xSeen[c.X], ySeen[c.Y] = true, true
		if xm.String != "" {
			xMeta[c.X] = xm.String
		}
		if ym.String != "" {
			yMeta[c.Y] = ym.String
		}
		yWeight[c.Y] += n
	}
	if err := rows.Err(); err != nil {
		return nil, axes, err
	}

	xKeys := sortedKeys(xSeen, x)
	yKeys := sortedKeys(ySeen, y)

	// A y-axis of 400 countries is unreadable. Keep the busiest rows, then
	// restore the axis's natural order so trimming doesn't also re-sort it.
	if len(yKeys) > limitY {
		byWeight := append([]string(nil), yKeys...)
		sort.SliceStable(byWeight, func(i, j int) bool { return yWeight[byWeight[i]] > yWeight[byWeight[j]] })
		keep := map[string]bool{}
		for _, k := range byWeight[:limitY] {
			keep[k] = true
		}
		yKeys = sortedKeys(keep, y)
		kept := cells[:0]
		for _, c := range cells {
			if keep[c.Y] {
				kept = append(kept, c)
			}
		}
		cells = kept
	}
	axes = MatrixAxes{XKeys: xKeys, YKeys: yKeys, XMeta: xMeta, YMeta: yMeta}
	return cells, axes, nil
}

// ── Time series ────────────────────────────────────────────────────────────

// SeriesPoint is one bucket of a time series: the bucket start (Unix seconds),
// its label, and the metric value.
type SeriesPoint struct {
	TS    int64    `json:"ts"`
	Label string   `json:"label"`
	V     *float64 `json:"v"`
	N     int64    `json:"n"`
}

// bucketSpecs maps a bucket name to the SQLite strftime format that floors a
// timestamp to it, the Go layout that parses the result back to a time, and the
// step between consecutive buckets (zero where they can't be enumerated).
var bucketSpecs = map[string]struct {
	format, layout string
	step           time.Duration
}{
	"hour": {"%Y-%m-%dT%H:00:00Z", "2006-01-02T15:04:05Z", time.Hour},
	"day":  {"%Y-%m-%d", "2006-01-02", 24 * time.Hour},
	"week": {"%Y-%W", "", 0}, // ISO-ish week; label only, no parseable instant
}

// OtherSeriesName is the label for the folded remainder of a split time series.
// It is not an entity, so the UI gives it a neutral colour rather than one of
// the categorical slots.
const OtherSeriesName = "Other"

// maxFillBuckets caps gap filling. Past this many buckets the axis is too dense
// to read anyway, and materialising every empty one is pure waste.
const maxFillBuckets = 2000

// bucketLabels enumerates every bucket label covering the filter window, so a
// series can be gap-filled. Returns nil when the bucket can't be enumerated or
// the window is too long to be worth filling.
func (f StatsFilter) bucketLabels(bucket string) []string {
	spec := bucketSpecs[bucket]
	if spec.step == 0 {
		return nil
	}
	start := f.From.UTC().Truncate(spec.step)
	if int(f.To.Sub(start)/spec.step)+1 > maxFillBuckets {
		return nil
	}
	var out []string
	for t := start; t.Before(f.To); t = t.Add(spec.step) {
		out = append(out, t.Format(spec.layout))
	}
	return out
}

// TimeSeries buckets the filtered spots over wall-clock time, optionally split
// into one series per value of `splitBy` (band, mode, continent, …).
//
// The returned map is series name → points; an unsplit query uses the single
// key "all".
func (s *SpotStore) TimeSeries(f StatsFilter, bucket, metric, splitBy string, maxSeries int) (map[string][]SeriesPoint, []string, error) {
	spec, ok := bucketSpecs[bucket]
	if !ok {
		return nil, nil, fmt.Errorf("unknown bucket %q", bucket)
	}
	m, ok := statsMetrics[metric]
	if !ok {
		return nil, nil, fmt.Errorf("unknown metric %q", metric)
	}
	if maxSeries <= 0 || maxSeries > 12 {
		maxSeries = 8
	}

	splitExpr := "'all'"
	if splitBy != "" {
		d, ok := statsDims[splitBy]
		if !ok {
			return nil, nil, fmt.Errorf("unknown split_by %q", splitBy)
		}
		splitExpr = d.Expr
	}

	whereSQL, args := f.where()
	bucketExpr := fmt.Sprintf(`strftime('%s', ts, 'unixepoch')`, spec.format)
	q := fmt.Sprintf(`
		SELECT %s AS b, COALESCE(NULLIF(%s,''),'—') AS sk, %s, COUNT(*)
		FROM spots
		WHERE %s
		GROUP BY b, sk
		ORDER BY b ASC`, bucketExpr, splitExpr, m.Expr, whereSQL)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	series := map[string][]SeriesPoint{}
	weight := map[string]int64{}
	for rows.Next() {
		var b, sk string
		var v sql.NullFloat64
		var n int64
		if err := rows.Scan(&b, &sk, &v, &n); err != nil {
			return nil, nil, err
		}
		p := SeriesPoint{Label: b, V: round1(v), N: n}
		if spec.layout != "" {
			if t, e := time.Parse(spec.layout, b); e == nil {
				p.TS = t.Unix()
			}
		}
		series[sk] = append(series[sk], p)
		weight[sk] += n
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// Rank series by volume and keep the top N — a legend with 200 entries is
	// worse than no legend. Colour follows the series name, so the surviving
	// series keep their identity when the filter changes.
	names := make([]string, 0, len(series))
	for k := range series {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		if weight[names[i]] != weight[names[j]] {
			return weight[names[i]] > weight[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) > maxSeries {
		// Copy the tail: names[maxSeries:] aliases the same backing array, so
		// appending "Other" to the truncated names would overwrite dropped[0]
		// — and the cleanup loop below would then delete the folded series.
		dropped := append([]string(nil), names[maxSeries:]...)
		names = names[:maxSeries:maxSeries]
		// Fold the tail into one "Other" line rather than discarding it: with
		// ten bands and a seven-slot palette, dropping the quiet three would
		// make the chart quietly under-report the period's spots. Only a
		// summable metric can be folded; for the rest the tail is genuinely
		// not combinable, and the caller is told how many were left out.
		if m.Summable {
			agg := map[string]*SeriesPoint{}
			for _, n := range dropped {
				for _, p := range series[n] {
					e, ok := agg[p.Label]
					if !ok {
						v := 0.0
						e = &SeriesPoint{Label: p.Label, TS: p.TS, V: &v}
						agg[p.Label] = e
					}
					if p.V != nil {
						*e.V += *p.V
					}
					e.N += p.N
				}
			}
			labels := make([]string, 0, len(agg))
			for l := range agg {
				labels = append(labels, l)
			}
			sort.Strings(labels) // bucket labels are ISO-ish, so this is chronological
			pts := make([]SeriesPoint, 0, len(labels))
			for _, l := range labels {
				pts = append(pts, *agg[l])
			}
			series[OtherSeriesName] = pts
			names = append(names, OtherSeriesName)
		}
		for _, n := range dropped {
			delete(series, n)
		}
	}

	// Fill the gaps. Without this a quiet slice — say FT8 from Germany on 40m —
	// returns only the handful of buckets that had spots, and a line drawn
	// through them implies steady activity across the silent hours between.
	if labels := f.bucketLabels(bucket); labels != nil {
		for _, n := range names {
			series[n] = fillBuckets(series[n], labels, spec.layout, m.Zero)
		}
	}
	return series, names, nil
}

// fillBuckets returns points for every label in order, inserting empty buckets
// where the query produced none. Counting metrics fill with 0; averages fill
// with nil so the client can break the line instead of drawing through them.
func fillBuckets(pts []SeriesPoint, labels []string, layout string, zero bool) []SeriesPoint {
	have := make(map[string]SeriesPoint, len(pts))
	for _, p := range pts {
		have[p.Label] = p
	}
	out := make([]SeriesPoint, 0, len(labels))
	for _, l := range labels {
		if p, ok := have[l]; ok {
			out = append(out, p)
			continue
		}
		p := SeriesPoint{Label: l}
		if zero {
			v := 0.0
			p.V = &v
		}
		if t, err := time.Parse(layout, l); err == nil {
			p.TS = t.Unix()
		}
		out = append(out, p)
	}
	return out
}

// ── Summary ────────────────────────────────────────────────────────────────

// Summary returns the headline figures for the current filter, plus the single
// best UTC hour and band — the direct answer to "when should I listen?".
func (s *SpotStore) Summary(f StatsFilter) (map[string]any, error) {
	whereSQL, args := f.where()

	var total, calls, countries, spotters, bands int64
	var avgSNR, maxSNR, avgDist, maxDist sql.NullFloat64
	var first, last sql.NullInt64
	err := s.db.QueryRow(`
		SELECT COUNT(*), COUNT(DISTINCT NULLIF(callsign,'')),
		       COUNT(DISTINCT NULLIF(country_code,'')),
		       COUNT(DISTINCT NULLIF(spotter,'')), COUNT(DISTINCT NULLIF(band,'')),
		       AVG(snr), MAX(snr), AVG(distance_km), MAX(distance_km),
		       MIN(ts), MAX(ts)
		FROM spots WHERE `+whereSQL, args...).
		Scan(&total, &calls, &countries, &spotters, &bands,
			&avgSNR, &maxSNR, &avgDist, &maxDist, &first, &last)
	if err != nil {
		return nil, err
	}

	out := map[string]any{
		"spots":     total,
		"callsigns": calls,
		"countries": countries,
		"spotters":  spotters,
		"bands":     bands,
		"avg_snr":   round1(avgSNR),
		"max_snr":   round1(maxSNR),
		"avg_dist":  round1(avgDist),
		"max_dist":  round1(maxDist),
		"first_ts":  first.Int64,
		"last_ts":   last.Int64,
	}

	// Spots per hour of coverage, so windows of different lengths compare.
	if hours := f.To.Sub(f.From).Hours(); hours > 0 {
		out["per_hour"] = math.Round(float64(total)/hours*10) / 10
	}

	// The headline "best" values — top group on the two axes people ask about.
	for key, dim := range map[string]string{"best_hour": "hour", "best_band": "band"} {
		rows, err := s.Breakdown(f, dim, "count", 1)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			out[key] = map[string]any{"key": rows[0].Key, "count": rows[0].Count, "avg_snr": rows[0].AvgSNR}
		}
	}
	return out, nil
}

// ── Facets ─────────────────────────────────────────────────────────────────

// Facets returns the distinct values actually present for the filterable
// dimensions in the current window, so the UI's dropdowns only ever offer
// choices that will return something.
func (s *SpotStore) Facets(f StatsFilter) (map[string]any, error) {
	out := map[string]any{}
	for _, dim := range []string{"band", "mode", "stream", "continent", "cc", "cq_zone"} {
		rows, err := s.Breakdown(f, dim, "count", 300)
		if err != nil {
			return nil, err
		}
		vals := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			vals = append(vals, map[string]any{"key": r.Key, "count": r.Count})
		}
		out[dim] = vals
	}
	// Countries carry both a code and a name; pair them up so the UI can show
	// the name while filtering on the code.
	whereSQL, args := f.where()
	rows, err := s.db.Query(`
		SELECT country_code, country, COUNT(*) FROM spots
		WHERE `+whereSQL+` AND country_code IS NOT NULL AND country_code != ''
		GROUP BY country_code ORDER BY COUNT(*) DESC LIMIT 400`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var countries []map[string]any
	for rows.Next() {
		var cc, name sql.NullString
		var n int64
		if err := rows.Scan(&cc, &name, &n); err != nil {
			return nil, err
		}
		countries = append(countries, map[string]any{"key": cc.String, "name": name.String, "count": n})
	}
	out["country"] = countries
	return out, rows.Err()
}

// ── Spot listing ───────────────────────────────────────────────────────────

// ListSpots returns the raw filtered spots, newest first — the drill-down
// behind every aggregate above.
func (s *SpotStore) ListSpots(f StatsFilter, limit, offset int) ([]Spot, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	whereSQL, args := f.where()

	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM spots WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.Query(`
		SELECT stream, ts, band, callsign, freq_hz, snr,
		       country, country_code, continent, cq_zone,
		       mode, comment, message, spotter, wpm, locator,
		       voice_mode, est_dial_freq, confidence, bandwidth,
		       avg_signal_db, peak_signal_db, distance_km, bearing_deg
		FROM spots WHERE `+whereSQL+`
		ORDER BY ts DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	out := []Spot{}
	for rows.Next() {
		sp, err := scanSpot(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, sp)
	}
	return out, total, rows.Err()
}

// scanSpot reads one spots row into a Spot. Every text/number column is
// nullable in practice — each stream populates a different subset.
func scanSpot(rows *sql.Rows) (Spot, error) {
	var sp Spot
	var ts int64
	var cqZone, wpm, estDialFreq, bandwidth sql.NullInt64
	var confidence, avgSignalDB, peakSignalDB, distanceKM, bearingDeg sql.NullFloat64
	var stream, band, callsign, country, countryCode, continent sql.NullString
	var mode, comment, message, spotter, locator, voiceMode sql.NullString

	err := rows.Scan(
		&stream, &ts, &band, &callsign, &sp.FreqHz, &sp.SNR,
		&country, &countryCode, &continent, &cqZone,
		&mode, &comment, &message, &spotter, &wpm, &locator,
		&voiceMode, &estDialFreq, &confidence, &bandwidth,
		&avgSignalDB, &peakSignalDB, &distanceKM, &bearingDeg,
	)
	if err != nil {
		return sp, err
	}
	sp.Stream = StreamType(stream.String)
	sp.Timestamp = time.Unix(ts, 0).UTC()
	sp.Band, sp.Callsign = band.String, callsign.String
	sp.Country, sp.CountryCode, sp.Continent = country.String, countryCode.String, continent.String
	sp.CQZone = int(cqZone.Int64)
	sp.Mode, sp.Comment, sp.Message = mode.String, comment.String, message.String
	sp.Spotter, sp.Locator, sp.VoiceMode = spotter.String, locator.String, voiceMode.String
	sp.WPM, sp.EstDialFreq, sp.Bandwidth = int(wpm.Int64), int(estDialFreq.Int64), int(bandwidth.Int64)
	sp.Confidence = confidence.Float64
	sp.AvgSignalDB, sp.PeakSignalDB = avgSignalDB.Float64, peakSignalDB.Float64
	sp.DistanceKM, sp.BearingDeg = distanceKM.Float64, bearingDeg.Float64
	return sp, nil
}

// ── helpers ────────────────────────────────────────────────────────────────

// parseTimeParam accepts RFC3339, a bare date, or Unix seconds.
func parseTimeParam(v string) (time.Time, error) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0).UTC(), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q", v)
}

// csvList splits a repeated-or-comma-separated query param into trimmed values.
func csvList(q url.Values, key string) []string {
	var out []string
	for _, raw := range q[key] {
		for _, part := range strings.Split(raw, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func csvUpper(q url.Values, key string) []string {
	out := csvList(q, key)
	for i := range out {
		out[i] = strings.ToUpper(out[i])
	}
	return out
}

func csvLower(q url.Values, key string) []string {
	out := csvList(q, key)
	for i := range out {
		out[i] = strings.ToLower(out[i])
	}
	return out
}

func optFloat(q url.Values, key string) (*float64, error) {
	v := strings.TrimSpace(q.Get(key))
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil, fmt.Errorf("bad %s: %q", key, v)
	}
	return &n, nil
}

func optInt(q url.Values, key string) (*int, error) {
	v := strings.TrimSpace(q.Get(key))
	if v == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil, fmt.Errorf("bad %s: %q", key, v)
	}
	return &n, nil
}

// escapeLike neutralises LIKE wildcards in user input so a search for "G_" is
// a literal underscore, not "any character". Pairs with ESCAPE '\'.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// round1 rounds a nullable aggregate to one decimal, preserving NULL as nil so
// "no data" stays distinguishable from "zero".
func round1(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	r := math.Round(v.Float64*10) / 10
	return &r
}

// sortedKeys returns map keys in axis order: by the dimension's explicit rank
// where it has one, else numerically for numeric dimensions (so hour 9 precedes
// hour 10), else lexically.
func sortedKeys(m map[string]bool, d statsDim) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	switch {
	case d.Rank != nil:
		sort.Slice(out, func(i, j int) bool {
			ri, oki := d.Rank[out[i]]
			rj, okj := d.Rank[out[j]]
			if oki != okj {
				return oki // ranked keys first; unknown labels sort to the end
			}
			if !oki {
				return out[i] < out[j]
			}
			return ri < rj
		})
	case d.Numeric:
		sort.Slice(out, func(i, j int) bool {
			a, _ := strconv.ParseFloat(out[i], 64)
			b, _ := strconv.ParseFloat(out[j], 64)
			return a < b
		})
	default:
		sort.Strings(out)
	}
	return out
}
