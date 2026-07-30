//go:build !windows

package ctlapi

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dinstein/agent-hub/internal/platform"
)

// TestListenRefusesAPipePathOffWindows is the half of the pipe branch that the
// platforms this project runs on can actually execute.
//
// The path is reachable by accident, not by malice: AGENTHUB_SOCKET is honoured
// everywhere, so a pipe name travels in a copied command line or a config file
// written on the other machine. What must not happen is the Unix branch taking
// it — net.Listen("unix", `\\.\pipe\agenthub-ctl-…`) succeeds on Linux and
// macOS, creating a FILE whose name contains backslashes in the working
// directory and serving the control plane on it. Every step reports success;
// the operator gets an endpoint nothing dials and a stray file to find later.
func TestListenRefusesAPipePathOffWindows(t *testing.T) {
	// Run inside a temp dir so a regression that DOES create the stray file
	// leaves it somewhere the test framework cleans up.
	dir := t.TempDir()
	t.Chdir(dir)

	const pipe = `\\.\pipe\agenthub-ctl-deadbeef`
	l, err := Listen(pipe)
	if err == nil {
		_ = l.Close()
		t.Fatal("Listen accepted a Windows pipe path on this platform")
	}
	if !errors.Is(err, platform.ErrUnsupportedPlatform) {
		t.Errorf("Listen(%q) err = %v, want ErrUnsupportedPlatform", pipe, err)
	}

	// Nothing may have been created, under that spelling or any other.
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range entries {
		t.Errorf("Listen left %q behind in the working directory", filepath.Join(dir, e.Name()))
	}
}
