package main

import (
	"fmt"
	"strconv"
	"strings"
)

// ── Band / frequency decoding ──────────────────────────────────────────────

// freqRange is a frequency range in kHz (inclusive).
type freqRange struct {
	Min float64
	Max float64
}

// bandFreqRanges maps band and region names to frequency ranges in kHz.
// This is HF-only (plus 6m) to match our upstream. Sub-band segments
// (cw/ssb/data) are approximate IARU Region 1 conventions.
//
// Names accepted: "hf", region "hf", individual bands ("20m"), and
// band/segment combinations handled separately via bandSegments.
var bandFreqRanges = map[string]freqRange{
	"2200m": {135.7, 137.8},
	"630m":  {472, 479},
	"600m":  {500, 510},
	"160m":  {1810, 2000},
	"80m":   {3500, 4000},
	"60m":   {5250, 5450},
	"40m":   {7000, 7300},
	"30m":   {10100, 10150},
	"20m":   {14000, 14350},
	"17m":   {18068, 18168},
	"15m":   {21000, 21450},
	"12m":   {24890, 24990},
	"11m":   {26965, 27405},
	"10m":   {28000, 29700},
	"6m":    {50000, 54000},
	// Regions
	"hf":  {1800, 30000},
	"vhf": {30000, 300000},
	"uhf": {300000, 3000000},
}

// segmentNames maps sub-band segment keywords to a canonical segment type.
// Spider uses band-specific sub-band tables; we approximate with the low
// portion for CW, mid for data, upper for SSB. Good enough for HF filtering.
var segmentNames = map[string]string{
	"cw":    "cw",
	"data":  "data",
	"rtty":  "data",
	"ssb":   "ssb",
	"phone": "ssb",
}

// hfCWSegments gives per-band CW segment ranges in kHz (approx IARU R1 CW ends).
var hfCWSegments = map[string]freqRange{
	"160m": {1810, 1838},
	"80m":  {3500, 3570},
	"60m":  {5250, 5366},
	"40m":  {7000, 7040},
	"30m":  {10100, 10130},
	"20m":  {14000, 14070},
	"17m":  {18068, 18095},
	"15m":  {21000, 21070},
	"12m":  {24890, 24915},
	"10m":  {28000, 28070},
	"6m":   {50000, 50100},
}

// hfSSBSegments gives per-band phone segment ranges in kHz (approx).
var hfSSBSegments = map[string]freqRange{
	"160m": {1840, 2000},
	"80m":  {3600, 4000},
	"40m":  {7040, 7300},
	"20m":  {14100, 14350},
	"17m":  {18110, 18168},
	"15m":  {21150, 21450},
	"12m":  {24930, 24990},
	"10m":  {28300, 29700},
	"6m":   {50100, 54000},
}

// decodeFreqTerm converts a freq/band term into one or more kHz ranges.
// Accepts:
//   - "N/M" or "N-M"  → explicit kHz range
//   - "20m"           → band range
//   - "hf"            → region range
//   - "hf/cw"         → all HF CW segments
//   - "20m/cw"        → that band's CW segment
//   - "20m/ssb"       → that band's SSB segment
//
// Returns nil if the term cannot be decoded.
func decodeFreqTerm(term string) []freqRange {
	term = strings.ToLower(strings.TrimSpace(term))
	if term == "" {
		return nil
	}

	// Explicit numeric range N/M or N-M
	if r, ok := parseKHzRange(term); ok {
		return []freqRange{r}
	}

	// band/segment form
	if idx := strings.Index(term, "/"); idx > 0 {
		band := term[:idx]
		seg := term[idx+1:]
		if _, isSeg := segmentNames[seg]; isSeg {
			return segmentRanges(band, seg)
		}
		// Not a known segment — try as explicit range fallback already handled.
	}

	// Plain band or region
	if fr, ok := bandFreqRanges[term]; ok {
		return []freqRange{fr}
	}

	return nil
}

// parseKHzRange parses "N/M" or "N-M" into a freqRange.
func parseKHzRange(s string) (freqRange, bool) {
	for _, sep := range []string{"/", "-"} {
		idx := strings.Index(s, sep)
		if idx <= 0 {
			continue
		}
		a, b := s[:idx], s[idx+1:]
		fa, e1 := strconv.ParseFloat(a, 64)
		fb, e2 := strconv.ParseFloat(b, 64)
		if e1 == nil && e2 == nil && fb >= fa {
			return freqRange{fa, fb}, true
		}
	}
	return freqRange{}, false
}

// segmentRanges returns the CW/SSB/data segment range(s) for a band or region.
func segmentRanges(band, seg string) []freqRange {
	norm := segmentNames[seg]
	var table map[string]freqRange
	switch norm {
	case "cw":
		table = hfCWSegments
	case "ssb":
		table = hfSSBSegments
	default:
		// "data" — approximate as just above CW; fall back to whole band.
		if fr, ok := bandFreqRanges[band]; ok {
			return []freqRange{fr}
		}
		return nil
	}

	if band == "hf" {
		// All HF segments
		var out []freqRange
		for _, fr := range table {
			out = append(out, fr)
		}
		return out
	}
	if fr, ok := table[band]; ok {
		return []freqRange{fr}
	}
	// Unknown band for this segment — fall back to whole band if known.
	if fr, ok := bandFreqRanges[band]; ok {
		return []freqRange{fr}
	}
	return nil
}

// ── Filter expression AST ──────────────────────────────────────────────────

// condType identifies a leaf condition's field.
type condType int

const (
	condFreq    condType = iota // frequency ranges (kHz) — matches FreqHz
	condCall                    // callsign prefix(es)
	condSpotter                 // spotter prefix(es)
	condInfo                    // substring in comment/message
	condCont                    // continent code(s)
	condCountry                 // country code(s)
	condMode                    // mode(s)
	condType_                   // stream type(s)
)

// exprNode is a node in the filter expression tree.
// It is either a leaf condition or a boolean operator node.
type exprNode struct {
	// Boolean operator: "and", "or", "not". Empty for leaf nodes.
	op string
	// Children for operator nodes. "not" has exactly one child.
	kids []*exprNode

	// Leaf condition (only when op == "").
	cond condType
	// Value slices for the condition (OR-matched within a field).
	freqs   []freqRange
	strVals []string
	types   []StreamType
}

// eval returns true if the spot matches this expression node.
func (n *exprNode) eval(s Spot) bool {
	switch n.op {
	case "all":
		return true
	case "and":
		for _, k := range n.kids {
			if !k.eval(s) {
				return false
			}
		}
		return true
	case "or":
		for _, k := range n.kids {
			if k.eval(s) {
				return true
			}
		}
		return false
	case "not":
		return !n.kids[0].eval(s)
	}
	// Leaf condition
	return n.evalCond(s)
}

func (n *exprNode) evalCond(s Spot) bool {
	switch n.cond {
	case condFreq:
		khz := s.FreqHz / 1000.0
		for _, fr := range n.freqs {
			if khz >= fr.Min && khz <= fr.Max {
				return true
			}
		}
		return false
	case condCall:
		up := strings.ToUpper(s.Callsign)
		for _, pfx := range n.strVals {
			if strings.HasPrefix(up, strings.ToUpper(pfx)) {
				return true
			}
		}
		return false
	case condSpotter:
		up := strings.ToUpper(s.Spotter)
		for _, pfx := range n.strVals {
			if strings.HasPrefix(up, strings.ToUpper(pfx)) {
				return true
			}
		}
		return false
	case condInfo:
		hay := strings.ToLower(s.Comment) + " " + strings.ToLower(s.Message)
		for _, needle := range n.strVals {
			if strings.Contains(hay, strings.ToLower(needle)) {
				return true
			}
		}
		return false
	case condCont:
		return containsCI(n.strVals, s.Continent)
	case condCountry:
		return containsCI(n.strVals, s.CountryCode)
	case condMode:
		spotMode := s.Mode
		if s.Stream == StreamCWSkimmer {
			spotMode = "CW"
		} else if s.Stream == StreamVoiceActivity {
			spotMode = s.VoiceMode
		}
		return containsCI(n.strVals, spotMode)
	case condType_:
		for _, t := range n.types {
			if s.Stream == t {
				return true
			}
		}
		return false
	}
	return false
}

// describe returns a human-readable form of the expression (for show/filter).
func (n *exprNode) describe() string {
	switch n.op {
	case "and":
		parts := make([]string, len(n.kids))
		for i, k := range n.kids {
			parts[i] = k.describe()
		}
		return "(" + strings.Join(parts, " and ") + ")"
	case "or":
		parts := make([]string, len(n.kids))
		for i, k := range n.kids {
			parts[i] = k.describe()
		}
		return "(" + strings.Join(parts, " or ") + ")"
	case "not":
		return "not " + n.kids[0].describe()
	}
	switch n.cond {
	case condFreq:
		fs := make([]string, len(n.freqs))
		for i, fr := range n.freqs {
			fs[i] = fmt.Sprintf("%g-%g", fr.Min, fr.Max)
		}
		return "freq " + strings.Join(fs, ",")
	case condCall:
		return "call " + strings.Join(n.strVals, ",")
	case condSpotter:
		return "by " + strings.Join(n.strVals, ",")
	case condInfo:
		return "info " + strings.Join(n.strVals, ",")
	case condCont:
		return "cont " + strings.Join(n.strVals, ",")
	case condCountry:
		return "country " + strings.Join(n.strVals, ",")
	case condMode:
		return "mode " + strings.Join(n.strVals, ",")
	case condType_:
		ts := make([]string, len(n.types))
		for i, t := range n.types {
			ts[i] = string(t)
		}
		return "type " + strings.Join(ts, ",")
	}
	return "?"
}

// ── Filter expression parser ───────────────────────────────────────────────

// fieldKeywords maps filter field names to their condition type.
var fieldKeywords = map[string]condType{
	"freq":      condFreq,
	"on":        condFreq,
	"call":      condCall,
	"prefix":    condCall,
	"spotter":   condSpotter,
	"by":        condSpotter,
	"info":      condInfo,
	"cont":      condCont,
	"continent": condCont,
	"country":   condCountry,
	"dxcc":      condCountry,
	"mode":      condMode,
	"type":      condType_,
}

// filterParser is a recursive-descent parser for filter expressions.
type filterParser struct {
	toks []string
	pos  int
}

// tokenizeFilter splits a filter expression into tokens, spacing out
// parentheses so they parse as separate tokens.
func tokenizeFilter(line string) []string {
	line = strings.ReplaceAll(line, "(", " ( ")
	line = strings.ReplaceAll(line, ")", " ) ")
	return strings.Fields(line)
}

// parseFilterExpr parses a full filter expression string into an exprNode tree.
// Returns nil, error on failure. An empty expression or "all" returns a
// node that matches everything (nil tree with a sentinel).
func parseFilterExpr(line string) (*exprNode, error) {
	toks := tokenizeFilter(line)
	if len(toks) == 0 {
		return &exprNode{op: "all"}, nil
	}
	if len(toks) == 1 && strings.ToLower(toks[0]) == "all" {
		return &exprNode{op: "all"}, nil
	}
	p := &filterParser{toks: toks}
	node, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("unexpected token %q", p.toks[p.pos])
	}
	return node, nil
}

func (p *filterParser) peek() string {
	if p.pos < len(p.toks) {
		return strings.ToLower(p.toks[p.pos])
	}
	return ""
}

func (p *filterParser) next() string {
	t := p.toks[p.pos]
	p.pos++
	return t
}

// parseOr handles the lowest precedence: OR (also implicit AND between terms).
func (p *filterParser) parseOr() (*exprNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek() == "or" {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &exprNode{op: "or", kids: []*exprNode{left, right}}
	}
	return left, nil
}

// parseAnd handles AND (explicit "and" or implicit adjacency).
func (p *filterParser) parseAnd() (*exprNode, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for {
		tok := p.peek()
		if tok == "and" {
			p.next()
			right, err := p.parseNot()
			if err != nil {
				return nil, err
			}
			left = &exprNode{op: "and", kids: []*exprNode{left, right}}
			continue
		}
		// Implicit AND: another field keyword or "(" or "not" follows
		if tok == "(" || tok == "not" || isFieldKeyword(tok) {
			right, err := p.parseNot()
			if err != nil {
				return nil, err
			}
			left = &exprNode{op: "and", kids: []*exprNode{left, right}}
			continue
		}
		break
	}
	return left, nil
}

func isFieldKeyword(tok string) bool {
	_, ok := fieldKeywords[tok]
	return ok
}

// parseNot handles NOT and primary terms.
func (p *filterParser) parseNot() (*exprNode, error) {
	if p.peek() == "not" {
		p.next()
		kid, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &exprNode{op: "not", kids: []*exprNode{kid}}, nil
	}
	return p.parsePrimary()
}

// parsePrimary handles parentheses and leaf conditions.
func (p *filterParser) parsePrimary() (*exprNode, error) {
	tok := p.peek()
	if tok == "(" {
		p.next()
		node, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peek() != ")" {
			return nil, fmt.Errorf("missing closing parenthesis")
		}
		p.next()
		return node, nil
	}

	// Leaf: <field> <value>
	ct, ok := fieldKeywords[tok]
	if !ok {
		// bare word → treat as callsign prefix (Spider compat)
		if tok != "" && tok != ")" {
			val := p.next()
			return &exprNode{cond: condCall, strVals: []string{strings.ToUpper(val)}}, nil
		}
		return nil, fmt.Errorf("expected filter field, got %q", tok)
	}
	p.next() // consume field keyword
	if p.pos >= len(p.toks) {
		return nil, fmt.Errorf("field %q needs a value", tok)
	}
	val := p.next()
	return buildLeaf(ct, val)
}

// buildLeaf constructs a leaf condition node from a field type and value string.
func buildLeaf(ct condType, val string) (*exprNode, error) {
	n := &exprNode{cond: ct}
	vals := splitComma(val)
	switch ct {
	case condFreq:
		for _, v := range vals {
			frs := decodeFreqTerm(v)
			if frs == nil {
				return nil, fmt.Errorf("cannot decode frequency/band %q", v)
			}
			n.freqs = append(n.freqs, frs...)
		}
	case condCall, condSpotter, condInfo:
		n.strVals = vals
	case condCont, condCountry, condMode:
		n.strVals = upperAll(vals)
	case condType_:
		n.types = parseTypes(vals)
	}
	return n, nil
}

// ── Filter slot (per-slot reject + accept) ─────────────────────────────────

// FilterSlot holds an optional reject rule and an optional accept rule,
// matching DX Spider's per-slot model. Each rule is an expression tree.
type FilterSlot struct {
	Reject     *exprNode
	RejectText string // original user text for show/filter
	Accept     *exprNode
	AcceptText string
}

// ── ClientFilter ───────────────────────────────────────────────────────────

// ClientFilter holds the numbered filter slots (0–9) plus the simple
// set/filter AND filters (applied as a fast pre-filter).
//
// Evaluation matches Spider's Filter::it():
//   - Simple AND filters are applied first (our own set/filter commands).
//   - Then slots 0–9 are iterated in order. Within each slot:
//   - if a reject rule exists and MATCHES → drop (stop).
//   - else if a reject rule exists and does not match → tentative PASS.
//   - if an accept rule exists and MATCHES → keep (stop).
//   - else if an accept rule exists and does not match → tentative DROP.
//   - The result is that of the last rule evaluated; if no slot rule matched,
//     the default depends on the last rule type (reject→pass, accept→drop).
type ClientFilter struct {
	// Simple AND filters (set/filter band, mode, etc.) — applied first.
	Bands     []string
	Modes     []string
	Types     []StreamType
	Conts     []string
	Countries []string
	CallPfx   []string
	MinSNR    *float64
	MaxSNR    *float64

	// Spider-style numbered slots (0–9). nil = unset.
	Slots [10]*FilterSlot
}

// hasSlots returns true if any slot has a rule.
func (f *ClientFilter) hasSlots() bool {
	for _, sl := range f.Slots {
		if sl != nil && (sl.Reject != nil || sl.Accept != nil) {
			return true
		}
	}
	return false
}

// matchSimple applies the simple AND filters. Returns false if the spot is
// rejected by any simple filter.
func (f *ClientFilter) matchSimple(s Spot) bool {
	if len(f.Bands) > 0 && !containsCI(f.Bands, s.Band) {
		return false
	}
	if len(f.Modes) > 0 {
		spotMode := s.Mode
		if s.Stream == StreamCWSkimmer {
			spotMode = "CW"
		} else if s.Stream == StreamVoiceActivity {
			spotMode = s.VoiceMode
		}
		if !containsCI(f.Modes, spotMode) {
			return false
		}
	}
	if len(f.Types) > 0 {
		found := false
		for _, t := range f.Types {
			if s.Stream == t {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(f.Conts) > 0 && !containsCI(f.Conts, s.Continent) {
		return false
	}
	if len(f.Countries) > 0 && !containsCI(f.Countries, s.CountryCode) {
		return false
	}
	if len(f.CallPfx) > 0 {
		matched := false
		for _, pfx := range f.CallPfx {
			if strings.HasPrefix(strings.ToUpper(s.Callsign), strings.ToUpper(pfx)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if f.MinSNR != nil && s.SNR < *f.MinSNR {
		return false
	}
	if f.MaxSNR != nil && s.SNR > *f.MaxSNR {
		return false
	}
	return true
}

// Match returns true if the spot passes all active filters.
func (f *ClientFilter) Match(s Spot) bool {
	// 1. Simple AND filters first
	if !f.matchSimple(s) {
		return false
	}

	// 2. No slot rules → pass
	if !f.hasSlots() {
		return true
	}

	// 3. Spider slot evaluation (Filter::it semantics)
	r := true // default result if nothing decisive matches
	for _, sl := range f.Slots {
		if sl == nil {
			continue
		}
		if sl.Reject != nil {
			if sl.Reject.eval(s) {
				return false // reject matched → drop, stop
			}
			r = true // reject didn't match → tentative pass
		}
		if sl.Accept != nil {
			if sl.Accept.eval(s) {
				return true // accept matched → keep, stop
			}
			r = false // accept didn't match → tentative drop
		}
	}
	return r
}

// Summary returns a human-readable description of all active filters.
func (f *ClientFilter) Summary() string {
	var lines []string

	if len(f.Bands) > 0 {
		lines = append(lines, fmt.Sprintf("  band    : %s", strings.Join(f.Bands, ", ")))
	}
	if len(f.Modes) > 0 {
		lines = append(lines, fmt.Sprintf("  mode    : %s", strings.Join(f.Modes, ", ")))
	}
	if len(f.Types) > 0 {
		ts := make([]string, len(f.Types))
		for i, t := range f.Types {
			ts[i] = string(t)
		}
		lines = append(lines, fmt.Sprintf("  type    : %s", strings.Join(ts, ", ")))
	}
	if len(f.Conts) > 0 {
		lines = append(lines, fmt.Sprintf("  cont    : %s", strings.Join(f.Conts, ", ")))
	}
	if len(f.Countries) > 0 {
		lines = append(lines, fmt.Sprintf("  country : %s", strings.Join(f.Countries, ", ")))
	}
	if len(f.CallPfx) > 0 {
		lines = append(lines, fmt.Sprintf("  call    : %s", strings.Join(f.CallPfx, ", ")))
	}
	if f.MinSNR != nil {
		lines = append(lines, fmt.Sprintf("  snr     : >= %.1f dB", *f.MinSNR))
	}
	if f.MaxSNR != nil {
		lines = append(lines, fmt.Sprintf("  maxsnr  : <= %.1f dB", *f.MaxSNR))
	}

	// Slots
	for i, sl := range f.Slots {
		if sl == nil {
			continue
		}
		if sl.Reject != nil {
			lines = append(lines, fmt.Sprintf("  reject/%d : %s", i, sl.RejectText))
		}
		if sl.Accept != nil {
			lines = append(lines, fmt.Sprintf("  accept/%d : %s", i, sl.AcceptText))
		}
	}

	if len(lines) == 0 {
		return "No active filters - receiving all spots."
	}
	return "Active filters:\r\n" + strings.Join(lines, "\r\n")
}

// ── ClientState ────────────────────────────────────────────────────────────

// ClientState holds per-connection state including filters and stream toggles.
type ClientState struct {
	Filter        ClientFilter
	WantAll       bool // receive any spots at all (set/dx / unset/dx)
	WantDigital   bool // receive digital decoder spots
	WantRBN       bool // receive CW/RBN spots
	WantVoice     bool // receive voice activity spots
	WantDXCluster bool // receive DX cluster spots
	Name          string
}

func newClientState() *ClientState {
	return &ClientState{
		WantAll:       true,
		WantDigital:   false, // disabled by default — user must enable with set/digital
		WantRBN:       true,
		WantVoice:     true,
		WantDXCluster: false, // disabled by default — user must enable with set/dxcluster
	}
}

// ShouldSend returns true if the spot should be sent to this client.
func (s *ClientState) ShouldSend(spot Spot) bool {
	if !s.WantAll {
		return false
	}
	switch spot.Stream {
	case StreamDecoder:
		if !s.WantDigital {
			return false
		}
	case StreamCWSkimmer:
		if !s.WantRBN {
			return false
		}
	case StreamVoiceActivity:
		if !s.WantVoice {
			return false
		}
	case StreamDXCluster:
		if !s.WantDXCluster {
			return false
		}
	}
	return s.Filter.Match(spot)
}

// ── Shared helpers ─────────────────────────────────────────────────────────

func containsCI(slice []string, val string) bool {
	v := strings.ToUpper(val)
	for _, s := range slice {
		if strings.ToUpper(s) == v {
			return true
		}
	}
	return false
}

func splitComma(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func upperAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToUpper(s)
	}
	return out
}

func parseTypes(vals []string) []StreamType {
	var out []StreamType
	for _, v := range vals {
		switch strings.ToLower(v) {
		case "digital", "decoder":
			out = append(out, StreamDecoder)
		case "cw", "cwskimmer", "rbn":
			out = append(out, StreamCWSkimmer)
		case "voice", "voiceactivity":
			out = append(out, StreamVoiceActivity)
		case "dx", "dxcluster", "cluster":
			out = append(out, StreamDXCluster)
		}
	}
	return out
}
