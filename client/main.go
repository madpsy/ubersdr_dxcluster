// Command ubersdr-dxcluster-client is a cross-platform desktop client that
// lists every UberSDR instance running the DX cluster add-on, connects to the
// one you choose over its DX cluster WebSocket terminal, and re-serves that
// spot stream to local telnet clients on 0.0.0.0:<port> (default 7300).
//
// It is the DX-cluster-only counterpart of the UberSDR Python client's DX
// Cluster Terminal window: no audio, no waterfall, nothing but DX spots.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"image/color"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

//go:embed ubersdr.ico
var appIcon []byte

const defaultTelnetPort = "7300"

// titleDisconnected is the window title when no session is active.
const titleDisconnected = "UberSDR DX Cluster"

// Preference keys (persisted per-OS by Fyne under the app ID).
const (
	prefCallsign    = "callsign"
	prefTelnetPort  = "telnet_port"
	prefAutoConnect = "auto_connect_uuid" // instance UUID / "local" sentinel / "none"
	prefStartupCmds = "startup_commands"  // newline-separated commands sent after login

	// prefAutoConnectLocal* are only written when the last auto-connect target
	// was a local (mDNS-discovered) instance. prefAutoConnect is set to the
	// sentinel value "local" in that case.
	prefAutoConnectLocalCallsign = "auto_connect_local_callsign" // e.g. "MM3NDH"
	prefAutoConnectLocalAddr     = "auto_connect_local_addr"     // e.g. "192.168.1.4:8080"

	// autoConnectLocalSentinel is stored in prefAutoConnect to signal that the
	// last chosen instance was local (not in the public directory).
	autoConnectLocalSentinel = "local"

	// Audio client integration (ubersdr-audio HTTP API).
	prefAudioEnabled     = "audio_tune_enabled" // bool — spot-tune feature on/off
	prefAudioHost        = "audio_host"         // e.g. "127.0.0.1"
	prefAudioPort        = "audio_port"         // e.g. "9770"
	prefAudioAutoConnect = "audio_auto_connect" // bool — connect audio client to the same instance

	// flrig integration (XML-RPC sync with flrig transceiver control).
	prefFlrigEnabled   = "flrig_enabled"   // bool — flrig sync on/off
	prefFlrigHost      = "flrig_host"      // e.g. "127.0.0.1"
	prefFlrigPort      = "flrig_port"      // e.g. "12345"
	prefFlrigDirection = "flrig_direction" // "sdr-to-rig" | "rig-to-sdr" | "both"
)

func main() {
	a := app.NewWithID("org.ubersdr.dxcluster.client")
	a.SetIcon(fyne.NewStaticResource("ubersdr.ico", appIcon))
	w := a.NewWindow(titleDisconnected)
	w.Resize(fyne.NewSize(960, 720))

	ui := newAppUI(w, a.Preferences())
	w.SetContent(ui.build())
	w.SetOnClosed(func() {
		ui.disconnect()
		ui.stopFlrig()
	})

	ui.startupPending = true
	ui.refresh() // initial fetch; then auto-connects or opens the picker
	w.ShowAndRun()
}

// appUI holds all widget references and the live connection state.
type appUI struct {
	win   fyne.Window
	prefs fyne.Preferences

	instances      []Instance // all fetched dxcluster instances
	current        *Instance  // the chosen instance, or nil
	startupPending bool       // run the startup action after the first fetch

	// updatingAutoCheck suppresses the auto-connect checkbox's OnChanged while
	// we set its state programmatically.
	updatingAutoCheck bool

	// client is the current upstream session. It is swapped on the UI thread
	// but read by the telnet listener's goroutines, so it is atomic. The
	// listener itself outlives an instance switch and is UI-thread-owned.
	client   atomic.Pointer[DXClusterClient]
	listener *TelnetListener

	instanceLabel  *widget.Label
	connDot        *canvas.Circle
	connDotBox     fyne.CanvasObject
	callsign       *widget.Entry
	portEntry      *widget.Entry
	connectBtn     *widget.Button
	chooseBtn      *widget.Button
	webBtn         *widget.Button
	dxBtn          *widget.Button
	autoCheck      *widget.Check
	helpBtn        *widget.Button
	audioTuneBtn   *widget.Button
	flrigBtn       *widget.Button
	startupCmdsBtn *widget.Button
	inputEntry     *widget.Entry
	sendBtn        *widget.Button
	statusLabel    *widget.Label
	telnetLabel    *widget.Label
	audioLabel     *widget.Label // shows last audio-client tune result
	flrigLabel     *widget.Label // shows flrig sync status
	term           *terminalView

	flrig *FlrigSync // bidirectional freq/mode sync with flrig

	headers []string
}

func newAppUI(w fyne.Window, prefs fyne.Preferences) *appUI {
	ui := &appUI{
		win:     w,
		prefs:   prefs,
		headers: []string{"Callsign", "Name", "Location"},
	}

	// Initialise flrig sync engine and apply saved preferences.
	ui.flrig = NewFlrigSync()
	ui.flrig.OnStatus = func(connected bool, msg string) {
		fyne.Do(func() {
			if ui.flrigLabel == nil {
				return
			}
			if connected {
				ui.flrigLabel.Importance = widget.SuccessImportance
				ui.flrigLabel.SetText("flrig ✓")
			} else {
				ui.flrigLabel.Importance = widget.MediumImportance
				if msg == "Disabled" {
					ui.flrigLabel.SetText("")
				} else {
					ui.flrigLabel.SetText("flrig ✗")
				}
			}
		})
	}
	ui.flrig.Configure(
		prefs.StringWithFallback(prefFlrigHost, "127.0.0.1"),
		prefs.IntWithFallback(prefFlrigPort, 12345),
		prefs.StringWithFallback(prefFlrigDirection, "sdr-to-rig"),
		prefs.BoolWithFallback(prefFlrigEnabled, false),
	)
	ui.flrig.Start()

	return ui
}

func (u *appUI) build() fyne.CanvasObject {
	// ── Toolbar: instance choice + connection controls ─────────────────────
	u.chooseBtn = widget.NewButton("Choose Instance…", u.showInstancePicker)
	u.instanceLabel = widget.NewLabelWithStyle("— none —", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})

	// Connection status dot: red = disconnected, green = connected.
	// canvas.Circle has no MinSize; wrap it in a fixed-size container so the
	// HBox allocates the right amount of space for it.
	u.connDot = canvas.NewCircle(color.NRGBA{R: 180, G: 40, B: 40, A: 255})
	u.connDotBox = container.NewGridWrap(fyne.NewSize(12, 12), u.connDot)

	u.autoCheck = widget.NewCheck("Auto-connect on startup", func(checked bool) {
		if u.updatingAutoCheck {
			return
		}
		if checked {
			u.saveAutoConnect(u.current)
		} else {
			// Write "none" (not "") so we can distinguish "user explicitly
			// disabled" from "never configured" on the next launch.
			u.prefs.SetString(prefAutoConnect, "none")
			// Clear local-instance prefs so they don't linger.
			u.prefs.RemoveValue(prefAutoConnectLocalCallsign)
			u.prefs.RemoveValue(prefAutoConnectLocalAddr)
		}
	})
	u.autoCheck.Disable() // enabled once an instance is chosen

	u.webBtn = widget.NewButton("UberSDR", func() {
		if u.current != nil {
			u.openURL(u.current.HTTPURL())
		}
	})
	u.webBtn.Disable()

	u.dxBtn = widget.NewButton("Web", func() {
		if u.current != nil {
			u.openURL(u.current.HTTPURL() + "/addon/dxcluster/")
		}
	})
	u.dxBtn.Disable()

	instanceRow := container.NewHBox(
		u.chooseBtn,
		widget.NewLabel("Instance:"),
		u.instanceLabel,
		layout.NewSpacer(),
		u.webBtn,
		u.dxBtn,
		u.autoCheck,
	)

	u.callsign = widget.NewEntry()
	u.callsign.SetPlaceHolder("Callsign")
	u.callsign.SetText(u.prefs.StringWithFallback(prefCallsign, ""))
	u.callsign.OnSubmitted = func(string) { u.onConnect() }

	u.portEntry = widget.NewEntry()
	u.portEntry.SetText(u.prefs.StringWithFallback(prefTelnetPort, defaultTelnetPort))

	u.connectBtn = widget.NewButton("Connect", u.onConnect)
	u.connectBtn.Importance = widget.HighImportance

	u.helpBtn = widget.NewButton("?", u.showHelpDialog)
	u.audioTuneBtn = widget.NewButton("Audio Client…", u.showAudioClientDialog)
	u.flrigBtn = widget.NewButton("flrig…", u.showFlrigDialog)
	u.startupCmdsBtn = widget.NewButton("Commands…", u.showStartupCmdsDialog)

	connectRow := container.NewHBox(
		widget.NewLabel("Callsign:"),
		container.NewGridWrap(fyne.NewSize(160, 37), u.callsign),
		widget.NewLabel("Telnet port:"),
		container.NewGridWrap(fyne.NewSize(90, 37), u.portEntry),
		u.connectBtn,
		layout.NewSpacer(),
		u.helpBtn,
		u.flrigBtn,
		u.audioTuneBtn,
		u.startupCmdsBtn,
	)

	toolbar := container.NewVBox(instanceRow, connectRow, widget.NewSeparator())

	// ── Terminal / spot stream (fills the window) ──────────────────────────
	u.term = newTerminalView()

	// Wire double-tap: parse the spot line, tune the audio client if enabled,
	// and push to flrig if enabled.
	u.term.onDoubleTap = func(line string) {
		freqHz, mode, ok := ParseSpotLine(line)
		if !ok {
			return // digital spot or non-spot line — do nothing
		}
		// Audio client path.
		if u.prefs.BoolWithFallback(prefAudioEnabled, false) {
			host := u.prefs.StringWithFallback(prefAudioHost, defaultAudioHost)
			port := u.prefs.StringWithFallback(prefAudioPort, defaultAudioPort)
			baseURL := AudioClientBaseURL(host, port)
			go func() {
				err := TuneAudioClient(baseURL, freqHz, mode)
				fyne.Do(func() {
					if err != nil {
						u.setAudioStatus("⚠ "+err.Error(), false)
					} else {
						u.setAudioStatus(fmt.Sprintf("⇒ %.1f kHz %s",
							float64(freqHz)/1000.0, strings.ToUpper(mode)), true)
					}
				})
			}()
		}
		// flrig path — PushSDRState is a no-op when disabled or not connected.
		u.flrig.PushSDRState(freqHz, mode)
	}

	// Wire secondary-tap (right-click): show a context menu for the spot line.
	u.term.onSecondaryTap = func(line string, pos fyne.Position) {
		call, freqKHz, band := parseSpotLineForMenu(line)
		if call == "" && freqKHz == 0 {
			return // not a spot line
		}
		u.showSpotContextMenu(call, freqKHz, band, pos)
	}

	termHeader := widget.NewLabelWithStyle(
		"DX Cluster spot stream", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})

	// Command input row — mirrors the Python client. Commands are sent to the
	// same WebSocket session that feeds the telnet listener, so telnet clients
	// see the resulting server output too.
	u.inputEntry = widget.NewEntry()
	u.inputEntry.SetPlaceHolder("Type a command (e.g. SH/DX, HELP, BYE) and press Enter")
	u.inputEntry.OnSubmitted = func(string) { u.sendCommand() }
	u.inputEntry.Disable()
	u.sendBtn = widget.NewButton("Send", u.sendCommand)
	u.sendBtn.Disable()
	inputRow := container.NewBorder(nil, nil, nil, u.sendBtn, u.inputEntry)

	center := container.NewBorder(termHeader, inputRow, nil, nil, u.term.scroll)

	// ── Status bar ─────────────────────────────────────────────────────────
	u.statusLabel = widget.NewLabel("")
	u.telnetLabel = widget.NewLabel("")
	u.audioLabel = widget.NewLabel("")
	u.flrigLabel = widget.NewLabel("")
	// Wrap the dot in a Centre container so it aligns vertically with the
	// label text regardless of the row height chosen by HBox.
	statusRow := container.NewHBox(
		container.NewCenter(u.connDotBox),
		u.statusLabel,
		layout.NewSpacer(),
		u.flrigLabel,
		u.audioLabel,
		u.telnetLabel,
	)
	bottom := container.NewVBox(widget.NewSeparator(), statusRow)

	// Wire up the initial (unlocked) OnChanged handlers for callsign and port.
	u.setControlsEnabled(true)

	return container.NewBorder(toolbar, bottom, nil, nil, center)
}

// ── Instance directory ─────────────────────────────────────────────────────

func (u *appUI) refresh() {
	u.setStatus("Fetching instances…")
	go func() {
		insts, err := FetchDXClusterInstances(context.Background())
		fyne.Do(func() {
			if err != nil {
				u.setStatus("Fetch error: " + err.Error())
				return
			}
			u.instances = insts
			if u.current == nil {
				u.setStatus(fmt.Sprintf("%d instance(s) with the DX Cluster add-on — choose one", len(insts)))
			}
			if u.startupPending {
				u.startupPending = false
				u.doStartup()
			}
		})
	}()
}

// doStartup runs once after the first successful fetch and decides what to do:
//
//  1. The user's saved auto-connect preference takes priority:
//     - "local" sentinel → probe the last-known local instance via mDNS/HTTP.
//     - a UUID string   → resolve against the public directory (host/port may
//     have changed, but the UUID is stable).
//     - "none"          → user explicitly disabled auto-connect; open picker.
//  2. If no auto-connect preference is set and the program's own filename
//     encodes an instance callsign (e.g. "dxcluster_m9psy"), connect to it.
//  3. Otherwise, open the instance picker.
func (u *appUI) doStartup() {
	saved := u.prefs.String(prefAutoConnect)

	switch {
	case saved == "none":
		// User explicitly unchecked auto-connect — respect that choice.
		if len(u.instances) > 0 {
			u.showInstancePicker()
		}
		return

	case saved == autoConnectLocalSentinel:
		// Last session used a local (mDNS) instance. Try to reconnect to it.
		// We need a callsign to connect, so bail early if none is saved.
		if strings.TrimSpace(u.callsign.Text) == "" {
			if len(u.instances) > 0 {
				u.showInstancePicker()
			}
			return
		}
		u.setStatus("Looking for local instance…")
		go u.doLocalAutoConnect()
		return

	case saved != "":
		// A UUID was previously saved — try to connect to that instance.
		if strings.TrimSpace(u.callsign.Text) != "" {
			if inst := instanceByID(u.instances, saved); inst != nil {
				u.setStatus("Auto-connecting to " + inst.Name + "…")
				u.chooseInstance(*inst)
				return
			}
			u.setStatus("Saved startup instance is not currently online — choose one")
		}
		// Preference exists but instance is offline or callsign missing:
		// open the picker rather than letting the filename override the
		// user's explicit choice.
		if len(u.instances) > 0 {
			u.showInstancePicker()
		}
		return
	}

	// saved == "" → never configured; check for a filename-encoded callsign.
	if cs := executableCallsign(); cs != "" {
		if inst := instanceByCallsign(u.instances, cs); inst != nil {
			u.setStatus("Connecting to " + inst.Callsign + " (from program name)…")
			u.chooseInstance(*inst)
			// First launch with a filename callsign: automatically enable
			// auto-connect so the user doesn't have to tick it manually.
			// They can uncheck it at any time; that writes "none" and this
			// branch will not run again.
			u.saveAutoConnect(inst)
			u.refreshAutoCheck()
			return
		}
		// Named for a callsign that isn't online right now — fall through.
	}

	if len(u.instances) > 0 {
		u.showInstancePicker()
	}
}

// doLocalAutoConnect attempts to reconnect to the last-used local instance.
// It runs in a background goroutine. Strategy:
//  1. Fast path: probe the last-known host:port directly (IP may be unchanged).
//  2. Slow path: start an mDNS browse and wait up to 8 s for an instance whose
//     callsign matches the saved callsign.
//  3. If neither succeeds, open the instance picker on the UI thread.
func (u *appUI) doLocalAutoConnect() {
	savedCallsign := strings.ToUpper(strings.TrimSpace(u.prefs.String(prefAutoConnectLocalCallsign)))
	savedAddr := u.prefs.String(prefAutoConnectLocalAddr)

	// matchesCallsign returns true if the instance callsign matches the saved
	// one, or if no callsign was saved (accept any local dxcluster instance).
	matchesCallsign := func(cs string) bool {
		return savedCallsign == "" || strings.ToUpper(strings.TrimSpace(cs)) == savedCallsign
	}

	// ── fast path: try the last-known address directly ─────────────────────
	if savedAddr != "" {
		host, port, err := splitHostPort(savedAddr)
		if err == nil {
			if desc, probeErr := probeDescription(host, port); probeErr == nil && desc.hasDXCluster() {
				if matchesCallsign(desc.Receiver.Callsign) {
					inst := instanceFromDesc(desc, host, port)
					fyne.Do(func() {
						u.setStatus("Auto-connecting to local instance " + inst.Name + "…")
						u.chooseInstance(inst)
					})
					return
				}
			}
		}
	}

	// ── slow path: mDNS browse with 8 s timeout ────────────────────────────
	mdns, err := NewMDNSDiscovery(nil) // onChange not needed; we poll below
	if err != nil {
		fyne.Do(func() {
			u.setStatus("mDNS unavailable — choose an instance manually")
			if len(u.instances) > 0 {
				u.showInstancePicker()
			}
		})
		return
	}
	defer mdns.Stop()

	// Poll the mDNS results every 250 ms until we find a callsign match or
	// the timeout expires. Polling avoids adding a callback channel to MDNSDiscovery.
	deadline := time.Now().Add(8 * time.Second)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		for _, inst := range mdns.Instances() {
			if matchesCallsign(inst.Callsign) {
				instCopy := inst
				fyne.Do(func() {
					u.setStatus("Auto-connecting to local instance " + instCopy.Name + "…")
					u.chooseInstance(instCopy)
				})
				return
			}
		}
		if time.Now().After(deadline) {
			fyne.Do(func() {
				u.setStatus("Local instance not found — choose one")
				if len(u.instances) > 0 {
					u.showInstancePicker()
				}
			})
			return
		}
	}
}

// splitHostPort splits "host:port" into host string and port int.
func splitHostPort(addr string) (host string, port int, err error) {
	h, p, ok := strings.Cut(addr, ":")
	if !ok {
		return "", 0, fmt.Errorf("no colon in %q", addr)
	}
	n, convErr := strconv.Atoi(p)
	if convErr != nil {
		return "", 0, fmt.Errorf("invalid port in %q: %w", addr, convErr)
	}
	return h, n, nil
}

// instanceFromDesc builds an Instance from a probed /api/description response.
func instanceFromDesc(desc *apiDescription, host string, port int) Instance {
	name := desc.Receiver.Name
	if name == "" {
		name = fmt.Sprintf("%s:%d", host, port)
	}
	return Instance{
		ID:               fmt.Sprintf("local:%s:%d", host, port),
		Callsign:         desc.Receiver.Callsign,
		Name:             name,
		Location:         desc.Receiver.Location,
		Host:             host,
		Port:             port,
		TLS:              false,
		AvailableClients: desc.AvailableClients,
		MaxClients:       desc.MaxClients,
		IsOnline:         true,
	}
}

// showInstancePicker opens a modal dialog with the filterable instance list.
// Selecting an instance chooses it (and reconnects if a session is active).
func (u *appUI) showInstancePicker() {
	filterEntry := widget.NewEntry()
	filterEntry.SetPlaceHolder("Search callsign, name or location…")

	// ── selection state ────────────────────────────────────────────────────
	// selectedLocal / selectedPublic track which row is highlighted in each
	// table. Only one can be active at a time; selecting in one clears the other.
	selectedLocal := -1
	selectedPublic := -1
	var localInsts []Instance // confirmed mDNS instances
	var filtered []Instance   // filtered public instances
	var localTable *widget.Table
	var table *widget.Table
	var d *dialog.ConfirmDialog

	confirmSelection := func() {
		if selectedLocal >= 0 && selectedLocal < len(localInsts) {
			if d != nil {
				d.Hide()
			}
			u.chooseInstance(localInsts[selectedLocal])
			return
		}
		if selectedPublic >= 0 && selectedPublic < len(filtered) {
			if d != nil {
				d.Hide()
			}
			u.chooseInstance(filtered[selectedPublic])
		}
	}

	// ── local (mDNS) table ─────────────────────────────────────────────────
	localHdr := widget.NewLabelWithStyle("Local (on this network)", fyne.TextAlignLeading,
		fyne.TextStyle{Bold: true})
	localScanLbl := widget.NewLabel("Finding local instances…")

	localHeaders := []string{"Callsign", "Name", "Location", "Address"}
	localTable = widget.NewTable(
		func() (int, int) { return len(localInsts), len(localHeaders) },
		func() fyne.CanvasObject {
			c := newTappableCell()
			c.onTap = func(row, col int) {
				selectedPublic = -1
				if table != nil {
					table.UnselectAll()
				}
				if localTable != nil {
					localTable.Select(widget.TableCellID{Row: row, Col: col})
				}
			}
			c.onDoubleTap = func(row int) {
				selectedLocal = row
				confirmSelection()
			}
			return c
		},
		func(id widget.TableCellID, o fyne.CanvasObject) {
			cell := o.(*tappableCell)
			cell.row, cell.col = id.Row, id.Col
			cell.TextStyle.Bold = id.Col == 0
			if id.Row < 0 || id.Row >= len(localInsts) {
				cell.SetText("")
				return
			}
			inst := localInsts[id.Row]
			switch id.Col {
			case 0:
				cell.SetText(inst.Callsign)
			case 1:
				cell.SetText(inst.Name)
			case 2:
				cell.SetText(inst.Location)
			case 3:
				cell.SetText(fmt.Sprintf("%s:%d", inst.Host, inst.Port))
			}
		},
	)
	localTable.ShowHeaderRow = true
	localTable.CreateHeader = func() fyne.CanvasObject {
		l := widget.NewLabel("")
		l.TextStyle.Bold = true
		return l
	}
	localTable.UpdateHeader = func(id widget.TableCellID, o fyne.CanvasObject) {
		if id.Col >= 0 && id.Col < len(localHeaders) {
			o.(*widget.Label).SetText(localHeaders[id.Col])
		}
	}
	localTable.SetColumnWidth(0, 130)
	localTable.SetColumnWidth(1, 220)
	localTable.SetColumnWidth(2, 220)
	localTable.SetColumnWidth(3, 160)
	localTable.OnSelected = func(id widget.TableCellID) {
		selectedLocal = id.Row
		selectedPublic = -1
		if table != nil {
			table.UnselectAll()
		}
	}

	localSection := container.NewVBox(
		container.NewHBox(localHdr, layout.NewSpacer(), localScanLbl),
		localTable,
	)

	// ── public table ───────────────────────────────────────────────────────
	table = widget.NewTable(
		func() (int, int) { return len(filtered), len(u.headers) },
		func() fyne.CanvasObject {
			c := newTappableCell()
			c.onTap = func(row, col int) {
				selectedLocal = -1
				if localTable != nil {
					localTable.UnselectAll()
				}
				if table != nil {
					table.Select(widget.TableCellID{Row: row, Col: col})
				}
			}
			c.onDoubleTap = func(row int) {
				selectedPublic = row
				confirmSelection()
			}
			return c
		},
		func(id widget.TableCellID, o fyne.CanvasObject) {
			cell := o.(*tappableCell)
			cell.row, cell.col = id.Row, id.Col
			cell.TextStyle.Bold = id.Col == 0
			if id.Row < 0 || id.Row >= len(filtered) {
				cell.SetText("")
				return
			}
			inst := filtered[id.Row]
			switch id.Col {
			case 0:
				cell.SetText(inst.Callsign)
			case 1:
				cell.SetText(inst.Name)
			case 2:
				cell.SetText(inst.Location)
			}
		},
	)
	table.ShowHeaderRow = true
	table.CreateHeader = func() fyne.CanvasObject {
		l := widget.NewLabel("")
		l.TextStyle.Bold = true
		return l
	}
	table.UpdateHeader = func(id widget.TableCellID, o fyne.CanvasObject) {
		if id.Col >= 0 && id.Col < len(u.headers) {
			o.(*widget.Label).SetText(u.headers[id.Col])
		}
	}
	table.SetColumnWidth(0, 130)
	table.SetColumnWidth(1, 320)
	table.SetColumnWidth(2, 320)
	table.OnSelected = func(id widget.TableCellID) {
		selectedPublic = id.Row
		selectedLocal = -1
		if localTable != nil {
			localTable.UnselectAll()
		}
	}

	publicHdr := widget.NewLabelWithStyle("Public instances", fyne.TextAlignLeading,
		fyne.TextStyle{Bold: true})

	apply := func() {
		f := strings.ToLower(strings.TrimSpace(filterEntry.Text))
		filtered = filtered[:0]
		for _, inst := range u.instances {
			if f == "" ||
				strings.Contains(strings.ToLower(inst.Name), f) ||
				strings.Contains(strings.ToLower(inst.Callsign), f) ||
				strings.Contains(strings.ToLower(inst.Location), f) {
				filtered = append(filtered, inst)
			}
		}
		selectedPublic = -1
		table.UnselectAll()
		table.Refresh()
	}
	filterEntry.OnChanged = func(string) { apply() }
	apply()

	refreshBtn := widget.NewButton("Refresh", nil)
	refreshBtn.OnTapped = func() {
		refreshBtn.Disable()
		go func() {
			insts, err := FetchDXClusterInstances(context.Background())
			fyne.Do(func() {
				refreshBtn.Enable()
				if err != nil {
					u.setStatus("Fetch error: " + err.Error())
					return
				}
				u.instances = insts
				apply()
			})
		}()
	}

	// ── mDNS discovery ─────────────────────────────────────────────────────
	// Declare mdns first so the onChange closure can reference it after assignment.
	var mdns *MDNSDiscovery
	var mdnsErr error
	mdns, mdnsErr = NewMDNSDiscovery(func() {
		fyne.Do(func() {
			localInsts = mdns.Instances()
			if len(localInsts) == 0 {
				localScanLbl.SetText("Finding local instances…")
			} else {
				localScanLbl.SetText(fmt.Sprintf("%d found", len(localInsts)))
			}
			localTable.Refresh()
		})
	})
	if mdnsErr != nil {
		localScanLbl.SetText("mDNS unavailable")
	} else {
		// After 5 s, if nothing has been found yet, update the label so the
		// user knows the initial sweep is done (browse continues in background).
		time.AfterFunc(5*time.Second, func() {
			fyne.Do(func() {
				if len(localInsts) == 0 {
					localScanLbl.SetText("None found — ensure DX Cluster add-on is installed and enabled")
				}
			})
		})
	}

	// ── layout ─────────────────────────────────────────────────────────────
	top := container.NewBorder(nil, nil, nil, refreshBtn, filterEntry)
	publicSection := container.NewBorder(
		widget.NewSeparator(), nil, nil, nil,
		container.NewBorder(publicHdr, nil, nil, nil, table),
	)
	// Local section gets ~30% of height, public gets the rest.
	split := container.NewVSplit(
		container.NewBorder(nil, nil, nil, nil, localSection),
		publicSection,
	)
	split.Offset = 0.3
	content := container.NewBorder(top, nil, nil, nil, split)

	d = dialog.NewCustomConfirm("Choose DX Cluster instance", "Select", "Cancel", content,
		func(ok bool) {
			if mdns != nil {
				mdns.Stop()
			}
			if !ok {
				return
			}
			confirmSelection()
		}, u.win)
	d.Resize(fyne.NewSize(840, 600))
	d.Show()
	u.win.Canvas().Focus(filterEntry)
}

// chooseInstance records the picked instance and, if a session was active (or
// a callsign is ready), (re)connects to it — the one-click "switch" path.
func (u *appUI) chooseInstance(inst Instance) {
	u.current = &inst
	u.instanceLabel.SetText(inst.Callsign + " — " + inst.Name)
	u.refreshAutoCheck()

	switch {
	case u.isConnected():
		// Seamless switch: swap the upstream while the telnet listener (and its
		// connected clients) stay in place.
		u.switchTo(inst)
	case strings.TrimSpace(u.callsign.Text) != "":
		u.onConnect()
	default:
		u.setStatus("Selected " + inst.Name + " — enter your callsign and Connect")
	}
}

// ── Auto-connect preference ────────────────────────────────────────────────

// loadAutoConnect returns the saved startup instance UUID, or "".
func (u *appUI) loadAutoConnect() string {
	return u.prefs.String(prefAutoConnect)
}

// saveAutoConnect records the instance as the startup target.
// For public instances the UUID is stored (stable across host/port changes).
// For local (mDNS) instances the sentinel "local" is stored together with the
// callsign and last-known address so doLocalAutoConnect can find it again.
func (u *appUI) saveAutoConnect(inst *Instance) {
	if inst == nil || inst.ID == "" {
		return
	}
	if strings.HasPrefix(inst.ID, "local:") {
		u.prefs.SetString(prefAutoConnect, autoConnectLocalSentinel)
		u.prefs.SetString(prefAutoConnectLocalCallsign, strings.ToUpper(strings.TrimSpace(inst.Callsign)))
		u.prefs.SetString(prefAutoConnectLocalAddr, fmt.Sprintf("%s:%d", inst.Host, inst.Port))
	} else {
		u.prefs.SetString(prefAutoConnect, inst.ID)
		// Clear any stale local prefs from a previous local session.
		u.prefs.RemoveValue(prefAutoConnectLocalCallsign)
		u.prefs.RemoveValue(prefAutoConnectLocalAddr)
	}
}

// refreshAutoCheck syncs the checkbox to the current instance without firing
// its OnChanged handler: enabled once an instance is chosen, ticked when that
// instance is the saved startup target.
func (u *appUI) refreshAutoCheck() {
	u.updatingAutoCheck = true
	defer func() { u.updatingAutoCheck = false }()

	if u.current == nil {
		u.autoCheck.SetChecked(false)
		u.autoCheck.Disable()
		if u.webBtn != nil {
			u.webBtn.Disable()
		}
		if u.dxBtn != nil {
			u.dxBtn.Disable()
		}
		return
	}
	u.autoCheck.Enable()
	if u.webBtn != nil {
		u.webBtn.Enable()
	}
	if u.dxBtn != nil {
		u.dxBtn.Enable()
	}
	saved := u.loadAutoConnect()
	var checked bool
	switch {
	case saved == autoConnectLocalSentinel && strings.HasPrefix(u.current.ID, "local:"):
		// Local instance: match by callsign (IP may have changed).
		savedCS := strings.ToUpper(strings.TrimSpace(u.prefs.String(prefAutoConnectLocalCallsign)))
		currentCS := strings.ToUpper(strings.TrimSpace(u.current.Callsign))
		checked = savedCS != "" && savedCS == currentCS
	case saved != "" && saved != "none" && saved != autoConnectLocalSentinel:
		// Public instance: match by UUID.
		checked = saved == u.current.ID
	}
	u.autoCheck.SetChecked(checked)
}

func instanceByID(list []Instance, id string) *Instance {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

func instanceByCallsign(list []Instance, callsign string) *Instance {
	cs := strings.ToUpper(strings.TrimSpace(callsign))
	for i := range list {
		if strings.ToUpper(strings.TrimSpace(list[i].Callsign)) == cs {
			return &list[i]
		}
	}
	return nil
}

// executableCallsign returns the instance callsign encoded in the program's
// filename, or "" if none. See callsignFromFilename.
func executableCallsign() string {
	path, err := os.Executable()
	if err != nil || path == "" {
		if len(os.Args) > 0 {
			path = os.Args[0]
		}
	}
	return callsignFromFilename(path)
}

// callsignFromFilename extracts the callsign after the last '_' in the base
// filename, dropping any extension: "dxcluster_m9psy" or "dxcluster_m9psy.exe"
// → "M9PSY". Returns "" when there is no underscore-delimited suffix.
func callsignFromFilename(name string) string {
	base := filepath.Base(name)
	base = strings.TrimSuffix(base, filepath.Ext(base)) // drop .exe etc.
	i := strings.LastIndex(base, "_")
	if i < 0 || i == len(base)-1 {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(base[i+1:]))
}

// ── Connect / disconnect ───────────────────────────────────────────────────

func (u *appUI) onConnect() {
	if u.isConnected() {
		u.disconnect()
		return
	}
	if u.current == nil {
		u.setStatus("Choose an instance first")
		return
	}

	call := strings.ToUpper(strings.TrimSpace(u.callsign.Text))
	if call == "" {
		u.setStatus("Enter your callsign first")
		return
	}
	port, err := strconv.Atoi(strings.TrimSpace(u.portEntry.Text))
	if err != nil || port < 1 || port > 65535 {
		u.setStatus("Invalid telnet port (1–65535)")
		return
	}
	inst := *u.current

	listener := u.newListener(port)
	if err := listener.Start(); err != nil {
		u.setStatus("Cannot listen on port " + strconv.Itoa(port) + ": " + err.Error())
		return
	}
	u.listener = listener

	u.term.clear()
	u.term.append(fmt.Sprintf("Connecting to %s (%s)…\r\n", inst.Name, inst.TerminalWSURL()))

	client := u.newClient(inst, call, listener)
	u.client.Store(client)
	client.Start()
	u.maybeAutoConnectAudioClient(inst)

	u.connectBtn.SetText("Disconnect")
	u.setControlsEnabled(false)
	u.setInputEnabled(true)
	u.updateTitle()
	u.win.Canvas().Focus(u.inputEntry)
	u.telnetLabel.SetText(fmt.Sprintf("Telnet 0.0.0.0:%d — 0 client(s)", port))
}

// switchTo swaps the upstream to a new instance without touching the telnet
// listener, so connected telnet clients stay connected across the switch.
func (u *appUI) switchTo(inst Instance) {
	call := strings.ToUpper(strings.TrimSpace(u.callsign.Text))
	if call == "" { // shouldn't happen while connected, but guard
		u.disconnect()
		u.setStatus("Enter your callsign and Connect")
		return
	}

	if old := u.client.Swap(nil); old != nil {
		go old.Stop() // sends BYE to the previous instance (off UI thread to avoid deadlock)
	}

	u.term.clear()
	u.term.append(fmt.Sprintf("Switching to %s (%s)…\r\n", inst.Name, inst.TerminalWSURL()))
	if u.listener != nil {
		u.listener.Broadcast(fmt.Sprintf("\r\n*** Switching to %s ***\r\n", inst.Name))
	}

	client := u.newClient(inst, call, u.listener)
	u.client.Store(client)
	client.Start()
	u.maybeAutoConnectAudioClient(inst)
	u.updateTitle()
	u.win.Canvas().Focus(u.inputEntry)
}

// maybeAutoConnectAudioClient tells the ubersdr-audio client to connect to the
// same instance this DX cluster client just connected to, but only when the
// "Auto connect to this instance in Audio Client" preference is enabled. The
// HTTP call runs off the UI thread; the result is surfaced in the status bar's
// audio label. Failures are non-fatal (the audio client may not be running).
func (u *appUI) maybeAutoConnectAudioClient(inst Instance) {
	if !u.prefs.BoolWithFallback(prefAudioAutoConnect, false) {
		return
	}
	host := u.prefs.StringWithFallback(prefAudioHost, defaultAudioHost)
	port := u.prefs.StringWithFallback(prefAudioPort, defaultAudioPort)
	baseURL := AudioClientBaseURL(host, port)
	instURL := inst.HTTPURL()
	label := inst.Callsign
	go func() {
		err := ConnectAudioClient(baseURL, instURL)
		fyne.Do(func() {
			if err != nil {
				u.setAudioStatus("⚠ audio client connect failed", false)
			} else {
				u.setAudioStatus("⇒ audio client → "+label, true)
			}
		})
	}()
}

// newListener builds the telnet listener. Its forward callback reads the
// current upstream atomically, so it keeps working across an instance switch.
func (u *appUI) newListener(port int) *TelnetListener {
	return NewTelnetListener(port,
		func(line string) { // a local telnet client typed a command
			if c := u.client.Load(); c != nil {
				_ = c.Send(line)
			}
			u.term.append("[telnet] > " + line + "\r\n")
		},
		func(n int) { // client count changed
			fyne.Do(func() {
				u.telnetLabel.SetText(fmt.Sprintf("Telnet 0.0.0.0:%d — %d client(s)", port, n))
			})
		},
	)
}

// newClient builds a WebSocket client bound to the given (persistent) listener.
func (u *appUI) newClient(inst Instance, call string, listener *TelnetListener) *DXClusterClient {
	var c *DXClusterClient
	c = NewDXClusterClient(inst.TerminalWSURL(), call,
		func(text string) { // server output
			// Guard: if this client is no longer the active one (user
			// disconnected), silently drop the text rather than appending
			// stale output to the terminal.
			if u.client.Load() != c {
				return
			}
			u.term.append(text)
			if listener != nil {
				listener.Broadcast(text)
			}
		},
		func(msg string, connected bool) { // status change
			fyne.Do(func() {
				u.setStatus(msg)
				u.setConnDot(connected)
			})
		},
	)
	c.SetOnLoggedIn(func() {
		u.sendStartupCommands(c)
	})
	return c
}

// sendStartupCommands sends each non-empty line from the startup commands
// preference to the server, 50 ms apart, in a background goroutine.
func (u *appUI) sendStartupCommands(c *DXClusterClient) {
	raw := u.prefs.String(prefStartupCmds)
	if raw == "" {
		return
	}
	var cmds []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			cmds = append(cmds, line)
		}
	}
	if len(cmds) == 0 {
		return
	}
	go func() {
		for _, cmd := range cmds {
			_ = c.Send(cmd)
			time.Sleep(50 * time.Millisecond)
		}
	}()
}

// showStartupCmdsDialog opens a modal that lets the user edit the list of
// commands sent automatically after login. One command per line.
func (u *appUI) showStartupCmdsDialog() {
	entry := widget.NewMultiLineEntry()
	entry.SetPlaceHolder("One command per line, e.g.:\nset/digital\nset/rbn\nset/filter band 20m")
	entry.SetText(u.prefs.String(prefStartupCmds))
	entry.Wrapping = fyne.TextWrapOff

	content := container.NewBorder(
		widget.NewLabelWithStyle(
			"Commands sent automatically after login (one per line):",
			fyne.TextAlignLeading, fyne.TextStyle{}),
		nil, nil, nil,
		container.NewScroll(entry),
	)

	d := dialog.NewCustomConfirm("Startup Commands", "Save", "Cancel", content,
		func(ok bool) {
			if !ok {
				return
			}
			u.prefs.SetString(prefStartupCmds, entry.Text)
		}, u.win)
	d.Resize(fyne.NewSize(520, 340))
	d.Show()
	u.win.Canvas().Focus(entry)
}

// showHelpDialog opens a modal explaining what the client does and how to use
// it, mirroring the web UI's "Desktop DX Cluster Client" info modal.
// The full telnet command reference (from helpText, copied from the server's
// helptext.go at build time) is shown in a scrollable monospace pane between
// the intro text and the Reset Preferences button at the bottom.
func (u *appUI) showHelpDialog() {
	intro := widget.NewRichTextFromMarkdown(`**UberSDR Desktop DX Cluster Client**

When you run this client it connects to the chosen UberSDR DX Cluster instance over a secure WebSocket and listens on **port 7300** (configurable) on your local machine.

You can then connect to ` + "`localhost:7300`" + ` with **telnet** or your favourite logging software (Log4OM, DXKeeper, N1MM+, etc.) to receive the live spot stream. Other machines on your local network can also connect using this machine's IP address on port 7300, if your firewall allows.

**Auto-connect:** if this program's filename contains an instance callsign (e.g. ` + "`dxcluster_m0abc`" + ` or ` + "`dxcluster_m0abc.exe`" + `), it will connect to that instance automatically on first launch and tick the *Auto-connect on startup* checkbox for you.

**Startup Commands:** use the *Commands…* button to enter commands sent automatically after login — one per line (e.g. ` + "`set/digital`" + `, ` + "`set/filter band 20m`" + `).`)
	intro.Wrapping = fyne.TextWrapWord

	cmdRef := widget.NewLabel(helpText)
	cmdRef.TextStyle = fyne.TextStyle{Monospace: true}
	cmdRef.Wrapping = fyne.TextWrapOff

	helpLines := strings.Split(helpText, "\n")

	filterEntry := widget.NewEntry()
	filterEntry.SetPlaceHolder("Filter commands…")
	filterEntry.OnChanged = func(q string) {
		q = strings.ToLower(strings.TrimSpace(q))
		if q == "" {
			cmdRef.SetText(helpText)
			return
		}
		var out []string
		for _, line := range helpLines {
			if strings.Contains(strings.ToLower(line), q) {
				out = append(out, line)
			}
		}
		if len(out) == 0 {
			cmdRef.SetText("(no matches)")
		} else {
			cmdRef.SetText(strings.Join(out, "\n"))
		}
	}

	copyBtn := widget.NewButton("📋 Copy", nil)
	copyBtn.OnTapped = func() {
		u.win.Clipboard().SetContent(cmdRef.Text)
		copyBtn.SetText("✓ Copied")
		time.AfterFunc(1500*time.Millisecond, func() {
			fyne.Do(func() { copyBtn.SetText("📋 Copy") })
		})
	}

	filterRow := container.NewBorder(nil, nil, nil, copyBtn, filterEntry)

	resetBtn := widget.NewButton("Reset All Preferences…", func() {
		dialog.ShowConfirm("Reset Preferences",
			"This will clear your saved callsign, port, auto-connect setting and startup commands.\n\nContinue?",
			func(ok bool) {
				if !ok {
					return
				}
				u.resetPreferences()
			}, u.win)
	})
	resetBtn.Importance = widget.DangerImportance

	content := container.NewBorder(
		container.NewVBox(intro, filterRow),
		container.NewVBox(widget.NewSeparator(), resetBtn),
		nil, nil,
		container.NewScroll(cmdRef),
	)

	d := dialog.NewCustom("About this Client", "Close", content, u.win)
	winSize := u.win.Canvas().Size()
	d.Resize(fyne.NewSize(
		fyne.Min(700, winSize.Width-40),
		winSize.Height-80,
	))
	d.Show()
}

// showAudioClientDialog opens a modal that lets the user configure the
// ubersdr-audio HTTP API integration. When enabled, double-clicking a CW,
// SSB, or voice spot line tunes the audio client to that frequency and mode.
func (u *appUI) showAudioClientDialog() {
	enabledCheck := widget.NewCheck("Enable UberSDR Audio Client tuning on spot double-click", nil)
	enabledCheck.SetChecked(u.prefs.BoolWithFallback(prefAudioEnabled, false))

	hostEntry := widget.NewEntry()
	hostEntry.SetPlaceHolder(defaultAudioHost)
	hostEntry.SetText(u.prefs.StringWithFallback(prefAudioHost, defaultAudioHost))

	portEntry := widget.NewEntry()
	portEntry.SetPlaceHolder(defaultAudioPort)
	portEntry.SetText(u.prefs.StringWithFallback(prefAudioPort, defaultAudioPort))

	form := widget.NewForm(
		widget.NewFormItem("Host", hostEntry),
		widget.NewFormItem("Port", portEntry),
	)

	// Test button + result label: performs a live GET /api/v1/status against
	// the host/port currently typed in the form (not the saved prefs) so the
	// user can verify connectivity before saving.
	testResult := widget.NewLabel("")
	testResult.Wrapping = fyne.TextWrapWord
	var testBtn *widget.Button
	testBtn = widget.NewButton("Test Connection", func() {
		host := strings.TrimSpace(hostEntry.Text)
		if host == "" {
			host = defaultAudioHost
		}
		port := strings.TrimSpace(portEntry.Text)
		if port == "" {
			port = defaultAudioPort
		}
		baseURL := AudioClientBaseURL(host, port)
		testBtn.Disable()
		testResult.Importance = widget.MediumImportance
		testResult.SetText("Testing " + host + ":" + port + "…")
		go func() {
			err := TestAudioClient(baseURL)
			fyne.Do(func() {
				testBtn.Enable()
				if err != nil {
					testResult.Importance = widget.DangerImportance
					testResult.SetText("✗ Cannot reach the UberSDR Audio Client. " +
						"Check that it is running and that the host and port are correct.")
				} else {
					testResult.Importance = widget.SuccessImportance
					testResult.SetText("✓ Connected to the UberSDR Audio Client.")
				}
			})
		}()
	})

	// Download button: opens the OS-appropriate audio client binary URL in the
	// user's default browser. Hidden/disabled on platforms with no prebuilt
	// binary (e.g. macOS).
	dlURL, dlPlatform, dlOK := AudioClientDownloadURL()
	var downloadBtn *widget.Button
	if dlOK {
		downloadBtn = widget.NewButton("Download Audio Client ("+dlPlatform+")", func() {
			u, err := url.Parse(dlURL)
			if err != nil {
				testResult.Importance = widget.DangerImportance
				testResult.SetText("✗ Invalid download URL.")
				return
			}
			if err := fyne.CurrentApp().OpenURL(u); err != nil {
				testResult.Importance = widget.DangerImportance
				testResult.SetText("✗ Could not open the download link: " + err.Error())
			}
		})
		downloadBtn.SetIcon(theme.DownloadIcon())
	} else {
		downloadBtn = widget.NewButton("Download Audio Client", func() {
			testResult.Importance = widget.WarningImportance
			testResult.SetText("No prebuilt UberSDR Audio Client is available for " +
				dlPlatform + ". Build it from source instead.")
		})
		downloadBtn.SetIcon(theme.DownloadIcon())
	}

	// ── Instance auto-connect section ──────────────────────────────────────
	// Tells the audio client which UberSDR instance to connect to (the same one
	// this DX cluster client is using), via POST /api/v1/connect.
	autoConnectCheck := widget.NewCheck(
		"Auto connect to this instance in Audio Client", nil)
	autoConnectCheck.SetChecked(u.prefs.BoolWithFallback(prefAudioAutoConnect, false))

	var connectBtn *widget.Button
	connectBtn = widget.NewButton("Connect Audio Client to this instance", func() {
		if u.current == nil {
			testResult.Importance = widget.WarningImportance
			testResult.SetText("Choose and connect to an instance first.")
			return
		}
		host := strings.TrimSpace(hostEntry.Text)
		if host == "" {
			host = defaultAudioHost
		}
		port := strings.TrimSpace(portEntry.Text)
		if port == "" {
			port = defaultAudioPort
		}
		baseURL := AudioClientBaseURL(host, port)
		instURL := u.current.HTTPURL()
		instLabel := u.current.Callsign
		connectBtn.Disable()
		testResult.Importance = widget.MediumImportance
		testResult.SetText("Connecting Audio Client to " + instLabel + "…")
		go func() {
			err := ConnectAudioClient(baseURL, instURL)
			fyne.Do(func() {
				connectBtn.Enable()
				if err != nil {
					testResult.Importance = widget.DangerImportance
					testResult.SetText("✗ Could not tell the Audio Client to connect. " +
						"Check that it is running and that the host and port are correct.")
				} else {
					testResult.Importance = widget.SuccessImportance
					testResult.SetText("✓ Audio Client is connecting to " + instLabel + ".")
				}
			})
		}()
	})
	connectBtn.SetIcon(theme.MediaPlayIcon())

	// Everything stacks top-to-bottom in a single VBox. The VBox is anchored to
	// the top of the dialog (via a top-region Border) so its children keep their
	// natural heights instead of being stretched to fill the dialog.
	body := container.NewVBox(
		enabledCheck,
		widget.NewSeparator(),
		widget.NewLabelWithStyle(
			"UberSDR Audio Client API address (default: 127.0.0.1:9770):",
			fyne.TextAlignLeading, fyne.TextStyle{}),
		form,
		container.NewHBox(testBtn, downloadBtn),
		widget.NewSeparator(),
		autoConnectCheck,
		container.NewHBox(connectBtn),
		widget.NewSeparator(),
		testResult,
	)
	content := container.NewBorder(body, nil, nil, nil)

	d := dialog.NewCustomConfirm("Audio Client", "Save", "Cancel", content,
		func(ok bool) {
			if !ok {
				return
			}
			u.prefs.SetBool(prefAudioEnabled, enabledCheck.Checked)
			host := strings.TrimSpace(hostEntry.Text)
			if host == "" {
				host = defaultAudioHost
			}
			port := strings.TrimSpace(portEntry.Text)
			if port == "" {
				port = defaultAudioPort
			}
			u.prefs.SetString(prefAudioHost, host)
			u.prefs.SetString(prefAudioPort, port)
			u.prefs.SetBool(prefAudioAutoConnect, autoConnectCheck.Checked)
		}, u.win)
	d.Resize(fyne.NewSize(500, 380))
	d.Show()
	u.win.Canvas().Focus(hostEntry)
}

// showFlrigDialog opens a modal that lets the user configure flrig XML-RPC sync.
// When enabled, double-clicking a spot line pushes the frequency and mode to
// flrig (sdr-to-rig), or flrig changes update the status bar (rig-to-sdr), or
// both directions are active simultaneously.
func (u *appUI) showFlrigDialog() {
	enabledCheck := widget.NewCheck("Enable flrig frequency/mode sync", nil)
	enabledCheck.SetChecked(u.prefs.BoolWithFallback(prefFlrigEnabled, false))

	hostEntry := widget.NewEntry()
	hostEntry.SetPlaceHolder("127.0.0.1")
	hostEntry.SetText(u.prefs.StringWithFallback(prefFlrigHost, "127.0.0.1"))

	portEntry := widget.NewEntry()
	portEntry.SetPlaceHolder("12345")
	portEntry.SetText(strconv.Itoa(u.prefs.IntWithFallback(prefFlrigPort, 12345)))

	dirSelect := widget.NewSelect(
		[]string{"sdr-to-rig", "rig-to-sdr", "both"},
		nil,
	)
	dirSelect.SetSelected(u.prefs.StringWithFallback(prefFlrigDirection, "sdr-to-rig"))

	form := widget.NewForm(
		widget.NewFormItem("Host", hostEntry),
		widget.NewFormItem("Port", portEntry),
		widget.NewFormItem("Direction", dirSelect),
	)

	// Test button: attempts system.listMethods against the host/port currently
	// typed in the form (not the saved prefs) to verify connectivity.
	testResult := widget.NewLabel("")
	testResult.Wrapping = fyne.TextWrapWord
	var testBtn *widget.Button
	testBtn = widget.NewButton("Test Connection", func() {
		host := strings.TrimSpace(hostEntry.Text)
		if host == "" {
			host = "127.0.0.1"
		}
		portStr := strings.TrimSpace(portEntry.Text)
		port := 12345
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 && p < 65536 {
			port = p
		}
		testBtn.Disable()
		testResult.Importance = widget.MediumImportance
		testResult.SetText(fmt.Sprintf("Testing %s:%d…", host, port))
		go func() {
			// Use a one-shot FlrigSync just for the connection test.
			probe := NewFlrigSync()
			probe.Configure(host, port, "sdr-to-rig", true)
			probe.Start()
			// Give it one reconnect attempt (tryConnect runs immediately).
			time.Sleep(500 * time.Millisecond)
			connected := probe.IsConnected()
			probe.Stop()
			fyne.Do(func() {
				testBtn.Enable()
				if connected {
					testResult.Importance = widget.SuccessImportance
					testResult.SetText("✓ Connected to flrig.")
				} else {
					testResult.Importance = widget.DangerImportance
					testResult.SetText("✗ Cannot reach flrig. Check that flrig is running and the host/port are correct.")
				}
			})
		}()
	})

	dirHelp := widget.NewLabelWithStyle(
		"sdr-to-rig: spot double-click → flrig\nrig-to-sdr: flrig tunes → shown in status bar\nboth: bidirectional with echo prevention",
		fyne.TextAlignLeading, fyne.TextStyle{Italic: true})
	dirHelp.Wrapping = fyne.TextWrapWord

	body := container.NewVBox(
		enabledCheck,
		widget.NewSeparator(),
		widget.NewLabelWithStyle(
			"flrig XML-RPC address (default: 127.0.0.1:12345):",
			fyne.TextAlignLeading, fyne.TextStyle{}),
		form,
		dirHelp,
		widget.NewSeparator(),
		container.NewHBox(testBtn),
		widget.NewSeparator(),
		testResult,
	)
	content := container.NewBorder(body, nil, nil, nil)

	d := dialog.NewCustomConfirm("flrig Sync", "Save", "Cancel", content,
		func(ok bool) {
			if !ok {
				return
			}
			host := strings.TrimSpace(hostEntry.Text)
			if host == "" {
				host = "127.0.0.1"
			}
			portStr := strings.TrimSpace(portEntry.Text)
			port := 12345
			if p, err := strconv.Atoi(portStr); err == nil && p > 0 && p < 65536 {
				port = p
			}
			dir := dirSelect.Selected
			if dir == "" {
				dir = "sdr-to-rig"
			}
			enabled := enabledCheck.Checked

			u.prefs.SetBool(prefFlrigEnabled, enabled)
			u.prefs.SetString(prefFlrigHost, host)
			u.prefs.SetInt(prefFlrigPort, port)
			u.prefs.SetString(prefFlrigDirection, dir)

			u.flrig.Configure(host, port, dir, enabled)
		}, u.win)
	d.Resize(fyne.NewSize(480, 400))
	d.Show()
	u.win.Canvas().Focus(hostEntry)
}

// resetPreferences clears all saved preferences via the Fyne API and reloads
// the UI fields to their defaults. Works on all platforms without any
// file-path logic.
func (u *appUI) resetPreferences() {
	for _, key := range []string{
		prefCallsign, prefTelnetPort, prefAutoConnect, prefStartupCmds,
		prefAutoConnectLocalCallsign, prefAutoConnectLocalAddr,
		prefAudioEnabled, prefAudioHost, prefAudioPort, prefAudioAutoConnect,
		prefFlrigEnabled, prefFlrigHost, prefFlrigPort, prefFlrigDirection,
	} {
		u.prefs.RemoveValue(key)
	}
	// Reload UI fields to defaults.
	u.callsign.SetText("")
	u.portEntry.SetText(defaultTelnetPort)
	u.refreshAutoCheck()
	// Disable flrig sync so it stops trying to connect after a reset.
	u.flrig.Configure("127.0.0.1", 12345, "sdr-to-rig", false)
}

// sendCommand forwards the GUI input line to the DX cluster session, echoing
// it locally the way the web/Python terminals do.
func (u *appUI) sendCommand() {
	c := u.client.Load()
	if c == nil {
		return
	}
	line := strings.TrimSpace(u.inputEntry.Text)
	if line == "" {
		return
	}
	u.term.append("> " + line + "\r\n")
	_ = c.Send(line)
	u.inputEntry.SetText("")
}

// spotSendCommand pre-fills the command input and sends it immediately if
// connected, or just pre-fills it (ready to send) if not connected.
// Must be called on the UI thread.
func (u *appUI) spotSendCommand(cmd string) {
	u.inputEntry.SetText(cmd)
	c := u.client.Load()
	if c == nil || !c.Connected() {
		// Not connected — leave the command in the input box for the user.
		u.win.Canvas().Focus(u.inputEntry)
		return
	}
	u.sendCommand()
}

// showSpotContextMenu builds and shows a Fyne PopUpMenu at pos for the given
// spot fields. call, freqKHz, band may be zero/empty if not available.
// Must be called on the UI thread.
func (u *appUI) showSpotContextMenu(call string, freqKHz float64, band string, pos fyne.Position) {
	var items []*fyne.MenuItem

	pfx := callPrefix(call)

	// ── Telnet commands ──────────────────────────────────────────────────────
	if call != "" {
		items = append(items,
			fyne.NewMenuItem("Show QRZ: "+call, func() {
				u.spotSendCommand("show/qrz " + call)
			}),
			fyne.NewMenuItem("Show DX: "+call, func() {
				u.spotSendCommand("show/dx 20 call " + call)
			}),
			fyne.NewMenuItem("Show heading: "+call, func() {
				u.spotSendCommand("show/heading " + call)
			}),
		)
		if pfx != "" {
			items = append(items,
				fyne.NewMenuItem("Show DXCC: "+pfx, func() {
					u.spotSendCommand("show/dxcc " + pfx)
				}),
				fyne.NewMenuItem("Show prefix: "+pfx, func() {
					u.spotSendCommand("show/prefix " + pfx)
				}),
			)
		}
	}
	if band != "" {
		items = append(items,
			fyne.NewMenuItem("Show DX on "+band, func() {
				u.spotSendCommand("show/dx 20 on " + band)
			}),
		)
	}

	// ── Enable / Disable stream types ────────────────────────────────────────
	streamTypes := []struct{ label, setCmd, unsetCmd string }{
		{"Digital", "set/digital", "unset/digital"},
		{"CW/RBN", "set/rbn", "unset/rbn"},
		{"Voice", "set/voice", "unset/voice"},
		{"DX Cluster", "set/dxcluster", "unset/dxcluster"},
	}
	var enableItems, disableItems []*fyne.MenuItem
	for _, st := range streamTypes {
		st := st // capture
		enableItems = append(enableItems, fyne.NewMenuItem(st.label, func() {
			u.spotSendCommand(st.setCmd)
		}))
		disableItems = append(disableItems, fyne.NewMenuItem(st.label, func() {
			u.spotSendCommand(st.unsetCmd)
		}))
	}
	enableMenu := fyne.NewMenuItem("Enable", nil)
	enableMenu.ChildMenu = fyne.NewMenu("", enableItems...)
	disableMenu := fyne.NewMenuItem("Disable", nil)
	disableMenu.ChildMenu = fyne.NewMenu("", disableItems...)
	items = append(items, fyne.NewMenuItemSeparator(), enableMenu, disableMenu)

	// ── Filter shortcuts ─────────────────────────────────────────────────────
	if band != "" {
		items = append(items, fyne.NewMenuItemSeparator())
		bandCopy := band // capture for closures
		items = append(items,
			fyne.NewMenuItem("Reject this band ("+band+")", func() {
				u.spotSendCommand("reject/spots 1 on " + bandCopy)
			}),
			fyne.NewMenuItem("Accept only this band ("+band+")", func() {
				u.spotSendCommand("accept/spots 1 on " + bandCopy)
			}),
		)
	}

	// ── Clipboard ────────────────────────────────────────────────────────────
	items = append(items, fyne.NewMenuItemSeparator())
	if call != "" {
		items = append(items,
			fyne.NewMenuItem("Copy callsign", func() {
				u.win.Clipboard().SetContent(call)
			}),
		)
	}
	if freqKHz > 0 {
		freqStr := fmt.Sprintf("%.1f", freqKHz)
		items = append(items,
			fyne.NewMenuItem("Copy frequency ("+freqStr+" kHz)", func() {
				u.win.Clipboard().SetContent(freqStr)
			}),
		)
	}

	// ── Online lookups ───────────────────────────────────────────────────────
	if call != "" {
		onlineItems := []*fyne.MenuItem{
			fyne.NewMenuItem("QRZ.com", func() {
				u.openURL("https://www.qrz.com/db/" + call)
			}),
			fyne.NewMenuItem("PSKReporter", func() {
				u.openURL("https://pskreporter.info/pskmap.html?callsign=" + call)
			}),
			fyne.NewMenuItem("Clublog", func() {
				u.openURL("https://clublog.org/logsearch/" + call)
			}),
			fyne.NewMenuItem("DXHeat", func() {
				u.openURL("https://dxheat.com/dxc/")
			}),
		}
		onlineMenu := fyne.NewMenuItem("Online", nil)
		onlineMenu.ChildMenu = fyne.NewMenu("", onlineItems...)
		items = append(items, fyne.NewMenuItemSeparator(), onlineMenu)
	}

	if len(items) == 0 {
		return
	}

	menu := fyne.NewMenu("", items...)
	widget.ShowPopUpMenuAtPosition(menu, u.win.Canvas(), pos)
}

// openURL opens a URL in the system default browser.
func (u *appUI) openURL(rawURL string) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	_ = fyne.CurrentApp().OpenURL(parsed)
}

// callPrefix extracts the DXCC prefix from a callsign: the leading letters
// before the first digit. e.g. "G1TLH" → "G", "DL3ABC" → "DL".
func callPrefix(call string) string {
	for i, ch := range call {
		if ch >= '0' && ch <= '9' {
			return call[:i]
		}
	}
	return call // no digit found — return whole call (e.g. "BEACON")
}

// parseSpotLineForMenu extracts the callsign, frequency (kHz), and band from a
// DX cluster spot line. Returns zero values if the line is not a spot.
//
// Line format (from FormatDXCluster / spotLineRe):
//
//	DX de SPOTTER:   14033.0  G1TLH          13 dB  23 WPM  CQ            1701Z
func parseSpotLineForMenu(line string) (call string, freqKHz float64, band string) {
	// Reuse the existing regex from audiotune.go — it captures freq and comment.
	// We need the callsign too, which is the token after the frequency.
	// Full line: "DX de SPOTTER:   FREQ  CALL   COMMENT   TIMEZ"
	line = strings.TrimRight(line, "\r\n ")
	if !strings.HasPrefix(line, "DX de ") {
		return
	}
	// Split on runs of whitespace; the fixed-width format means:
	// fields[0]="DX" [1]="de" [2]="SPOTTER:" [3]=freq [4]=call [5..]=comment+time
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return
	}
	var err error
	freqKHz, err = strconv.ParseFloat(fields[3], 64)
	if err != nil {
		return "", 0, ""
	}
	call = strings.ToUpper(fields[4])
	band = freqKHzToBand(freqKHz)
	return
}

// freqKHzToBand maps a frequency in kHz to an amateur band string (e.g. "20m").
// Returns "" if the frequency doesn't fall in a recognised band.
func freqKHzToBand(kHz float64) string {
	switch {
	case kHz >= 135.7 && kHz <= 137.8:
		return "2200m"
	case kHz >= 472 && kHz <= 479:
		return "630m"
	case kHz >= 1800 && kHz <= 2000:
		return "160m"
	case kHz >= 3500 && kHz <= 4000:
		return "80m"
	case kHz >= 5258 && kHz <= 5450:
		return "60m"
	case kHz >= 7000 && kHz <= 7300:
		return "40m"
	case kHz >= 10100 && kHz <= 10150:
		return "30m"
	case kHz >= 14000 && kHz <= 14350:
		return "20m"
	case kHz >= 18068 && kHz <= 18168:
		return "17m"
	case kHz >= 21000 && kHz <= 21450:
		return "15m"
	case kHz >= 24890 && kHz <= 24990:
		return "12m"
	case kHz >= 28000 && kHz <= 29700:
		return "10m"
	case kHz >= 50000 && kHz <= 54000:
		return "6m"
	default:
		return ""
	}
}

func (u *appUI) disconnect() {
	// Swap out the client and stop it off the UI thread to avoid a deadlock:
	// c.Stop() blocks on wg.Wait() while the read goroutine may be trying to
	// schedule UI work via fyne.Do, which would deadlock if Stop() were called
	// on the UI thread.
	if c := u.client.Swap(nil); c != nil {
		go c.Stop()
	}
	if u.listener != nil {
		u.listener.Stop()
		u.listener = nil
	}
	// Stop the flrig sync engine. It will be restarted (if enabled) the next
	// time the user connects, or it can be left running for rig-to-sdr use.
	// We call Stop here only when the window is closing (called from SetOnClosed);
	// during a normal disconnect we leave it running so the rig stays in sync.
	u.connectBtn.SetText("Connect")
	u.setControlsEnabled(true)
	u.setInputEnabled(false)
	u.updateTitle()
	u.telnetLabel.SetText("")
	u.setStatus("Disconnected")
	u.setConnDot(false)
}

// stopFlrig shuts down the flrig sync engine. Called when the window closes.
func (u *appUI) stopFlrig() {
	if u.flrig != nil {
		u.flrig.Stop()
	}
}

func (u *appUI) isConnected() bool { return u.client.Load() != nil }

// updateTitle sets the window title: the plain app name when idle, or
// "DXCluster - <instance callsign>" while a session is active.
func (u *appUI) updateTitle() {
	if u.isConnected() && u.current != nil {
		id := u.current.Callsign
		if id == "" {
			id = u.current.Name
		}
		u.win.SetTitle("DXCluster - " + id)
		return
	}
	u.win.SetTitle(titleDisconnected)
}

// ── Small helpers ──────────────────────────────────────────────────────────

func (u *appUI) setStatus(msg string) { u.statusLabel.SetText(msg) }

// setAudioStatus updates the audio-client result label in the status bar.
// ok=true → green "⇒ freq mode"; ok=false → red "⚠ error".
// The label auto-clears after 5 seconds.
func (u *appUI) setAudioStatus(msg string, ok bool) {
	if u.audioLabel == nil {
		return
	}
	if ok {
		u.audioLabel.Importance = widget.SuccessImportance
	} else {
		u.audioLabel.Importance = widget.DangerImportance
	}
	u.audioLabel.SetText(msg)
	time.AfterFunc(5*time.Second, func() {
		fyne.Do(func() {
			if u.audioLabel.Text == msg {
				u.audioLabel.SetText("")
				u.audioLabel.Importance = widget.MediumImportance
			}
		})
	})
}

// setConnDot updates the connection status dot colour.
// Must be called on the UI thread (inside fyne.Do or a Fyne callback).
func (u *appUI) setConnDot(connected bool) {
	if u.connDot == nil {
		return
	}
	if connected {
		u.connDot.FillColor = color.NRGBA{R: 40, G: 180, B: 40, A: 255}
	} else {
		u.connDot.FillColor = color.NRGBA{R: 180, G: 40, B: 40, A: 255}
	}
	u.connDot.Refresh()
}

func (u *appUI) setControlsEnabled(enabled bool) {
	if enabled {
		// Restore normal OnChanged handlers so edits are accepted and saved.
		u.callsign.OnChanged = func(s string) {
			up := strings.ToUpper(s)
			if up != s {
				u.callsign.SetText(up)
			}
			u.prefs.SetString(prefCallsign, up)
		}
		u.portEntry.OnChanged = func(s string) {
			u.prefs.SetString(prefTelnetPort, strings.TrimSpace(s))
		}
	} else {
		// Lock the entries while connected: revert any edit immediately.
		// A guard flag prevents the SetText call from re-triggering OnChanged.
		var revertingCallsign bool
		u.callsign.OnChanged = func(string) {
			if revertingCallsign {
				return
			}
			revertingCallsign = true
			u.callsign.SetText(u.prefs.StringWithFallback(prefCallsign, ""))
			revertingCallsign = false
		}
		var revertingPort bool
		u.portEntry.OnChanged = func(string) {
			if revertingPort {
				return
			}
			revertingPort = true
			u.portEntry.SetText(u.prefs.StringWithFallback(prefTelnetPort, defaultTelnetPort))
			revertingPort = false
		}
	}
}

func (u *appUI) setInputEnabled(enabled bool) {
	for _, w := range []fyne.Disableable{u.inputEntry, u.sendBtn} {
		if enabled {
			w.Enable()
		} else {
			w.Disable()
		}
	}
}

// ── tappableCell: a table cell label that supports single- and double-tap ──

// tappableCell is a Label that reports taps. The glfw driver delivers events
// to the innermost object implementing Tappable/DoubleTappable, so a table of
// these cells receives clicks directly: a single tap selects the row (forwarded
// to the table so the highlight updates) and a double tap confirms it.
type tappableCell struct {
	widget.Label
	row, col    int
	onTap       func(row, col int)
	onDoubleTap func(row int)
}

func newTappableCell() *tappableCell {
	c := &tappableCell{}
	c.ExtendBaseWidget(c)
	return c
}

func (c *tappableCell) Tapped(_ *fyne.PointEvent) {
	if c.onTap != nil {
		c.onTap(c.row, c.col)
	}
}

func (c *tappableCell) DoubleTapped(_ *fyne.PointEvent) {
	if c.onDoubleTap != nil {
		c.onDoubleTap(c.row)
	}
}

// ── termWidget: custom widget that renders spot lines as canvas.Text ────────
//
// termWidget extends widget.BaseWidget and renders each line as a canvas.Text
// object positioned at y = i*rowHeight with zero padding between rows.
// It implements DoubleTapped so the caller can map a click to a line index.
// Wrap it in container.NewVScroll; MinSize reports the full content height so
// the scroll container knows how tall the content is.

// termMaxLines is the maximum number of lines kept in the scrollback buffer.
const termMaxLines = 250

// termFlushInterval is how often the widget is refreshed. Batching updates at
// 4 Hz means at most one Refresh call per 250 ms regardless of spot rate.
const termFlushInterval = 250 * time.Millisecond

type termWidget struct {
	widget.BaseWidget

	onDoubleTap    func(line string)                    // called on UI thread; nil = no-op
	onSecondaryTap func(line string, pos fyne.Position) // called on UI thread; nil = no-op

	mu        sync.Mutex
	lines     []string
	rowHeight float32 // height of one text row, measured once at construction
}

func newTermWidget() *termWidget {
	tw := &termWidget{}
	// Measure the true glyph-box height of one monospace text row using
	// fyne.MeasureText. Unlike widget.Label.MinSize(), this excludes the
	// label's internal vertical padding (2×InnerPadding), so rows sit flush
	// against one another with no visible gap. Both the renderer (which places
	// each canvas.Text at y = i*rowHeight) and DoubleTapped (which maps
	// Position.Y / rowHeight back to a line index) use this same value, so
	// hit-testing stays perfectly aligned with what is drawn.
	sz := theme.TextSize()
	m := fyne.MeasureText("Xygj|", sz, fyne.TextStyle{Monospace: true})
	tw.rowHeight = m.Height
	tw.ExtendBaseWidget(tw)
	return tw
}

func (tw *termWidget) MinSize() fyne.Size {
	tw.mu.Lock()
	n := len(tw.lines)
	tw.mu.Unlock()
	return fyne.NewSize(0, float32(n)*tw.rowHeight)
}

func (tw *termWidget) CreateRenderer() fyne.WidgetRenderer {
	return &termRenderer{tw: tw}
}

// DoubleTapped maps the tap Y coordinate to a line index and fires onDoubleTap.
func (tw *termWidget) DoubleTapped(e *fyne.PointEvent) {
	if tw.rowHeight <= 0 {
		return
	}
	row := int(e.Position.Y / tw.rowHeight)
	tw.mu.Lock()
	var text string
	if row >= 0 && row < len(tw.lines) {
		text = tw.lines[row]
	}
	tw.mu.Unlock()
	if text != "" && tw.onDoubleTap != nil {
		tw.onDoubleTap(text)
	}
}

// TappedSecondary maps the secondary-tap (right-click) Y coordinate to a line
// index and fires onSecondaryTap with the line text and absolute canvas position.
func (tw *termWidget) TappedSecondary(e *fyne.PointEvent) {
	if tw.rowHeight <= 0 {
		return
	}
	row := int(e.Position.Y / tw.rowHeight)
	tw.mu.Lock()
	var text string
	if row >= 0 && row < len(tw.lines) {
		text = tw.lines[row]
	}
	tw.mu.Unlock()
	if text != "" && tw.onSecondaryTap != nil {
		tw.onSecondaryTap(text, e.AbsolutePosition)
	}
}

// termRenderer renders each line as a canvas.Text at y = i*rowHeight.
type termRenderer struct {
	tw    *termWidget
	texts []*canvas.Text
}

func (r *termRenderer) Layout(size fyne.Size) {
	for i, t := range r.texts {
		t.Move(fyne.NewPos(0, float32(i)*r.tw.rowHeight))
		t.Resize(fyne.NewSize(size.Width, r.tw.rowHeight))
	}
}

func (r *termRenderer) MinSize() fyne.Size { return r.tw.MinSize() }

func (r *termRenderer) Refresh() {
	r.tw.mu.Lock()
	lines := make([]string, len(r.tw.lines))
	copy(lines, r.tw.lines)
	r.tw.mu.Unlock()

	// Grow the canvas.Text pool if needed.
	fg := theme.ForegroundColor()
	sz := theme.TextSize()
	for len(r.texts) < len(lines) {
		t := canvas.NewText("", fg)
		t.TextStyle = fyne.TextStyle{Monospace: true}
		t.TextSize = sz
		r.texts = append(r.texts, t)
	}
	// Update text content; hide unused slots.
	for i, t := range r.texts {
		if i < len(lines) {
			t.Text = lines[i]
			t.Show()
		} else {
			t.Text = ""
			t.Hide()
		}
		t.Refresh()
	}
	r.Layout(r.tw.Size())
}

func (r *termRenderer) Objects() []fyne.CanvasObject {
	objs := make([]fyne.CanvasObject, len(r.texts))
	for i, t := range r.texts {
		objs[i] = t
	}
	return objs
}

func (r *termRenderer) Destroy() {}

// ── terminalView: scroll wrapper + data management ──────────────────────────

// terminalView wraps termWidget in a VScroll and manages the line buffer.
// The onDoubleTap field is set by the caller (appUI) and receives the raw
// line text; it is called on the Fyne UI thread.
type terminalView struct {
	tw     *termWidget
	scroll *container.Scroll

	onDoubleTap    func(line string)                    // forwarded to tw.onDoubleTap
	onSecondaryTap func(line string, pos fyne.Position) // forwarded to tw.onSecondaryTap

	mu      sync.Mutex
	lines   []string // ring of up to termMaxLines complete lines
	pending string   // incomplete line fragment not yet terminated by \n
	dirty   bool     // true when lines/pending changed since last flush
}

func newTerminalView() *terminalView {
	tw := newTermWidget()
	tv := &terminalView{
		tw:     tw,
		scroll: container.NewVScroll(tw),
	}
	// Forward the double-tap and secondary-tap callbacks.
	tw.onDoubleTap = func(line string) {
		if tv.onDoubleTap != nil {
			tv.onDoubleTap(line)
		}
	}
	tw.onSecondaryTap = func(line string, pos fyne.Position) {
		if tv.onSecondaryTap != nil {
			tv.onSecondaryTap(line, pos)
		}
	}

	// Background ticker: flush pending changes at most 4×/sec.
	go func() {
		ticker := time.NewTicker(termFlushInterval)
		defer ticker.Stop()
		for range ticker.C {
			tv.mu.Lock()
			if !tv.dirty {
				tv.mu.Unlock()
				continue
			}
			snapshot := make([]string, len(tv.lines))
			copy(snapshot, tv.lines)
			tv.dirty = false
			tv.mu.Unlock()
			fyne.Do(func() {
				tw.mu.Lock()
				tw.lines = snapshot
				tw.mu.Unlock()
				tw.Refresh()
				tv.scroll.ScrollToBottom()
			})
		}
	}()

	return tv
}

// append adds server text to the pane. Safe to call from any goroutine.
// Text is split on newlines; complete lines are added to the ring buffer and
// the widget is refreshed by the background ticker (at most 4×/sec).
func (tv *terminalView) append(text string) {
	// Normalise CR/LF → LF so lines split cleanly.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	tv.mu.Lock()
	defer tv.mu.Unlock()

	// Combine any leftover fragment with the new text, then split on newlines.
	combined := tv.pending + text
	parts := strings.Split(combined, "\n")

	// All but the last element are complete lines.
	for _, line := range parts[:len(parts)-1] {
		if line == "" {
			continue // skip blank lines produced by \r\n normalisation
		}
		tv.lines = append(tv.lines, line)
		if len(tv.lines) > termMaxLines {
			tv.lines = tv.lines[len(tv.lines)-termMaxLines:]
		}
	}
	// The last element is the new incomplete fragment (may be "").
	tv.pending = parts[len(parts)-1]
	tv.dirty = true
}

func (tv *terminalView) clear() {
	tv.mu.Lock()
	tv.lines = tv.lines[:0]
	tv.pending = ""
	tv.dirty = false
	tv.mu.Unlock()
	fyne.Do(func() {
		tv.tw.mu.Lock()
		tv.tw.lines = nil
		tv.tw.mu.Unlock()
		tv.tw.Refresh()
	})
}
