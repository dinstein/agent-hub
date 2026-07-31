//go:build wails

// Command agenthub-gui is the optional Wails3 desktop GUI (canonical.md §1).
// This file is the real application; the default build gets the placeholder
// in main.go instead (see there for why).
package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/dinstein/agent-hub/cmd/agenthub-gui/services"
)

// assets is the Vite build output. `make gui` builds the frontend first;
// a stale or missing dist is a build error rather than a blank window, which
// is why the embed points at the directory instead of an optional handler.
//
//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// FIRST, before anything resolves a path: a development build has to point
	// itself at the development data directory, and the variable it sets is
	// inherited by the daemon this process may go on to spawn.
	applyChannel()

	dist, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		log.Fatalf("agenthub-gui: embedded assets: %v", err)
	}
	if _, err := fs.Stat(dist, "index.html"); err != nil {
		fmt.Fprintln(os.Stderr,
			"agenthub-gui: frontend/dist/index.html is missing — run `make gui-frontend` first")
		os.Exit(1)
	}

	app := application.New(application.Options{
		Name:        "agenthub",
		Description: "Local agent service hub",
		Services: []application.Service{
			application.NewService(services.NewHubService()),
		},
		// MarshalError keeps the control-plane error CODE reachable from the
		// frontend (it arrives as the rejection's `cause`), so pages can tell
		// "daemon offline" from "endpoint not served yet" from "someone else
		// wrote this first".
		MarshalError: services.MarshalError,
		Assets: application.AssetOptions{
			Handler: application.BundledAssetFileServer(dist),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:      "main",
		Title:     "agenthub",
		Width:     1100,
		Height:    760,
		MinWidth:  720,
		MinHeight: 480,
		URL:       "/",
	})

	// A termination signal has to reach the service too. Wails calls
	// ServiceShutdown when the LAST WINDOW closes, but a SIGTERM or SIGINT —
	// a logout, a `kill`, Ctrl-C on a foreground run — ends the process
	// without that path ever running, and a daemon this GUI started would be
	// left behind. Quit unwinds through the same shutdown Wails uses, so the
	// cleanup is written once.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		app.Quit()
	}()

	if err := app.Run(); err != nil {
		log.Fatalf("agenthub-gui: %v", err)
	}
}
