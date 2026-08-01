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
// (docs/canonical.md §7 item 3).
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
