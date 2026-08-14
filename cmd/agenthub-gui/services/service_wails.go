//go:build wails

package services

import (
	"context"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// HubService is the Wails-bound service. It is a thin shell around Hub: the
// embedded pointer's exported methods are the bound method set (Wails binds
// promoted methods), so nothing here decides what the frontend can call.
//
// This is the ONLY file in the package that imports Wails, and it is behind
// the `wails` build tag: CI runners have no GTK/WebKit development packages,
// so the default `go build ./...` must not reach a webview dependency
// (docs/decisions/0003-wails3-and-the-frontend-stack.md).
type HubService struct {
	*Hub
}

// NewHubService returns the service instance to register with the
// application. The emitter is wired lazily in ServiceStartup, because the
// application does not exist yet at construction time.
func NewHubService(buildVersion string) *HubService {
	h := NewHub(nil)
	h.buildVersion = buildVersion
	return &HubService{Hub: h}
}

// ServiceName names the service in Wails logs.
func (s *HubService) ServiceName() string { return "agenthub.HubService" }

// ServiceStartup connects to the daemon and starts the SSE bridge.
//
// It deliberately returns nil even when the daemon is unreachable: a
// non-nil error aborts application startup, and a GUI that refuses to open
// because the daemon is down leaves the user with no surface to diagnose it
// from. The failure is reported through the daemon status event instead, and
// every data call fails with ErrOffline until Connect succeeds.
func (s *HubService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	app := application.Get()
	s.Hub.emitter = EmitterFunc(func(name string, data any) {
		app.Event.Emit(name, data)
	})
	s.Hub.start(ctx)
	return nil
}

// ServiceShutdown stops the bridge and releases the control connection.
func (s *HubService) ServiceShutdown() error {
	s.Hub.stop()
	return nil
}

// ---------------------------------------------------------------------------
// Window control. These are the only bound methods that do not map to a
// control-plane call, and they are not a GUI privilege: hiding a window is not
// something a CLI could be missing. They live in this file because they are
// the one thing that genuinely needs Wails, and docs/decisions/0003-wails3-and-the-frontend-stack.md keeps
// that dependency inside tagged assembly.
//
// The frontend calls them from the one-time close dialog, which is the only
// place a user answers "keep running" or "quit" — the tray drives the window
// directly from Go and does not come through here.
// ---------------------------------------------------------------------------

// HideWindow minimises the application to the tray.
//
// A no-op when the window has already gone: the dialog that calls this can
// only be on screen while it exists, but the answer arrives asynchronously and
// a quit racing it must not panic on the way out.
func (s *HubService) HideWindow() {
	if w, ok := application.Get().Window.GetByName(MainWindowName); ok {
		w.Hide()
	}
}

// QuitApplication ends the process through the normal shutdown path, so
// ServiceShutdown still runs and a daemon this GUI started is still stopped.
// It is deliberately not an os.Exit: skipping that teardown would strand the
// daemon this process is responsible for.
func (s *HubService) QuitApplication() {
	application.Get().Quit()
}
