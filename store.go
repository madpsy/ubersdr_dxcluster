package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ── Schema ─────────────────────────────────────────────────────────────────

const createSchema = `
CREATE TABLE IF NOT EXISTS spots (
    id             INTEGER PRIMARY KEY,
    stream         TEXT    NOT NULL,
    ts             INTEGER NOT NULL,
    band           TEXT,
    callsign       TEXT,
    freq_hz        REAL,
    snr            REAL,
    country        TEXT,
    country_code   TEXT,
    continent      TEXT,
    cq_zone        INTEGER,
    mode           TEXT,
    comment        TEXT,
    message        TEXT,
    spotter        TEXT,
    wpm            INTEGER,
    locator        TEXT,
    voice_mode     TEXT,
    est_dial_freq  INTEGER,
    confidence     REAL,
    bandwidth      INTEGER,
    avg_signal_db  REAL,
    peak_signal_db REAL,
    distance_km    REAL,
    bearing_deg    REAL
);

CREATE INDEX IF NOT EXISTS idx_spots_ts
    ON spots(ts DESC);
CREATE INDEX IF NOT EXISTS idx_spots_band_ts
    ON spots(band, ts DESC);
CREATE INDEX IF NOT EXISTS idx_spots_call_ts
    ON spots(callsign, ts DESC);
CREATE INDEX IF NOT EXISTS idx_spots_spotter_ts
    ON spots(spotter, ts DESC);
CREATE INDEX IF NOT EXISTS idx_spots_stream_ts
    ON spots(stream, ts DESC);
CREATE INDEX IF NOT EXISTS idx_spots_cont_ts
    ON spots(continent, ts DESC);
CREATE INDEX IF NOT EXISTS idx_spots_cc_ts
    ON spots(country_code, ts DESC);
`

const insertSpot = `
INSERT INTO spots (
    stream, ts, band, callsign, freq_hz, snr,
    country, country_code, continent, cq_zone,
    mode, comment, message, spotter, wpm, locator,
    voice_mode, est_dial_freq, confidence, bandwidth,
    avg_signal_db, peak_signal_db, distance_km, bearing_deg
) VALUES (
    ?,?,?,?,?,?,
    ?,?,?,?,
    ?,?,?,?,?,?,
    ?,?,?,?,
    ?,?,?,?
)`

// ── SpotStore ──────────────────────────────────────────────────────────────

// SpotStore persists spots to a SQLite database and provides query access.
type SpotStore struct {
	db *sql.DB
	in chan Spot

	// Voice spot dedup: skip inserting a voice spot if the same
	// (callsign, freq kHz, voice_mode) was seen within voiceDedupWindow.
	voiceDedupWindow time.Duration
	voiceDedupMu     sync.Mutex
	voiceDedup       map[string]time.Time // key → last-seen time

	// Decoder spot dedup: skip inserting a decoder spot if the same
	// (callsign, band, mode) was seen within decoderDedupWindow.
	decoderDedupWindow time.Duration
	decoderDedupMu     sync.Mutex
	decoderDedup       map[string]time.Time // key → last-seen time
}

// OpenStore opens (or creates) the SQLite database at path, applies the
// schema, and returns a ready-to-use SpotStore.
func OpenStore(path string) (*SpotStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	// auto_vacuum must be set BEFORE any table is created to take effect on a
	// fresh database. INCREMENTAL mode lets us reclaim freed pages on demand
	// (via PRAGMA incremental_vacuum) after each purge, without the full-file
	// rewrite cost of a plain VACUUM. On an existing DB created without
	// auto_vacuum, this pragma is a no-op until a one-off VACUUM converts it —
	// which OpenStore performs below.
	if _, err := db.Exec("PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		return nil, fmt.Errorf("pragma auto_vacuum: %w", err)
	}

	// Performance PRAGMAs — must be set before any other operations.
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous   = NORMAL",
		"PRAGMA cache_size    = -8000", // 8 MB
		"PRAGMA temp_store    = MEMORY",
		"PRAGMA foreign_keys  = OFF",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return nil, fmt.Errorf("pragma %q: %w", p, err)
		}
	}

	if _, err := db.Exec(createSchema); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}

	// If the database predates auto_vacuum being enabled, convert it once.
	// auto_vacuum returns 0 (NONE), 1 (FULL) or 2 (INCREMENTAL).
	var av int
	if err := db.QueryRow("PRAGMA auto_vacuum").Scan(&av); err == nil && av == 0 {
		log.Printf("[store] converting existing DB to incremental auto_vacuum (one-off VACUUM)")
		if _, err := db.Exec("VACUUM"); err != nil {
			log.Printf("[store] VACUUM during auto_vacuum conversion failed: %v", err)
		}
	}

	s := &SpotStore{
		db:           db,
		in:           make(chan Spot, 4096),
		voiceDedup:   make(map[string]time.Time),
		decoderDedup: make(map[string]time.Time),
	}
	return s, nil
}

// SetVoiceDedupWindow configures the dedup window for voice spots.
// A zero or negative value disables dedup entirely.
func (s *SpotStore) SetVoiceDedupWindow(d time.Duration) {
	s.voiceDedupWindow = d
}

// voiceDedupKey returns the dedup key for a voice spot:
// "callsign|freqKHz|mode". Frequency is rounded to the nearest kHz to
// absorb small drift without creating false "new" entries.
func voiceDedupKey(sp Spot) string {
	freqKHz := int64(sp.FreqHz/1000 + 0.5)
	return fmt.Sprintf("%s|%d|%s", sp.Callsign, freqKHz, sp.VoiceMode)
}

// SetDecoderDedupWindow configures the dedup window for decoder (digital) spots.
// A zero or negative value disables dedup entirely.
func (s *SpotStore) SetDecoderDedupWindow(d time.Duration) {
	s.decoderDedupWindow = d
}

// decoderDedupKey returns the dedup key for a decoder spot:
// "callsign|band|mode" — same callsign on the same band/mode within the
// window is considered a duplicate.
func decoderDedupKey(sp Spot) string {
	return fmt.Sprintf("%s|%s|%s", sp.Callsign, sp.Band, sp.Mode)
}

// Publish queues a spot for async insertion. Non-blocking — drops if full.
// Voice spots are deduplicated: if the same (callsign, freq, mode) was seen
// within voiceDedupWindow, the spot is silently dropped from the DB write
// path (real-time streaming via the hub ring buffer is unaffected).
// Decoder (digital) spots are deduplicated: if the same (callsign, band, mode)
// was seen within decoderDedupWindow, the spot is silently dropped from the DB
// write path.
func (s *SpotStore) Publish(spot Spot) {
	if spot.Stream == StreamVoiceActivity && s.voiceDedupWindow > 0 {
		key := voiceDedupKey(spot)
		now := time.Now()

		s.voiceDedupMu.Lock()
		last, seen := s.voiceDedup[key]
		if seen && now.Sub(last) < s.voiceDedupWindow {
			s.voiceDedupMu.Unlock()
			return // duplicate within window — skip DB insert
		}
		s.voiceDedup[key] = now
		// Lazy sweep: remove entries older than the window to bound map size.
		for k, t := range s.voiceDedup {
			if now.Sub(t) >= s.voiceDedupWindow {
				delete(s.voiceDedup, k)
			}
		}
		s.voiceDedupMu.Unlock()
	}

	if spot.Stream == StreamDecoder && s.decoderDedupWindow > 0 {
		key := decoderDedupKey(spot)
		now := time.Now()

		s.decoderDedupMu.Lock()
		last, seen := s.decoderDedup[key]
		if seen && now.Sub(last) < s.decoderDedupWindow {
			s.decoderDedupMu.Unlock()
			return // duplicate within window — skip DB insert
		}
		s.decoderDedup[key] = now
		// Lazy sweep: remove entries older than the window to bound map size.
		for k, t := range s.decoderDedup {
			if now.Sub(t) >= s.decoderDedupWindow {
				delete(s.decoderDedup, k)
			}
		}
		s.decoderDedupMu.Unlock()
	}

	select {
	case s.in <- spot:
	default:
		// channel full — drop rather than block the hub
	}
}

// Run is the store's write loop. It batches inserts every second.
// Call in a goroutine; returns when the channel is closed.
func (s *SpotStore) Run() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var batch []Spot

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.insertBatch(batch); err != nil {
			log.Printf("[store] insert batch error: %v", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case spot, ok := <-s.in:
			if !ok {
				flush()
				return
			}
			batch = append(batch, spot)
			if len(batch) >= 500 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// insertBatch writes a slice of spots in a single transaction.
func (s *SpotStore) insertBatch(batch []Spot) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(insertSpot)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, sp := range batch {
		_, err := stmt.Exec(
			string(sp.Stream),
			sp.Timestamp.Unix(),
			sp.Band,
			sp.Callsign,
			sp.FreqHz,
			sp.SNR,
			sp.Country,
			sp.CountryCode,
			sp.Continent,
			nullInt(sp.CQZone),
			sp.Mode,
			sp.Comment,
			sp.Message,
			sp.Spotter,
			nullInt(sp.WPM),
			sp.Locator,
			sp.VoiceMode,
			nullInt(sp.EstDialFreq),
			nullFloat(sp.Confidence),
			nullInt(sp.Bandwidth),
			nullFloat(sp.AvgSignalDB),
			nullFloat(sp.PeakSignalDB),
			nullFloat(sp.DistanceKM),
			nullFloat(sp.BearingDeg),
		)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ── Purge ──────────────────────────────────────────────────────────────────

// RunPurge runs a daily purge of spots older than retainDays.
// Call in a goroutine.
func (s *SpotStore) RunPurge(retainDays int) {
	// Run once at startup (in case the process was down for a while),
	// then every 24 hours.
	s.purge(retainDays)
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		s.purge(retainDays)
	}
}

func (s *SpotStore) purge(retainDays int) {
	cutoff := time.Now().AddDate(0, 0, -retainDays).Unix()
	res, err := s.db.Exec(`DELETE FROM spots WHERE ts < ?`, cutoff)
	if err != nil {
		log.Printf("[store] purge error: %v", err)
		return
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Printf("[store] purged %d spots older than %d days", n, retainDays)
	}
	// Checkpoint the WAL, then return freed pages to the OS. With
	// auto_vacuum=INCREMENTAL, incremental_vacuum shrinks the main DB file by
	// releasing pages freed by the DELETE above — cheap compared to a full VACUUM.
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	_, _ = s.db.Exec(`PRAGMA incremental_vacuum`)
}

// ── Query ──────────────────────────────────────────────────────────────────

// ShowDXParams holds the parsed parameters from a show/dx command.
type ShowDXParams struct {
	Limit       int     // max rows to return (default 20, max 200)
	Offset      int     // skip first N results (for show/dx 30-40 style)
	DayFrom     int     // look back at most DayFrom days (0 = use DayTo or default 1)
	DayTo       int     // look back at least DayTo days (0 = not set)
	Band        string  // e.g. "20m"
	FreqMinKHz  float64 // lower bound of frequency range in kHz (0 = not set)
	FreqMaxKHz  float64 // upper bound of frequency range in kHz (0 = not set)
	CallPrefix  string  // callsign prefix, e.g. "DL"
	Spotter     string  // spotter prefix, e.g. "G3ABC"
	InfoText    string  // substring to match in comment or message
	Continent   string  // e.g. "EU"
	CountryCode string  // ISO 3166-1 alpha-2, e.g. "DE"
	CountryName string  // exact country name match, e.g. "England" — used when CountryCode is empty
	Mode        string  // e.g. "FT8"
	Stream      string  // stream type, e.g. "cwskimmer"
}

// Query executes a show/dx query and returns matching spots newest-first.
func (s *SpotStore) Query(p ShowDXParams) ([]Spot, error) {
	if p.Limit <= 0 {
		p.Limit = 20
	}
	if p.Limit > 200 {
		p.Limit = 200
	}

	// Resolve day range → Unix cutoffs.
	// DayFrom is the older bound (further back), DayTo is the newer bound.
	// show/dx day 7        → DayFrom=7, DayTo=0  → ts > now-7d
	// show/dx day 7-14     → DayFrom=14, DayTo=7 → ts BETWEEN now-14d AND now-7d
	// default (no day arg) → DayFrom=1, DayTo=0  → ts > now-1d
	if p.DayFrom <= 0 {
		p.DayFrom = 1
	}
	now := time.Now()
	cutoffOld := now.AddDate(0, 0, -p.DayFrom).Unix()

	var where []string
	var args []any

	where = append(where, "ts > ?")
	args = append(args, cutoffOld)

	if p.DayTo > 0 {
		cutoffNew := now.AddDate(0, 0, -p.DayTo).Unix()
		where = append(where, "ts < ?")
		args = append(args, cutoffNew)
	}

	if p.Band != "" {
		where = append(where, "band = ?")
		args = append(args, p.Band)
	}
	if p.FreqMinKHz > 0 {
		where = append(where, "freq_hz >= ?")
		args = append(args, p.FreqMinKHz*1000)
	}
	if p.FreqMaxKHz > 0 {
		where = append(where, "freq_hz <= ?")
		args = append(args, p.FreqMaxKHz*1000)
	}
	if p.CallPrefix != "" {
		where = append(where, "callsign LIKE ?")
		args = append(args, strings.ToUpper(p.CallPrefix)+"%")
	}
	if p.Spotter != "" {
		where = append(where, "spotter LIKE ?")
		args = append(args, strings.ToUpper(p.Spotter)+"%")
	}
	if p.Continent != "" {
		where = append(where, "continent = ?")
		args = append(args, strings.ToUpper(p.Continent))
	}
	if p.CountryCode != "" {
		where = append(where, "country_code = ?")
		args = append(args, strings.ToUpper(p.CountryCode))
	} else if p.CountryName != "" {
		// Fallback for non-sovereign entities that have no ISO2 code:
		// match on the country name column (case-insensitive exact match).
		where = append(where, "LOWER(country) = LOWER(?)")
		args = append(args, p.CountryName)
	}
	if p.Mode != "" {
		where = append(where, "mode = ?")
		args = append(args, strings.ToUpper(p.Mode))
	}
	if p.Stream != "" {
		where = append(where, "stream = ?")
		args = append(args, p.Stream)
	}
	if p.InfoText != "" {
		where = append(where, "(comment LIKE ? OR message LIKE ?)")
		pat := "%" + p.InfoText + "%"
		args = append(args, pat, pat)
	}

	query := `SELECT
		stream, ts, band, callsign, freq_hz, snr,
		country, country_code, continent, cq_zone,
		mode, comment, message, spotter, wpm, locator,
		voice_mode, est_dial_freq, confidence, bandwidth,
		avg_signal_db, peak_signal_db, distance_km, bearing_deg
	FROM spots
	WHERE ` + strings.Join(where, " AND ") + `
	ORDER BY ts DESC
	LIMIT ? OFFSET ?`
	args = append(args, p.Limit, p.Offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Spot
	for rows.Next() {
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
			return nil, err
		}

		sp.Stream = StreamType(stream.String)
		sp.Timestamp = time.Unix(ts, 0).UTC()
		sp.Band = band.String
		sp.Callsign = callsign.String
		sp.Country = country.String
		sp.CountryCode = countryCode.String
		sp.Continent = continent.String
		sp.CQZone = int(cqZone.Int64)
		sp.Mode = mode.String
		sp.Comment = comment.String
		sp.Message = message.String
		sp.Spotter = spotter.String
		sp.WPM = int(wpm.Int64)
		sp.Locator = locator.String
		sp.VoiceMode = voiceMode.String
		sp.EstDialFreq = int(estDialFreq.Int64)
		sp.Confidence = confidence.Float64
		sp.Bandwidth = int(bandwidth.Int64)
		sp.AvgSignalDB = avgSignalDB.Float64
		sp.PeakSignalDB = peakSignalDB.Float64
		sp.DistanceKM = distanceKM.Float64
		sp.BearingDeg = bearingDeg.Float64

		out = append(out, sp)
	}
	return out, rows.Err()
}

// Count returns the total number of spots in the database.
func (s *SpotStore) Count() int64 {
	var n int64
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM spots`).Scan(&n)
	return n
}

// StreamCount is one row of the show/status stream breakdown.
type StreamCount struct {
	Stream string
	Count  int64
}

// StatsOverview returns per-stream spot counts and the oldest/newest spot
// timestamps for the show/status command.
func (s *SpotStore) StatsOverview() (streams []StreamCount, oldest, newest time.Time, dbSizeKB int64, err error) {
	// Per-stream counts
	rows, e := s.db.Query(`SELECT stream, COUNT(*) FROM spots GROUP BY stream ORDER BY COUNT(*) DESC`)
	if e != nil {
		err = e
		return
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var sc StreamCount
		if e := rows.Scan(&sc.Stream, &sc.Count); e != nil {
			err = e
			return
		}
		streams = append(streams, sc)
	}
	if e := rows.Err(); e != nil {
		err = e
		return
	}

	// Oldest and newest spot timestamps
	var oldestTS, newestTS sql.NullInt64
	_ = s.db.QueryRow(`SELECT MIN(ts), MAX(ts) FROM spots`).Scan(&oldestTS, &newestTS)
	if oldestTS.Valid {
		oldest = time.Unix(oldestTS.Int64, 0).UTC()
	}
	if newestTS.Valid {
		newest = time.Unix(newestTS.Int64, 0).UTC()
	}

	// DB file size via SQLite page_count * page_size
	var pageCount, pageSize int64
	_ = s.db.QueryRow(`PRAGMA page_count`).Scan(&pageCount)
	_ = s.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize)
	dbSizeKB = (pageCount * pageSize) / 1024
	return
}

// DayCount is one row of the show/dxstats output: a date and spot total.
type DayCount struct {
	Date  string // YYYY-MM-DD (UTC)
	Count int64
}

// StatsPerDay returns spot totals per UTC day for the last `days` days,
// newest day first. Powers show/dxstats.
func (s *SpotStore) StatsPerDay(days int) ([]DayCount, int64, error) {
	if days <= 0 {
		days = 31
	}
	if days > 366 {
		days = 366
	}
	cutoff := time.Now().AddDate(0, 0, -days).Unix()
	rows, err := s.db.Query(`
		SELECT date(ts, 'unixepoch') AS d, COUNT(*)
		FROM spots
		WHERE ts > ?
		GROUP BY d
		ORDER BY d DESC`, cutoff)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var out []DayCount
	var total int64
	for rows.Next() {
		var dc DayCount
		if err := rows.Scan(&dc.Date, &dc.Count); err != nil {
			return nil, 0, err
		}
		out = append(out, dc)
		total += dc.Count
	}
	return out, total, rows.Err()
}

// hfTableBands is the fixed column set for show/hfstats, matching DX Spider's
// cmd/show/hfstats.pl band order.
var hfTableBands = []string{"160m", "80m", "60m", "40m", "30m", "20m", "17m", "15m", "12m", "10m"}

// HFDayRow is one day's row in the show/hfstats pivot table: the date plus a
// per-band spot count keyed by band label.
type HFDayRow struct {
	Date    string           // YYYY-MM-DD (UTC)
	PerBand map[string]int64 // band label → count
}

// StatsHFTable returns per-day, per-band spot counts for the last `days` days
// (newest day first), restricted to the HF bands in hfTableBands. Powers the
// DX Spider-style show/hfstats pivot table.
func (s *SpotStore) StatsHFTable(days int) ([]HFDayRow, error) {
	if days <= 0 {
		days = 31
	}
	if days > 366 {
		days = 366
	}
	cutoff := time.Now().AddDate(0, 0, -days).Unix()
	rows, err := s.db.Query(`
		SELECT date(ts, 'unixepoch') AS d, band, COUNT(*)
		FROM spots
		WHERE ts > ? AND band != ''
		GROUP BY d, band
		ORDER BY d DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	// Preserve day order while accumulating per-band counts.
	order := make([]string, 0)
	byDate := make(map[string]*HFDayRow)
	for rows.Next() {
		var d, band string
		var count int64
		if err := rows.Scan(&d, &band, &count); err != nil {
			return nil, err
		}
		row, ok := byDate[d]
		if !ok {
			row = &HFDayRow{Date: d, PerBand: make(map[string]int64)}
			byDate[d] = row
			order = append(order, d)
		}
		row.PerBand[band] += count
	}
	out := make([]HFDayRow, 0, len(order))
	for _, d := range order {
		out = append(out, *byDate[d])
	}
	return out, rows.Err()
}

// ── SQL null helpers ───────────────────────────────────────────────────────

// nullInt returns nil for zero values so SQLite stores NULL instead of 0.
func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// nullFloat returns nil for zero values so SQLite stores NULL instead of 0.
func nullFloat(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}
