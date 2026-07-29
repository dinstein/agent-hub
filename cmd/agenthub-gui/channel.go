package main

import (
	"fmt"
	"os"

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
// reach further than this process: DialOrStart spawns `agenthub daemon start`,
// and the child inherits the environment. One assignment therefore puts the
// GUI, the daemon it starts, and anything either of them execs on the same
// directory. Passing a path through api's options would have fixed only the
// first of those.
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
}
