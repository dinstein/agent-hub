package api

import (
	"errors"
	"testing"
)

// env returns a lookup func over a fixed map.
func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func homeFn(h string) func() (string, error) {
	return func() (string, error) { return h, nil }
}

// testSID is a fixed SID so the windows rows below can assert a whole pipe
// name. Its hash is stable by construction — sha8 of this exact string — which
// is what makes the expectation writable at all.
const testSID = "S-1-5-21-1111-2222-3333-1001"

// testSIDHash is sha8(testSID), written out because internal/platform's side of
// the contract (TestWindowsEndpointContract in internal/ctlapi) pins the same
// digest as a literal. That is the one place the two duplicated
// implementations meet: this package cannot import internal/platform, and a
// test that recomputed the digest with the same two lines both packages use
// would assert only that SHA-256 is deterministic.
const testSIDHash = "7abec184"

func sidFn(s string) func() (string, error) {
	return func() (string, error) { return s, nil }
}

func TestSIDHashMatchesTheFrozenDigest(t *testing.T) {
	if got := sha8(testSID); got != testSIDHash {
		t.Fatalf("sha8(%q) = %q, want %q — internal/ctlapi's contract test pins this literal",
			testSID, got, testSIDHash)
	}
}

// TestDefaultSocketPath re-states the resolution contract of
// internal/platform.CtlSocketPath (CONTRACT comment in paths.go). The
// cross-package test that pins both implementations byte-identical lives
// on the internal/ctlapi side; this table keeps the local copy honest.
func TestDefaultSocketPath(t *testing.T) {
	cases := []struct {
		name string
		goos string
		env  map[string]string
		home string
		want string
	}{
		{
			name: "env_socket_override_wins_everywhere",
			goos: "plan9", // even unsupported platforms honor the override
			env:  map[string]string{"AGENTHUB_SOCKET": "/tmp/x.sock"},
			want: "/tmp/x.sock",
		},
		{
			// The pipe name is a frozen identifier: asserted whole, not by
			// prefix, so a change to either half has to be deliberate.
			name: "windows_control_pipe",
			goos: "windows",
			env:  map[string]string{"APPDATA": `C:\Users\u\AppData\Roaming`},
			home: `C:\Users\u`,
			want: `\\.\pipe\agenthub-ctl-` + testSIDHash,
		},
		{
			// The pipe does not live in the data directory, so relocating that
			// directory must not move it — the property that costs Windows the
			// free channel separation every other platform gets, and the reason
			// DevSocketPath exists.
			name: "windows_pipe_ignores_the_data_dir_override",
			goos: "windows",
			env: map[string]string{
				"AGENTHUB_DATA_DIR": `D:\elsewhere`,
				"APPDATA":           `C:\Users\u\AppData\Roaming`,
			},
			home: `C:\Users\u`,
			want: `\\.\pipe\agenthub-ctl-` + testSIDHash,
		},
		{
			name: "windows_socket_override_still_wins",
			goos: "windows",
			env:  map[string]string{"AGENTHUB_SOCKET": `\\.\pipe\custom`},
			want: `\\.\pipe\custom`,
		},
		{
			name: "darwin_default",
			goos: "darwin",
			env:  map[string]string{},
			home: "/Users/u",
			want: "/Users/u/Library/Application Support/AgentHub/run/ctl.sock",
		},
		{
			name: "darwin_ignores_xdg_runtime_dir",
			goos: "darwin",
			env:  map[string]string{"XDG_RUNTIME_DIR": "/run/user/501"},
			home: "/Users/u",
			want: "/Users/u/Library/Application Support/AgentHub/run/ctl.sock",
		},
		{
			name: "linux_xdg_runtime_dir",
			goos: "linux",
			env:  map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"},
			home: "/home/u",
			want: "/run/user/1000/AgentHub/ctl.sock",
		},
		{
			name: "linux_relative_xdg_runtime_dir_ignored",
			goos: "linux",
			env:  map[string]string{"XDG_RUNTIME_DIR": "run/user/1000"},
			home: "/home/u",
			want: "/home/u/.local/share/AgentHub/run/ctl.sock",
		},
		{
			name: "linux_xdg_data_home_fallback",
			goos: "linux",
			env:  map[string]string{"XDG_DATA_HOME": "/data"},
			home: "/home/u",
			want: "/data/AgentHub/run/ctl.sock",
		},
		{
			name: "linux_relative_xdg_data_home_ignored",
			goos: "linux",
			env:  map[string]string{"XDG_DATA_HOME": "data"},
			home: "/home/u",
			want: "/home/u/.local/share/AgentHub/run/ctl.sock",
		},
		{
			name: "linux_home_fallback",
			goos: "linux",
			env:  map[string]string{},
			home: "/home/u",
			want: "/home/u/.local/share/AgentHub/run/ctl.sock",
		},
		{
			name: "agenthub_data_dir_override",
			goos: "linux",
			env:  map[string]string{"AGENTHUB_DATA_DIR": "/custom"},
			home: "/home/u",
			want: "/custom/run/ctl.sock",
		},
		{
			// The socket follows a relocated data directory rather than staying
			// in the one-per-user XDG_RUNTIME_DIR, so two differently-sandboxed
			// agenthubs cannot land on the same endpoint.
			name: "linux_data_dir_override_beats_xdg_runtime_dir",
			goos: "linux",
			env: map[string]string{
				"AGENTHUB_DATA_DIR": "/custom",
				"XDG_RUNTIME_DIR":   "/run/user/1000",
			},
			home: "/home/u",
			want: "/custom/run/ctl.sock",
		},
		{
			name: "linux_empty_data_dir_override_still_allows_xdg_runtime_dir",
			goos: "linux",
			env: map[string]string{
				"AGENTHUB_DATA_DIR": "",
				"XDG_RUNTIME_DIR":   "/run/user/1000",
			},
			home: "/home/u",
			want: "/run/user/1000/AgentHub/ctl.sock",
		},
		{
			name: "darwin_agenthub_data_dir_override",
			goos: "darwin",
			env:  map[string]string{"AGENTHUB_DATA_DIR": "/custom"},
			home: "/Users/u",
			want: "/custom/run/ctl.sock",
		},
		{
			name: "empty_env_values_ignored",
			goos: "darwin",
			env:  map[string]string{"AGENTHUB_SOCKET": "", "AGENTHUB_DATA_DIR": ""},
			home: "/Users/u",
			want: "/Users/u/Library/Application Support/AgentHub/run/ctl.sock",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := defaultSocketPath(tc.goos, env(tc.env), homeFn(tc.home), sidFn(testSID))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestDefaultSocketPathUnsupportedPlatform(t *testing.T) {
	_, err := defaultSocketPath("plan9", env(nil), homeFn("/h"), sidFn(testSID))
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("want ErrUnsupportedPlatform, got %v", err)
	}
}

// TestDevSocketPathSeparatesFromRelease is the api-side copy of
// internal/platform's TestDevResolverSeparatesFromRelease, and it is here for
// the platform that made both necessary.
//
// The Linux version of this bug shipped: a development build and an installed
// release resolved the same ctl.sock under XDG_RUNTIME_DIR, and whoever bound
// it first owned it while the other talked to a daemon built from code it had
// never seen. Windows has the same shape for a different reason — the endpoint
// is a pipe name, so relocating the data directory does not move it — and this
// is the side of the fence the GUI is on.
func TestDevSocketPathSeparatesFromRelease(t *testing.T) {
	cases := []struct {
		name string
		goos string
		env  map[string]string
		home string
		want string
	}{
		{
			name: "windows_dev_pipe",
			goos: "windows",
			env:  map[string]string{"APPDATA": `C:\Users\u\AppData\Roaming`},
			home: `C:\Users\u`,
			want: `\\.\pipe\agenthub-ctl-dev-` + testSIDHash,
		},
		{
			name: "darwin_dev_socket_under_the_dev_data_dir",
			goos: "darwin",
			home: "/Users/u",
			want: "/Users/u/Library/Application Support/AgentHubDev/run/ctl.sock",
		},
		{
			// NOT /run/user/1000/AgentHub/ctl.sock. A dev build has relocated
			// its data directory by definition, and the shared runtime
			// directory is exactly where the two channels collided before.
			name: "linux_dev_socket_ignores_xdg_runtime_dir",
			goos: "linux",
			env:  map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"},
			home: "/home/u",
			want: "/home/u/.local/share/AgentHubDev/run/ctl.sock",
		},
		{
			name: "explicit_socket_override_wins_for_the_dev_channel_too",
			goos: "windows",
			env:  map[string]string{"AGENTHUB_SOCKET": `\\.\pipe\custom`},
			want: `\\.\pipe\custom`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := devSocketPath(tc.goos, env(tc.env), homeFn(tc.home), sidFn(testSID))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
			if tc.env["AGENTHUB_SOCKET"] != "" {
				return
			}
			release, err := defaultSocketPath(tc.goos, env(tc.env), homeFn(tc.home), sidFn(testSID))
			if err != nil {
				t.Fatalf("release socket: %v", err)
			}
			if got == release {
				t.Errorf("dev and release share the endpoint %q: the channel split is cosmetic", got)
			}
		})
	}
}

// TestWindowsRunDirIsWhereDaemonJSONLives pins the answer DialOrStart needs and
// cannot get from the endpoint on Windows: the pipe has no directory, so
// filepath.Dir would hand back `\\.\pipe` and every attempt to read the
// handshake file would fail — quietly, because DialOrStart falls back to the
// endpoint it was given and only loses the ability to notice a daemon serving a
// different one.
func TestWindowsRunDirIsWhereDaemonJSONLives(t *testing.T) {
	got, err := runDir("windows", env(map[string]string{"APPDATA": `C:\Users\u\AppData\Roaming`}), homeFn(`C:\Users\u`))
	if err != nil {
		t.Fatalf("runDir: %v", err)
	}
	if want := `C:\Users\u\AppData\Roaming\AgentHub\run`; got != want {
		t.Errorf("runDir = %q, want %q", got, want)
	}
	// APPDATA missing is the documented fallback, not an error: a service
	// account or a stripped environment still has a home directory.
	got, err = runDir("windows", env(nil), homeFn(`C:\Users\u`))
	if err != nil {
		t.Fatalf("runDir without APPDATA: %v", err)
	}
	if want := `C:\Users\u\AppData\Roaming\AgentHub\run`; got != want {
		t.Errorf("runDir without APPDATA = %q, want %q", got, want)
	}
}
