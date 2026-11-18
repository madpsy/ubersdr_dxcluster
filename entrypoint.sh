#!/bin/sh
# entrypoint.sh — translate environment variables into ubersdr_dxcluster flags
#
# Environment variables:
#   UBERSDR_URL     UberSDR base HTTP URL (default: http://ubersdr:8080)
#   WEB_PORT        Port for the web UI server (default: 6087)
#   TELNET_PORT     Port for the DX cluster telnet server (default: 7300)
#   SPOTTER_CALL    Callsign shown as spotter for digital/voice spots
#                   (default: fetched from /api/description at startup —
#                   startup waits, retrying every 5s, until that succeeds)
#   SSTV_URL        SSTV addon base URL (default: http://sstv:6091). Used only
#                   when the receiver lists the sstv addon as enabled.
#   REQUIRE_LOGIN   Require a valid callsign on telnet connect (default: true)
#                   Set to "false" or "0" to allow anonymous connections
#   DATA_DIR        Directory holding spots.db (default: /data)
#   DB_CHECK        Integrity-check (and if needed repair) spots.db before
#                   starting (default: true). Set to "false" to skip.
#
# Note: The URL base path for reverse-proxy is read automatically from the
#       X-Forwarded-Prefix request header — no configuration needed.

set -e

# ---------------------------------------------------------------------------
# Spot database integrity check / repair
#
# Runs PRAGMA quick_check on spots.db. If the file is damaged (e.g. after a
# power cut), the damaged copy is moved aside as spots.db.corrupt-<time> and
# everything readable is salvaged with sqlite3's .recover into a new spots.db.
# The app exits on detecting corruption at runtime, so a container restart
# lands here and repairs it automatically.
# ---------------------------------------------------------------------------
db_log() { echo "$(date '+%Y/%m/%d %H:%M:%S') [dxcluster] [db-check] $*"; }

db_check_and_repair() {
    data_dir="${DATA_DIR:-/data}"
    db="$data_dir/spots.db"

    [ -s "$db" ] || return 0
    if ! command -v sqlite3 >/dev/null 2>&1; then
        db_log "sqlite3 not installed — skipping integrity check"
        return 0
    fi

    db_log "checking $db ($(du -h "$db" | cut -f1))"
    start=$(date +%s)
    result=$(sqlite3 -bail "$db" "PRAGMA busy_timeout=10000; PRAGMA quick_check(20);" 2>&1 | grep -v '^10000$' || true)
    if [ "$result" = "ok" ]; then
        db_log "ok ($(( $(date +%s) - start ))s)"
        return 0
    fi

    # Problems that are not damage to the file itself: leave it alone.
    case "$result" in
        *"locked"*|*"busy"*|*"unable to open"*|*"readonly"*|*"ermission denied"*|*"disk I/O"*)
            db_log "could not check database, leaving it untouched: $result"
            return 0 ;;
    esac

    db_log "database is CORRUPT:"
    echo "$result" | head -10 | while IFS= read -r line; do db_log "  $line"; done

    bad="$data_dir/spots.db.corrupt-$(date +%Y%m%d-%H%M%S)"
    # Keep only the most recent corrupt copy so repeated failures can't fill the disk.
    rm -f "$data_dir"/spots.db.corrupt-*
    mv "$db" "$bad"
    for ext in -wal -shm; do
        [ -e "$db$ext" ] && mv "$db$ext" "$bad$ext"
    done
    db_log "moved damaged database to $bad"

    tmp="$db.recovering"
    rm -f "$tmp" "$tmp-wal" "$tmp-shm" "$tmp-journal"
    db_log "recovering readable data (this can take several minutes for a large DB)"
    start=$(date +%s)
    errlog="$data_dir/recover.log"
    rows=""
    if sqlite3 "$bad" ".recover --ignore-freelist" 2>"$errlog" | sqlite3 -bail "$tmp" >>"$errlog" 2>&1 \
        && ! grep -q "error" "$errlog" \
        && sqlite3 "$tmp" "DROP TABLE IF EXISTS lost_and_found;" >>"$errlog" 2>&1 \
        && [ "$(sqlite3 "$tmp" "PRAGMA quick_check;" 2>&1)" = "ok" ]; then
        rows=$(sqlite3 "$tmp" "SELECT count(*) FROM spots;" 2>/dev/null || true)
    fi
    case "$rows" in
        ''|*[!0-9]*)
            rm -f "$tmp" "$tmp-wal" "$tmp-shm" "$tmp-journal"
            db_log "recovery FAILED — starting with an empty database; damaged copy kept at $bad"
            head -5 "$errlog" 2>/dev/null | while IFS= read -r line; do db_log "  $line"; done ;;
        *)
            mv "$tmp" "$db"
            rm -f "$errlog"
            db_log "recovered $rows spots in $(( $(date +%s) - start ))s; damaged copy kept at $bad (safe to delete)" ;;
    esac
}

if [ "$DB_CHECK" != "false" ] && [ "$DB_CHECK" != "0" ]; then
    db_check_and_repair
fi

args=""

[ -n "$UBERSDR_URL"  ] && args="$args -url $UBERSDR_URL"
[ -n "$SPOTTER_CALL" ] && args="$args -spotter $SPOTTER_CALL"
[ -n "$SSTV_URL"     ] && args="$args -sstv-url $SSTV_URL"

# REQUIRE_LOGIN: pass -require-login=false if explicitly disabled
if [ "$REQUIRE_LOGIN" = "false" ] || [ "$REQUIRE_LOGIN" = "0" ]; then
    args="$args -require-login=false"
fi

# WEB_PORT → -listen :<port>
if [ -n "$WEB_PORT" ]; then
    args="$args -listen :$WEB_PORT"
else
    args="$args -listen :6087"
fi

# TELNET_PORT → -telnet :<port>
if [ -n "$TELNET_PORT" ]; then
    args="$args -telnet :$TELNET_PORT"
else
    args="$args -telnet :7300"
fi

# Append any CLI args passed directly to the container
# shellcheck disable=SC2086
exec /usr/local/bin/ubersdr_dxcluster $args "$@"
