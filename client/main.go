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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
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
	prefAutoConnect = "auto_connect_uuid" // instance UUID to auto-connect on startup, or ""
	prefStartupCmds = "startup_commands"  // newline-separated commands sent after login
)

func main() {
	a := app.NewWithID("org.ubersdr.dxcluster.client")
	a.SetIcon(fyne.NewStaticResource("ubersdr.ico", appIcon))
	w := a.NewWindow(titleDisconnected)
	w.Resize(fyne.NewSize(960, 720))

	ui := newAppUI(w, a.Preferences())
	w.SetContent(ui.build())
	w.SetOnClosed(ui.disconnect)

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
	callsign       *widget.Entry
	portEntry      *widget.Entry
	connectBtn     *widget.Button
	chooseBtn      *widget.Button
	autoCheck      *widget.Check
	helpBtn        *widget.Button
	startupCmdsBtn *widget.Button
	inputEntry     *widget.Entry
	sendBtn        *widget.Button
	statusLabel    *widget.Label
	telnetLabel    *widget.Label
	term           *terminalView

	headers []string
}

func newAppUI(w fyne.Window, prefs fyne.Preferences) *appUI {
	return &appUI{
		win:     w,
		prefs:   prefs,
		headers: []string{"Callsign", "Name", "Location"},
	}
}

func (u *appUI) build() fyne.CanvasObject {
	// ── Toolbar: instance choice + connection controls ─────────────────────
	u.chooseBtn = widget.NewButton("Choose Instance…", u.showInstancePicker)
	u.instanceLabel = widget.NewLabelWithStyle("— none —", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})

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
		}
	})
	u.autoCheck.Disable() // enabled once an instance is chosen

	instanceRow := container.NewHBox(
		u.chooseBtn,
		widget.NewLabel("Instance:"),
		u.instanceLabel,
		layout.NewSpacer(),
		u.autoCheck,
	)

	u.callsign = widget.NewEntry()
	u.callsign.SetPlaceHolder("Callsign")
	u.callsign.SetText(u.prefs.StringWithFallback(prefCallsign, ""))
	u.callsign.OnChanged = func(s string) {
		up := strings.ToUpper(s)
		if up != s {
			u.callsign.SetText(up) // retriggers OnChanged with the upper form
		}
		u.prefs.SetString(prefCallsign, up)
	}
	u.callsign.OnSubmitted = func(string) { u.onConnect() }

	u.portEntry = widget.NewEntry()
	u.portEntry.SetText(u.prefs.StringWithFallback(prefTelnetPort, defaultTelnetPort))
	u.portEntry.OnChanged = func(s string) {
		u.prefs.SetString(prefTelnetPort, strings.TrimSpace(s))
	}

	u.connectBtn = widget.NewButton("Connect", u.onConnect)
	u.connectBtn.Importance = widget.HighImportance

	u.helpBtn = widget.NewButton("?", u.showHelpDialog)
	u.startupCmdsBtn = widget.NewButton("Commands…", u.showStartupCmdsDialog)

	connectRow := container.NewHBox(
		widget.NewLabel("Callsign:"),
		container.NewGridWrap(fyne.NewSize(160, 37), u.callsign),
		widget.NewLabel("Telnet port:"),
		container.NewGridWrap(fyne.NewSize(90, 37), u.portEntry),
		u.connectBtn,
		layout.NewSpacer(),
		u.helpBtn,
		u.startupCmdsBtn,
	)

	toolbar := container.NewVBox(instanceRow, connectRow, widget.NewSeparator())

	// ── Terminal / spot stream (fills the window) ──────────────────────────
	u.term = newTerminalView()
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
	statusRow := container.NewHBox(u.statusLabel, layout.NewSpacer(), u.telnetLabel)
	bottom := container.NewVBox(widget.NewSeparator(), statusRow)

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
//  1. The user's saved auto-connect preference (a UUID) takes priority over
//     everything else — a filename-encoded callsign must not override it.
//     It is resolved against the fresh directory so a changed host/port is fine.
//  2. If no auto-connect preference is set and the program's own filename
//     encodes an instance callsign (e.g. "dxcluster_m9psy" or
//     "dxcluster_m9psy.exe"), connect to the matching instance.
//  3. Otherwise, open the instance picker.
func (u *appUI) doStartup() {
	saved := u.prefs.String(prefAutoConnect)

	switch {
	case saved == "none":
		// User explicitly unchecked auto-connect — respect that choice.
		// Still open the picker so they can connect manually.
		if len(u.instances) > 0 {
			u.showInstancePicker()
		}
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
	localScanLbl := widget.NewLabel("Scanning…")

	localTable = widget.NewTable(
		func() (int, int) { return len(localInsts), len(u.headers) },
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
			}
		},
	)
	localTable.SetColumnWidth(0, 130)
	localTable.SetColumnWidth(1, 320)
	localTable.SetColumnWidth(2, 320)
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
				localScanLbl.SetText("Scanning…")
			} else {
				localScanLbl.SetText(fmt.Sprintf("%d found", len(localInsts)))
			}
			localTable.Refresh()
		})
	})
	if mdnsErr != nil {
		localScanLbl.SetText("mDNS unavailable")
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

// saveAutoConnect records the instance's UUID as the startup target.
func (u *appUI) saveAutoConnect(inst *Instance) {
	if inst == nil || inst.ID == "" {
		return
	}
	u.prefs.SetString(prefAutoConnect, inst.ID)
}

// refreshAutoCheck syncs the checkbox to the current instance without firing
// its OnChanged handler: enabled once an instance is chosen, ticked when that
// instance's UUID is the saved startup target.
func (u *appUI) refreshAutoCheck() {
	u.updatingAutoCheck = true
	defer func() { u.updatingAutoCheck = false }()

	if u.current == nil {
		u.autoCheck.SetChecked(false)
		u.autoCheck.Disable()
		return
	}
	u.autoCheck.Enable()
	uuid := u.loadAutoConnect()
	u.autoCheck.SetChecked(uuid != "" && uuid == u.current.ID)
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
	u.updateTitle()
	u.win.Canvas().Focus(u.inputEntry)
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
		func(msg string, _ bool) { // status change
			fyne.Do(func() { u.setStatus(msg) })
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
		intro,
		container.NewVBox(widget.NewSeparator(), resetBtn),
		nil, nil,
		container.NewScroll(cmdRef),
	)

	d := dialog.NewCustom("About this Client", "Close", content, u.win)
	d.Resize(fyne.NewSize(700, 600))
	d.Show()
}

// resetPreferences clears all saved preferences via the Fyne API and reloads
// the UI fields to their defaults. Works on all platforms without any
// file-path logic.
func (u *appUI) resetPreferences() {
	for _, key := range []string{prefCallsign, prefTelnetPort, prefAutoConnect, prefStartupCmds} {
		u.prefs.RemoveValue(key)
	}
	// Reload UI fields to defaults.
	u.callsign.SetText("")
	u.portEntry.SetText(defaultTelnetPort)
	u.refreshAutoCheck()
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
	u.connectBtn.SetText("Connect")
	u.setControlsEnabled(true)
	u.setInputEnabled(false)
	u.updateTitle()
	u.telnetLabel.SetText("")
	u.setStatus("Disconnected")
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

func (u *appUI) setControlsEnabled(_ bool) {
	// Callsign and port entries are intentionally left always-enabled.
	// Disabling them in dark mode produces very low-contrast text that is
	// hard to read. The connect button already prevents re-connecting while
	// a session is active (it becomes "Disconnect"), so there is no
	// functional need to dim these fields.
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

// ── terminalView: a bounded, auto-scrolling virtualised text pane ──────────

// termMaxLines is the maximum number of lines kept in the scrollback buffer.
// With only 250 short lines the label render is fast and there's no
// inter-row padding (unlike widget.List which adds padding between rows).
const termMaxLines = 250

// termFlushInterval is how often the UI label is refreshed. Batching updates
// at 10 Hz means at most one SetText call per 100 ms regardless of spot rate.
const termFlushInterval = 100 * time.Millisecond

// terminalView uses a single widget.Label inside a scroll container.
// The 250-line ring buffer keeps the rendered string small, and the 10 Hz
// ticker throttles redraws so CPU usage stays low even at high spot rates.
type terminalView struct {
	label  *widget.Label
	scroll *container.Scroll

	mu      sync.Mutex
	lines   []string // ring of up to termMaxLines complete lines
	pending string   // incomplete line fragment not yet terminated by \n
	dirty   bool     // true when lines/pending changed since last flush
}

func newTerminalView() *terminalView {
	lbl := widget.NewLabel("")
	lbl.TextStyle = fyne.TextStyle{Monospace: true}
	lbl.Wrapping = fyne.TextWrapOff
	tv := &terminalView{label: lbl}
	tv.scroll = container.NewVScroll(lbl)

	// Background ticker: flush pending changes to the label at most 10×/sec.
	go func() {
		ticker := time.NewTicker(termFlushInterval)
		defer ticker.Stop()
		for range ticker.C {
			tv.mu.Lock()
			if !tv.dirty {
				tv.mu.Unlock()
				continue
			}
			content := strings.Join(tv.lines, "\n")
			tv.dirty = false
			tv.mu.Unlock()
			fyne.Do(func() {
				tv.label.SetText(content)
				tv.scroll.ScrollToBottom()
			})
		}
	}()

	return tv
}

// append adds server text to the pane. Safe to call from any goroutine.
// Text is split on newlines; complete lines are added to the ring buffer and
// the list is refreshed by the background ticker (at most 10×/sec).
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
	fyne.Do(func() { tv.label.SetText("") })
}
