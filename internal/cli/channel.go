package cli

import "github.com/dinstein/agent-hub/internal/platform"

// ChannelRelease is the one build channel that means "shipped". It is compared
// against the value cmd/agenthub stamps in via
// -ldflags "-X main.channel=release".
const ChannelRelease = "release"

// ForChannel fills in every Options field that is decided by the build
// channel, and nothing else. It lives here rather than in cmd/agenthub because
// that package is a main with no test binary: a line there is unprovable by
// construction, and the channel decides both what data directory a build
// writes to and what its help page teaches. Both are silent when wrong.
//
// Failure direction: only the exact string "release" is a release build.
// Anything else — "dev", "", a typo, a forgotten -ldflags — resolves to the
// development build, which is the recoverable side of every consequence
// below. A release build mistaken for dev shows an empty sandbox and a
// slightly wider help page, both of which the user can see. A dev build
// mistaken for release writes to the installed copy's registry and can spend
// a one-time OAuth refresh token belonging to it, which noticing afterwards
// does not undo.
//
// The three consequences are derived from one comparison so they cannot drift
// into disagreeing about which build this is.
func (o *Options) ForChannel(channel string) {
	release := channel == ChannelRelease

	// A shipped build leads with the everyday path and keeps the scope,
	// governance and operate groups off the help page. They stay registered
	// and stay runnable, so this decides what the binary TEACHES, never what
	// it can do.
	o.ReducedHelp = release

	if !release {
		// Marks a development build in `--version`. A release build appends
		// nothing, so a shipped binary reports exactly its tagged version.
		o.Version += " (dev)"
		// platform stays a pure resolver: it is told which directory to use,
		// it does not know that build flavours exist.
		o.Resolver = platform.DevResolver(nil)
	}
}
