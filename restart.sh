#!/usr/bin/env bash
# restart.sh — restart the ubersdr_dxcluster service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/dxcluster"

cd "${INSTALL_DIR}"
echo "Stopping ubersdr_dxcluster..."
docker compose down
echo "Starting ubersdr_dxcluster..."
docker compose up -d --remove-orphans
echo "Done."
echo "  View logs : docker compose logs -f"
echo "  Web UI    : http://localhost:6087"
