#!/usr/bin/env bash
# start.sh — start the ubersdr_dxcluster service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/dxcluster"

cd "${INSTALL_DIR}"
echo "Starting ubersdr_dxcluster..."
docker compose up -d --remove-orphans
echo "Done."
echo "  View logs : docker compose logs -f"
echo "  Web UI    : http://localhost:6087"
