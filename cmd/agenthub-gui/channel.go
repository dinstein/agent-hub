package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/dinstein/agent-hub/api"
)

// The build channel, mirroring cmd/agenthub. "release" only when a build says
// so, via -ldflags "-X main.channel=release"; every other build is a
// development one.
//
// This file carries no build tag on purpose. The GUI has two mains — the real
// one behind `wails` and the placeholder without it — and a linker -X flag can
// only set a variable that is actually compiled in. Putting the channel in
// either main would silently do nothing for the other build.
var channel = "dev"

// applyChannel points a development build at the development data directory,
// by exporting AGENTHUB_DATA_DIR for this process.
//
// WHY THIS EXISTS. cmd/agenthub has had a channel since the dev/release split;
// the GUI never did. It resolves paths through the api package, which knows
// only the release directory name, so a `make gui` build read and wrote the
// INSTALLED release's data. Two consequences, and the second is the worse one:
//
//   - It could not see the development daemon at all. `make bin` produced a
//     daemon listening in AgentHubDev while the GUI looked in AgentHub, so a
//     developer running both got "daemon offline" with a daemon plainly
//     running.
//   - Debugging the GUI meant operating on real user data. That is exactly the
//     separation the dev channel was introduced to provide, absent on one side
//     of it — the dangerous kind of gap, because everything looks like it works
//     until the thing being edited turns out to be the real registry.
//
// An environment variable rather than a resolver argument, because it has to
// reach further than this process: the application runs `agenthub daemon start
// --foreground` as its own child (api.StartSupervised), and the child inherits
// the environment. One assignment therefore puts the GUI, the hub it runs, and
// anything either of them execs on the same directory. Passing a path through
// api's options would have fixed only the first of those.
//
// Failure direction: an AGENTHUB_DATA_DIR the user already set is left alone.
// An explicit override losing to a build flavour would be a surprise in the
// direction nobody wants — they named a directory and would get a different
// one. If the dev directory cannot be resolved at all, the process continues
// unchanged rather than refusing to start: the GUI's whole reason for opening
// when things are broken is to be the surface you diagnose them from.
func applyChannel() {
	if channel == "release" {
		return
	}
	if v, ok := os.LookupEnv("AGENTHUB_DATA_DIR"); ok && v != "" {
		return
	}
	dir, err := api.DevDataDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agenthub-gui: development build could not resolve its data directory: %v\n", err)
		fmt.Fprintln(os.Stderr, "agenthub-gui: continuing against the default directory, which is the RELEASE one")
		return
	}
	if err := os.Setenv("AGENTHUB_DATA_DIR", dir); err != nil {
		fmt.Fprintf(os.Stderr, "agenthub-gui: could not point this development build at %s: %v\n", dir, err)
	}
	applyChannelEndpoint()
}

// applyChannelEndpoint finishes the job on platforms where moving the data
// directory does not move the control endpoint.
//
// On Unix it does, which is why one assignment was enough for two years: the
// socket is <run>/ctl.sock, the run directory follows the data directory, and
// both sides — this GUI and the daemon it spawns — recompute the same path from
// the variable already set above. A Windows control endpoint is a named pipe
// whose name comes from the user's SID, so it follows nothing: without this, a
// development GUI would dial \\.\pipe\agenthub-ctl-<sha8(SID)>, which is the
// INSTALLED RELEASE's endpoint, and then operate on the release's servers and
// credentials while its own data directory sat unused. Everything would look
// like it worked.
//
// AGENTHUB_SOCKET, rather than passing a path into api.StartOptions, for the
// same reason applyChannel uses the environment at all: the hub is spawned as
// a child process, and it has to arrive at the same endpoint.
//
// Failure direction: a variable the user already set is left alone, and a
// resolution failure is reported and survived. The GUI's reason for opening
// when things are broken is to be the surface you diagnose them from.
func applyChannelEndpoint() { applyChannelEndpointFor(runtime.GOOS) }

// applyChannelEndpointFor is applyChannelEndpoint with the platform named, so
// the Windows branch is reachable from a test on the machines that run them.
func applyChannelEndpointFor(goos string) {
	// Windows only, and the narrowness is deliberate. Setting AGENTHUB_SOCKET
	// everywhere would pin the endpoint where it is currently DERIVED, and the
	// derivation has a rule this function does not know: on Linux the socket
	// leaves XDG_RUNTIME_DIR as soon as the data directory moves. Pinning it
	// here would freeze one answer to that question at GUI startup and hand the
	// same answer to a daemon that should have computed its own.
	if goos != "windows" {
		return
	}
	if v, ok := os.LookupEnv("AGENTHUB_SOCKET"); ok && v != "" {
		return
	}
	pipe, err := api.DevSocketPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agenthub-gui: development build could not resolve its control endpoint: %v\n", err)
		fmt.Fprintln(os.Stderr, "agenthub-gui: continuing against the default endpoint, which is the RELEASE one")
		return
	}
	if err := os.Setenv("AGENTHUB_SOCKET", pipe); err != nil {
		fmt.Fprintf(os.Stderr, "agenthub-gui: could not point this development build at %s: %v\n", pipe, err)
	}
}
