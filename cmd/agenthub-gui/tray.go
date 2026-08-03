package main

// The system tray, expressed as a value.
//
// WHY THIS FILE CARRIES NO BUILD TAG. The GUI compresses its dependency on
// the Wails alpha into as few files as possible (docs/canonical.md §7 item 3),
// and everything a tray does that can be WRONG — which label a state produces,
// which servers survive the cap, whether the close button quits or hides — is
// decidable without a menu bar. So the decisions live here as plain values and
// are unit-tested by the default build, while the tagged assembly does nothing
// but render them into `application.Menu` and route the clicks back.
//
// The tray is READ-ONLY plus navigation plus one lifecycle entry. It never
// writes the registry: a menu has no confirmation surface and no place to show
// an error, so a mis-click there would change governance configuration and the
// 409 that came back would have nowhere to go. Anything that decides what a
// client may reach stays in the window, where the user can see what happened.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/cmd/agenthub-gui/services"
)

// maxTrayServers caps the server readout. The list is sorted worst-first, so
// the cap drops the servers that are FINE — and the count it drops is stated
// in the menu rather than silently swallowed, because a truncated list that
// does not say it is truncated reads as "that is all of them".
const maxTrayServers = 10

// trayIcon is which of the three pictures the tray shows. The icon is the
// only part of this UI visible without opening anything, so it answers the
// only question worth answering at a glance: is the hub serving, is something
// broken, or is it not running at all.
type trayIcon int

const (
	// trayIconOffline: no daemon reachable. Nothing is being served.
	trayIconOffline trayIcon = iota
	// trayIconAttention: connected, but at least one enabled server is not
	// healthy.
	trayIconAttention
	// trayIconOK: connected, and nothing is asking for help.
	trayIconOK
)

func (i trayIcon) String() string {
	switch i {
	case trayIconOffline:
		return "offline"
	case trayIconAttention:
		return "attention"
	default:
		return "ok"
	}
}

// trayState is everything the menu is rendered from. It is a snapshot: the
// caller re-reads it whenever the daemon says something changed, exactly like
// a page does (events are notifications, not snapshots — canonical.md §5c).
type trayState struct {
	// Status is the daemon connection state, as the shell shows it.
	Status services.Status
	// Servers is the last successful read of the server list.
	Servers []api.Server
	// ServersKnown separates "the list is empty" from "we have not read it
	// yet". Without it a tray opened during startup claims the user has no
	// servers configured, which is the one wrong answer that looks calm.
	ServersKnown bool
	// CloseToTray is the current value of the window-local preference.
	CloseToTray bool
	// OwnsDaemon reports that THIS process started the daemon it is talking
	// to, which is what makes Quit stop the hub as well.
	OwnsDaemon bool
}

// trayAction is what a click means. A string rather than an int so that a
// test failure names the action instead of an ordinal.
type trayAction string

const (
	// trayActionNone is a readout: no click behaviour, rendered disabled.
	trayActionNone trayAction = ""
	// trayActionOpen shows the window. Arg, when set, is the hash route to
	// navigate to first.
	trayActionOpen trayAction = "open"
	// trayActionStartHub connects, starting the daemon if necessary — the
	// same thing the Settings page's "Connect / retry" button does.
	trayActionStartHub trayAction = "start-hub"
	// trayActionCopySocket copies the control socket path to the clipboard.
	trayActionCopySocket trayAction = "copy-socket"
	// trayActionCloseToTray toggles the close-button preference.
	trayActionCloseToTray trayAction = "close-to-tray"
	// trayActionQuit terminates the application through the normal shutdown
	// path (which stops a daemon this process started).
	trayActionQuit trayAction = "quit"
)

// trayItem is one menu entry. A nil Action with no Items is a readout and is
// rendered disabled; Items make it a submenu.
type trayItem struct {
	Label     string
	Tooltip   string
	Action    trayAction
	Arg       string
	Separator bool
	Checkbox  bool
	Checked   bool
	Disabled  bool
	Items     []trayItem
}

// trayRoutes are the destinations reachable from the tray. It is deliberately
// SHORTER than the sidebar: the tray is not a second navigation, it is the
// four questions worth asking without the window open ("what are my servers
// doing", "what just ran", "who is connected", "how is this configured").
//
// The route names mirror the frontend's hash routes. A rename there and not
// here fails soft — the router falls back to Servers for an unknown hash —
// which is why this list is allowed to be a copy rather than generated.
var trayRoutes = []struct{ Route, Label string }{
	{"servers", "Servers"},
	{"activity", "Calls"},
	{"events", "Events"},
	{"clients", "Clients"},
	{"settings", "Settings"},
}

// serverCounts is the tally the status line reports.
type serverCounts struct {
	Total     int
	Attention int
	Disabled  int
	Active    int
}

// serverBucket classifies one server the same way the Servers page does, and
// for the same reason: the Health contract is computed server-side by one pure
// function, and a second opinion formed from `Enabled` or `State` would be the
// frontend-invented status docs/modules/controlplane.md forbids. Disabled
// reports level=healthy on purpose, so admin state is tested FIRST.
func serverBucket(s api.Server) string {
	if s.Health.AdminState == api.AdminStateDisabled {
		return "disabled"
	}
	if s.Health.Level == api.HealthLevelHealthy {
		return "active"
	}
	return "attention"
}

func countServers(servers []api.Server) serverCounts {
	var c serverCounts
	c.Total = len(servers)
	for _, s := range servers {
		switch serverBucket(s) {
		case "disabled":
			c.Disabled++
		case "active":
			c.Active++
		default:
			c.Attention++
		}
	}
	return c
}

// bucketRank orders the buckets worst-first, so the cap in trayServerItems
// drops the servers nobody needs to look at.
func bucketRank(s api.Server) int {
	switch serverBucket(s) {
	case "attention":
		return 0
	case "active":
		return 1
	default:
		return 2
	}
}

// withinBucketRank is the Servers page's ordering inside a bucket: the
// unusable before the merely degraded, what is serving tools before what
// nobody is watching.
func withinBucketRank(s api.Server) int {
	switch serverBucket(s) {
	case "attention":
		if s.Health.Level == api.HealthLevelUnhealthy {
			return 0
		}
		return 1
	case "active":
		switch s.State {
		case "connected":
			return 0
		case "connecting":
			return 1
		default:
			return 2
		}
	default:
		return 0
	}
}

// sortServersForTray returns a worst-first copy. It never sorts the caller's
// slice: that slice is the last successful read, shared with whoever else is
// holding the snapshot.
func sortServersForTray(servers []api.Server) []api.Server {
	out := slices.Clone(servers)
	slices.SortStableFunc(out, func(a, b api.Server) int {
		if d := bucketRank(a) - bucketRank(b); d != 0 {
			return d
		}
		if d := withinBucketRank(a) - withinBucketRank(b); d != 0 {
			return d
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// trayIconFor picks the picture. Disabled servers never raise attention —
// they are switched off on purpose — which is exactly what the bucket says.
func trayIconFor(st trayState) trayIcon {
	if !st.Status.Connected {
		return trayIconOffline
	}
	if countServers(st.Servers).Attention > 0 {
		return trayIconAttention
	}
	return trayIconOK
}

// trayStatusLine is the first row of the menu and the tray tooltip: one line
// that makes opening the window unnecessary when nothing is wrong.
func trayStatusLine(st trayState) string {
	if !st.Status.Connected {
		return "Hub offline"
	}
	if !st.ServersKnown {
		return "Hub connected"
	}
	c := countServers(st.Servers)
	if c.Total == 0 {
		return "Hub connected · no servers"
	}
	line := fmt.Sprintf("Hub connected · %d/%d servers", c.Active, c.Total)
	if c.Attention > 0 {
		line += fmt.Sprintf(" · %d need attention", c.Attention)
	}
	return line
}

// trayTooltip is what hovering the icon says. It names the application too,
// because the icon alone is not self-identifying in a crowded menu bar.
func trayTooltip(st trayState) string {
	return "AgentHub — " + trayStatusLine(st)
}

// serverGlyph is the state marker in front of a server id. It repeats what
// the tooltip says in a form that survives being read at a glance.
func serverGlyph(s api.Server) string {
	switch serverBucket(s) {
	case "attention":
		return "⚠"
	case "disabled":
		return "○"
	default:
		return "●"
	}
}

// trayServerItems is the status readout. Every row is DISABLED on purpose:
// there is no per-server action a tray can offer honestly (enabling one is a
// registry write, and this menu has nowhere to report a conflict), so a row
// that looked clickable would be promising something it cannot do. The one
// action sits at the bottom and says where it goes.
func trayServerItems(st trayState) []trayItem {
	if !st.Status.Connected {
		return []trayItem{{Label: "Hub offline", Disabled: true}}
	}
	if !st.ServersKnown {
		return []trayItem{{Label: "Loading…", Disabled: true}}
	}
	if len(st.Servers) == 0 {
		return []trayItem{
			{Label: "No servers configured", Disabled: true},
			{Separator: true},
			{Label: "Add a server…", Action: trayActionOpen, Arg: "catalog"},
		}
	}

	sorted := sortServersForTray(st.Servers)
	shown := min(len(sorted), maxTrayServers)
	items := make([]trayItem, 0, shown+3)
	for _, s := range sorted[:shown] {
		items = append(items, trayItem{
			Label:    serverGlyph(s) + " " + s.ID,
			Tooltip:  serverTooltip(s),
			Disabled: true,
		})
	}
	if dropped := len(sorted) - shown; dropped > 0 {
		items = append(items, trayItem{
			Label:    fmt.Sprintf("+%d more", dropped),
			Disabled: true,
		})
	}
	items = append(items,
		trayItem{Separator: true},
		trayItem{Label: "Open Servers page", Action: trayActionOpen, Arg: "servers"},
	)
	return items
}

// serverTooltip renders the Health contract verbatim. The summary is the
// daemon's sentence, not ours.
func serverTooltip(s api.Server) string {
	if s.Health.Summary == "" {
		return s.ID
	}
	if s.Health.Detail == "" {
		return s.Health.Summary
	}
	return s.Health.Summary + " — " + s.Health.Detail
}

// trayHubItems is the daemon submenu: identity, and the ONE lifecycle action
// that is safe without a confirmation.
//
// Starting a hub that is not running can only help. Stopping or restarting a
// running one cuts off every client mid-session, so it is not offered here at
// all — a destructive action needs a surface that can ask first, and that
// surface is the window.
func trayHubItems(st trayState) []trayItem {
	if !st.Status.Connected {
		items := []trayItem{{Label: "Start the hub", Action: trayActionStartHub}}
		if st.Status.Error != "" {
			items = append(items,
				trayItem{Separator: true},
				trayItem{Label: "Last error", Tooltip: st.Status.Error, Disabled: true},
			)
		}
		return items
	}

	identity := "Hub running"
	if st.Status.Version != "" {
		identity = "Hub " + st.Status.Version
	}
	if st.Status.Pid > 0 {
		identity += fmt.Sprintf(" · pid %d", st.Status.Pid)
	}
	items := []trayItem{
		{Label: identity, Disabled: true},
		{Label: fmt.Sprintf("Registry generation %d", st.Status.Generation), Disabled: true},
		{Separator: true},
		{
			Label:    "Copy socket path",
			Tooltip:  st.Status.Socket,
			Action:   trayActionCopySocket,
			Disabled: st.Status.Socket == "",
		},
	}
	return items
}

// trayQuitLabel spells out the consequence when there is one. A daemon this
// process started dies with it, and after the close button stops quitting,
// Quit is the only path that reaches that — so the menu item is where the
// warning has to be.
func trayQuitLabel(st trayState) string {
	if st.OwnsDaemon {
		return "Quit AgentHub (stops the hub)"
	}
	return "Quit AgentHub"
}

// trayMenu is the whole menu, top to bottom: state, then the way back into
// the window, then the readouts, then the one preference the tray owns, then
// the way out.
func trayMenu(st trayState) []trayItem {
	open := make([]trayItem, 0, len(trayRoutes))
	for _, r := range trayRoutes {
		open = append(open, trayItem{Label: r.Label, Action: trayActionOpen, Arg: r.Route})
	}
	return []trayItem{
		{Label: trayStatusLine(st), Tooltip: st.Status.Error, Disabled: true},
		{Separator: true},
		{Label: "Open AgentHub", Action: trayActionOpen},
		{Label: "Go to", Items: open},
		{Separator: true},
		{Label: "Servers", Items: trayServerItems(st)},
		{Label: "Hub", Items: trayHubItems(st)},
		{Separator: true},
		{
			Label:    "Close button minimises to tray",
			Checkbox: true,
			Checked:  st.CloseToTray,
			Action:   trayActionCloseToTray,
		},
		{Separator: true},
		{Label: trayQuitLabel(st), Action: trayActionQuit},
	}
}

// traySignature is what the rendered menu says, flattened. The assembly
// installs a new native menu only when this changes.
//
// The daemon publishes a `servers` event for every probe, so a tray that
// rebuilt on each one would replace the native menu several times a second —
// and every replacement is a fresh set of native objects the previous menu
// does not obviously release. Comparing the rendered text first turns that
// into "rebuild when the user would see a difference", which is a handful of
// times an hour.
func traySignature(items []trayItem) string {
	var b strings.Builder
	writeTraySignature(&b, items)
	return b.String()
}

func writeTraySignature(b *strings.Builder, items []trayItem) {
	for _, it := range items {
		if it.Separator {
			b.WriteString("--\n")
			continue
		}
		fmt.Fprintf(b, "%s|%s|%s|%s|%t|%t|%t\n",
			it.Label, it.Tooltip, it.Action, it.Arg, it.Checkbox, it.Checked, it.Disabled)
		if len(it.Items) > 0 {
			b.WriteString("<\n")
			writeTraySignature(b, it.Items)
			b.WriteString(">\n")
		}
	}
}

// trayAvailableOn reports whether this build drives a tray icon on the named
// platform.
//
// Linux is excluded deliberately, not accidentally. Wails renders its tray
// through the dbus StatusNotifierItem protocol, and a desktop with no
// StatusNotifierHost — a default GNOME session, for one — accepts the
// registration and then shows nothing. Since closeIntentFor turns "no tray"
// back into "the close button quits", saying false here leaves Linux with
// exactly the behaviour it had before this feature, which is the honest answer
// until someone verifies it on a real session (docs/modules/gui.md).
func trayAvailableOn(goos string) bool {
	switch goos {
	case "darwin", "windows":
		return true
	default:
		return false
	}
}

// closeIntent is what pressing the window's close button should do.
type closeIntent int

const (
	// closeIntentQuit is the pre-tray behaviour: the window closes and the
	// application ends.
	closeIntentQuit closeIntent = iota
	// closeIntentHide keeps the process (and the hub connection) alive with
	// only the tray icon visible.
	closeIntentHide
	// closeIntentAsk shows the window and asks, once, before hiding for the
	// first time.
	closeIntentAsk
)

func (i closeIntent) String() string {
	switch i {
	case closeIntentQuit:
		return "quit"
	case closeIntentHide:
		return "hide"
	default:
		return "ask"
	}
}

// closeIntentFor decides it.
//
// FAILURE DIRECTION. When the tray did not come up — no status area, a desktop
// with no host for it, a platform where the icon silently never appears — the
// answer is QUIT, the behaviour that existed before this feature. Hiding into
// a tray that is not there produces a running process with no visible surface
// at all: the user cannot reach the window, cannot reach a menu, and the only
// remaining way out is a process list. A window that closes when the user
// asked it to minimise is a surprise; a hub that cannot be quit is a trap.
//
// The first hide ASKS. Silently vanishing into the status area is the standard
// complaint about tray applications, and the ask is also where the "I actually
// meant quit" escape lives — otherwise the user has to discover the tray menu
// to undo a click they did not know was reversible.
func closeIntentFor(trayReady, closeToTray, noticeSeen bool) closeIntent {
	if !trayReady || !closeToTray {
		return closeIntentQuit
	}
	if !noticeSeen {
		return closeIntentAsk
	}
	return closeIntentHide
}
