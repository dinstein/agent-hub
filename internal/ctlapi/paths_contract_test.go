package ctlapi

import (
	"testing"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/platform"
)

// TestAPIDefaultSocketPathContract pins the cross-package contract stated
// in api/paths.go: api.DefaultSocketPath (which cannot import
// internal/platform, depguard rule 1) must resolve byte-identically to
// platform.CtlSocketPath in every environment shape. This test lives here
// because internal/ctlapi may import both sides.
func TestAPIDefaultSocketPathContract(t *testing.T) {
	// Each case fully specifies the env vars both implementations read.
	cases := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "explicit socket override",
			env: map[string]string{
				"AGENTHUB_SOCKET":   "/x/y/ctl.sock",
				"AGENTHUB_DATA_DIR": "",
				"XDG_RUNTIME_DIR":   "",
				"XDG_DATA_HOME":     "",
			},
		},
		{
			name: "data dir override",
			env: map[string]string{
				"AGENTHUB_SOCKET":   "",
				"AGENTHUB_DATA_DIR": "/tmp/ah-data",
				"XDG_RUNTIME_DIR":   "",
				"XDG_DATA_HOME":     "",
			},
		},
		{
			name: "xdg runtime dir (linux-only effect)",
			env: map[string]string{
				"AGENTHUB_SOCKET":   "",
				"AGENTHUB_DATA_DIR": "",
				"XDG_RUNTIME_DIR":   "/run/user/1000",
				"XDG_DATA_HOME":     "",
			},
		},
		{
			// The interesting row on Linux (and the one CI actually exercises):
			// both variables set, and the data directory has to win. On macOS
			// it degenerates into the previous case, which is exactly why this
			// disagreement could exist unnoticed for as long as it did.
			name: "data dir override alongside xdg runtime dir",
			env: map[string]string{
				"AGENTHUB_SOCKET":   "",
				"AGENTHUB_DATA_DIR": "/tmp/ah-data",
				"XDG_RUNTIME_DIR":   "/run/user/1000",
				"XDG_DATA_HOME":     "",
			},
		},
		{
			name: "relative xdg values ignored",
			env: map[string]string{
				"AGENTHUB_SOCKET":   "",
				"AGENTHUB_DATA_DIR": "",
				"XDG_RUNTIME_DIR":   "relative/run",
				"XDG_DATA_HOME":     "relative/data",
			},
		},
		{
			name: "bare defaults",
			env: map[string]string{
				"AGENTHUB_SOCKET":   "",
				"AGENTHUB_DATA_DIR": "",
				"XDG_RUNTIME_DIR":   "",
				"XDG_DATA_HOME":     "",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			apiPath, apiErr := api.DefaultSocketPath()
			platPath, platErr := platform.CtlSocketPath()
			if (apiErr == nil) != (platErr == nil) {
				t.Fatalf("error disagreement: api=%v platform=%v", apiErr, platErr)
			}
			if apiErr != nil {
				return // both unsupported: contract holds
			}
			if apiPath != platPath {
				t.Errorf("api.DefaultSocketPath = %q, platform.CtlSocketPath = %q", apiPath, platPath)
			}
			if apiPath == "" {
				t.Error("empty socket path")
			}
		})
	}
}

// TestAPIDevDataDirContract is the other half of the same contract, and the
// half whose absence let a real bug through.
//
// api.DevDataDir exists for cmd/agenthub-gui, which cannot import
// internal/platform either. Before it did, the GUI resolved only the RELEASE
// directory, so a `make gui` build read the installed release's data and could
// not see the daemon `make bin` had started. Nothing failed to compile and
// nothing failed to run; the two sides simply pointed at different
// directories, which is precisely the failure a compiler cannot catch when the
// constant is duplicated on purpose.
func TestAPIDevDataDirContract(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"defaults", map[string]string{}},
		{"explicit data dir wins over the dev flavour", map[string]string{
			"AGENTHUB_DATA_DIR": "/tmp/explicit-agenthub",
		}},
		{"empty data dir is ignored", map[string]string{
			"AGENTHUB_DATA_DIR": "",
		}},
		{"xdg data home", map[string]string{
			"XDG_DATA_HOME": "/tmp/xdg-data",
		}},
		{"relative xdg data home is ignored", map[string]string{
			"XDG_DATA_HOME": "relative/not-absolute",
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENTHUB_DATA_DIR", "")
			t.Setenv("XDG_DATA_HOME", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			apiPath, apiErr := api.DevDataDir()
			platPath, platErr := platform.DevResolver(nil).DataDir()

			if (apiErr == nil) != (platErr == nil) {
				t.Fatalf("api.DevDataDir err=%v, platform dev DataDir err=%v", apiErr, platErr)
			}
			if apiErr != nil {
				return
			}
			if apiPath != platPath {
				t.Errorf("api.DevDataDir = %q, platform dev DataDir = %q", apiPath, platPath)
			}
		})
	}
}

// TestDevAndReleaseDirectoriesDiffer is the invariant the whole channel split
// rests on. If these ever resolved to the same path the separation would be
// gone while every test above still passed.
func TestDevAndReleaseDirectoriesDiffer(t *testing.T) {
	t.Setenv("AGENTHUB_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")

	// The release side has no exported api helper — the GUI never needed one,
	// it goes through DefaultSocketPath. platform is the authority here and
	// ctlapi may import it.
	release, err := platform.DataDir()
	if err != nil {
		t.Skipf("release data dir unavailable on this platform: %v", err)
	}
	dev, err := api.DevDataDir()
	if err != nil {
		t.Fatalf("dev data dir: %v", err)
	}
	if release == dev {
		t.Fatalf("dev and release resolve to the same directory %q: the channel split is a no-op", release)
	}
}
