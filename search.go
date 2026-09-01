package main

// search.go — the parametric search layer over the spots table.
//
// Search reuses StatsFilter for the "which rows" half of the question, so every
// filter the analytics API understands works here unchanged and there is only
// one place where user input becomes SQL. What it adds is the "which rows, in
// what order, how many at a time" half: a whitelisted sort key, keyset (cursor)
// pagination for the time-ordered case, bounded counting, field projection and
// a CSV rendering of the same rows.
//
// Three rules keep an open endpoint from becoming a way to stall the database:
//
//   - every identifier reaching SQL comes from a whitelist, every value is bound;
//   - the time window is capped (maxWindowDays) and the row count is capped
//     (maxSearchLimit), so no single request can ask for the whole table;
//   - the row count is answered by a bounded subquery rather than a full
//     COUNT(*), so "how many match" costs at most searchCountCap rows.

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Result-size and cost bounds. These are echoed by /api/search/meta so a client
// can size its paging without discovering the limits by trial and error.
const (
	defaultSearchLimit = 100
	maxSearchLimit     = 1000

	// maxSearchOffset bounds classic paging. OFFSET makes SQLite walk and
	// discard every skipped row, so deep paging costs the same as reading the
	// whole prefix; past this point the cursor is the only sensible way through
	// and the error says so.
	maxSearchOffset = 100_000

	// searchCountCap bounds the "total matches" figure. Counting is a separate
	// scan from fetching the page, and on a wide window that scan dominates the
	// request — so it stops here and the response says the total is capped.
	searchCountCap = 100_000

	// searchTimeout is the server-side deadline for one search. A sort on an
	// unindexed column over a month of spots is legitimate but slow; this
	// bounds it rather than banning it.
	searchTimeout = 15 * time.Second
)

// ── Sort keys ──────────────────────────────────────────────────────────────

// searchSort is one whitelisted ordering. Expr is interpolated into the ORDER
// BY, so it may only ever come from this table.
type searchSort struct {
	Expr  string // SQL expression to order by
	Label string // human-readable name
	// Cursor marks the ordering that supports keyset pagination. Only `ts` does:
	// it is the one sort backed by an index, and the one whose value is carried
	// in the cursor token.
	Cursor bool
	// Desc is the direction that reads as "most interesting first", used when
	// the caller names a sort but no order.
	Desc bool
}

var searchSorts = map[string]searchSort{
	"ts":         {Expr: "ts", Label: "Time", Cursor: true, Desc: true},
	"freq":       {Expr: "freq_hz", Label: "Frequency", Desc: false},
	"snr":        {Expr: "snr", Label: "SNR", Desc: true},
	"distance":   {Expr: "distance_km", Label: "Distance", Desc: true},
	"wpm":        {Expr: "wpm", Label: "CW speed", Desc: true},
	"confidence": {Expr: "confidence", Label: "Confidence", Desc: true},
	"callsign":   {Expr: "callsign", Label: "Callsign", Desc: false},
	"spotter":    {Expr: "spotter", Label: "Spotter", Desc: false},
	"country":    {Expr: "country", Label: "Country", Desc: false},
	"mode":       {Expr: modeExpr, Label: "Mode", Desc: false},
	"locator":    {Expr: "locator", Label: "Grid square", Desc: false},
}

// SearchSortsList describes the sort whitelist for /api/search/meta.
func SearchSortsList() []map[string]any {
	out := make([]map[string]any, 0, len(searchSorts))
	for _, k := range sortedMapKeys(searchSorts) {
		s := searchSorts[k]
		out = append(out, map[string]any{
			"key": k, "label": s.Label,
			"default_order": orderWord(s.Desc),
			"cursor":        s.Cursor,
		})
	}
	return out
}

// ── Field projection ───────────────────────────────────────────────────────

// searchField is one selectable output column. The same table drives the
// `fields` parameter, the CSV column set and the meta listing, so a field can
// never be documented without being emittable (or vice versa).
type searchField struct {
	Key   string
	Label string
	Get   func(Spot) any
}

var searchFields = []searchField{
	{"timestamp", "Time (UTC)", func(s Spot) any { return s.Timestamp.UTC().Format(time.RFC3339) }},
	{"stream", "Source", func(s Spot) any { return string(s.Stream) }},
	{"callsign", "Callsign", func(s Spot) any { return s.Callsign }},
	{"freq_hz", "Frequency (Hz)", func(s Spot) any { return s.FreqHz }},
	{"band", "Band", func(s Spot) any { return s.Band }},
	{"mode", "Mode", func(s Spot) any { return s.Mode }},
	{"voice_mode", "Voice mode", func(s Spot) any { return s.VoiceMode }},
	{"snr", "SNR (dB)", func(s Spot) any { return s.SNR }},
	{"spotter", "Spotter", func(s Spot) any { return s.Spotter }},
	{"country", "Country", func(s Spot) any { return s.Country }},
	{"country_code", "Country code", func(s Spot) any { return s.CountryCode }},
	{"continent", "Continent", func(s Spot) any { return s.Continent }},
	{"cq_zone", "CQ zone", func(s Spot) any { return s.CQZone }},
	{"locator", "Grid square", func(s Spot) any { return s.Locator }},
	{"distance_km", "Distance (km)", func(s Spot) any { return s.DistanceKM }},
	{"bearing_deg", "Bearing (°)", func(s Spot) any { return s.BearingDeg }},
	{"wpm", "CW speed (WPM)", func(s Spot) any { return s.WPM }},
	{"confidence", "Confidence", func(s Spot) any { return s.Confidence }},
	{"bandwidth", "Bandwidth (Hz)", func(s Spot) any { return s.Bandwidth }},
	{"est_dial_freq", "Est. dial freq (Hz)", func(s Spot) any { return s.EstDialFreq }},
	{"avg_signal_db", "Avg signal (dB)", func(s Spot) any { return s.AvgSignalDB }},
	{"peak_signal_db", "Peak signal (dB)", func(s Spot) any { return s.PeakSignalDB }},
	{"comment", "Comment", func(s Spot) any { return s.Comment }},
	{"message", "Message", func(s Spot) any { return s.Message }},
}

// searchFieldByKey indexes searchFields for lookup during parsing.
var searchFieldByKey = func() map[string]searchField {
	m := make(map[string]searchField, len(searchFields))
	for _, f := range searchFields {
		m[f.Key] = f
	}
	return m
}()

// SearchFieldsList describes the projectable fields for /api/search/meta.
func SearchFieldsList() []map[string]any {
	out := make([]map[string]any, 0, len(searchFields))
	for _, f := range searchFields {
		out = append(out, map[string]any{"key": f.Key, "label": f.Label})
	}
	return out
}

// Project renders a spot as the selected subset of fields. An empty selection
// means "the whole spot", which callers render as the Spot struct itself.
func projectSpot(sp Spot, fields []searchField) map[string]any {
	m := make(map[string]any, len(fields))
	for _, f := range fields {
		m[f.Key] = f.Get(sp)
	}
	return m
}

// ── Parameters ─────────────────────────────────────────────────────────────

// SearchParams is one parsed search request: which rows (Filter), in what order
// (Sort/Order), which slice of them (Limit/Offset/Cursor), how much of each row
// (Fields) and how hard to work out how many there are (Count).
type SearchParams struct {
	Filter StatsFilter

	Sort  string
	Order string // "asc" | "desc"

	Limit  int
	Offset int
	Cursor string

	// Count selects the row-count strategy: "capped" (default, stops at
	// searchCountCap), "exact" (full COUNT, may be slow on a wide window) or
	// "none" (skip it — the cheapest option, and enough when you only page
	// forward).
	Count string

	Fields []searchField // empty → whole spot
	Format string        // "json" | "csv"

	// Warnings records values that were clamped rather than rejected, so a
	// caller sees why it got 1000 rows after asking for 50000.
	Warnings []string
}

// ParseSearchParams validates and ranges a search query string.
//
// Out-of-range numbers are clamped and reported in Warnings rather than
// rejected — a caller exploring the API should get results plus an explanation,
// not a 400. Values that cannot mean anything (an unknown sort key, an
// unparseable cursor, a reversed time window) are errors, because guessing at
// them would silently answer a different question than the one asked.
func ParseSearchParams(q url.Values) (SearchParams, error) {
	var p SearchParams
	var err error

	if p.Filter, err = ParseStatsFilter(q); err != nil {
		return p, err
	}
	// ParseStatsFilter silently pulls `from` back to the retention horizon.
	// Say so, otherwise a query for "the last 5 years" quietly becomes 400 days.
	if requested := strings.TrimSpace(q.Get("from")); requested != "" {
		if t, e := parseTimeParam(requested); e == nil && t.Before(p.Filter.From) {
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"from clamped to %s (search reaches back at most %d days)",
				p.Filter.From.Format(time.RFC3339), maxWindowDays))
		}
	}

	// Sort key and direction.
	p.Sort = strings.ToLower(strings.TrimSpace(q.Get("sort")))
	if p.Sort == "" {
		p.Sort = "ts"
	}
	sd, ok := searchSorts[p.Sort]
	if !ok {
		return p, fmt.Errorf("unknown sort %q (valid: %s)", p.Sort, strings.Join(sortedMapKeys(searchSorts), ", "))
	}
	switch strings.ToLower(strings.TrimSpace(q.Get("order"))) {
	case "asc":
		p.Order = "asc"
	case "desc":
		p.Order = "desc"
	case "":
		p.Order = orderWord(sd.Desc)
	default:
		return p, fmt.Errorf("bad order %q (valid: asc, desc)", q.Get("order"))
	}

	// Page size.
	p.Limit = defaultSearchLimit
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil {
			return p, fmt.Errorf("bad limit: %q", v)
		}
		switch {
		case n < 1:
			p.Limit = 1
			p.Warnings = append(p.Warnings, "limit raised to 1")
		case n > maxSearchLimit:
			p.Limit = maxSearchLimit
			p.Warnings = append(p.Warnings, fmt.Sprintf("limit capped at %d", maxSearchLimit))
		default:
			p.Limit = n
		}
	}

	// Paging position: a cursor and an offset are two answers to the same
	// question, so taking both would mean silently ignoring one.
	p.Cursor = strings.TrimSpace(q.Get("cursor"))
	if v := strings.TrimSpace(q.Get("offset")); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil {
			return p, fmt.Errorf("bad offset: %q", v)
		}
		if n < 0 {
			n = 0
		}
		if n > maxSearchOffset {
			return p, fmt.Errorf("offset above %d — page with `cursor` instead, which stays fast at any depth", maxSearchOffset)
		}
		p.Offset = n
	}
	if p.Cursor != "" {
		if p.Offset > 0 {
			return p, fmt.Errorf("cursor and offset are alternatives — pass one or the other")
		}
		if !sd.Cursor {
			return p, fmt.Errorf("cursor paging needs sort=ts; sort=%s pages with offset", p.Sort)
		}
		if _, _, e := decodeSearchCursor(p.Cursor); e != nil {
			return p, e
		}
	}

	// Count strategy.
	switch p.Count = strings.ToLower(strings.TrimSpace(q.Get("count"))); p.Count {
	case "":
		p.Count = "capped"
	case "capped", "exact", "none":
	default:
		return p, fmt.Errorf("bad count %q (valid: capped, exact, none)", q.Get("count"))
	}

	// Field projection.
	for _, k := range csvLower(q, "fields") {
		f, ok := searchFieldByKey[k]
		if !ok {
			return p, fmt.Errorf("unknown field %q", k)
		}
		p.Fields = append(p.Fields, f)
	}

	// Output format.
	switch p.Format = strings.ToLower(strings.TrimSpace(q.Get("format"))); p.Format {
	case "", "json":
		p.Format = "json"
	case "csv":
	default:
		return p, fmt.Errorf("bad format %q (valid: json, csv)", q.Get("format"))
	}

	return p, nil
}

// Describe echoes the resolved search parameters, so a response is
// self-explanatory without the client re-parsing its own query string.
func (p SearchParams) Describe() map[string]any {
	m := map[string]any{
		"sort": p.Sort, "order": p.Order,
		"limit": p.Limit, "offset": p.Offset,
		"count": p.Count, "format": p.Format,
	}
	if len(p.Fields) > 0 {
		keys := make([]string, len(p.Fields))
		for i, f := range p.Fields {
			keys[i] = f.Key
		}
		m["fields"] = keys
	}
	return m
}

// ── Cursor ─────────────────────────────────────────────────────────────────

// Keyset pagination carries the last row's (ts, id) instead of a row offset, so
// page N costs the same as page 1: the index seeks straight to the boundary
// rather than counting past everything before it. id breaks ties — spots arrive
// in batches and a whole second's worth can share a timestamp, which an
// offset-free scheme keyed on ts alone would either repeat or skip.
func encodeSearchCursor(ts, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d.%d", ts, id)))
}

func decodeSearchCursor(s string) (ts, id int64, err error) {
	raw, e := base64.RawURLEncoding.DecodeString(s)
	if e != nil {
		return 0, 0, fmt.Errorf("bad cursor: not valid base64url")
	}
	parts := strings.SplitN(string(raw), ".", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("bad cursor: malformed")
	}
	if ts, err = strconv.ParseInt(parts[0], 10, 64); err != nil {
		return 0, 0, fmt.Errorf("bad cursor: malformed timestamp")
	}
	if id, err = strconv.ParseInt(parts[1], 10, 64); err != nil {
		return 0, 0, fmt.Errorf("bad cursor: malformed id")
	}
	return ts, id, nil
}

// ── Query ──────────────────────────────────────────────────────────────────

// SearchResult is one page of matching spots plus the paging state needed to
// ask for the next one.
type SearchResult struct {
	Spots       []Spot
	Total       int64 // -1 when count=none
	TotalCapped bool  // true when the count stopped at searchCountCap
	HasMore     bool
	NextCursor  string
	NextOffset  int
	TookMS      int64
}

// spotColumns is the column list every spot-returning query selects, in the
// order scanSpot expects.
const spotColumns = `stream, ts, band, callsign, freq_hz, snr,
	country, country_code, continent, cq_zone,
	mode, comment, message, spotter, wpm, locator,
	voice_mode, est_dial_freq, confidence, bandwidth,
	avg_signal_db, peak_signal_db, distance_km, bearing_deg`

// Search runs one parametric search.
//
// It fetches Limit+1 rows: the extra row answers "is there another page"
// without a second query, and is then discarded.
func (s *SpotStore) Search(ctx context.Context, p SearchParams) (*SearchResult, error) {
	start := time.Now()
	res := &SearchResult{Total: -1, Spots: []Spot{}}

	whereSQL, filterArgs := p.Filter.where()

	// The total is over the whole filter, not the page, so it is computed from
	// the filter clause alone — before the cursor predicate narrows it.
	switch p.Count {
	case "exact":
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM spots WHERE `+whereSQL, filterArgs...,
		).Scan(&res.Total); err != nil {
			return nil, err
		}
	case "capped":
		// LIMIT inside the subquery is what bounds the work: SQLite stops
		// scanning once it has seen searchCountCap+1 matching rows, so a
		// filter matching millions costs the same as one matching the cap.
		args := append(append([]any{}, filterArgs...), searchCountCap+1)
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM (SELECT 1 FROM spots WHERE `+whereSQL+` LIMIT ?)`, args...,
		).Scan(&res.Total); err != nil {
			return nil, err
		}
		if res.Total > searchCountCap {
			res.Total, res.TotalCapped = searchCountCap, true
		}
	}

	sd := searchSorts[p.Sort]
	desc := p.Order == "desc"

	pageWhere := whereSQL
	args := append([]any{}, filterArgs...)

	if p.Cursor != "" {
		cts, cid, err := decodeSearchCursor(p.Cursor)
		if err != nil {
			return nil, err
		}
		cmp := ">"
		if desc {
			cmp = "<"
		}
		pageWhere += fmt.Sprintf(" AND (ts %s ? OR (ts = ? AND id %s ?))", cmp, cmp)
		args = append(args, cts, cts, cid)
	}

	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	// id is the tiebreaker in every ordering, which is what makes paging
	// stable: without it, rows sharing a sort value can shuffle between pages.
	orderBy := sd.Expr + " " + dir + ", id " + dir
	if !sd.Cursor {
		// Off the ts fast path the column is nullable, and SQLite sorts NULL
		// first ascending — a page of blanks ahead of every real value. Push
		// them to the end in both directions instead.
		orderBy = "(" + sd.Expr + " IS NULL) ASC, " + orderBy
	}

	query := `SELECT ` + spotColumns + `, id FROM spots WHERE ` + pageWhere +
		` ORDER BY ` + orderBy + ` LIMIT ?`
	args = append(args, p.Limit+1)
	if p.Offset > 0 {
		query += ` OFFSET ?`
		args = append(args, p.Offset)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var lastTS, lastID int64
	for rows.Next() {
		var id int64
		sp, err := scanSpot(rows, &id)
		if err != nil {
			return nil, err
		}
		if len(res.Spots) == p.Limit {
			// The probe row: it proves another page exists and is not returned.
			res.HasMore = true
			break
		}
		res.Spots = append(res.Spots, sp)
		lastTS, lastID = sp.Timestamp.Unix(), id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if res.HasMore {
		if sd.Cursor {
			res.NextCursor = encodeSearchCursor(lastTS, lastID)
		}
		res.NextOffset = p.Offset + len(res.Spots)
	}
	res.TookMS = time.Since(start).Milliseconds()
	return res, nil
}

// ── helpers ────────────────────────────────────────────────────────────────

func orderWord(desc bool) string {
	if desc {
		return "desc"
	}
	return "asc"
}

// sortedMapKeys returns a map's keys in lexical order, for stable API output
// and error messages that always list the options the same way.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// csvValue renders one projected field for CSV. Floats lose their trailing
// zeros so a frequency column reads 14074000 rather than 1.4074e+07, and whole
// numbers stay whole.
func csvValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case int:
		if t == 0 {
			return ""
		}
		return strconv.Itoa(t)
	case float64:
		if t == 0 {
			return ""
		}
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatFloat(t, 'f', 0, 64)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}
