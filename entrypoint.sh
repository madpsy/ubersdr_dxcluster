#!/bin/sh
# entrypoint.sh — translate environment variables into ubersdr_dxcluster flags
#
# Environment variables:
#   UBERSDR_URL    UberSDR base HTTP URL (default: http://ubersdr:8080)
#   WEB_PORT       Port for the web UI server (default: 6087)
#   TELNET_PORT    Port for the DX cluster telnet server (default: 7300)
#   SPOTTER_CALL   Callsign shown as spotter for digital/voice spots
#                  (default: fetched from /api/description at startup)
#
# Note: The URL base path for reverse-proxy is read automatically from the
#       X-Forwarded-Prefix request header — no configuration needed.

set -e

args=""

[ -n "$UBERSDR_URL"  ] && args="$args -url $UBERSDR_URL"
[ -n "$SPOTTER_CALL" ] && args="$args -spotter $SPOTTER_CALL"

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
