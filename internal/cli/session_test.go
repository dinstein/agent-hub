package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// pointSocketAtNothing makes the control socket path resolve to a file that
// does not exist, so every online-only command takes the offline branch
// deterministically (no dependence on whether a daemon happens to run on
// this machine).
func pointSocketAtNothing(t *testing.T) {
	t.Helper()
	t.Setenv("AGENTHUB_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
}

// TestOnlineOnlyCommandsExit4 pins the online/offline matrix of docs/modules/controlplane.md
// : a command whose subject is a daemon RUNTIME object must refuse with
// E_DAEMON_DOWN rather than invent an offline answer.
func TestOnlineOnlyCommandsExit4(t *testing.T) {
	cases := [][]string{
		{"session", "ls"},
		{"session", "ls", "-f"},
		{"session", "show", "claude-code:1"},
		{"session", "scope", "claude-code:1", "--disable-server", "github"},
		{"session", "kill", "claude-code:1"},
		{"events"},
		{"events", "--follow"},
		{"audit", "tail", "-f"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			setDataDir(t)
			pointSocketAtNothing(t)
			code, out, stderr := runCLI(t, "", args...)
			if code != ExitDaemonDown {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, ExitDaemonDown, out, stderr)
			}
			if !strings.Contains(stderr, "daemon start") {
				t.Errorf("stderr must tell the user how to start the daemon: %q", stderr)
			}
		})
	}
}

// TestOnlineOnlyCommandsJSONEnvelope: the offline refusal must still be a
// well-formed failure envelope under --json, with the frozen code.
func TestOnlineOnlyCommandsJSONEnvelope(t *testing.T) {
	setDataDir(t)
	pointSocketAtNothing(t)
	code, out, _ := runCLI(t, "", "session", "ls", "--json")
	if code != ExitDaemonDown {
		t.Fatalf("exit = %d, want %d", code, ExitDaemonDown)
	}
	env := decodeEnvelope(t, out)
	if env.OK || env.Error == nil || env.Error.Code != CodeDaemonDown {
		t.Errorf("envelope = %+v, want ok:false E_DAEMON_DOWN", env)
	}
	if env.Error.Hint == "" {
		t.Errorf("the offline error must carry a hint")
	}
}

// TestSessionScopeUsageErrors: the flag validation happens BEFORE the
// daemon is contacted, so a malformed invocation is exit 2 even offline —
// a user error must not be reported as an infrastructure problem.
func TestSessionScopeUsageErrors(t *testing.T) {
	cases := [][]string{
		{"session", "scope", "sid"},
		{"session", "scope", "sid", "--discovery", "bogus"},
		{"session", "scope", "sid", "--tools", "no-colon"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			setDataDir(t)
			pointSocketAtNothing(t)
			if code, _, stderr := runCLI(t, "", args...); code != ExitUsage {
				t.Errorf("exit = %d, want %d (%s)", code, ExitUsage, stderr)
			}
		})
	}
}

// TestEventsRejectsUnknownTopic: an unknown topic is caught locally so the
// user is not left staring at a stream that will never deliver anything.
func TestEventsRejectsUnknownTopic(t *testing.T) {
	setDataDir(t)
	pointSocketAtNothing(t)
	code, _, stderr := runCLI(t, "", "events", "--topics", "bogus")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "servers") {
		t.Errorf("the error must list the known topics: %q", stderr)
	}
}

// TestParseToolSpecs is the table for the frozen `--tools s:t1,t2` flag,
// including the block-all (empty list) case the three-state semantics need.
func TestParseToolSpecs(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    map[string][]string
		wantErr bool
	}{
		{name: "nil", in: nil, want: nil},
		{name: "single", in: []string{"github:a,b"}, want: map[string][]string{"github": {"a", "b"}}},
		{name: "sorted and deduped", in: []string{"github:b,a,b"}, want: map[string][]string{"github": {"a", "b"}}},
		{name: "block all", in: []string{"github:"}, want: map[string][]string{"github": {}}},
		{name: "two servers", in: []string{"a:x", "b:y"}, want: map[string][]string{"a": {"x"}, "b": {"y"}}},
		{name: "no colon", in: []string{"github"}, wantErr: true},
		{name: "empty server", in: []string{":x"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseToolSpecs(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %v", got)
				}
				if ExitCodeFor(err) != ExitUsage {
					t.Errorf("exit code = %d, want %d", ExitCodeFor(err), ExitUsage)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, want := range tc.want {
				have, ok := got[k]
				if !ok {
					t.Fatalf("missing key %q in %v", k, got)
				}
				// nil and empty are NOT interchangeable: empty is block-all.
				if (have == nil) != (want == nil) || len(have) != len(want) {
					t.Fatalf("%q = %#v, want %#v", k, have, want)
				}
				for i := range want {
					if have[i] != want[i] {
						t.Fatalf("%q = %v, want %v", k, have, want)
					}
				}
			}
		})
	}
}

// TestScopeNarrowBodySeparatesWideningFromNarrowing pins the rule that makes
// one --tools flag safe to share between two opposite edits.
//
// `--enable-server x --tools x:read` means "file a grant to OPEN x's read
// tool". If that spec also reached the narrowing request it would mean
// "restrict x to exactly read" — applied immediately, with no approval, which
// is the authority the widen path exists to withhold. Servers not being
// widened must still narrow normally in the same invocation.
func TestScopeNarrowBodySeparatesWideningFromNarrowing(t *testing.T) {
	tests := []struct {
		name      string
		flags     sessionScopeFlags
		specs     map[string][]string
		wantOK    bool
		wantTools map[string][]string
	}{
		{
			name:   "tools for a widened server are not a narrowing",
			flags:  sessionScopeFlags{enableServer: []string{"x"}},
			specs:  map[string][]string{"x": {"read"}},
			wantOK: false,
		},
		{
			name:      "tools for another server still narrow",
			flags:     sessionScopeFlags{enableServer: []string{"x"}},
			specs:     map[string][]string{"x": {"read"}, "y": {"list"}},
			wantOK:    true,
			wantTools: map[string][]string{"y": {"list"}},
		},
		{
			name:      "plain narrowing is untouched",
			flags:     sessionScopeFlags{},
			specs:     map[string][]string{"y": {"list"}},
			wantOK:    true,
			wantTools: map[string][]string{"y": {"list"}},
		},
		{
			name:   "no flags at all is not a request",
			flags:  sessionScopeFlags{},
			wantOK: false,
		},
		{
			name:   "reset alone is a request",
			flags:  sessionScopeFlags{reset: true},
			wantOK: true,
		},
		{
			name:   "discovery alone is a request",
			flags:  sessionScopeFlags{discovery: "lazy"},
			wantOK: true,
		},
		{
			name:   "disable-server alone is a request",
			flags:  sessionScopeFlags{disableServer: []string{"z"}},
			wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, ok := scopeNarrowBody(tt.flags, tt.specs)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (body %+v)", ok, tt.wantOK, body)
			}
			if len(body.Tools) != len(tt.wantTools) {
				t.Fatalf("tools = %v, want %v", body.Tools, tt.wantTools)
			}
			for id, want := range tt.wantTools {
				got, present := body.Tools[id]
				if !present || len(got) != len(want) {
					t.Fatalf("tools[%s] = %v, want %v", id, got, want)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("tools[%s] = %v, want %v", id, got, want)
					}
				}
			}
			// An empty map and a nil map serialize differently; a widened-only
			// spec must leave the field absent, not present-and-empty.
			if !tt.wantOK && body.Tools != nil {
				t.Errorf("Tools = %v, want nil when there is nothing to narrow", body.Tools)
			}
		})
	}
}
