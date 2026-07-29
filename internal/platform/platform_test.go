package platform_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/platform"
)

// fakeResolver builds a Resolver with an injected OS, env map and home dir,
// so tests never mutate real process state.
func fakeResolver(goos string, env map[string]string, home string) *platform.Resolver {
	return &platform.Resolver{
		GOOS: goos,
		LookupEnv: func(key string) (string, bool) {
			v, ok := env[key]
			return v, ok
		},
		UserHomeDir: func() (string, error) {
			if home == "" {
				return "", errors.New("no home")
			}
			return home, nil
		},
	}
}

func TestPathResolution(t *testing.T) {
	type want struct {
		path string // expected path; empty means an error is expected
		err  error  // sentinel to match with errors.Is; nil means any error
	}
	tests := []struct {
		name    string
		goos    string
		env     map[string]string
		home    string
		resolve func(*platform.Resolver) (string, error)
		want    want
	}{
		// --- DataDir ---
		{
			name: "darwin default data dir",
			goos: "darwin", home: "/Users/alice",
			resolve: (*platform.Resolver).DataDir,
			want:    want{path: "/Users/alice/Library/Application Support/AgentHub"},
		},
		{
			name: "linux default data dir without XDG_DATA_HOME",
			goos: "linux", home: "/home/alice",
			resolve: (*platform.Resolver).DataDir,
			want:    want{path: "/home/alice/.local/share/AgentHub"},
		},
		{
			name: "linux XDG_DATA_HOME wins over home fallback",
			goos: "linux", home: "/home/alice",
			env:     map[string]string{"XDG_DATA_HOME": "/mnt/data"},
			resolve: (*platform.Resolver).DataDir,
			want:    want{path: "/mnt/data/AgentHub"},
		},
		{
			name: "linux relative XDG_DATA_HOME ignored per XDG spec",
			goos: "linux", home: "/home/alice",
			env:     map[string]string{"XDG_DATA_HOME": "relative/dir"},
			resolve: (*platform.Resolver).DataDir,
			want:    want{path: "/home/alice/.local/share/AgentHub"},
		},
		{
			name: "AGENTHUB_DATA_DIR beats XDG_DATA_HOME on linux",
			goos: "linux", home: "/home/alice",
			env: map[string]string{
				"AGENTHUB_DATA_DIR": "/custom/hub",
				"XDG_DATA_HOME":     "/mnt/data",
			},
			resolve: (*platform.Resolver).DataDir,
			want:    want{path: "/custom/hub"},
		},
		{
			name: "AGENTHUB_DATA_DIR beats platform default on darwin",
			goos: "darwin", home: "/Users/alice",
			env:     map[string]string{"AGENTHUB_DATA_DIR": "/custom/hub"},
			resolve: (*platform.Resolver).DataDir,
			want:    want{path: "/custom/hub"},
		},
		{
			name: "empty AGENTHUB_DATA_DIR is ignored",
			goos: "darwin", home: "/Users/alice",
			env:     map[string]string{"AGENTHUB_DATA_DIR": ""},
			resolve: (*platform.Resolver).DataDir,
			want:    want{path: "/Users/alice/Library/Application Support/AgentHub"},
		},
		{
			name: "windows data dir from APPDATA",
			goos: "windows", home: `C:\Users\alice`,
			env:     map[string]string{"APPDATA": `C:\Users\alice\AppData\Roaming`},
			resolve: (*platform.Resolver).DataDir,
			want:    want{path: `C:\Users\alice\AppData\Roaming\AgentHub`},
		},
		{
			name: "windows data dir falls back to home when APPDATA is unset",
			goos: "windows", home: `C:\Users\alice`,
			resolve: (*platform.Resolver).DataDir,
			want:    want{path: `C:\Users\alice\AppData\Roaming\AgentHub`},
		},
		{
			name: "unknown platform still unsupported",
			goos: "plan9", home: "/usr/alice",
			resolve: (*platform.Resolver).DataDir,
			want:    want{err: platform.ErrUnsupportedPlatform},
		},
		{
			name: "darwin home resolution failure propagates",
			goos: "darwin", home: "",
			resolve: (*platform.Resolver).DataDir,
			want:    want{},
		},

		// --- RegistryDir ---
		{
			name: "registry defaults under data dir",
			goos: "darwin", home: "/Users/alice",
			resolve: (*platform.Resolver).RegistryDir,
			want:    want{path: "/Users/alice/Library/Application Support/AgentHub/registry"},
		},
		{
			name: "AGENTHUB_REGISTRY overrides independently of data dir",
			goos: "linux", home: "/home/alice",
			env: map[string]string{
				"AGENTHUB_DATA_DIR": "/custom/hub",
				"AGENTHUB_REGISTRY": "/elsewhere/registry",
			},
			resolve: (*platform.Resolver).RegistryDir,
			want:    want{path: "/elsewhere/registry"},
		},
		{
			name: "registry follows AGENTHUB_DATA_DIR override",
			goos: "linux", home: "/home/alice",
			env:     map[string]string{"AGENTHUB_DATA_DIR": "/custom/hub"},
			resolve: (*platform.Resolver).RegistryDir,
			want:    want{path: "/custom/hub/registry"},
		},

		// --- Derived dirs ---
		{
			name: "logs dir under data",
			goos: "linux", home: "/home/alice",
			resolve: (*platform.Resolver).LogsDir,
			want:    want{path: "/home/alice/.local/share/AgentHub/logs"},
		},
		{
			name: "cache dir under data",
			goos: "linux", home: "/home/alice",
			resolve: (*platform.Resolver).CacheDir,
			want:    want{path: "/home/alice/.local/share/AgentHub/cache"},
		},
		{
			name: "state dir under data",
			goos: "darwin", home: "/Users/alice",
			resolve: (*platform.Resolver).StateDir,
			want:    want{path: "/Users/alice/Library/Application Support/AgentHub/state"},
		},

		// --- RunDir ---
		{
			name: "linux run dir prefers XDG_RUNTIME_DIR",
			goos: "linux", home: "/home/alice",
			env:     map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"},
			resolve: (*platform.Resolver).RunDir,
			want:    want{path: "/run/user/1000/AgentHub"},
		},
		{
			name: "linux run dir falls back to data/run without XDG_RUNTIME_DIR",
			goos: "linux", home: "/home/alice",
			resolve: (*platform.Resolver).RunDir,
			want:    want{path: "/home/alice/.local/share/AgentHub/run"},
		},
		{
			name: "linux relative XDG_RUNTIME_DIR ignored",
			goos: "linux", home: "/home/alice",
			env:     map[string]string{"XDG_RUNTIME_DIR": "run/user"},
			resolve: (*platform.Resolver).RunDir,
			want:    want{path: "/home/alice/.local/share/AgentHub/run"},
		},
		{
			// The socket must follow a relocated data directory: XDG_RUNTIME_DIR
			// is one directory per user, so honouring it here would put two
			// differently-sandboxed agenthubs on one ctl.sock.
			name: "linux AGENTHUB_DATA_DIR moves the run dir out of XDG_RUNTIME_DIR",
			goos: "linux", home: "/home/alice",
			env: map[string]string{
				"AGENTHUB_DATA_DIR": "/custom/hub",
				"XDG_RUNTIME_DIR":   "/run/user/1000",
			},
			resolve: (*platform.Resolver).RunDir,
			want:    want{path: "/custom/hub/run"},
		},
		{
			name: "linux empty AGENTHUB_DATA_DIR still allows XDG_RUNTIME_DIR",
			goos: "linux", home: "/home/alice",
			env: map[string]string{
				"AGENTHUB_DATA_DIR": "",
				"XDG_RUNTIME_DIR":   "/run/user/1000",
			},
			resolve: (*platform.Resolver).RunDir,
			want:    want{path: "/run/user/1000/AgentHub"},
		},
		{
			name: "darwin run dir ignores XDG_RUNTIME_DIR",
			goos: "darwin", home: "/Users/alice",
			env:     map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"},
			resolve: (*platform.Resolver).RunDir,
			want:    want{path: "/Users/alice/Library/Application Support/AgentHub/run"},
		},
		{
			name: "windows run dir under data dir",
			goos: "windows", home: `C:\Users\alice`,
			env:     map[string]string{"APPDATA": `C:\Users\alice\AppData\Roaming`},
			resolve: (*platform.Resolver).RunDir,
			want:    want{path: `C:\Users\alice\AppData\Roaming\AgentHub\run`},
		},
		{
			name: "unknown platform run dir unsupported",
			goos: "plan9", home: "/usr/alice",
			resolve: (*platform.Resolver).RunDir,
			want:    want{err: platform.ErrUnsupportedPlatform},
		},

		// --- CtlSocketPath ---
		{
			name: "ctl socket under linux XDG runtime dir",
			goos: "linux", home: "/home/alice",
			env:     map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"},
			resolve: (*platform.Resolver).CtlSocketPath,
			want:    want{path: "/run/user/1000/AgentHub/ctl.sock"},
		},
		{
			name: "ctl socket under darwin data run dir",
			goos: "darwin", home: "/Users/alice",
			resolve: (*platform.Resolver).CtlSocketPath,
			want:    want{path: "/Users/alice/Library/Application Support/AgentHub/run/ctl.sock"},
		},
		{
			name: "AGENTHUB_SOCKET overrides ctl socket path",
			goos: "linux", home: "/home/alice",
			env:     map[string]string{"AGENTHUB_SOCKET": "/tmp/test-ctl.sock"},
			resolve: (*platform.Resolver).CtlSocketPath,
			want:    want{path: "/tmp/test-ctl.sock"},
		},
		{
			name: "AGENTHUB_SOCKET works even on windows",
			goos: "windows", home: `C:\Users\alice`,
			env:     map[string]string{"AGENTHUB_SOCKET": "/tmp/test-ctl.sock"},
			resolve: (*platform.Resolver).CtlSocketPath,
			want:    want{path: "/tmp/test-ctl.sock"},
		},
		{
			name: "unknown platform ctl socket unsupported without override",
			goos: "plan9", home: "/usr/alice",
			resolve: (*platform.Resolver).CtlSocketPath,
			want:    want{err: platform.ErrUnsupportedPlatform},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := fakeResolver(tt.goos, tt.env, tt.home)
			got, err := tt.resolve(r)
			if tt.want.path != "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tt.want.path {
					t.Fatalf("got %q, want %q", got, tt.want.path)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error, got path %q", got)
			}
			if tt.want.err != nil && !errors.Is(err, tt.want.err) {
				t.Fatalf("error %v does not match sentinel %v", err, tt.want.err)
			}
		})
	}
}

// A development build must not be able to touch an installed release's data,
// and the two directories must stay siblings so neither can be reached by
// walking out of the other.
//
// The table runs every supported OS on purpose, and the linux-with-
// XDG_RUNTIME_DIR row is the reason it exists. That row used to FAIL: RunDir
// spelled the XDG subdirectory with the release constant regardless of
// channel, so on any Linux with a desktop session or a CI runner (both set
// XDG_RUNTIME_DIR) a development build and an installed release resolved the
// same ctl.sock — whoever bound it first owned it, and the other spoke to a
// daemon built from code it had never seen. macOS was unaffected, which is why
// the gap survived: the only platform anyone ran this on locally was the one
// where <data>/run made the separation fall out for free.
func TestDevResolverSeparatesFromRelease(t *testing.T) {
	cases := []struct {
		name string
		goos string
		home string
		env  map[string]string
		// endpointSeparates is false on Windows alone, where the control
		// endpoint is a named pipe derived from the user's SID and not from any
		// directory — so the channel split does NOT reach it. That is a real
		// gap, recorded in docs/backlog.md and docs/windows.md rather than
		// papered over here: asserting a separation Windows does not have would
		// make this test lie, and asserting nothing would let the Unix
		// regression back in.
		endpointSeparates bool
	}{
		{name: "darwin", goos: "darwin", home: "/Users/alice", endpointSeparates: true},
		{name: "linux without XDG_RUNTIME_DIR", goos: "linux", home: "/home/alice", endpointSeparates: true},
		{
			name: "linux with XDG_RUNTIME_DIR",
			goos: "linux", home: "/home/alice",
			env:               map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"},
			endpointSeparates: true,
		},
		{
			name: "windows",
			goos: "windows", home: `C:\Users\alice`,
			env:               map[string]string{"APPDATA": `C:\Users\alice\AppData\Roaming`},
			endpointSeparates: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := fakeResolver(tc.goos, tc.env, tc.home)
			dev := platform.DevResolver(base)

			release, err := base.DataDir()
			if err != nil {
				t.Fatalf("release data dir: %v", err)
			}
			devData, err := dev.DataDir()
			if err != nil {
				t.Fatalf("dev data dir: %v", err)
			}
			if devData == release {
				t.Fatalf("dev and release resolve the same directory %q", devData)
			}
			if parentDir(tc.goos, devData) != parentDir(tc.goos, release) {
				t.Fatalf("dev %q and release %q are not siblings", devData, release)
			}

			if !tc.endpointSeparates {
				return
			}
			// The endpoint is what actually collides. Comparing the data
			// directories alone is what let the Linux gap through: those
			// differed the whole time while the sockets did not.
			releaseSock, err := base.CtlSocketPath()
			if err != nil {
				t.Fatalf("release ctl socket: %v", err)
			}
			devSock, err := dev.CtlSocketPath()
			if err != nil {
				t.Fatalf("dev ctl socket: %v", err)
			}
			if devSock == releaseSock {
				t.Fatalf("dev and release share the control endpoint %q: the channel split is cosmetic", devSock)
			}
		})
	}
}

// parentDir is filepath.Dir that also understands Windows spellings, which the
// cross-platform table produces while running on macOS or Linux.
func parentDir(goos, p string) string {
	if goos == "windows" {
		if i := strings.LastIndex(p, `\`); i >= 0 {
			return p[:i]
		}
		return p
	}
	return filepath.Dir(p)
}

// AGENTHUB_SOCKET is the operator's last word: it must survive every rule
// above, including the one that moves the run directory when the data
// directory moves. A build flavour silently redirecting a named socket would
// break every harness that pins one.
func TestExplicitSocketOverrideSurvivesDevChannel(t *testing.T) {
	base := fakeResolver("linux", map[string]string{
		"AGENTHUB_SOCKET": "/tmp/pinned.sock",
		"XDG_RUNTIME_DIR": "/run/user/1000",
	}, "/home/alice")

	for name, r := range map[string]*platform.Resolver{
		"release": base,
		"dev":     platform.DevResolver(base),
	} {
		got, err := r.CtlSocketPath()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != "/tmp/pinned.sock" {
			t.Errorf("%s: got %q, want the pinned /tmp/pinned.sock", name, got)
		}
	}
}

// An explicit AGENTHUB_DATA_DIR outranks the build flavour: CI, the e2e suite
// and multi-sandbox debugging all set it, and a build that quietly ignored it
// would look like those harnesses were broken.
func TestDevResolverYieldsToExplicitOverride(t *testing.T) {
	base := &platform.Resolver{
		GOOS:        "darwin",
		UserHomeDir: func() (string, error) { return "/Users/alice", nil },
		LookupEnv: func(key string) (string, bool) {
			if key == platform.EnvDataDir {
				return "/explicit/data", true
			}
			return "", false
		},
	}
	got, err := platform.DevResolver(base).DataDir()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/explicit/data" {
		t.Fatalf("got %q, want the explicit override /explicit/data", got)
	}
}

func TestZeroValueResolverUsesRealEnvironment(t *testing.T) {
	t.Setenv(platform.EnvDataDir, "/zv/data")
	var r platform.Resolver
	got, err := r.DataDir()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/zv/data" {
		t.Fatalf("got %q, want /zv/data", got)
	}
}

func TestPackageLevelWrappersHonorOverrides(t *testing.T) {
	t.Setenv(platform.EnvDataDir, "/pkg/data")
	t.Setenv(platform.EnvRegistry, "/pkg/registry")
	t.Setenv(platform.EnvSocket, "/pkg/ctl.sock")

	if got, err := platform.DataDir(); err != nil || got != "/pkg/data" {
		t.Fatalf("DataDir = %q, %v", got, err)
	}
	if got, err := platform.RegistryDir(); err != nil || got != "/pkg/registry" {
		t.Fatalf("RegistryDir = %q, %v", got, err)
	}
	if got, err := platform.CtlSocketPath(); err != nil || got != "/pkg/ctl.sock" {
		t.Fatalf("CtlSocketPath = %q, %v", got, err)
	}
	if got, err := platform.LogsDir(); err != nil || got != "/pkg/data/logs" {
		t.Fatalf("LogsDir = %q, %v", got, err)
	}
	if got, err := platform.CacheDir(); err != nil || got != "/pkg/data/cache" {
		t.Fatalf("CacheDir = %q, %v", got, err)
	}
	if got, err := platform.StateDir(); err != nil || got != "/pkg/data/state" {
		t.Fatalf("StateDir = %q, %v", got, err)
	}
	if got, err := platform.RunDir(); err != nil || got != "/pkg/data/run" {
		// On Linux XDG_RUNTIME_DIR may be set in the CI environment.
		if os.Getenv("XDG_RUNTIME_DIR") == "" {
			t.Fatalf("RunDir = %q, %v", got, err)
		}
	}
}

func TestEnsureDirCreatesWith0700(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "a", "b", "run")
	if err := platform.EnsureDir(dir); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("perm = %o, want 700", perm)
	}
}

func TestEnsureDirTightensLoosePermissions(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "loose")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := platform.EnsureDir(dir); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("perm = %o, want 700", perm)
	}
}

func TestEnsureDirsStopsAtFirstError(t *testing.T) {
	base := t.TempDir()
	good := filepath.Join(base, "good")
	// A path whose parent is a regular file cannot be created.
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	bad := filepath.Join(file, "child")
	notReached := filepath.Join(base, "not-reached")

	err := platform.EnsureDirs(good, bad, notReached)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, statErr := os.Stat(good); statErr != nil {
		t.Fatalf("good dir should exist: %v", statErr)
	}
	if _, statErr := os.Stat(notReached); !os.IsNotExist(statErr) {
		t.Fatalf("not-reached should not exist, stat err = %v", statErr)
	}
}
