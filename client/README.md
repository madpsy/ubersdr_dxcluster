# UberSDR DX Cluster Client

A small, cross-platform (Linux / Windows) desktop client whose only job is to
connect to an UberSDR instance's **DX Cluster** add-on and re-serve its spot
stream to local telnet clients.

It is the DX-cluster-only counterpart of the UberSDR Python client's *DX
Cluster Terminal* window — no audio, no waterfall, nothing but DX spots. Built
with [Fyne](https://fyne.io).

## What it does

1. **Lists instances** — fetches the public instance directory
   (`https://instances.ubersdr.org/api/instances`) and shows only the receivers
   that advertise the `dxcluster` add-on.
2. **Connects** — opens the chosen instance's DX cluster WebSocket terminal
   (`wss://<host>/addon/dxcluster/api/terminal`), logs in with your callsign,
   and streams the live spots.
3. **Re-serves over telnet** — listens on `0.0.0.0:<port>` (default **7300**)
   and bridges the stream to any standard DX cluster telnet consumer (logging
   software, a terminal, etc.). Commands typed by a telnet client are forwarded
   upstream.

The WebSocket connection **auto-reconnects** with exponential backoff (1 s →
doubling → capped at 10 s), resetting after any session that connected.

Your **callsign** and **telnet port** are remembered between runs via Fyne's
cross-platform preferences store (Linux `~/.config/fyne/<app-id>/`, Windows
`%APPDATA%\fyne\<app-id>\`, macOS `~/Library/Preferences/<app-id>/`).

Tick **Auto-connect on startup** (next to the instance name) to have the app
connect to the current instance automatically next launch. The instance is
saved by its **UUID**, so it's resolved against the directory at startup — a
changed hostname or port doesn't matter. If that instance isn't online when you
launch, the picker opens instead.

**Self-targeting by filename:** if you rename the binary with a callsign after
an underscore — e.g. `dxcluster_m9psy` or `dxcluster_m9psy.exe` — it connects to
the instance whose callsign matches on launch. This is a fallback only: a saved
**Auto-connect on startup** preference always takes priority and is never
overridden by the filename. If no instance matches the filename callsign, the
app opens the picker as usual.

## Using it

1. Launch the app. The **instance picker** opens automatically once the
   directory loads.
2. Search (real-time filter across **callsign**, **name** and **location**),
   then **double-click a row** (or select it and press **Select**).
3. Enter your **callsign**, optionally change the **telnet port**, and press
   **Connect**.
4. The spot stream fills the window; type cluster commands (`SH/DX`, `HELP`,
   `BYE`, …) in the input box beneath it and press Enter.
5. Point your logging/telnet software at `0.0.0.0:<port>` (or `localhost:7300`)
   — it mirrors the same session, so commands from either side share one
   upstream connection.
6. Press **Choose Instance…** at any time to switch — if you're connected the
   upstream swaps to the new instance in one click and your **telnet clients
   stay connected** across the switch. Pressing **Disconnect** (or closing the
   app) does drop the telnet clients.

## Building

Fyne needs CGO. The Linux build uses the system GCC; the Windows build
cross-compiles with the `mingw-w64` toolchain (`x86_64-w64-mingw32-gcc`).

```sh
./build.sh           # both targets → dist/
./build.sh linux     # Linux only
./build.sh windows   # Windows only
```

Outputs:

- `dist/ubersdr-dxcluster-client-linux-amd64`
- `dist/ubersdr-dxcluster-client-windows-amd64.exe`

### Prerequisites

- Go 1.25+
- Linux: GCC and the usual Fyne dev libs (`libgl1-mesa-dev xorg-dev`)
- Windows cross-build: `mingw-w64` (its `windres` embeds the app icon
  `ubersdr.ico` into the `.exe`)

The window/taskbar icon is embedded in the binary on every OS via
`//go:embed ubersdr.ico`; the Windows `.exe` file icon is added by compiling
`resource.rc` to `resource_windows_amd64.syso` during the Windows build.

## Tests

`integration_test.go` verifies the whole non-GUI pipeline end to end (fetch →
WebSocket login → telnet re-broadcast) against the stable `m9psy` instance.
Requires network access:

```sh
go test -run TestEndToEnd -v      # live network test
go test -short ./...              # skip the network test
```

## Layout

| File                  | Responsibility                                         |
|-----------------------|--------------------------------------------------------|
| `instances.go`        | Fetch & filter the instance directory; build WS URLs   |
| `wsclient.go`         | DX cluster WebSocket client, auto-login, reconnect      |
| `telnetlistener.go`   | Local `0.0.0.0:<port>` telnet listener / bridge         |
| `main.go`             | Fyne UI: instance picker, controls, spot-stream view    |
