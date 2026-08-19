//go:build !wails

// Command agenthub-gui is the optional Wails3 desktop GUI (docs/conventions.md#frozen-identifiers).
//
// This file is the placeholder produced by the DEFAULT build. The real GUI
// lives behind the `wails` build tag (gui_main.go) because a webview build
// needs GTK/WebKit development packages that CI runners do not have; keeping
// the default `go build ./...` and `golangci-lint run` free of that
// dependency is what lets the GUI live in the same module as the daemon
// (docs/decisions/0003-wails3-and-the-frontend-stack.md, ruling A.6 #3).
//
//	make gui        # build the real thing (frontend + -tags wails)
//
// Constraint (docs/conventions.md#dependency-directions rule 1, enforced by depguard and proven by
// internal/depguardtest): this package must never import internal/*. It may
// only talk to the daemon through the public api package (control-plane
// DTOs + Go client), exactly like any third-party integration.
package main

// The Health display constants (docs/subsystems/controlplane.md) are generated out of the api
// package into the frontend so that Go, TypeScript and the wire cannot
// drift. Regenerate with `go generate ./cmd/agenthub-gui/...`; the golden
// test in internal/healthgen fails while the checked-in file is stale.
//
//go:generate go run ./internal/healthgen/cmd/healthts -api ../../api -out frontend/src/generated/health.ts

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintf(os.Stderr,
		"agenthub-gui %s: this binary was built without the GUI.\n"+
			"Build the real one with `make gui` (frontend + `go build -tags wails ./cmd/agenthub-gui`).\n"+
			"The agenthub CLI can do everything the GUI can — the GUI is optional by construction.\n",
		version)
	os.Exit(1)
}
