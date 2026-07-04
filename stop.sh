#!/usr/bin/env bash
# stop.sh — stop the ubersdr_dxcluster service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/dxcluster"

cd "${INSTALL_DIR}"
echo "Stopping ubersdr_dxcluster..."
docker compose down
echo "Done."
