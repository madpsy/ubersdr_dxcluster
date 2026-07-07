package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ── Help text ──────────────────────────────────────────────────────────────

const helpText = `
UberSDR DX Cluster — Command Reference
=======================================

Commands can be abbreviated: SET/FILTER → SET/F, SHOW/DX → SH/DX, etc.

SIMPLE FILTERS  (AND-combined; multiple values within a field are OR-combined)
  set/filter band <bands>       Filter by band (comma-separated)
                                  e.g. set/filter band 20m
                                       set/filter band 40m,20m,15m
                                  Bands: 2200m 630m 600m 160m 80m 60m 40m 30m 20m 17m 15m 12m 11m 10m 6m

  set/filter mode <modes>       Filter by mode (comma-separated)
                                  Digital: FT8 FT4 WSPR JS8 FT2
                                  CW:      CW
                                  Voice:   USB LSB
                                  e.g. set/filter mode FT8,FT4,WSPR

  set/filter type <types>       Filter by activity type (comma-separated)
                                  Types: digital  cw  voice  dx
                                  e.g. set/filter type cw,digital

  set/filter cont <conts>       Filter by continent (comma-separated)
                                  Codes: EU NA SA AF AS OC AN
                                  e.g. set/filter cont EU,NA

  set/filter country <codes>    Filter by country ISO 3166-1 alpha-2 (comma-separated)
                                  e.g. set/filter country DE,PA,ON

  set/filter call <prefixes>    Filter by callsign prefix (comma-separated)
                                  e.g. set/filter call DL,VK,ZL

  set/filter snr <dB>           Minimum SNR threshold, e.g. set/filter snr 10
  set/filter maxsnr <dB>        Maximum SNR threshold, e.g. set/filter maxsnr 30

CLEARING SIMPLE FILTERS
  clear/filter                  Clear ALL active filters (simple + slots)
  clear/filter band             Clear band filter only
  clear/filter mode             Clear mode filter only
  clear/filter type             Clear type filter only
  clear/filter cont             Clear continent filter only
  clear/filter country          Clear country filter only
  clear/filter call             Clear callsign prefix filter only
  clear/filter snr              Clear minimum SNR filter
  clear/filter maxsnr           Clear maximum SNR filter

DX SPIDER ACCEPT/REJECT FILTERS  (numbered slots 0-9, default slot 1)
  accept/spots [N] <expr>       Accept spots matching expression (slot N, default 1)
  reject/spots [N] <expr>       Reject spots matching expression (slot N, default 1)
  accept/rbn [N] <expr>         Accept RBN spots matching expression
  reject/rbn [N] <expr>         Reject RBN spots matching expression

  Filter expression fields (combine with AND / OR / NOT and parentheses):
    on <freq>                   Band, region or kHz range:
                                  on 20m            (band)
                                  on hf             (region: 1800-30000 kHz)
                                  on hf/cw          (all HF CW segments)
                                  on 20m/cw         (that band's CW segment)
                                  on 20m/ssb        (that band's phone segment)
                                  on 14000/14070    (explicit kHz range)
    freq <freq>                 Alias for 'on'
    call <prefix>               Callsign prefix (comma-separated), e.g. call DL,PA
    by <call>                   Spotter callsign prefix
    cont <code>                 Continent, e.g. cont EU,NA
    country <code>              Country code, e.g. country DE
    mode <mode>                 Mode, e.g. mode FT8,FT4
    type <type>                 Stream type: digital cw voice dx
    info <text>                 Comment/message substring
    iota                        IOTA spots
    qsl                         QSL/VIA spots
    all                         Match everything

  BOOLEAN LOGIC:
    Use 'and', 'or', 'not' and parentheses to build complex expressions.
    IMPORTANT: when using OR, always use brackets to group terms.
    Adjacent terms without 'and'/'or' are implicitly AND-combined.

  SLOT SEMANTICS (matching DX Spider):
    Each numbered slot (0-9) can hold BOTH a reject rule and an accept rule.
    Slots are evaluated in order. Within a slot, reject is checked first:
      - if reject matches → spot is DROPPED
      - if accept matches → spot is KEPT
    If only reject rules exist: everything passes EXCEPT matching spots.
    If only accept rules exist: ONLY matching spots pass.

  Examples:
    accept/spots on 20m
    accept/spots 1 on hf and call DL
    accept/spots on hf and (cont EU or cont NA)
    reject/spots on hf/cw
    reject/spots on hf/cw and not info iota
    accept/spots not on hf/cw or info iota
    reject/spots 1 on hf/cw
    accept/spots 2 on hf
    accept/rbn on 40m
    clear/spots 1
    clear/spots all

CLEARING SLOT FILTERS
  clear/spots [N|all]           Clear accept/reject spot filter slot N (or all slots)
  clear/rbn [N|all]             Clear accept/reject RBN filter slot N (or all slots)

SPOT STREAM TOGGLES  (each stream can be enabled/disabled independently)
  set/dx                        Enable ALL spots (DX Spider compat, default: on)
  unset/dx                      Disable ALL spots

  set/digital                   Enable digital decoder spots (FT8/FT4/WSPR/JS8, default: off)
  unset/digital                 Disable digital decoder spots

  set/rbn                       Enable CW/RBN skimmer spots (default: on)
  unset/rbn                     Disable CW/RBN skimmer spots
  set/skimmer                   Alias for set/rbn
  unset/skimmer                 Alias for unset/rbn

  set/voice                     Enable voice activity spots (default: on)
  unset/voice                   Disable voice activity spots

  set/dxcluster                 Enable DX cluster spots (default: off)
  unset/dxcluster               Disable DX cluster spots
  set/cluster                   Alias for set/dxcluster
  unset/cluster                 Alias for unset/dxcluster

INFORMATION
  show/filter                   Show all currently active filters
  show/dx [N] [options]         Query spot history (up to 30 days, default: last 20 spots)
    Options (can be combined):
      <N>                         Number of spots to show (default: 20, max: 200)
      <from>-<to>                 Spot offset range, e.g. 30-40 (spots 30 to 40)
      on <band>                   Filter by band, e.g. on 20m
      on <kHz>-<kHz>              Filter by frequency range, e.g. on 14000-14033
      call <prefix>               Filter by callsign prefix, e.g. call DL
      prefix <prefix>             Alias for call
      <callsign>                  Bare callsign/prefix (no keyword needed), e.g. g0vgs
      by <call>                   Filter by spotter callsign, e.g. by G3ABC
      info <text>                 Search comment/message text, e.g. info iota
      iota [<ref>]                Search for IOTA spots, e.g. iota or iota EU-064
      qsl                         Search for QSL/VIA info in comments
      day <N>                     Look back N days (default: 1, max: 30)
      day <from>-<to>             Day range, e.g. day 7-14 (7 to 14 days ago)
      cont <code>                 Filter by continent, e.g. cont EU
      country <code>              Filter by country code, e.g. country DE
      mode <mode>                 Filter by mode, e.g. mode FT8
      type <type>                 Filter by stream type: digital cw voice dx
    Examples:
      show/dx
      show/dx 5
      show/dx 20
      show/dx on 20m
      show/dx 10 on 20m
      show/dx g0vgs
      show/dx 10 g0vgs
      show/dx 30-40
      show/dx 14000-14033
      show/dx iota
      show/dx iota EU-064
      show/dx qsl
      show/dx day 30
      show/dx 20 call 9a on 20m day 30
      show/dx on 40m mode CW day 3
  show/status                   Show cluster status: uptime, clients, DB stats
                                  e.g. show/status  (or sh/stat)
  show/qrz <callsign>           Look up callbook details for a callsign
                                  e.g. show/qrz g1tlh  (or sh/qrz g1tlh)
                                  Data provided by qrz.com via UberSDR
  show/prefix <call|pfx> ...    Show DXCC country, CQ/ITU zone, lat/lon
                                  e.g. show/prefix G  (or sh/pre G)
  show/dxcc <prefix> [opts]     Show recent spots for a DXCC country
                                  e.g. show/dxcc G on 20m
  show/heading <call|pfx> ...   Show beam heading + distance from receiver
                                  e.g. show/heading VK  (or sh/hea VK)
  show/dxstats [days]           Show spot totals per day (default: 31 days)
  show/hfstats [days]           Show spot totals per band (default: 31 days)
  show/time                     Show current UTC time
  show/version                  Show cluster software version
  help [<command>]              Show this help text

SESSION
  bye / quit                    Disconnect from the cluster

NOTES
  - Simple filters (set/filter) are AND-combined across fields
  - Multiple values within a field are OR-matched (e.g. band 40m,20m = 40m OR 20m)
  - Accept/reject slots follow DX Spider semantics:
      reject is checked first; if any reject slot matches, the spot is dropped
      if accept slots are set, the spot must match at least one to pass
  - Callsign prefix matching is case-insensitive prefix match (DL matches DL1ABC)
  - Country codes are exact ISO 3166-1 alpha-2 match (case-insensitive)
  - Filters persist for the duration of your connection only
  - show/dx queries the persistent database (up to 30 days of history)

`

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
		return fmt.Sprintf("UberSDR DX Cluster — %s", t.version)

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
			return fmt.Sprintf("Invalid slot number %q — use 0-9 or 'all'", parts[1])
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
				return "Invalid SNR value — use a number, e.g. set/filter snr 10"
			}
			state.Filter.MinSNR = &v
			return fmt.Sprintf("Filter set: snr >= %.1f dB", v)
		case "maxsnr":
			v, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return "Invalid SNR value — use a number, e.g. set/filter maxsnr 30"
			}
			state.Filter.MaxSNR = &v
			return fmt.Sprintf("Filter set: maxsnr <= %.1f dB", v)
		default:
			return fmt.Sprintf("Unknown filter field %q — type HELP for usage", field)
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
			return fmt.Sprintf("Unknown filter field %q — type HELP for usage", field)
		}

	default:
		return fmt.Sprintf("Unknown command %q — type HELP for a list of commands", parts[0])
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
	"cont": true, "continent": true, "country": true,
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
