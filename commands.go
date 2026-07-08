package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ── Abbreviation expansion ─────────────────────────────────────────────────

// expandAbbrev expands common DX Spider command abbreviations to their full form.
func expandAbbrev(cmd string) string {
	lc := strings.ToLower(cmd)
	abbrevs := map[string]string{
		"sh/dx":      "show/dx",
		"sh/f":       "show/filter",
		"sh/filter":  "show/filter",
		"sh/time":    "show/time",
		"sh/ver":     "show/version",
		"sh/version": "show/version",
		"sh/qrz":     "show/qrz",
		"sh/pre":     "show/prefix",
		"sh/prefix":  "show/prefix",
		"sh/dxcc":    "show/dxcc",
		"sh/hea":     "show/heading",
		"sh/heading": "show/heading",
		"sh/dxst":    "show/dxstats",
		"sh/dxstats": "show/dxstats",
		"sh/hfstat":  "show/hfstats",
		"sh/hfstats": "show/hfstats",
		"sh/stat":    "show/status",
		"sh/status":  "show/status",
		"sh/who":     "show/who",
		"set/f":      "set/filter",
		"clr/f":      "clear/filter",
		"clr/filter": "clear/filter",
		"acc/spots":  "accept/spots",
		"acc/rbn":    "accept/rbn",
		"rej/spots":  "reject/spots",
		"rej/rbn":    "reject/rbn",
		"clr/spots":  "clear/spots",
		"clr/rbn":    "clear/rbn",
	}
	if full, ok := abbrevs[lc]; ok {
		return full
	}
	return lc
}

// ── Command handler ────────────────────────────────────────────────────────

// handleCommand parses and executes a single command line, returning the response string.
func (t *TelnetServer) handleCommand(line string, state *ClientState) string {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return ""
	}

	cmd := expandAbbrev(parts[0])

	switch cmd {
	// ── Help ──────────────────────────────────────────────────────────────
	case "help", "?":
		return strings.ReplaceAll(helpText, "\n", "\r\n")

	// ── Show commands ──────────────────────────────────────────────────────
	case "show/filter":
		return state.Filter.Summary()

	case "show/time":
		return fmt.Sprintf("UTC: %s", time.Now().UTC().Format("2006-01-02 15:04:05"))

	case "show/version":
		return fmt.Sprintf("UberSDR DX Cluster - %s", t.version)

	case "show/dx":
		return t.handleShowDX(parts[1:], state)

	case "show/qrz":
		if len(parts) < 2 {
			return "SHOW/QRZ <callsign>, e.g. SH/QRZ g1tlh"
		}
		call := strings.ToUpper(parts[1])
		result, err := lookupQRZ(t.ubersdrURL, call)
		if err != nil {
			return fmt.Sprintf("qrz> Error: %v", err)
		}
		if result == nil {
			return fmt.Sprintf("qrz> %s not found in the QRZ database", call)
		}
		return formatQRZ(result)

	case "show/prefix":
		return t.handleShowPrefix(parts[1:])

	case "show/dxcc":
		return t.handleShowDXCC(parts[1:], state)

	case "show/heading":
		return t.handleShowHeading(parts[1:])

	case "show/dxstats":
		return t.handleShowDXStats(parts[1:])

	case "show/hfstats":
		return t.handleShowHFStats(parts[1:])

	case "show/status":
		return t.handleShowStatus()

	case "show/who":
		return t.handleShowWho()

	// ── Stream toggles ─────────────────────────────────────────────────────
	case "set/dx":
		state.WantAll = true
		return "All spots enabled."
	case "unset/dx":
		state.WantAll = false
		return "All spots disabled."

	case "set/digital":
		state.WantDigital = true
		return "Digital decoder spots enabled."
	case "unset/digital":
		state.WantDigital = false
		return "Digital decoder spots disabled."

	case "set/rbn", "set/skimmer":
		state.WantRBN = true
		return "CW/RBN spots enabled."
	case "unset/rbn", "unset/skimmer":
		state.WantRBN = false
		return "CW/RBN spots disabled."

	case "set/voice":
		state.WantVoice = true
		return "Voice activity spots enabled."
	case "unset/voice":
		state.WantVoice = false
		return "Voice activity spots disabled."

	case "set/dxcluster", "set/cluster":
		state.WantDXCluster = true
		return "DX cluster spots enabled."
	case "unset/dxcluster", "unset/cluster":
		state.WantDXCluster = false
		return "DX cluster spots disabled."

	// ── accept/spots and accept/rbn ────────────────────────────────────────
	// Syntax: accept/spots [N] <expr>
	// where N is an optional slot number 0-9 (default 1)
	case "accept/spots", "accept/rbn":
		slotNum, expr := parseSlotArgs(parts[1:])
		if len(expr) == 0 {
			return "Usage: accept/spots [0-9] <expression>  (see HELP)"
		}
		exprText := strings.Join(expr, " ")
		node, err := parseFilterExpr(exprText)
		if err != nil {
			return fmt.Sprintf("Filter error: %v", err)
		}
		if state.Filter.Slots[slotNum] == nil {
			state.Filter.Slots[slotNum] = &FilterSlot{}
		}
		state.Filter.Slots[slotNum].Accept = node
		state.Filter.Slots[slotNum].AcceptText = exprText
		return fmt.Sprintf("accept/spots slot %d set: %s", slotNum, exprText)

	// ── reject/spots and reject/rbn ────────────────────────────────────────
	case "reject/spots", "reject/rbn":
		slotNum, expr := parseSlotArgs(parts[1:])
		if len(expr) == 0 {
			return "Usage: reject/spots [0-9] <expression>  (see HELP)"
		}
		exprText := strings.Join(expr, " ")
		node, err := parseFilterExpr(exprText)
		if err != nil {
			return fmt.Sprintf("Filter error: %v", err)
		}
		if state.Filter.Slots[slotNum] == nil {
			state.Filter.Slots[slotNum] = &FilterSlot{}
		}
		state.Filter.Slots[slotNum].Reject = node
		state.Filter.Slots[slotNum].RejectText = exprText
		return fmt.Sprintf("reject/spots slot %d set: %s", slotNum, exprText)

	// ── clear/spots and clear/rbn ──────────────────────────────────────────
	// Syntax: clear/spots [N|all]
	case "clear/spots", "clear/rbn":
		if len(parts) < 2 || strings.ToLower(parts[1]) == "all" {
			for i := range state.Filter.Slots {
				state.Filter.Slots[i] = nil
			}
			return "All spot filter slots cleared."
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil || n < 0 || n > 9 {
			return fmt.Sprintf("Invalid slot number %q - use 0-9 or 'all'", parts[1])
		}
		state.Filter.Slots[n] = nil
		return fmt.Sprintf("Filter slot %d cleared.", n)

	// ── set/filter ─────────────────────────────────────────────────────────
	case "set/filter":
		if len(parts) < 3 {
			return "Usage: set/filter <field> <value[,value...]>"
		}
		field := strings.ToLower(parts[1])
		val := parts[2]
		vals := splitComma(val)
		switch field {
		case "band":
			state.Filter.Bands = vals
			return fmt.Sprintf("Filter set: band = %s", strings.Join(vals, ", "))
		case "mode":
			state.Filter.Modes = upperAll(vals)
			return fmt.Sprintf("Filter set: mode = %s", strings.Join(state.Filter.Modes, ", "))
		case "type":
			state.Filter.Types = parseTypes(vals)
			ts := make([]string, len(state.Filter.Types))
			for i, t := range state.Filter.Types {
				ts[i] = string(t)
			}
			return fmt.Sprintf("Filter set: type = %s", strings.Join(ts, ", "))
		case "cont":
			state.Filter.Conts = upperAll(vals)
			return fmt.Sprintf("Filter set: cont = %s", strings.Join(state.Filter.Conts, ", "))
		case "country":
			state.Filter.Countries = upperAll(vals)
			return fmt.Sprintf("Filter set: country = %s", strings.Join(state.Filter.Countries, ", "))
		case "call":
			state.Filter.CallPfx = upperAll(vals)
			return fmt.Sprintf("Filter set: call = %s", strings.Join(state.Filter.CallPfx, ", "))
		case "snr":
			v, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return "Invalid SNR value - use a number, e.g. set/filter snr 10"
			}
			state.Filter.MinSNR = &v
			return fmt.Sprintf("Filter set: snr >= %.1f dB", v)
		case "maxsnr":
			v, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return "Invalid SNR value - use a number, e.g. set/filter maxsnr 30"
			}
			state.Filter.MaxSNR = &v
			return fmt.Sprintf("Filter set: maxsnr <= %.1f dB", v)
		default:
			return fmt.Sprintf("Unknown filter field %q - type HELP for usage", field)
		}

	// ── clear/filter ───────────────────────────────────────────────────────
	case "clear/filter":
		if len(parts) == 1 {
			state.Filter = ClientFilter{}
			return "All filters cleared."
		}
		field := strings.ToLower(parts[1])
		switch field {
		case "band":
			state.Filter.Bands = nil
			return "Filter cleared: band"
		case "mode":
			state.Filter.Modes = nil
			return "Filter cleared: mode"
		case "type":
			state.Filter.Types = nil
			return "Filter cleared: type"
		case "cont":
			state.Filter.Conts = nil
			return "Filter cleared: cont"
		case "country":
			state.Filter.Countries = nil
			return "Filter cleared: country"
		case "call":
			state.Filter.CallPfx = nil
			return "Filter cleared: call"
		case "snr":
			state.Filter.MinSNR = nil
			return "Filter cleared: snr"
		case "maxsnr":
			state.Filter.MaxSNR = nil
			return "Filter cleared: maxsnr"
		default:
			return fmt.Sprintf("Unknown filter field %q - type HELP for usage", field)
		}

	default:
		return fmt.Sprintf("Unknown command %q - type HELP for a list of commands", parts[0])
	}
}

// parseSlotArgs extracts an optional leading slot number (0-9) from args,
// returning (slotNum, remainingArgs). Default slot is 1.
func parseSlotArgs(args []string) (int, []string) {
	if len(args) == 0 {
		return 1, nil
	}
	n, err := strconv.Atoi(args[0])
	if err == nil && n >= 0 && n <= 9 {
		return n, args[1:]
	}
	return 1, args
}

// ── show/dx query handler ──────────────────────────────────────────────────

// spiderKeywords is the set of reserved words in show/dx syntax.
// A bare token that is NOT in this set is treated as a callsign prefix.
var spiderKeywords = map[string]bool{
	"on": true, "freq": true, "call": true, "info": true, "spotter": true,
	"by": true, "ip": true, "or": true, "and": true, "not": true,
	"dxcc": true, "call_dxcc": true, "by_dxcc": true, "bydxcc": true,
	"origin": true, "call_itu": true, "itu": true, "call_zone": true,
	"zone": true, "cq": true, "bycq": true, "byitu": true, "by_itu": true,
	"by_zone": true, "byzone": true, "call_state": true, "state": true,
	"bystate": true, "by_state": true, "day": true, "days": true,
	"exact": true, "rt": true, "real": true, "filt": true,
	"qsl": true, "iota": true, "qra": true,
	"cont": true, "continent": true, "country": true, "countryname": true,
	"mode": true, "type": true, "prefix": true,
}

// handleShowDX parses the arguments after "show/dx" and queries the store.
//
// Parsing follows DX Spider conventions:
//   - Bare integer            → limit (number of spots)
//   - N-M (both < 1000)       → offset range (spots N to M, i.e. OFFSET N LIMIT M-N)
//   - N-M (either >= 1000)    → frequency range in kHz
//   - day N                   → look back N days
//   - day N-M                 → look back between N and M days ago
//   - on/freq <band|kHz>      → band or frequency filter
//   - call <pfx>              → callsign prefix filter
//   - prefix <pfx>            → alias for call
//   - by/spotter <call>       → spotter prefix filter
//   - info <text>             → substring search in comment/message
//   - iota [<ref>]            → search for IOTA references in comment/message
//   - qsl                     → search for QSL/VIA in comment/message
//   - cont <code>             → continent filter
//   - country <code>          → country code filter
//   - mode <mode>             → mode filter
//   - type <type>             → stream type filter
//   - bare non-keyword word   → treated as callsign prefix (Spider compat)
func (t *TelnetServer) handleShowDX(args []string, state *ClientState) string {
	p := ShowDXParams{
		Limit:   20,
		DayFrom: 1,
	}

	barePrefix := ""
	rangeSet := false

	i := 0
	for i < len(args) {
		raw := args[i]
		tok := strings.ToLower(raw)

		// ── N-M range (first occurrence only) ─────────────────────────────
		if !rangeSet {
			if from, to, ok := parseRange(raw); ok {
				rangeSet = true
				if from >= 1000 || to >= 1000 {
					p.FreqMinKHz = from
					p.FreqMaxKHz = to
				} else {
					p.Offset = int(from)
					p.Limit = int(to - from)
					if p.Limit <= 0 {
						p.Limit = 20
					}
				}
				i++
				continue
			}
		}

		// ── Bare integer → limit ───────────────────────────────────────────
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			if v > 200 {
				v = 200
			}
			p.Limit = v
			i++
			continue
		}

		next := ""
		if i+1 < len(args) {
			next = args[i+1]
		}

		switch tok {
		case "on", "freq":
			if next != "" {
				if from, to, ok := parseRange(next); ok && (from >= 1000 || to >= 1000) {
					p.FreqMinKHz = from
					p.FreqMaxKHz = to
				} else {
					p.Band = strings.ToLower(next)
				}
				i += 2
			} else {
				i++
			}
			continue

		case "call", "prefix":
			if next != "" {
				p.CallPrefix = next
				i += 2
			} else {
				i++
			}
			continue

		case "by", "spotter":
			if next != "" {
				p.Spotter = next
				i += 2
			} else {
				i++
			}
			continue

		case "info":
			if next != "" {
				p.InfoText = next
				i += 2
			} else {
				i++
			}
			continue

		case "iota":
			if next != "" && isIOTARef(next) {
				p.InfoText = next
				i += 2
			} else {
				p.InfoText = "iota"
				i++
			}
			continue

		case "qsl":
			p.InfoText = "qsl"
			i++
			continue

		case "day", "days":
			if next != "" {
				if from, to, ok := parseRange(next); ok {
					p.DayFrom = int(to)
					p.DayTo = int(from)
				} else if v, err := strconv.Atoi(next); err == nil && v > 0 {
					if v > 30 {
						v = 30
					}
					p.DayFrom = v
				}
				i += 2
			} else {
				i++
			}
			continue

		case "cont", "continent":
			if next != "" {
				p.Continent = strings.ToUpper(next)
				i += 2
			} else {
				i++
			}
			continue

		case "country":
			if next != "" {
				p.CountryCode = strings.ToUpper(next)
				i += 2
			} else {
				i++
			}
			continue

		case "countryname":
			if next != "" {
				p.CountryName = next
				i += 2
			} else {
				i++
			}
			continue

		case "mode":
			if next != "" {
				p.Mode = strings.ToUpper(next)
				i += 2
			} else {
				i++
			}
			continue

		case "type":
			if next != "" {
				switch strings.ToLower(next) {
				case "digital", "decoder":
					p.Stream = string(StreamDecoder)
				case "cw", "cwskimmer", "rbn":
					p.Stream = string(StreamCWSkimmer)
				case "voice", "voiceactivity":
					p.Stream = string(StreamVoiceActivity)
				case "dx", "dxcluster", "cluster":
					p.Stream = string(StreamDXCluster)
				default:
					p.Stream = next
				}
				i += 2
			} else {
				i++
			}
			continue
		}

		// ── Bare non-keyword token → callsign prefix (Spider compat) ──────
		if !spiderKeywords[tok] && barePrefix == "" {
			barePrefix = raw
		}
		i++
	}

	if barePrefix != "" && p.CallPrefix == "" {
		p.CallPrefix = barePrefix
	}

	var spots []Spot
	if t.store != nil {
		var err error
		spots, err = t.store.Query(p)
		if err != nil {
			return fmt.Sprintf("show/dx query error: %v", err)
		}
	} else {
		history := t.hub.History("")
		for _, s := range history {
			if state.ShouldSend(s) {
				spots = append(spots, s)
				if len(spots) >= p.Limit {
					break
				}
			}
		}
	}

	if len(spots) == 0 {
		return "No spots found matching your query."
	}

	lines := make([]string, 0, len(spots))
	for _, s := range spots {
		lines = append(lines, s.FormatDXCluster(t.spotterCall))
	}
	return strings.Join(lines, "\r\n")
}

// ── Parsing helpers ────────────────────────────────────────────────────────

// parseRange parses "N-M" or "N/M" into (from, to float64, ok bool).
func parseRange(s string) (float64, float64, bool) {
	for _, sep := range []string{"-", "/"} {
		idx := strings.Index(s, sep)
		if idx <= 0 {
			continue
		}
		a, b := s[:idx], s[idx+1:]
		fa, err1 := strconv.ParseFloat(a, 64)
		fb, err2 := strconv.ParseFloat(b, 64)
		if err1 == nil && err2 == nil && fb > fa {
			return fa, fb, true
		}
	}
	return 0, 0, false
}

// isIOTARef returns true if s looks like an IOTA island reference (e.g. EU-064).
func isIOTARef(s string) bool {
	s = strings.ToUpper(s)
	prefixes := []string{"EU-", "NA-", "SA-", "AF-", "AS-", "OC-", "AN-"}
	for _, pfx := range prefixes {
		if strings.HasPrefix(s, pfx) {
			rest := s[len(pfx):]
			if len(rest) >= 2 && len(rest) <= 3 {
				allDigits := true
				for _, c := range rest {
					if c < '0' || c > '9' {
						allDigits = false
						break
					}
				}
				if allDigits {
					return true
				}
			}
		}
	}
	return false
}
