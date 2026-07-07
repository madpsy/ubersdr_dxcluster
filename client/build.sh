#!/usr/bin/env bash
#
# Build the UberSDR DX Cluster client for Linux and Windows (both amd64).
#
# Fyne needs CGO, so the Windows build cross-compiles with the mingw-w64
# toolchain (x86_64-w64-mingw32-gcc). Binaries are written to ./dist.
#
# Usage:
#   ./build.sh              # build both targets
#   ./build.sh linux        # Linux only
#   ./build.sh windows      # Windows only

set -euo pipefail

cd "$(dirname "$0")"

APP="ubersdr-dxcluster-client"
DIST="dist"
MINGW_CC="x86_64-w64-mingw32-gcc"
MINGW_WINDRES="x86_64-w64-mingw32-windres"

mkdir -p "$DIST"

build_linux() {
  echo "==> Building Linux amd64…"
  CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build -trimpath -o "$DIST/${APP}-linux-amd64" .
  echo "    $DIST/${APP}-linux-amd64"
}

build_windows() {
  echo "==> Building Windows amd64…"
  if ! command -v "$MINGW_CC" >/dev/null 2>&1; then
    echo "ERROR: $MINGW_CC not found — install mingw-w64 to cross-compile for Windows." >&2
    exit 1
  fi
  if ! command -v "$MINGW_WINDRES" >/dev/null 2>&1; then
    echo "ERROR: $MINGW_WINDRES not found — install mingw-w64 (provides windres) for the .exe icon." >&2
    exit 1
  fi
  # Compile the Windows resource so Go links the app icon into the .exe.
  # The _windows_amd64 suffix scopes it to the Windows build so it never
  # interferes with the Linux build. Regenerated each time; it is gitignored.
  echo "    compiling resource (icon)…"
  "$MINGW_WINDRES" resource.rc -O coff -o resource_windows_amd64.syso
  # -H windowsgui suppresses the console window for the GUI app.
  CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC="$MINGW_CC" \
    go build -trimpath -ldflags "-H windowsgui" -o "$DIST/${APP}-windows-amd64.exe" .
  echo "    $DIST/${APP}-windows-amd64.exe"
}

target="${1:-all}"
case "$target" in
  linux)   build_linux ;;
  windows) build_windows ;;
  all)     build_linux; build_windows ;;
  *)
    echo "Usage: $0 [linux|windows|all]" >&2
    exit 2
    ;;
esac

echo "Done."
