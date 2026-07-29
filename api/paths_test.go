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
			goos: "windows", // even unsupported platforms honor the override
			env:  map[string]string{"AGENTHUB_SOCKET": "/tmp/x.sock"},
			want: "/tmp/x.sock",
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
			got, err := defaultSocketPath(tc.goos, env(tc.env), homeFn(tc.home))
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
	_, err := defaultSocketPath("windows", env(nil), homeFn("/h"))
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("want ErrUnsupportedPlatform, got %v", err)
	}
}
