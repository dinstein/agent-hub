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

	// Held rather than constructed inline: the tray reads its state and the
	// close hook reads its preferences, so the assembly needs the instance
	// Wails is going to bind.
	hub := services.NewHubService(version)

	app := application.New(application.Options{
		Name:        "AgentHub",
		Description: "Local agent service hub",
		Services: []application.Service{
			application.NewService(hub),
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
			// FALSE now that a hidden window is a normal state. The flag is no
			// longer what ends the application either way: the close hook
			// decides, and where it decides to quit it says so explicitly, on
			// every platform rather than only this one. Leaving it true would
			// mean a window that really closes takes the process down while
			// the tray icon is still in the menu bar offering to reopen it.
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
	})

	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:      services.MainWindowName,
		Title:     "",
		Width:     1240,
		Height:    800,
		MinWidth:  900,
		MinHeight: 620,
		URL:       "/",
	})

	// The tray is optional by construction: nil means this platform gets the
	// behaviour it had before, with the close button ending the application.
	t := newTray(app, hub.Hub, win)
	hub.SetTrayAvailable(t != nil)
	installCloseBehaviour(app, hub.Hub, win, t != nil)

	// A termination signal has to reach the service too. Wails calls
	// ServiceShutdown on the way out of Quit, but a SIGTERM or SIGINT —
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
