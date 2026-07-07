# syntax=docker/dockerfile:1
# ---------------------------------------------------------------------------
# Stage 0: build the desktop clients (Fyne, Linux + Windows amd64) from source
# ---------------------------------------------------------------------------
# Pinned to linux/amd64 so the shipped desktop binaries are always amd64, even
# when the runtime image is built for arm64 or as a multi-platform manifest.
FROM --platform=linux/amd64 golang:1.25-bookworm AS client-builder

# Fyne (Linux) needs OpenGL/X11 dev headers; the Windows build cross-compiles
# with the mingw-w64 toolchain (gcc + windres for the .exe icon).
RUN apt-get update && apt-get install -y --no-install-recommends \
        gcc \
        libgl1-mesa-dev xorg-dev \
        gcc-mingw-w64-x86-64 binutils-mingw-w64-x86-64 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src/client
COPY client/go.mod client/go.sum ./
RUN go mod download
COPY client/ ./
# Produces dist/ubersdr-dxcluster-client-{linux-amd64,windows-amd64.exe}
RUN chmod +x build.sh && ./build.sh

# ---------------------------------------------------------------------------
# Stage 1: build ubersdr_dxcluster Go binary
# ---------------------------------------------------------------------------
FROM golang:1.25-bookworm AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /out/ubersdr_dxcluster ./...

# ---------------------------------------------------------------------------
# Stage 2: minimal runtime image
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        wget \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -s /bin/false dxcluster

COPY --from=go-builder /out/ubersdr_dxcluster /usr/local/bin/ubersdr_dxcluster

# Desktop client binaries, served for download by the web UI at /clients/.
# Renamed to the defaults the web UI links to (dxcluster / dxcluster.exe).
COPY --from=client-builder /src/client/dist/ubersdr-dxcluster-client-linux-amd64 \
        /usr/local/share/dxcluster/clients/dxcluster
COPY --from=client-builder /src/client/dist/ubersdr-dxcluster-client-windows-amd64.exe \
        /usr/local/share/dxcluster/clients/dxcluster.exe

# Copy entrypoint script (translates env vars to ubersdr_dxcluster flags)
COPY entrypoint.sh /usr/local/bin/entrypoint.sh

# Create the default data directory and ensure the dxcluster user owns it.
# Note: no VOLUME declaration — the docker-compose.yml bind mount handles persistence.
# A VOLUME declaration would cause Docker to create a root-owned anonymous volume
# that overwrites the chown, preventing the dxcluster user from writing to /data.
RUN chmod +x /usr/local/bin/entrypoint.sh \
    && mkdir -p /data \
    && chown dxcluster:dxcluster /data \
    && chmod 755 /data

USER dxcluster

# Expose the web UI port and DX cluster telnet port
EXPOSE 6087
EXPOSE 7300

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/bin/wget", "-q", "-O", "/dev/null", "http://localhost:6087/"]

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
