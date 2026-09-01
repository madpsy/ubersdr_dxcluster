package main

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// searchParams parses a search query string, failing the test on a rejection.
func searchParams(t *testing.T, q url.Values) SearchParams {
	t.Helper()
	if _, ok := q["days"]; !ok {
		q.Set("days", "2")
	}
	p, err := ParseSearchParams(q)
	if err != nil {
		t.Fatalf("ParseSearchParams(%v): %v", q, err)
	}
	return p
}

// runSearch parses and runs a search against the shared test store.
func runSearch(t *testing.T, s *SpotStore, q url.Values) *SearchResult {
	t.Helper()
	res, err := s.Search(context.Background(), searchParams(t, q))
	if err != nil {
		t.Fatalf("Search(%v): %v", q, err)
	}
	return res
}

func calls(res *SearchResult) []string {
	out := make([]string, len(res.Spots))
	for i, sp := range res.Spots {
		out[i] = sp.Callsign
	}
	return out
}

// TestSearchDefaults checks that a bare search returns everything in the window
// newest-first, with a total that matches.
func TestSearchDefaults(t *testing.T) {
	s := newTestStore(t)
	res := runSearch(t, s, url.Values{})

	if len(res.Spots) != 6 {
		t.Fatalf("returned %d spots, want 6", len(res.Spots))
	}
	if res.Total != 6 || res.TotalCapped {
		t.Errorf("total = %d (capped %v), want 6 uncapped", res.Total, res.TotalCapped)
	}
	if res.HasMore {
		t.Error("has_more set on a complete result")
	}
	for i := 1; i < len(res.Spots); i++ {
		if res.Spots[i].Timestamp.After(res.Spots[i-1].Timestamp) {
			t.Fatalf("not newest-first at index %d", i)
		}
	}
}

// TestSearchFilterReuse spot-checks that the stats filter parameters carry over
// to search unchanged — the whole point of building on StatsFilter.
func TestSearchFilterReuse(t *testing.T) {
	cases := []struct {
		name  string
		query url.Values
		want  []string
	}{
		{"band", url.Values{"band": {"20m"}}, []string{"JA1XYZ", "N0CALL"}},
		{"mode derived from stream", url.Values{"mode": {"CW"}}, []string{"DL3CCC"}},
		{"country code", url.Values{"country_code": {"JP"}}, []string{"JA1XYZ"}},
		{"callsign prefix", url.Values{"callsign": {"DL"}}, []string{"DL1AAA", "DL2BBB", "DL3CCC", "DL4DDD"}},
		{"callsign exact", url.Values{"callsign_exact": {"DL1AAA"}}, []string{"DL1AAA"}},
		{"snr range", url.Values{"snr_min": {"5"}}, []string{"DL3CCC", "N0CALL"}},
		{"distance range", url.Values{"dist_min": {"5000"}}, []string{"JA1XYZ"}},
		{"frequency range kHz", url.Values{"freq_min": {"14000"}, "freq_max": {"14100"}}, []string{"JA1XYZ"}},
		{"spotter exclusion", url.Values{"mode": {"CW"}, "spotter_exclude": {"G3ABC"}}, nil},
	}
	s := newTestStore(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := calls(runSearch(t, s, tc.query))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			seen := map[string]bool{}
			for _, c := range got {
				seen[c] = true
			}
			for _, c := range tc.want {
				if !seen[c] {
					t.Errorf("got %v, missing %s", got, c)
				}
			}
		})
	}
}

// TestSearchSortOrder covers each sort direction and the NULLs-last rule that
// keeps unpopulated columns off the front page.
func TestSearchSortOrder(t *testing.T) {
	s := newTestStore(t)

	res := runSearch(t, s, url.Values{"sort": {"snr"}, "order": {"desc"}})
	if got := calls(res)[0]; got != "DL3CCC" {
		t.Errorf("strongest spot = %s, want DL3CCC (+12 dB)", got)
	}

	res = runSearch(t, s, url.Values{"sort": {"distance"}, "order": {"desc"}})
	if got := calls(res)[0]; got != "JA1XYZ" {
		t.Errorf("furthest spot = %s, want JA1XYZ", got)
	}

	// distance_km is NULL for the voice spot; ascending must not float it to
	// the top ahead of every real distance.
	res = runSearch(t, s, url.Values{"sort": {"distance"}, "order": {"asc"}})
	got := calls(res)
	if got[0] != "DL1AAA" {
		t.Errorf("nearest spot = %s, want DL1AAA (800 km)", got[0])
	}
	if got[len(got)-1] != "N0CALL" {
		t.Errorf("last spot = %s, want the NULL-distance N0CALL", got[len(got)-1])
	}

	if _, err := ParseSearchParams(url.Values{"sort": {"; DROP TABLE spots"}}); err == nil {
		t.Error("unknown sort key accepted")
	}
	if _, err := ParseSearchParams(url.Values{"order": {"sideways"}}); err == nil {
		t.Error("unknown order accepted")
	}
}

// TestSearchCursorPaging walks the whole result set one row at a time and
// checks the pages join up exactly — no repeats, no gaps — including across
// the three spots that share a timestamp.
func TestSearchCursorPaging(t *testing.T) {
	s := newTestStore(t)

	var seen []string
	q := url.Values{"days": {"2"}, "limit": {"1"}}
	for page := 0; page < 10; page++ {
		res := runSearch(t, s, q)
		seen = append(seen, calls(res)...)
		if !res.HasMore {
			break
		}
		if res.NextCursor == "" {
			t.Fatal("has_more with no cursor on sort=ts")
		}
		q = url.Values{"days": {"2"}, "limit": {"1"}, "cursor": {res.NextCursor}}
	}

	if len(seen) != 6 {
		t.Fatalf("paged through %d spots (%v), want 6", len(seen), seen)
	}
	uniq := map[string]bool{}
	for _, c := range seen {
		if uniq[c] {
			t.Errorf("callsign %s returned on two pages: %v", c, seen)
		}
		uniq[c] = true
	}
}

// TestSearchOffsetPaging covers the offset path, which is the only one
// available off the ts sort.
func TestSearchOffsetPaging(t *testing.T) {
	s := newTestStore(t)

	all := calls(runSearch(t, s, url.Values{"sort": {"callsign"}, "order": {"asc"}}))
	page := runSearch(t, s, url.Values{"sort": {"callsign"}, "order": {"asc"}, "limit": {"2"}, "offset": {"2"}})

	got := calls(page)
	if len(got) != 2 || got[0] != all[2] || got[1] != all[3] {
		t.Errorf("offset page = %v, want %v", got, all[2:4])
	}
	if !page.HasMore || page.NextOffset != 4 {
		t.Errorf("has_more=%v next_offset=%d, want true/4", page.HasMore, page.NextOffset)
	}
	if page.NextCursor != "" {
		t.Error("cursor offered for a non-ts sort")
	}
}

// TestSearchParamValidation covers the line between "clamp and warn" and
// "reject": a number outside its range is a typo worth fixing silently, a
// combination that means two different things is not.
func TestSearchParamValidation(t *testing.T) {
	p := searchParams(t, url.Values{"limit": {"99999"}})
	if p.Limit != maxSearchLimit {
		t.Errorf("limit = %d, want clamp to %d", p.Limit, maxSearchLimit)
	}
	if len(p.Warnings) == 0 {
		t.Error("clamped limit produced no warning")
	}

	p = searchParams(t, url.Values{"from": {"2000-01-01"}})
	if time.Since(p.Filter.From) > (maxWindowDays+1)*24*time.Hour {
		t.Errorf("from = %v, want clamp to the retention horizon", p.Filter.From)
	}
	if len(p.Warnings) == 0 {
		t.Error("clamped window produced no warning")
	}

	for _, tc := range []struct {
		name  string
		query url.Values
	}{
		{"garbage limit", url.Values{"limit": {"lots"}}},
		{"offset past the cursor threshold", url.Values{"offset": {"999999"}}},
		{"cursor and offset together", url.Values{"cursor": {encodeSearchCursor(1, 1)}, "offset": {"10"}}},
		{"cursor on a non-ts sort", url.Values{"cursor": {encodeSearchCursor(1, 1)}, "sort": {"snr"}}},
		{"malformed cursor", url.Values{"cursor": {"not-a-cursor!!"}}},
		{"unknown field", url.Values{"fields": {"callsign,secret_column"}}},
		{"unknown format", url.Values{"format": {"xml"}}},
		{"unknown count mode", url.Values{"count": {"maybe"}}},
		{"reversed window", url.Values{"from": {"2026-02-01"}, "to": {"2026-01-01"}}},
		{"hour out of range", url.Values{"hour_min": {"25"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseSearchParams(tc.query); err == nil {
				t.Errorf("%v was accepted, want an error", tc.query)
			}
		})
	}
}

// TestSearchCountModes checks the three counting strategies, including that
// `none` really does skip the count rather than returning a wrong one.
func TestSearchCountModes(t *testing.T) {
	s := newTestStore(t)

	if got := runSearch(t, s, url.Values{"count": {"exact"}}).Total; got != 6 {
		t.Errorf("exact total = %d, want 6", got)
	}
	if got := runSearch(t, s, url.Values{"count": {"capped"}}).Total; got != 6 {
		t.Errorf("capped total = %d, want 6", got)
	}
	if got := runSearch(t, s, url.Values{"count": {"none"}}).Total; got != -1 {
		t.Errorf("count=none total = %d, want -1 (absent)", got)
	}
}

// TestSearchTotalIgnoresPaging checks that the total describes the filter, not
// the page — otherwise a paging UI would count down as the user advanced.
func TestSearchTotalIgnoresPaging(t *testing.T) {
	s := newTestStore(t)
	res := runSearch(t, s, url.Values{"limit": {"2"}})
	if res.Total != 6 {
		t.Errorf("total = %d on a 2-row page, want 6", res.Total)
	}
	if len(res.Spots) != 2 {
		t.Errorf("returned %d spots, want 2", len(res.Spots))
	}
	if !res.HasMore {
		t.Error("has_more not set with 4 rows left")
	}
}

// TestSearchFieldProjection checks that `fields` narrows the row to exactly
// what was asked for.
func TestSearchFieldProjection(t *testing.T) {
	p := searchParams(t, url.Values{"fields": {"callsign,snr"}})
	if len(p.Fields) != 2 {
		t.Fatalf("parsed %d fields, want 2", len(p.Fields))
	}
	row := projectSpot(Spot{Callsign: "G3ABC", SNR: 7, Comment: "hidden"}, p.Fields)
	if len(row) != 2 {
		t.Fatalf("projected %d keys, want 2: %v", len(row), row)
	}
	if row["callsign"] != "G3ABC" || row["snr"] != 7.0 {
		t.Errorf("projection = %v", row)
	}
}

// TestSearchTextEscaping checks that LIKE metacharacters in user input stay
// literal — a `q=%` must not match every spot.
func TestSearchTextEscaping(t *testing.T) {
	s := newTestStore(t)
	if got := len(runSearch(t, s, url.Values{"q": {"%"}}).Spots); got != 0 {
		t.Errorf("q=%% matched %d spots, want 0 — wildcard leaked into LIKE", got)
	}
	if got := len(runSearch(t, s, url.Values{"callsign": {"DL_"}}).Spots); got != 0 {
		t.Errorf("callsign=DL_ matched %d spots, want 0", got)
	}
}

// TestSearchCursorRoundTrip covers the token encoding directly, since a
// silently-corrupted cursor would show up as missing rows rather than an error.
func TestSearchCursorRoundTrip(t *testing.T) {
	ts, id, err := decodeSearchCursor(encodeSearchCursor(1767225600, 4242))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if ts != 1767225600 || id != 4242 {
		t.Errorf("round trip = (%d, %d), want (1767225600, 4242)", ts, id)
	}
	for _, bad := range []string{"", "!!!", "YWJj", encodeSearchCursor(1, 1) + "x"} {
		if _, _, err := decodeSearchCursor(bad); err == nil {
			t.Errorf("decodeSearchCursor(%q) accepted", bad)
		}
	}
}

// TestSearchMetaCoversTheParser guards the self-describing document against
// drift: every sort key and field it advertises has to actually work.
func TestSearchMetaCoversTheParser(t *testing.T) {
	s := newTestStore(t)
	for _, d := range SearchSortsList() {
		key := d["key"].(string)
		if _, err := ParseSearchParams(url.Values{"sort": {key}}); err != nil {
			t.Errorf("advertised sort %q rejected: %v", key, err)
		}
		if _, err := s.Search(context.Background(), searchParams(t, url.Values{"sort": {key}})); err != nil {
			t.Errorf("advertised sort %q failed to run: %v", key, err)
		}
	}
	for _, d := range SearchFieldsList() {
		key := d["key"].(string)
		if _, err := ParseSearchParams(url.Values{"fields": {key}}); err != nil {
			t.Errorf("advertised field %q rejected: %v", key, err)
		}
	}
	// Every documented filter must be one the parser actually reads. An
	// undocumented parameter is merely undiscoverable; a documented one that
	// does nothing is a lie, and the two lists sit in different files.
	parser, err := os.ReadFile("stats.go")
	if err != nil {
		t.Fatalf("read stats.go: %v", err)
	}
	for _, d := range searchFilterDocs {
		if !strings.Contains(string(parser), `"`+d.Key+`"`) {
			t.Errorf("documented filter %q is not read by ParseStatsFilter", d.Key)
		}
	}
}

// TestSearchCSVValues covers the CSV rendering of the numeric columns, where a
// float printed in scientific notation would quietly corrupt a spreadsheet.
func TestSearchCSVValues(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{14074000.0, "14074000"},
		{-12.5, "-12.5"},
		{0.0, ""},
		{0, ""},
		{25, "25"},
		{"G3ABC", "G3ABC"},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := csvValue(tc.in); got != tc.want {
			t.Errorf("csvValue(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
