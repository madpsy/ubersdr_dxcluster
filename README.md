# UberSDR DX Cluster

A full-featured DX cluster add-on for [UberSDR](https://ubersdr.org) that turns your SDR receiver into a live spot source for the amateur radio community. It ships in two parts: the **server add-on** that runs alongside UberSDR, and a lightweight **desktop client** that any operator can download and use to connect to any UberSDR DX Cluster instance.

---

## Table of Contents

- [What is it?](#what-is-it)
  - [The Add-on (Server)](#the-add-on-server)
  - [The Desktop Client](#the-desktop-client)
- [Installation](#installation)
  - [Prerequisites](#prerequisites)
  - [Install via UberSDR Admin Interface](#install-via-ubersdr-admin-interface)
  - [Manual / Docker Install](#manual--docker-install)
- [Configuration](#configuration)
  - [Environment Variables](#environment-variables)
- [Web UI](#web-ui)
- [Statistics & Analysis](#statistics--analysis)
  - [Stats API](#stats-api)
- [Telnet Interface](#telnet-interface)
  - [Connecting with Logging Software](#connecting-with-logging-software)
  - [Command Reference](#command-reference)
- [Desktop Client](#desktop-client)
  - [Getting the Client](#getting-the-client)
  - [Using the Client](#using-the-client)
  - [Instance Discovery](#instance-discovery)
  - [Auto-connect](#auto-connect)
  - [Audio Client Integration](#audio-client-integration)
  - [Building the Client](#building-the-client)
- [Spot Streams](#spot-streams)
- [Architecture](#architecture)
- [API Endpoints](#api-endpoints)
- [Development](#development)
- [License](#license)

---

## What is it?

### The Add-on (Server)

The UberSDR DX Cluster add-on is a Go service that runs as a Docker container alongside your UberSDR instance. It subscribes to UberSDR's internal Server-Sent Events (SSE) streams — digital decoder, CW skimmer, voice activity, and upstream DX cluster — and re-publishes them in the standard **AR-Cluster / DX Spider telnet format** that every piece of amateur radio logging software understands.

In plain terms: once installed, your UberSDR receiver becomes a DX cluster node. Any logging program (Log4OM, DXKeeper, N1MM+, Ham Radio Deluxe, etc.) can connect to it on port 7300 and receive a live stream of spots decoded directly from the air by your SDR.

**What it provides:**

- **Live spot streaming** over standard DX cluster telnet (port 7300) — compatible with all major logging software
- **Web UI** — a browser-based terminal with real-time spot display, filtering, and a built-in telnet command interface
- **WebSocket terminal** — the same telnet session accessible over a secure WebSocket, used by the desktop client and the web UI
- **Persistent spot history** — up to 30 days of spots stored in a local SQLite database, queryable with `SHOW/DX`
- **DXCC lookups** — `SHOW/PREFIX`, `SHOW/HEADING`, `SHOW/DXCC` powered by the built-in CTY database
- **QRZ callbook lookups** — `SHOW/QRZ` via UberSDR's QRZ integration
- **Rich filtering** — DX Spider-compatible `ACCEPT/SPOTS` and `REJECT/SPOTS` filter slots, plus simple `SET/FILTER` commands for band, mode, continent, country, callsign prefix, and SNR

### The Desktop Client

The desktop client is a small, cross-platform (Linux / Windows) GUI application that connects to any UberSDR DX Cluster instance and re-serves its spot stream to local telnet clients on your machine.

Think of it as a bridge: the client connects to the UberSDR DX Cluster over a secure WebSocket, then listens on `localhost:7300` so your logging software can connect to it as if it were a local DX cluster node — even when the UberSDR instance is on the other side of the internet.

**What it provides:**

- **Instance browser** — lists all public UberSDR instances with the DX Cluster add-on enabled, plus local network discovery via mDNS
- **Local telnet bridge** — re-serves the spot stream on `0.0.0.0:7300` (configurable) for logging software
- **Built-in terminal** — view the live spot stream and type cluster commands directly in the app
- **Audio client integration** — double-click any spot to tune the UberSDR audio client to that frequency and mode
- **Persistent preferences** — callsign, port, and auto-connect settings saved between sessions
- **Seamless instance switching** — swap to a different UberSDR instance in one click; telnet clients stay connected

---

## Installation

### Prerequisites

- [UberSDR](https://ubersdr.org) installed and running
- Docker and Docker Compose v2

### Install via UberSDR Admin Interface

> **This is the recommended installation method.**

1. Log in to your UberSDR instance and open the **Admin** interface.
2. Go to the **Addon** tab and click **Manage Addons**.
3. Find **DX Cluster** in the addon list and click **Install**.
4. UberSDR will pull the Docker image, configure the proxy, and start the service automatically.
5. The web UI will be available at `http://your-ubersdr-host/addon/dxcluster/`.

### Manual / Docker Install

If you prefer to install manually or need more control over the configuration:

```bash
curl -fsSL https://raw.githubusercontent.com/madpsy/ubersdr_dxcluster/main/install.sh | bash
```

Or with force-update to overwrite an existing installation:

```bash
curl -fsSL https://raw.githubusercontent.com/madpsy/ubersdr_dxcluster/main/install.sh | FORCE_UPDATE=1 bash
```

The script:
1. Creates `~/ubersdr/dxcluster/` and downloads `docker-compose.yml`
2. Downloads helper scripts (`start.sh`, `stop.sh`, `restart.sh`, `update.sh`)
3. Creates the data directory (`dxcluster_data/`) for the SQLite database
4. Pulls the Docker image and starts the service

After installation, configure the UberSDR reverse proxy so the web UI is accessible through UberSDR's interface:

| Setting       | Value        |
|---------------|--------------|
| Name          | `dxcluster`  |
| Host          | `dxcluster`  |
| Port          | `6087`       |
| Enabled       | `true`       |
| Strip prefix  | `true`       |
| Rate Limit    | `100`        |

The web UI will then be available at `http://your-ubersdr-host/addon/dxcluster/`.

**Helper scripts** (installed to `~/ubersdr/dxcluster/`):

```bash
./start.sh      # Start the service
./stop.sh       # Stop the service
./restart.sh    # Restart the service
./update.sh     # Pull the latest image and restart
```

---

## Configuration

Edit `~/ubersdr/dxcluster/docker-compose.yml` and set the environment variables you need, then run `./restart.sh`.

### Environment Variables

| Variable              | Default                    | Description |
|-----------------------|----------------------------|-------------|
| `UBERSDR_URL`         | `http://ubersdr:8080`      | Base HTTP URL of your UberSDR instance. The add-on fetches SSE streams from this address. |
| `SPOTTER_CALL`        | *(from `/api/description`)* | Callsign shown as the spotter in telnet spot lines (e.g. `DX de M9PSY-#:`). Defaults to the receiver's callsign fetched from UberSDR at startup. |
| `WEB_PORT`            | `6087`                     | Port the web UI listens on inside the container. |
| `TELNET_PORT`         | `7300`                     | Port the DX cluster telnet server listens on. Exposed to the host via `ports:` in `docker-compose.yml`. |
| `REQUIRE_LOGIN`       | `true`                     | Require a valid amateur callsign on telnet connect. Set to `false` to allow anonymous connections. |
| `DATA_DIR`            | `/data`                    | Directory inside the container for the SQLite spot database. Mapped to `./dxcluster_data` on the host. |
| `RETENTION_DAYS`      | `30`                       | Days of spot history to keep. Older spots are purged daily and the database is compacted. |
| `VOICE_DEDUP_MINS`    | `10`                       | Deduplication window (minutes) for voice spots stored to the database. Prevents the same signal being stored repeatedly. Set to `0` to disable. Real-time streaming is unaffected. |
| `DECODER_DEDUP_MINS`  | `5`                        | Deduplication window (minutes) for digital decoder spots stored to the database. Set to `0` to disable. |
| `WS_MAX_CONNS`        | `25`                       | Maximum simultaneous WebSocket terminal sessions across all IPs. |
| `WS_MAX_CONNS_PER_IP` | `2`                        | Maximum simultaneous WebSocket terminal sessions from a single IP address. |

---

## Web UI

The web UI is served at `http://your-ubersdr-host/addon/dxcluster/` (or directly at `http://your-host:6087/` if accessing without the UberSDR proxy).

**Features:**

- **Live spot stream** — spots appear in real time as they are decoded, colour-coded by stream type (Digital, CW, Voice, DX Cluster)
- **Stream status indicators** — status pills in the header show which upstream streams (Digital, CW, Voice, DX) are connected
- **Filtering** — filter the displayed spots by band, mode, stream type, continent, and country without affecting the telnet stream
- **Built-in terminal** — a full DX cluster telnet session in the browser; type commands and see responses inline
- **Spot history** — the page loads recent spots from the database on connect so you see activity immediately
- **Telnet connection info** — the header shows the telnet address so you can quickly configure your logging software
- **Desktop client download** — a download button serves the correct client binary for your OS, pre-named with this instance's callsign for automatic targeting
- **Command reference** — a searchable help modal with the full telnet command reference
- **Statistics dashboard** — a 📊 Stats link in the header opens the analysis pages described below

---

## Statistics & Analysis

The statistics dashboard lives at `/stats` (linked from the header of the main
web UI). It analyses the spot database — every stream, every field — behind one
shared filter row, so a question like *"when is the best time to hear Germany on
40m?"* is a couple of dropdowns rather than a SQL query.

**Filters** (applied to every chart and table on every tab): time period or an
explicit UTC date range, band, mode, continent, country, source stream, callsign
prefix, spotter prefix, grid prefix, SNR range, distance range, UTC hour-of-day
window (which may wrap midnight), and a comment/message substring.

Note the difference between **Source** and **Mode**: Source is which stream
produced the spot (Digital decoder / CW skimmer / voice detector / upstream DX
cluster), Mode is the modulation within it. The mode picker is grouped by source
to make the relationship visible. Digital fans out into FT8, FT4, WSPR, JS8 and
FT2, and Voice into USB and LSB — but the CW skimmer only ever decodes CW, so
`source = CW` and `mode = CW` select the same spots today. They are not the same
question: if a spot with mode CW ever arrived from another source, `mode=CW`
would find it and `source=CW` would not. Upstream DX cluster spots carry no mode
field at all, so they are reachable by source but never by mode.

The **mode picker** is built from the cluster's own list of modes
(`StreamModes` in `spot.go`, served via `/api/stats/meta`), grouped by the
source that produces them — Digital (FT8, FT4, WSPR, JS8, FT2), CW, and Voice
(USB, LSB). Spot counts for the selected period are shown beside each mode, and
a mode with none is still offered, greyed as `(0)`, rather than vanishing from
the filter because it had a quiet week. Any mode present in the database but not
in that list — a new upstream decoder, or older history — is collected under
**Other**, so nothing in the data is unreachable. Band, continent and country
pickers are the opposite case and are driven purely by what the database holds.

**Tabs:**

- **Overview** — headline totals (spots, unique callsigns, countries, spotters,
  average SNR, furthest spot), activity over time on a real time axis, and
  breakdowns by source, country, band, mode and callsign. Sources are shown
  under their display names (Digital, CW, Voice, DX cluster) rather than the raw
  stream keys. The time chart's interval
  (hourly / daily / weekly, or automatic) and its split (total, or one line per
  band / mode / source / continent) are both selectable, so *"the last 24 hours
  of FT8 from Germany on 40m, by hour"* is the period plus three filters.
- **Best Time** — the propagation question, answered directly: the busiest UTC
  hour for the current filter stated in words, spot counts by hour, average SNR
  by hour, and an hour × band heatmap.
- **Rankings** — group by any dimension, rank by any metric, with the full
  metric set in a sortable table.
- **Spotters** — who is submitting spots. DX cluster, locally submitted and CW
  skimmer spots carry a spotter callsign (this receiver's own digital and voice
  decoders do not), so this tab scopes itself to those sources: unique spotter
  count, a leaderboard rankable by spots submitted / unique callsigns / countries
  / bands, and per-spotter first and last activity. It also answers **"who
  spotted this callsign?"** — enter a callsign for an exact-match list of every
  spot of it in the period, with the spotter, time, frequency, band, mode, SNR
  and comment, plus a per-spotter tally.

  **Click any spotter callsign to exclude it.** This receiver's own skimmer
  submits most of the spots on its own cluster and would otherwise top every
  ranking; excluding it shows everyone else. Excluded stations appear as badges
  in the Filters bar — click the `×` on a badge to bring one back, or *Clear
  exclusions* to drop them all. Exclusions apply to every tab and are remembered
  in the browser, since "not me" is a standing preference rather than a one-off.
- **Compare** — an arbitrary cross-tab: pick any two dimensions and a metric and
  read the result as a heatmap (hour × country, band × continent, weekday ×
  mode, …).
- **Spots** — the raw rows behind any aggregate, paged, with CSV download.

Every chart has a **Table** button showing the same numbers as text, and all
times are UTC. While a query is running the previous charts are held at reduced
opacity — never blanked, so nothing jumps when the new data lands — and a
spinner names what is being fetched ("Loading best-time analysis…"). It appears
only after ~120 ms, so quick queries don't flash, and it respects
`prefers-reduced-motion`.

### Stats API

Each tab is powered by `/api/stats/*`. Every endpoint accepts the **same filter
query string** and echoes the resolved filter back in its response, so a chart
and its drill-down can be driven from one URL.

| Endpoint | Description |
|----------|-------------|
| `GET /api/stats/meta` | Available dimensions, metrics, buckets, streams and **mode groups** — the UI builds its pickers from this |
| `GET /api/stats/facets` | Distinct values actually present under the filter (so a picker never offers a dead end) |
| `GET /api/stats/summary` | Headline totals for the filter, plus the busiest hour and band |
| `GET /api/stats/breakdown` | Group by one dimension (`group_by`), ranked by a metric (`sort`, `limit`) |
| `GET /api/stats/matrix` | Cross-tabulate two dimensions (`x`, `y`, `metric`, `limit_y`) |
| `GET /api/stats/series` | Time series (`bucket=hour\|day\|week`, `metric`), optionally split into one series per value of `split_by` |
| | Hourly and daily series are **gap-filled** across the whole window: quiet buckets come back as `0` for counting metrics and `null` for averages, so a chart shows the silence rather than interpolating over it |
| | A split series is capped at `max_series` (default 8, max 12). The remainder is **folded into an `Other` series** rather than discarded, so the total still accounts for every spot — folding only applies to `count`, the one metric that can legitimately be summed across series |
| `GET /api/stats/spots` | The raw filtered spots (`limit`, `offset`), newest first |

**Filter parameters** (shared by all of the above):

| Parameter | Meaning |
|-----------|---------|
| `days` / `hours` | Relative lookback; defaults to the last 24 hours, matching the dashboard |
| `from` / `to` | Absolute bounds — RFC3339, `YYYY-MM-DD`, or Unix seconds |
| `band`, `mode`, `stream`, `continent`, `country_code`, `country`, `cq_zone` | Multi-value (repeat the parameter or comma-separate); values OR within a parameter and AND across parameters |
| `callsign`, `spotter`, `locator` | Prefix match (a trailing `*` is accepted and ignored) |
| `callsign_exact`, `spotter_exact` | Exact match — use these to look a station up rather than browse a prefix, so `G3ABC` does not also return `G3ABCD` |
| `callsign_exclude`, `spotter_exclude` | Multi-value exclusions, removing those stations from every count and listing. Spots that have no spotter at all are kept |
| `q` | Substring of the spot comment or message |
| `snr_min` / `snr_max` | SNR bounds in dB |
| `dist_min` / `dist_max` | Distance bounds in km |
| `freq_min` / `freq_max` | Frequency bounds in kHz |
| `wpm_min` / `wpm_max` | CW speed bounds |
| `conf_min` | Minimum voice-detection confidence |
| `hour_min` / `hour_max` | UTC hour-of-day window, 0–23; wraps midnight when `hour_min > hour_max` |

**Dimensions** (`group_by`, `x`, `y`, `split_by`): `hour`, `weekday`, `date`,
`month`, `band`, `mode`, `stream`, `callsign`, `spotter`, `country`, `cc`,
`continent`, `cq_zone`, `locator`, `field`, `wpm`, `voice_mode`, `snr_bucket`,
`dist_bucket`, `freq_bucket`.

**Metrics** (`metric`, `sort`): `count`, `calls`, `countries`, `spotters`,
`bands`, `avg_snr`, `max_snr`, `avg_dist`, `max_dist`, `avg_wpm`.

Distinct counts (`calls`, `countries`, `spotters`, `bands`) ignore empty values,
so spots from streams that don't populate a field — digital and voice spots have
no spotter — don't inflate the total.

Anything outside those whitelists is rejected with a `400` — only whitelisted
identifiers ever reach the SQL, and every filter value is bound as a parameter.

**Example** — the busiest UTC hours for Germany on 40m over the last 30 days:

```
GET /api/stats/breakdown?days=30&band=40m&country_code=DE&group_by=hour&sort=count
```

**Example** — an hour × band heatmap of average SNR for European CW spots:

```
GET /api/stats/matrix?days=14&continent=EU&mode=CW&x=hour&y=band&metric=avg_snr
```

**Example** — the last 24 hours of FT8 spots from Germany on 40m, bucketed by
hour (one point per hour, zeroes included):

```
GET /api/stats/series?hours=24&band=40m&mode=FT8&country_code=DE&bucket=hour
```

**Example** — the spotter leaderboard for the last week, busiest first, leaving
out this receiver's own skimmer:

```
GET /api/stats/breakdown?days=7&stream=dxcluster,localspot,cwskimmer&group_by=spotter&sort=count&spotter_exclude=M9PSY-%23
```

**Example** — who spotted G3ABC, and every spot of it with frequency and time:

```
GET /api/stats/breakdown?days=30&callsign_exact=G3ABC&group_by=spotter&sort=count
GET /api/stats/spots?days=30&callsign_exact=G3ABC
```

---

## Telnet Interface

The DX cluster telnet server listens on port **7300** (configurable). It speaks standard AR-Cluster / DX Spider protocol, so any software that can connect to a DX cluster will work.

### Connecting with Logging Software

Point your logging software at:

```
Host: your-ubersdr-host (or IP address)
Port: 7300
```

On first connect you will be prompted for your callsign. Enter it and press Enter. The server validates it against the standard amateur callsign format (DX Spider rules) and then begins streaming spots.

**Compatible software includes:** Log4OM, DXKeeper, N1MM+, Ham Radio Deluxe, DX4WIN, Logger32, and any other software that supports a standard DX cluster telnet connection.

### Command Reference

Commands can be abbreviated (e.g. `SH/DX` for `SHOW/DX`, `SET/F` for `SET/FILTER`).

#### Simple Filters

These filters are AND-combined. Multiple values within a field are OR-combined.

| Command | Description |
|---------|-------------|
| `set/filter band <bands>` | Filter by band, e.g. `set/filter band 20m` or `set/filter band 40m,20m,15m` |
| `set/filter mode <modes>` | Filter by mode: `FT8 FT4 WSPR JS8 FT2 CW USB LSB` |
| `set/filter type <types>` | Filter by stream type: `digital cw voice dx` |
| `set/filter cont <codes>` | Filter by continent: `EU NA SA AF AS OC AN` |
| `set/filter country <codes>` | Filter by ISO 3166-1 alpha-2 country code, e.g. `DE,PA,ON` |
| `set/filter call <prefixes>` | Filter by callsign prefix, e.g. `DL,VK,ZL` |
| `set/filter snr <dB>` | Minimum SNR threshold |
| `set/filter maxsnr <dB>` | Maximum SNR threshold |
| `clear/filter [field]` | Clear all filters, or a specific field |

#### DX Spider Accept/Reject Filters

Numbered slots (0–9) with full DX Spider boolean expression syntax:

| Command | Description |
|---------|-------------|
| `accept/spots [N] <expr>` | Accept spots matching expression in slot N (default 1) |
| `reject/spots [N] <expr>` | Reject spots matching expression in slot N (default 1) |
| `accept/rbn [N] <expr>` | Accept RBN/CW spots matching expression |
| `reject/rbn [N] <expr>` | Reject RBN/CW spots matching expression |
| `clear/spots [N\|all]` | Clear spot filter slot N or all slots |
| `clear/rbn [N\|all]` | Clear RBN filter slot N or all slots |

Filter expression fields: `on <band/freq>`, `call <prefix>`, `by <spotter>`, `cont <code>`, `country <code>`, `mode <mode>`, `type <type>`, `info <text>`, `iota`, `qsl`, `all`. Combine with `and`, `or`, `not`, and parentheses.

#### Stream Toggles

| Command | Description |
|---------|-------------|
| `set/digital` / `unset/digital` | Enable/disable digital decoder spots (FT8/FT4/WSPR/JS8) — default **off** |
| `set/rbn` / `unset/rbn` | Enable/disable CW/RBN skimmer spots — default **on** |
| `set/voice` / `unset/voice` | Enable/disable voice activity spots — default **on** |
| `set/dxcluster` / `unset/dxcluster` | Enable/disable upstream DX cluster spots — default **off** |
| `set/dx` / `unset/dx` | Enable/disable ALL spots |

#### Information Commands

| Command | Description |
|---------|-------------|
| `show/dx [N] [options]` | Query spot history (up to 30 days). Options: `on <band>`, `call <prefix>`, `by <spotter>`, `info <text>`, `iota`, `qsl`, `day <N>`, `cont <code>`, `country <code>`, `mode <mode>`, `type <type>` |
| `show/filter` | Show all currently active filters |
| `show/status` | Show cluster status: uptime, connected clients, database stats |
| `show/qrz <callsign>` | Look up callbook details via QRZ.com (through UberSDR) |
| `show/prefix <call\|pfx>` | Show DXCC country, CQ/ITU zone, lat/lon |
| `show/dxcc <prefix>` | Show recent spots for a DXCC country |
| `show/heading <call\|pfx>` | Show beam heading and distance from the receiver |
| `show/dxstats [days]` | Show spot totals per day |
| `show/hfstats [days]` | Show spot totals per band |
| `show/who` | List all connected clients with IP, callsign, and connect time |
| `show/time` | Show current UTC time |
| `show/version` | Show cluster software version |
| `help [command]` | Show the full command reference |
| `bye` / `quit` | Disconnect |

---

## Desktop Client

The desktop client is a standalone GUI application (built with [Fyne](https://fyne.io)) for Linux and Windows. It connects to any UberSDR DX Cluster instance over a secure WebSocket and re-serves the spot stream to local telnet clients on your machine — so your logging software connects to `localhost:7300` as if it were a local cluster node.

### Getting the Client

**From the web UI:** Click the **Download Client** button in the web UI. The server detects your OS and serves the correct binary, pre-named with the instance's callsign (e.g. `dxcluster_m9psy` or `dxcluster_m9psy.exe`) for automatic targeting on first launch.

**From the releases page:** Download the appropriate binary for your platform.

### Using the Client

1. **Launch the app.** The instance picker opens automatically once the directory loads.
2. **Choose an instance.** Search by callsign, name, or location. Double-click a row (or select it and press **Select**).
3. **Enter your callsign** and optionally change the telnet port (default: 7300). Press **Connect**.
4. The spot stream fills the window. Type cluster commands (`SH/DX`, `HELP`, `BYE`, etc.) in the input box at the bottom.
5. **Connect your logging software** to `localhost:7300` (or `0.0.0.0:7300` from another machine on your network). Commands from either side share the same upstream session.
6. Press **Choose Instance…** at any time to switch instances. If you are connected, the upstream swaps seamlessly — your telnet clients stay connected across the switch.

**Status bar:** A status dot (red = disconnected, green = connected) and a status message show the current connection state. The telnet label shows how many local clients are connected.

**Right-click a spot line** for a context menu with quick commands: `SHOW/QRZ`, `SHOW/DX`, `SHOW/HEADING`, `SHOW/DXCC`, band filter shortcuts, stream enable/disable toggles, clipboard copy, and online lookups (QRZ.com, PSKReporter, Clublog, DXHeat).

**Startup Commands:** Use the **Commands…** button to enter commands sent automatically after login — one per line (e.g. `set/digital`, `set/filter band 20m`).

### Instance Discovery

The client discovers instances in two ways:

- **Public directory** — fetches `https://instances.ubersdr.org/api/instances` and shows only receivers that advertise the `dxcluster` add-on. Searchable by callsign, name, and location.
- **Local network (mDNS)** — browses for `_ubersdr._tcp` services on the local network and probes each one's `/api/description` to confirm the DX Cluster add-on is installed. Local instances appear in a separate section at the top of the picker.

### Auto-connect

Tick **Auto-connect on startup** (next to the instance name) to have the app connect to the current instance automatically on the next launch.

- For **public instances**, the instance UUID is saved — so a changed hostname or port doesn't matter; it's resolved from the directory at startup.
- For **local (mDNS) instances**, the callsign and last-known address are saved. On startup the client first tries the last-known address directly (fast path), then falls back to an 8-second mDNS browse if that fails.
- If the saved instance is offline at startup, the picker opens instead.

**Self-targeting by filename:** If you rename the binary with a callsign after an underscore — e.g. `dxcluster_m9psy` or `dxcluster_m9psy.exe` — it connects to the matching instance automatically on first launch and ticks Auto-connect for you. A saved Auto-connect preference always takes priority over the filename.

### Audio Client Integration

The client can integrate with the **UberSDR Audio Client** (the separate audio streaming application):

- **Spot tuning:** Enable *Audio Client tuning* in the **Audio Client…** dialog. Double-clicking any CW, SSB, or voice spot line tunes the audio client to that frequency and mode.
- **Auto-connect:** Enable *Auto connect to this instance in Audio Client* to have the audio client automatically connect to the same UberSDR instance when you connect the DX cluster client.
- The audio client API address defaults to `127.0.0.1:9770`. Use the **Test Connection** button to verify connectivity.

### Building the Client

Fyne requires CGO. The Linux build uses the system GCC; the Windows build cross-compiles with the `mingw-w64` toolchain.

```bash
cd client
./build.sh           # both targets → dist/
./build.sh linux     # Linux only
./build.sh windows   # Windows only
```

**Outputs:**
- `dist/ubersdr-dxcluster-client-linux-amd64`
- `dist/ubersdr-dxcluster-client-windows-amd64.exe`

**Prerequisites:**
- Go 1.21+
- Linux: GCC and Fyne dev libs (`libgl1-mesa-dev xorg-dev`)
- Windows cross-build: `mingw-w64` (its `windres` embeds the app icon into the `.exe`)

---

## Spot Streams

The add-on subscribes to four upstream SSE streams from UberSDR and merges them into a single spot feed:

| Stream | Source | Default state | Description |
|--------|--------|---------------|-------------|
| `digital` | `/api/decoder/stream` | Off | FT8, FT4, WSPR, JS8, FT2 decodes from UberSDR's digital decoder |
| `cwskimmer` | `/api/cwskimmer/stream` | On | CW spots from UberSDR's CW skimmer (RBN-format) |
| `voice` | `/api/voice-activity/stream` | On | Voice activity detections from UberSDR's voice activity detector |
| `dxcluster` | `/api/dxcluster/stream` | Off | Spots relayed from an upstream DX cluster connected to UberSDR |

Each stream reconnects automatically with exponential back-off (1 s → doubling → capped at 60 s) if the upstream connection drops.

Spots are formatted in standard DX cluster line format:

```
DX de M9PSY-#:    14033.0  R6AU           13 dB  23 WPM  CQ            1701Z
DX de DK8MM:     144337.0  DM4KCS/P       JO53 > JO30JM                1748Z
```

- **Own skimmer spots** (digital/CW/voice): the receiver's callsign with `-#` suffix is used as the spotter (RBN convention).
- **Upstream DX cluster spots**: the original spotter callsign is preserved.

---

## Architecture

```
UberSDR instance
  ├── /api/decoder/stream      (SSE)  ─┐
  ├── /api/cwskimmer/stream    (SSE)  ─┤
  ├── /api/voice-activity/stream (SSE)─┤─→ SSE Consumers → Hub → ┬→ Telnet Server (:7300)
  └── /api/dxcluster/stream   (SSE)  ─┘                          ├→ Web SSE relay (/api/events)
                                                                   ├→ WebSocket terminal (/api/terminal)
                                                                   └→ SQLite store (spots.db)
```

**Key components:**

| Component | File(s) | Description |
|-----------|---------|-------------|
| SSE consumers | `sse_consumer.go` | Subscribe to UberSDR's four SSE streams; parse JSON events into `Spot` structs; reconnect with back-off |
| Hub | `hub.go` | In-memory pub/sub broker; distributes spots to all subscribers (telnet clients, SSE relay, WebSocket terminal) |
| Spot store | `store.go` | SQLite-backed persistent store; deduplication; daily purge of old spots; `SHOW/DX` query engine |
| Telnet server | `telnet.go`, `commands.go` | DX Spider-compatible telnet server; callsign login; per-client filter state; command handling |
| Web server | `web.go` | Serves the web UI, SSE relay, WebSocket terminal proxy, REST endpoints, and client binary downloads |
| Terminal proxy | `terminal.go` | Bridges WebSocket connections to the telnet server's connection handler |
| CTY database | `cty.go` | Built-in DXCC country database for prefix/heading/zone lookups |
| Filter engine | `filter.go` | DX Spider-compatible `ACCEPT/REJECT` filter parser and evaluator |

---

## API Endpoints

The web server exposes the following HTTP endpoints (all accessible through the UberSDR proxy at `/addon/dxcluster/`):

| Endpoint | Description |
|----------|-------------|
| `GET /` | Web UI (HTML) |
| `GET /api/events` | Server-Sent Events relay — streams live spots as JSON |
| `GET /api/terminal` | WebSocket terminal — bidirectional DX cluster session |
| `GET /api/spots?stream=<type>` | REST spot history (optional `stream` filter: `decoder`, `cwskimmer`, `voice`, `dxcluster`) |
| `GET /api/status` | JSON status: uptime, telnet client count and list, available streams |
| `GET /api/help` | Plain-text telnet command reference |
| `GET /api/countries` | JSON list of DXCC countries (for the web UI country filter) |
| `GET /stats` | Statistics dashboard (HTML) |
| `GET /api/stats/*` | Analytics API — see [Stats API](#stats-api) |
| `GET /clients/dxcluster` | Desktop client binary (Linux) |
| `GET /clients/dxcluster.exe` | Desktop client binary (Windows) |
| `GET /client/download` | OS-detected client download, named after the instance callsign |

---

## Development

### Building the Add-on

```bash
go build ./...
```

### Running Locally

```bash
go run . -url http://your-ubersdr:8080 -listen :6087 -telnet :7300
```

### Tests

```bash
go test ./...                    # all tests
go test -run TestFilter ./...    # filter engine tests
go test -run TestWebClients ./.. # web client tests
```

### Client Tests

```bash
cd client
go test -short ./...             # skip network tests
go test -run TestEndToEnd -v     # live end-to-end test (requires network)
```

### Docker Build

```bash
./docker.sh          # build and push the Docker image
```

---

## License

See [LICENSE.TXT](LICENSE.TXT).
