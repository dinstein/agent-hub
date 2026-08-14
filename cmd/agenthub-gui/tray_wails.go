//go:build wails

package main

// The tray assembly: it renders the model in tray.go into a native menu,
// routes the clicks back, and owns the close button's behaviour.
//
// This is the third file in the tree that touches the Wails alpha, and it is
// deliberately thin for the reason docs/decisions/0003-wails3-and-the-frontend-stack.md gives: an alpha API
// that moves should break assembly, never judgement. Nothing here decides what
// a menu says — it asks trayMenu — and nothing here decides whether the close
// button hides — it asks closeIntentFor.

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/dinstein/agent-hub/cmd/agenthub-gui/services"
)

const (
	// trayIconSizeMac is rendered for a Retina menu bar; macOS scales a
	// template icon down to the bar height and the extra pixels are what
	// keep the curve smooth.
	trayIconSizeMac = 44
	// trayIconSize is the Windows notification-area size.
	trayIconSize = 32

	// trayCoalesce is how long a refresh waits for more events before it
	// rebuilds. Probes publish a `servers` event each, so this is the
	// difference between rebuilding a native menu a few times a second and
	// once per settled change.
	trayCoalesce = 750 * time.Millisecond
	// trayHeartbeat re-reads even when nothing was announced. Cheap, because
	// an unchanged menu is not reinstalled — it exists so a missed event
	// cannot leave the icon lying indefinitely.
	trayHeartbeat = 30 * time.Second
	// trayReadTimeout bounds the server list read behind one rebuild.
	trayReadTimeout = 3 * time.Second
)

// tray is the running tray. One per application.
type tray struct {
	app  *application.App
	hub  *services.Hub
	win  *application.WebviewWindow
	item *application.SystemTray

	wake chan struct{}

	mu   sync.Mutex
	sig  string
	icon trayIcon
	// iconSet guards the first paint: the zero value of trayIcon is a real
	// state (offline), so without this an application that starts offline
	// would never install an icon at all.
	iconSet bool
}

// newTray builds the tray and starts its refresh loop. It returns nil on a
// platform this build does not drive a tray on, and every caller treats nil as
// "there is no tray" rather than as an error: the GUI opening is not
// conditional on a menu bar.
func newTray(app *application.App, hub *services.Hub, win *application.WebviewWindow) *tray {
	if !trayAvailableOn(runtime.GOOS) {
		return nil
	}
	t := &tray{
		app:  app,
		hub:  hub,
		win:  win,
		item: app.SystemTray.New(),
		wake: make(chan struct{}, 1),
	}
	t.item.SetTooltip(trayTooltip(trayState{}))

	// The daemon connection and the server list are the two things the tray
	// reports; both arrive as the events the pages already consume.
	app.Event.On(services.EventDaemon, func(*application.CustomEvent) { t.signal() })
	app.Event.On(services.EventServers, func(*application.CustomEvent) { t.signal() })
	// The checkbox in this menu is also a switch in Settings. Without this the
	// two agree only as often as the heartbeat fires.
	app.Event.On(services.EventWindowPrefs, func(*application.CustomEvent) { t.signal() })

	go t.run()
	t.signal()
	return t
}

// signal asks for a rebuild without blocking. A pending request absorbs the
// new one — that is the whole point of the buffered channel.
func (t *tray) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *tray) run() {
	heartbeat := time.NewTicker(trayHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-heartbeat.C:
			t.rebuild()
		case <-t.wake:
			// Let the burst finish before reading anything: a fleet probe
			// publishes one event per server.
			time.Sleep(trayCoalesce)
			select {
			case <-t.wake:
			default:
			}
			t.rebuild()
		}
	}
}

// rebuild reads the current state and installs the menu it produces, if that
// menu differs from the one already up.
func (t *tray) rebuild() {
	st := trayState{
		Status:      t.hub.Status(),
		CloseToTray: t.hub.WindowPreferences().CloseToTray,
		OwnsDaemon:  t.hub.OwnsDaemon(),
	}
	if st.Status.Connected {
		ctx, cancel := context.WithTimeout(context.Background(), trayReadTimeout)
		servers, err := t.hub.ListServers(ctx)
		cancel()
		// A failed read leaves ServersKnown false, so the menu says "Loading…"
		// rather than "no servers" — an empty list and an unanswered question
		// must never look the same.
		if err == nil {
			st.Servers, st.ServersKnown = servers, true
		}
	}

	items := trayMenu(st)
	sig := traySignature(items)
	icon := trayIconFor(st)

	t.mu.Lock()
	menuChanged := sig != t.sig
	iconChanged := !t.iconSet || icon != t.icon
	t.sig, t.icon, t.iconSet = sig, icon, true
	t.mu.Unlock()

	if menuChanged {
		t.item.SetMenu(renderTrayMenu(items, t.onAction))
		t.item.SetTooltip(trayTooltip(st))
	}
	if iconChanged {
		t.applyIcon(icon)
	}
}

// applyIcon installs the drawn glyph. A failure to render is survivable — the
// previous icon stays up — and there is nowhere to report it from anyway.
func (t *tray) applyIcon(icon trayIcon) {
	if runtime.GOOS == "darwin" {
		// A template icon carries only alpha; macOS colours it for the menu
		// bar, including under a dark or tinted background.
		if png, err := trayIconPNG(icon, trayIconSizeMac, false); err == nil {
			t.item.SetTemplateIcon(png)
		}
		return
	}
	if png, err := trayIconPNG(icon, trayIconSize, false); err == nil {
		t.item.SetIcon(png)
	}
	if png, err := trayIconPNG(icon, trayIconSize, true); err == nil {
		t.item.SetDarkModeIcon(png)
	}
}

// renderTrayMenu turns the model into a native menu.
func renderTrayMenu(items []trayItem, on func(trayAction, string)) *application.Menu {
	menu := application.NewMenu()
	appendTrayItems(menu, items, on)
	return menu
}

func appendTrayItems(menu *application.Menu, items []trayItem, on func(trayAction, string)) {
	for _, it := range items {
		switch {
		case it.Separator:
			menu.AddSeparator()
		case len(it.Items) > 0:
			appendTrayItems(menu.AddSubmenu(it.Label), it.Items, on)
		default:
			var mi *application.MenuItem
			if it.Checkbox {
				mi = menu.AddCheckbox(it.Label, it.Checked)
			} else {
				mi = menu.Add(it.Label)
			}
			if it.Tooltip != "" {
				mi.SetTooltip(it.Tooltip)
			}
			// A readout is disabled, so that nothing in this menu looks
			// clickable without being so.
			mi.SetEnabled(!it.Disabled && it.Action != trayActionNone)
			mi.OnClick(func(*application.Context) { on(it.Action, it.Arg) })
		}
	}
}

// onAction performs one menu click.
func (t *tray) onAction(action trayAction, arg string) {
	switch action {
	case trayActionOpen:
		t.show(arg)
	case trayActionStartHub:
		// Off the UI thread: starting a daemon means spawning a process and
		// waiting for it to bind a socket, and the menu must not be frozen
		// while that happens.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = t.hub.Connect(ctx) //nolint:errcheck // reported through the status event
			t.signal()
		}()
	case trayActionCopySocket:
		t.app.Clipboard.SetText(t.hub.Status().Socket)
	case trayActionCloseToTray:
		t.hub.ToggleCloseToTray()
		t.signal()
	case trayActionQuit:
		t.app.Quit()
	case trayActionNone:
	}
}

// show brings the window back, optionally on a specific page.
//
// The route is emitted BEFORE the window appears so the user does not watch it
// change pages after it opens. The webview is still loaded while the window is
// hidden, so the listener is there to receive it.
func (t *tray) show(route string) {
	if route != "" {
		t.app.Event.Emit(services.EventNavigate, route)
	}
	// On macOS the whole application can be hidden (Cmd-H), and a window
	// shown while that is true stays invisible.
	//
	// InvokeSync, and NOT because it is tidy: App.Show is one of the few Wails
	// calls that does not hop to the main thread for you. Window.Show, Focus,
	// Hide, App.Quit and the clipboard all do it internally, which makes the
	// exception invisible at the call site. A menu click runs on its own
	// goroutine, AppKit traps a cross-thread [NSApp unhide:], and Wails turns
	// the resulting panic into os.Exit(1) — a tray item that quit the
	// application instead of opening it, with no crash report to explain it.
	application.InvokeSync(t.app.Show)
	t.win.Show()
	t.win.Focus()
}

// installCloseBehaviour makes the close button minimise instead of quitting.
//
// It hooks rather than listens, and that is the whole mechanism: Wails runs
// hooks synchronously before the listener that destroys the window, and a
// cancelled event stops there. The listener runs in its own goroutine, so
// cancelling from one would be a race with the destruction it is trying to
// prevent. Every platform this build supports maps its native close to
// Common.WindowClosing (mac:WindowShouldClose, windows:WindowClosing,
// linux:WindowDeleteEvent) and has already told the OS not to close the
// window itself, so the Go side is the only actor.
//
// trayReady is passed in rather than read from the Hub on purpose. The Hub's
// copy is bound and therefore settable from the webview; this decision must
// not be.
func installCloseBehaviour(app *application.App, hub *services.Hub, win *application.WebviewWindow, trayReady bool) {
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		prefs := hub.WindowPreferences()
		switch closeIntentFor(trayReady, prefs.CloseToTray, prefs.HideNoticeSeen) {
		case closeIntentQuit:
			// Cancel and quit explicitly, rather than letting the window
			// destroy itself and hoping the process follows. Whether it does
			// is per-platform (on macOS it is one option flag; elsewhere it is
			// whatever the last window's teardown happens to trigger), and the
			// user asked to close the application, not to find out.
			e.Cancel()
			app.Quit()
		case closeIntentHide:
			e.Cancel()
			win.Hide()
		case closeIntentAsk:
			// The dialog lives inside the window, so the window has to stay.
			e.Cancel()
			app.Event.Emit(services.EventConfirmClose, nil)
		}
	})

	if runtime.GOOS == "darwin" {
		// With no window on screen, the Dock icon is the other way back in —
		// clicking it must not be a no-op just because the window is hidden
		// rather than closed.
		app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) {
			win.Show()
			win.Focus()
		})
	}
}
