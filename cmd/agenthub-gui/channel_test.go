package main

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/api"
)

// A development GUI must resolve the DEVELOPMENT socket.
//
// This is the regression. cmd/agenthub has had a channel since the dev/release
// split; the GUI did not, and resolved paths through api, which knew only the
// release directory. So `make gui` produced a GUI that read the installed
// release's data and could not see the daemon `make bin` had just started —
// two processes both behaving correctly, pointed at different directories,
// with nothing failing to compile or run.
func TestDevChannelResolvesTheDevSocket(t *testing.T) {
	t.Setenv("AGENTHUB_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")

	before, err := api.DefaultSocketPath()
	if err != nil {
		t.Skipf("socket path unavailable on this platform: %v", err)
	}

	channel = "dev"
	applyChannel()

	after, err := api.DefaultSocketPath()
	if err != nil {
		t.Fatalf("socket path after applyChannel: %v", err)
	}
	if after == before {
		t.Fatalf("a dev build still resolves the release socket %q", after)
	}

	dev, err := api.DevDataDir()
	if err != nil {
		t.Fatalf("dev data dir: %v", err)
	}
	if !strings.HasPrefix(after, dev) {
		t.Errorf("socket %q is not under the dev data directory %q", after, dev)
	}
}

// A release build must not be redirected. The .app ships a release GUI beside
// a release CLI; if this ever moved the GUI, the two halves of one bundle
// would read different directories — the same bug, mirrored.
func TestReleaseChannelIsLeftAlone(t *testing.T) {
	t.Setenv("AGENTHUB_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")

	before, err := api.DefaultSocketPath()
	if err != nil {
		t.Skipf("socket path unavailable on this platform: %v", err)
	}

	channel = "release"
	applyChannel()

	if v, ok := os.LookupEnv("AGENTHUB_DATA_DIR"); ok && v != "" {
		t.Fatalf("a release build set AGENTHUB_DATA_DIR=%q", v)
	}
	after, err := api.DefaultSocketPath()
	if err != nil {
		t.Fatalf("socket path after applyChannel: %v", err)
	}
	if after != before {
		t.Errorf("a release build moved its socket from %q to %q", before, after)
	}
}

// On Windows the data directory is not enough: the control endpoint is a named
// pipe derived from the user's SID, so moving the directory leaves the endpoint
// exactly where it was — the installed release's. A development GUI would then
// drive the release daemon's servers, credentials and approvals with nothing
// appearing wrong.
//
// The platform is passed in because that branch is unreachable on the machines
// this suite runs on, and an untested branch on the one platform nobody can run
// is how the gap it closes got there in the first place.
func TestDevChannelPinsTheEndpointOnWindows(t *testing.T) {
	t.Setenv("AGENTHUB_DATA_DIR", "")
	t.Setenv("AGENTHUB_SOCKET", "")

	applyChannelEndpointFor("windows")

	got := os.Getenv("AGENTHUB_SOCKET")
	if got == "" {
		t.Fatal("AGENTHUB_SOCKET was not set: a dev GUI would dial the release endpoint")
	}
	want, err := api.DevSocketPath()
	if err != nil {
		t.Fatalf("dev socket path: %v", err)
	}
	if got != want {
		t.Errorf("AGENTHUB_SOCKET = %q, want the dev endpoint %q", got, want)
	}
}

// The same precedence as the data directory: an endpoint the user named must
// survive. Someone debugging two sandboxes at once passes AGENTHUB_SOCKET, and
// a build flavour that overrode it would break them in a way that looks like
// the tool is ignoring its own documented variable.
func TestExplicitSocketWinsOverTheChannel(t *testing.T) {
	const explicit = `\\.\pipe\somebody-elses-choice`
	t.Setenv("AGENTHUB_DATA_DIR", "")
	t.Setenv("AGENTHUB_SOCKET", explicit)

	applyChannelEndpointFor("windows")

	if got := os.Getenv("AGENTHUB_SOCKET"); got != explicit {
		t.Errorf("AGENTHUB_SOCKET = %q, want the explicit %q", got, explicit)
	}
}

// Off Windows the endpoint must stay DERIVED, not pinned. The socket follows
// the run directory, which follows the data directory — and on Linux it also
// leaves the shared XDG_RUNTIME_DIR the moment that directory moves. Freezing a
// path at GUI startup would answer that question once, for a daemon that has to
// answer it itself.
func TestDevChannelDoesNotPinTheEndpointOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the pinning branch is the correct behavior here")
	}
	t.Setenv("AGENTHUB_DATA_DIR", "")
	t.Setenv("AGENTHUB_SOCKET", "")

	channel = "dev"
	applyChannel()

	if got := os.Getenv("AGENTHUB_SOCKET"); got != "" {
		t.Errorf("AGENTHUB_SOCKET = %q on %s; the endpoint must stay derived here", got, runtime.GOOS)
	}
}

// An explicit override outranks the build flavour. Someone who names a
// directory has to get that directory; silently preferring the dev one
// because of how the binary was linked is a surprise in the direction nobody
// wants.
func TestExplicitDataDirWinsOverTheChannel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTHUB_DATA_DIR", dir)

	channel = "dev"
	applyChannel()

	if got := os.Getenv("AGENTHUB_DATA_DIR"); got != dir {
		t.Errorf("AGENTHUB_DATA_DIR = %q, want the explicit %q", got, dir)
	}
}
