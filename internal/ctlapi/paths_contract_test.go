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

// TestWindowsEndpointContract is the same contract as
// TestAPIDefaultSocketPathContract, for the platform neither side can reach by
// running on it.
//
// The two tests above compare the real functions in the real environment, which
// means they only ever exercise the host's branch: on the Linux and macOS
// runners this project has, the Windows halves of api/paths.go and
// internal/platform are compared by nobody. And those halves are the ones most
// able to disagree — three frozen identifiers (two pipe names and the data
// directory), a SID hash, and a %APPDATA% fallback, written twice on purpose
// because api may not import internal/platform (canonical.md §2 rule 1).
//
// So both sides are driven through their injectable seams with goos forced to
// "windows". The platform resolver is told the process is NOT inside an MSIX
// container, which is not a convenience: it is the assertion that api's copy
// omitting the container escape is correct, because api's only caller — a
// desktop GUI the user launches — cannot be inside one. A packaged process
// resolves its paths through internal/platform, which does have the escape.
func TestWindowsEndpointContract(t *testing.T) {
	const (
		sid     = "S-1-5-21-1111-2222-3333-1001"
		appData = `C:\Users\alice\AppData\Roaming`
		home    = `C:\Users\alice`
	)

	env := map[string]string{"APPDATA": appData}
	plat := &platform.Resolver{
		GOOS: "windows",
		LookupEnv: func(k string) (string, bool) {
			v, ok := env[k]
			return v, ok
		},
		UserHomeDir:     func() (string, error) { return home, nil },
		UserSID:         func() (string, error) { return sid, nil },
		PackageIdentity: func() platform.PackageIdentity { return platform.PackageIdentity{Packaged: false} },
	}

	// WHY LITERALS AND NOT api's OWN FUNCTIONS. api resolves through
	// runtime.GOOS, and its injectable seams are unexported — reachable only
	// from inside the package, and a test file in api/ may not import
	// internal/platform (depguard rule 1), which is the same reason this test
	// lives here. Exporting the seams to let this test call them would put
	// test scaffolding in a public API to check a private duplication.
	//
	// So the pinning is done in two halves that meet at these strings: this
	// test asserts internal/platform produces them, and api's own
	// TestDefaultSocketPath / TestDevSocketPathSeparatesFromRelease assert api
	// produces the same ones. Changing either side's constant fails one of the
	// two tests, and the failure names the other copy.
	release, err := plat.CtlSocketPath()
	if err != nil {
		t.Fatalf("platform release endpoint: %v", err)
	}
	dev, err := platform.DevResolver(plat).CtlSocketPath()
	if err != nil {
		t.Fatalf("platform dev endpoint: %v", err)
	}
	// sha8(sid). Written out rather than recomputed here: recomputing it with
	// the same two lines of code both sides use would assert only that
	// SHA-256 is deterministic. api/paths_test.go pins the same digest.
	const hash = "7abec184"
	for _, tc := range []struct {
		what string
		got  string
		want string
	}{
		{"release pipe", release, `\\.\pipe\agenthub-ctl-` + hash},
		{"dev pipe", dev, `\\.\pipe\agenthub-ctl-dev-` + hash},
		{"data dir", mustDataDir(t, plat), appData + `\AgentHub`},
		{"dev data dir", mustDataDir(t, platform.DevResolver(plat)), appData + `\AgentHubDev`},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: internal/platform resolves %q, and the frozen spelling is %q.\n"+
				"api/paths.go carries its own copy of this value (it may not import "+
				"internal/platform) and asserts the same string in api's own tests. If this "+
				"change is intended, both must move together — otherwise the GUI dials an "+
				"endpoint no daemon serves, or reads a directory no daemon writes, with both "+
				"sides looking correct.", tc.what, tc.got, tc.want)
		}
	}
}

func mustDataDir(t *testing.T, r *platform.Resolver) string {
	t.Helper()
	dir, err := r.DataDir()
	if err != nil {
		t.Fatalf("data dir: %v", err)
	}
	return dir
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

// TestAPITopicsMatchTheServedSet pins the other cross-package contract in
// this direction: api's four Topic constants and the set this server actually
// serves must be the same set, in both directions.
//
// api/events.go states the rule and why it is not a soft one — the daemon's
// set is CLOSED, so an unlisted name is a 400 rather than a subscription that
// quietly delivers nothing, and "leaving the constant behind does not degrade
// to 'that topic is empty', it takes the whole subscription down with it."
// Until this test existed nothing checked it, and neither direction is
// visible from the side that breaks it:
//
//   - a constant left in api after the daemon retires the topic makes every
//     Subscribe that includes it fail, including the ones asking for
//     unrelated topics in the same call;
//   - a topic the daemon starts serving without a constant here is one no
//     api consumer — the GUI included — can name.
//
// It lives in this package because api may not import internal/* (depguard
// rule 1) while internal/ctlapi may import both sides, the same reason
// TestAPIDefaultSocketPathContract is here.
func TestAPITopicsMatchTheServedSet(t *testing.T) {
	declared := map[string]string{
		api.TopicServers:  "api.TopicServers",
		api.TopicSessions: "api.TopicSessions",
		api.TopicActivity: "api.TopicActivity",
		api.TopicSkills:   "api.TopicSkills",
	}
	if len(declared) != 4 {
		t.Fatalf("api declares %d distinct topics, want 4 — two constants collided", len(declared))
	}
	for name, constant := range declared {
		if !sseTopics[name] {
			t.Errorf("%s = %q is not in the served set, so Subscribe(%q) is a 400 that "+
				"fails the whole call — including its unrelated topics. Retire the constant "+
				"in the same change that retires the topic.", constant, name, name)
		}
	}
	for name := range sseTopics {
		if _, ok := declared[name]; !ok {
			t.Errorf("this server serves %q and api declares no constant for it, so no api "+
				"consumer can name it. Add it to api/events.go in the same change.", name)
		}
	}
}
