package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ── show/who ────────────────────────────────────────────────────────────────

// handleShowWho implements SHOW/WHO.
// Lists all currently connected telnet/WebSocket clients with their masked IP,
// callsign (or "(logging in…)" if not yet logged in), and how long they have
// been connected.
func (t *TelnetServer) handleShowWho() string {
	clients := t.ConnectedClients()
	if len(clients) == 0 {
		return "No clients currently connected."
	}
	now := time.Now()
	var lines []string
	lines = append(lines, fmt.Sprintf("%-12s %-20s %s", "Callsign", "IP", "Connected"))
	lines = append(lines, strings.Repeat("-", 50))
	for _, c := range clients {
		call := c.Callsign
		if call == "" {
			call = "(logging in…)"
		}
		dur := formatDuration(now.Sub(c.ConnectedAt))
		lines = append(lines, fmt.Sprintf("%-12s %-20s %s", call, c.MaskedIP, dur))
	}
	lines = append(lines, fmt.Sprintf("\n%d client(s) connected.", len(clients)))
	return strings.Join(lines, "\r\n")
}

// ── show/prefix ─────────────────────────────────────────────────────────────

// handleShowPrefix implements SHOW/PREFIX <call|prefix> ...
// Shows DXCC country, CQ/ITU zone, lat/lon and continent for each argument.
// Mirrors DX Spider's cmd/show/prefix.pl output style.
func (t *TelnetServer) handleShowPrefix(args []string) string {
	if len(args) == 0 {
		return "Usage: show/prefix <callsign|prefix> [...]"
	}
	var lines []string
	for _, arg := range args {
		call := strings.ToUpper(arg)
		r, err := lookupCTY(t.ubersdrURL, call)
		if err != nil {
			return fmt.Sprintf("show/prefix error: %v", err)
		}
		if r == nil {
			lines = append(lines, fmt.Sprintf("%s: not found", call))
			continue
		}
		// DX Spider format (cmd/show/prefix.pl):
		//   <call> CC: <cc> IZ: <itu> CZ: <cq> LL: <lat> <lon> (<pfx>, <name>)
		// Spider's CC is the numeric DXCC entity; UberSDR's CTY provides the
		// ISO country_code instead, which we use in its place.
		lines = append(lines, fmt.Sprintf(
			"%-10s CC: %-4s IZ: %2d CZ: %2d LL: %-5s %-6s (%s)",
			call, r.CountryCode, r.ITUZone, r.CQZone,
			slat(r.Latitude), slong(r.Longitude), r.Country))
	}
	return strings.Join(lines, "\r\n")
}

// ── show/heading ────────────────────────────────────────────────────────────

// handleShowHeading implements SHOW/HEADING <call|prefix> ...
// Shows the beam heading and great-circle distance from the receiver to each
// callsign/prefix. Mirrors DX Spider's cmd/show/heading.pl.
func (t *TelnetServer) handleShowHeading(args []string) string {
	if len(args) == 0 {
		return "Usage: show/heading <callsign|prefix> [...]"
	}
	if t.rxLat == 0 && t.rxLon == 0 {
		return "Receiver location is not configured — headings unavailable."
	}
	var lines []string
	for _, arg := range args {
		call := strings.ToUpper(arg)
		r, err := lookupCTY(t.ubersdrURL, call)
		if err != nil {
			return fmt.Sprintf("show/heading error: %v", err)
		}
		if r == nil {
			lines = append(lines, fmt.Sprintf("%s: not found", call))
			continue
		}
		bearing, distKM := bearingDistance(t.rxLat, t.rxLon, r.Latitude, r.Longitude)
		recip := bearing + 180
		if recip >= 360 {
			recip -= 360
		}
		miles := distKM * 0.621371
		lines = append(lines, fmt.Sprintf(
			"%-10s %s: %.0f degs - dist: %.0f mi, %.0f km  Reciprocal heading: %.0f degs",
			call, r.Country, bearing, miles, distKM, recip))
	}
	return strings.Join(lines, "\r\n")
}

// ── show/dxcc ───────────────────────────────────────────────────────────────

// handleShowDXCC implements SHOW/DXCC <prefix> — show recent spots for the
// DXCC country that <prefix> resolves to. In DX Spider this is a pure alias
// for "SHOW/DX dxcc <prefix>" (see cmd/Aliases: '^sho?w?/dxcc' → 'show/dx dxcc'),
// producing normal spot lines with no extra header. We match that by resolving
// the prefix to a country_code via CTY and delegating to show/dx.
func (t *TelnetServer) handleShowDXCC(args []string, state *ClientState) string {
	if len(args) == 0 {
		return "Usage: show/dxcc <prefix> [options]  (e.g. show/dxcc G on 20m)"
	}
	pfx := strings.ToUpper(args[0])
	r, err := lookupCTY(t.ubersdrURL, pfx)
	if err != nil {
		return fmt.Sprintf("show/dxcc error: %v", err)
	}
	if r == nil || r.CountryCode == "" {
		return fmt.Sprintf("show/dxcc: cannot resolve %q to a DXCC country", pfx)
	}

	// Delegate to show/dx with a country filter, honouring any remaining
	// show/dx-style options after the prefix. No custom header — matches Spider.
	rest := append([]string{"country", r.CountryCode}, args[1:]...)
	return t.handleShowDX(rest, state)
}

// ── show/dxstats ────────────────────────────────────────────────────────────

// handleShowDXStats implements SHOW/DXSTATS [days] — total spots per day.
// Mirrors DX Spider's cmd/show/dxstats.pl.
func (t *TelnetServer) handleShowDXStats(args []string) string {
	if t.store == nil {
		return "Spot database is not available."
	}
	days := 31
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
			days = v
		}
	}
	rows, total, err := t.store.StatsPerDay(days)
	if err != nil {
		return fmt.Sprintf("show/dxstats error: %v", err)
	}
	if len(rows) == 0 {
		return "No spots in the database for the requested period."
	}
	// DX Spider header: "Total DX Spots for <days> days from <date>"
	today := time.Now().UTC().Format("2-Jan-2006")
	var b strings.Builder
	fmt.Fprintf(&b, "Total DX Spots for %d days from %s\r\n", days, today)
	for _, dc := range rows {
		fmt.Fprintf(&b, "%12s: %7d\r\n", dc.Date, dc.Count)
	}
	fmt.Fprintf(&b, "%12s: %7d", "Total", total)
	return b.String()
}

// ── show/hfstats ────────────────────────────────────────────────────────────

// handleShowHFStats implements SHOW/HFSTATS [days] as a per-day × per-band
// pivot table, matching DX Spider's cmd/show/hfstats.pl:
//
//	  Date| Total| 160m|  80m|  60m|  40m|  30m|  20m|  17m|  15m|  12m|  10m|
//	NN-Mon|   NNN|  NNN|  NNN| ...
//	 Total|   NNN|  NNN| ...
func (t *TelnetServer) handleShowHFStats(args []string) string {
	if t.store == nil {
		return "Spot database is not available."
	}
	days := 31
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
			days = v
		}
	}
	rows, err := t.store.StatsHFTable(days)
	if err != nil {
		return fmt.Sprintf("show/hfstats error: %v", err)
	}
	if len(rows) == 0 {
		return "No spots in the database for the requested period."
	}

	today := time.Now().UTC().Format("2-Jan-2006")
	var b strings.Builder
	fmt.Fprintf(&b, "HF DX Spot Stats, last %d days from %s\r\n", days, today)

	// Header row: Date | Total | <bands...>
	fmt.Fprintf(&b, "%6s|%6s|", "Date", "Total")
	for _, band := range hfTableBands {
		fmt.Fprintf(&b, "%5s|", strings.TrimSuffix(band, "m"))
	}
	b.WriteString("\r\n")

	// Column totals accumulator
	colTotal := make(map[string]int64)
	var grandTotal int64

	fmtCount := func(n int64) string {
		if n == 0 {
			return "     "
		}
		return fmt.Sprintf("%5d", n)
	}

	for _, row := range rows {
		var lineTotal int64
		for _, band := range hfTableBands {
			lineTotal += row.PerBand[band]
		}
		// Date column: DX Spider strips the year (dd-Mon)
		date := row.Date
		if len(date) == 10 { // YYYY-MM-DD → dd-Mon
			if tt, e := time.Parse("2006-01-02", date); e == nil {
				date = tt.Format("2-Jan")
			}
		}
		fmt.Fprintf(&b, "%6s|%6d|", date, lineTotal)
		for _, band := range hfTableBands {
			c := row.PerBand[band]
			colTotal[band] += c
			b.WriteString(fmtCount(c) + "|")
		}
		b.WriteString("\r\n")
		grandTotal += lineTotal
	}

	// Totals row
	fmt.Fprintf(&b, "%6s|%6d|", "Total", grandTotal)
	for _, band := range hfTableBands {
		b.WriteString(fmtCount(colTotal[band]) + "|")
	}
	return b.String()
}

// ── show/status ─────────────────────────────────────────────────────────────

// handleShowStatus shows database statistics, uptime, and connected clients.
func (t *TelnetServer) handleShowStatus() string {
	var b strings.Builder

	// Uptime
	uptime := time.Since(t.startTime)
	days := int(uptime.Hours()) / 24
	hours := int(uptime.Hours()) % 24
	mins := int(uptime.Minutes()) % 60
	fmt.Fprintf(&b, "UberSDR DX Cluster — %s\r\n", t.version)
	fmt.Fprintf(&b, "Uptime    : %dd %02dh %02dm\r\n", days, hours, mins)
	fmt.Fprintf(&b, "Clients   : %d connected\r\n", t.ClientCount())
	fmt.Fprintf(&b, "Receiver  : %s — %s\r\n", t.rxName, t.rxLocation)

	if t.store == nil {
		b.WriteString("Database  : not available\r\n")
		return b.String()
	}

	streams, oldest, newest, sizeKB, err := t.store.StatsOverview()
	if err != nil {
		fmt.Fprintf(&b, "Database  : error — %v\r\n", err)
		return b.String()
	}

	// Total spot count
	var total int64
	for _, sc := range streams {
		total += sc.Count
	}
	fmt.Fprintf(&b, "DB Spots  : %s total", commaInt(total))
	if sizeKB > 0 {
		if sizeKB >= 1024 {
			fmt.Fprintf(&b, " (%.1f MB on disk)", float64(sizeKB)/1024)
		} else {
			fmt.Fprintf(&b, " (%d KB on disk)", sizeKB)
		}
	}
	b.WriteString("\r\n")

	// Date range
	if !oldest.IsZero() && !newest.IsZero() {
		fmt.Fprintf(&b, "DB Range  : %s → %s\r\n",
			oldest.Format("2-Jan-2006 15:04Z"),
			newest.Format("2-Jan-2006 15:04Z"))
	}

	// Per-stream breakdown
	streamLabels := map[string]string{
		string(StreamDecoder):       "Digital (FT8/FT4/WSPR)",
		string(StreamCWSkimmer):     "CW Skimmer",
		string(StreamVoiceActivity): "Voice Activity",
		string(StreamDXCluster):     "DX Cluster",
	}
	for _, sc := range streams {
		label := streamLabels[sc.Stream]
		if label == "" {
			label = sc.Stream
		}
		fmt.Fprintf(&b, "  %-24s: %s\r\n", label, commaInt(sc.Count))
	}

	return strings.TrimRight(b.String(), "\r\n")
}

// commaInt formats an int64 with thousands separators, e.g. 1234567 → "1,234,567".
func commaInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}
	return string(out)
}
