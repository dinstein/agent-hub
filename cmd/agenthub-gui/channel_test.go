package main

import (
	"os"
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
