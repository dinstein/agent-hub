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
		{"session", "kill", "claude-code:1"},
		// `events` is deliberately NOT here any more. It used to subscribe to
		// the daemon SSE stream; it now reads the event LOG off disk, which a
		// stdio gateway writes with no daemon in the picture — so refusing
		// offline would refuse the installation with the most to explain.
		// TestEventsWorksOffline is the other half of this claim.
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

// The offline half of the matrix change above: reading the event log needs
// no daemon, and on an installation that has never run one there is simply
// nothing recorded yet — which is a normal state, not a failure.
func TestEventsWorksOffline(t *testing.T) {
	setDataDir(t)
	pointSocketAtNothing(t)
	code, out, stderr := runCLI(t, "", "events")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — reading the event log must not need a daemon\nstderr: %s", code, stderr)
	}
	// And it says which file it looked at, so "nothing here" is not confused
	// with "I did not look".
	if !strings.Contains(out, "events.jsonl") {
		t.Errorf("the empty result does not name the stream it read: %q", out)
	}
}

// The vocabulary is closed so a caller can be TOLD it got a name wrong.
// Answering a typo with an empty result would hand back exactly the output
// of "this has not happened", which is the confusion a closed set prevents.
func TestEventsRejectsUnknownSelectors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"events", "--scope", "bogus"}, "server"},
		{[]string{"events", "--kind", "exploded"}, "connected"},
		// A kind that is real at ANOTHER scope is still wrong at this one.
		{[]string{"events", "--scope", "server", "--kind", "listener_bound"}, "circuit_open"},
	} {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			setDataDir(t)
			pointSocketAtNothing(t)
			code, _, stderr := runCLI(t, "", tc.args...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d", code, ExitUsage)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("the error must list the valid names (looking for %q): %q", tc.want, stderr)
			}
		})
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
