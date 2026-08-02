package services

// Window-local state: the two preferences that decide what the close button
// does, and the daemon-ownership answer the tray needs to label Quit.
//
// WHY THE PREFERENCES LIVE IN MEMORY HERE AND ON DISK IN THE FRONTEND. This
// package may not read or write the data directory (canonical.md §2 rule 1),
// and the daemon's registry is the wrong home for them anyway: "the close
// button minimises" is a property of THIS window on THIS machine, exactly like
// the theme, and storing it in the registry would imply the hub has an opinion
// about it. So localStorage remains the durable copy, the frontend pushes it
// in at startup, and this holds the runtime answer the Go side acts on.
//
// The direction of the sync is therefore: frontend → SetWindowPreferences on
// startup and whenever the user changes it; Go → EventWindowPrefs when the
// TRAY changed it, which the frontend persists. The frontend never answers
// that event by calling back, or the two would ring.

// Additional emitted event names (the topic events are in hub.go).
const (
	// EventWindowPrefs carries a WindowPrefs the Go side changed — today,
	// the tray's checkbox. The frontend persists it and re-renders.
	EventWindowPrefs = EventPrefix + "window-prefs"
	// EventNavigate carries a hash route the window should show. Emitted
	// when the tray sends the user somewhere specific.
	EventNavigate = EventPrefix + "navigate"
	// EventConfirmClose asks the frontend to run the one-time "this will
	// keep running in the tray" dialog. The window has already been shown
	// again when it arrives, because the dialog is inside the window.
	EventConfirmClose = EventPrefix + "confirm-close"
)

// MainWindowName is the name the application's only window is registered
// under. Both the assembly that creates it and the window-control methods
// that look it up spell it from here, so a rename cannot leave one of them
// addressing a window that no longer answers.
const MainWindowName = "main"

// WindowPrefs is the window-local preference set.
type WindowPrefs struct {
	// CloseToTray makes the close button hide the window instead of ending
	// the application.
	CloseToTray bool `json:"closeToTray"`
	// HideNoticeSeen records that the user has been told, once, that the
	// application keeps running after the window disappears.
	HideNoticeSeen bool `json:"hideNoticeSeen"`
}

// defaultWindowPrefs is what a GUI assumes before the frontend has pushed
// anything — which includes the whole of startup, and every build where
// localStorage is unavailable (it throws in some embedded webviews).
//
// Minimising is the default because it is the behaviour the tray exists to
// provide; the notice is unseen by default because assuming otherwise would
// silently hide a window for a user who was never told it would come back.
func defaultWindowPrefs() WindowPrefs { return WindowPrefs{CloseToTray: true} }

// WindowPreferences returns the current values.
//
// An atomic rather than the Hub mutex: this is read on the UI thread from
// inside the window-closing hook, and that thread must not be able to queue
// behind a control-plane call that happens to be holding the lock.
func (h *Hub) WindowPreferences() WindowPrefs {
	if p := h.prefs.Load(); p != nil {
		return *p
	}
	return defaultWindowPrefs()
}

// SetWindowPreferences replaces them and returns what is now in effect.
// It does not emit: the caller decides, because the frontend pushing its own
// stored value back is not news, while the tray toggling it is.
func (h *Hub) SetWindowPreferences(p WindowPrefs) WindowPrefs {
	h.prefs.Store(&p)
	return p
}

// setWindowPreferencesFromTray is the other direction: it stores and tells the
// frontend, which owns the durable copy.
func (h *Hub) setWindowPreferencesFromTray(p WindowPrefs) WindowPrefs {
	out := h.SetWindowPreferences(p)
	h.emit(EventWindowPrefs, out)
	return out
}

// SetTrayAvailable records whether a tray icon actually came up. The tray
// assembly calls it once at startup.
//
// DISPLAY ONLY. The close button's own decision reads the assembly's flag
// directly, not this one, and the split is deliberate: this value is bound and
// therefore settable from the webview, and a wrong `true` reaching the close
// path would hide the window into a status area that is not there — a running
// process with no reachable surface. The worst a wrong value can do here is
// mislabel a checkbox in Settings.
func (h *Hub) SetTrayAvailable(v bool) { h.trayAvailable.Store(v) }

// TrayAvailable reports what the assembly recorded, so Settings can explain
// why the close-to-tray preference is unavailable instead of showing a switch
// that silently does nothing.
func (h *Hub) TrayAvailable() bool { return h.trayAvailable.Load() }

// OwnsDaemon reports whether the daemon this GUI is connected to was started
// BY this GUI — the same claim that licenses stop to shut it down again.
//
// It exists so the tray can say so on the Quit item. Once the close button
// stops quitting, Quit is the only path that reaches that shutdown, which
// makes the label the last place the consequence can be stated.
func (h *Hub) OwnsDaemon() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.spawned
}
