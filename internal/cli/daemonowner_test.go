package cli

import (
	"bytes"
	"net"
	"strings"
	"testing"
)

// The admission rule, from the outside: a hub belongs to the application, and
// every way of asking for one that belongs to nobody is refused by name.

func TestDaemonStartRefusesAnOwnerlessHub(t *testing.T) {
	setDaemonEnv(t)
	for _, args := range [][]string{
		{"daemon", "start", "--json"},
		{"daemon", "start", "--foreground", "--json"},
		{"daemon", "restart", "--json"},
	} {
		code, out, errOut := runCLI(t, "", args...)
		if code != ExitUsage {
			t.Fatalf("%v: exit = %d, want %d (stdout %q, stderr %q)", args, code, ExitUsage, out, errOut)
		}
		env := decodeEnvelope(t, out)
		if env.Error == nil || env.Error.Code != CodeDaemonUnowned {
			t.Fatalf("%v: error envelope = %s", args, out)
		}
		if env.Error.Hint == "" {
			t.Fatalf("%v: refusal carries no way forward: %s", args, out)
		}
	}
}

// A restart must not stop the running hub before it discovers that its
// replacement is inadmissible. That would leave the machine with no hub at
// all — the one outcome a restart may never produce.
func TestDaemonRestartRefusesBeforeStopping(t *testing.T) {
	_, socket := setDaemonEnv(t)
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	code, out, _ := runCLI(t, "", "daemon", "restart", "--json")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d: %s", code, ExitUsage, out)
	}
	// The listener is the stand-in for a live hub. A stop attempt would have
	// reported on it (it cannot ping, so it would have failed differently);
	// reaching the admission refusal proves the order.
	env := decodeEnvelope(t, out)
	if env.Error == nil || env.Error.Code != CodeDaemonUnowned {
		t.Fatalf("error envelope = %s", out)
	}
}

// `daemon status` has to answer "will this hub still be here in an hour",
// because from a terminal there is no other way to ask. Both answers are
// spelled out rather than one being the absence of the other: a line that
// simply omitted the owner would read as "this build does not know", which is
// the state an operator most wants to tell apart from "nobody owns it".
func TestDaemonStatusSaysWhoTheHubBelongsTo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner int
		want  string
	}{
		{name: "headless", owner: 0, want: "headless"},
		{name: "owned by an application", owner: 4242, want: "owned by pid 4242"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := DaemonStatus{
				Running: true, Pid: 100, Version: "test", Socket: "/tmp/ctl.sock",
				Owner: tc.owner,
			}
			if err := s.Human(&buf); err != nil {
				t.Fatalf("Human: %v", err)
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Fatalf("status line = %q, want it to contain %q", buf.String(), tc.want)
			}
		})
	}
}

func TestOwnerFlagsResolve(t *testing.T) {
	t.Run("headless is ownerless", func(t *testing.T) {
		owner, err := ownerFlags{headless: true}.resolve(true)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if owner.Owned() {
			t.Fatalf("headless resolved to an owned daemon: %+v", owner)
		}
	})

	t.Run("headless and an owner are mutually exclusive", func(t *testing.T) {
		// Accepting both would leave it undecided which one the daemon
		// watches, and the two answers differ in when the hub shuts down.
		if _, err := (ownerFlags{headless: true, pid: 4242}).resolve(true); err == nil {
			t.Fatal("--headless with --owner-pid was accepted")
		}
	})

	t.Run("a pid alone is enough", func(t *testing.T) {
		owner, err := ownerFlags{pid: 4242}.resolve(false)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if owner.PID != 4242 {
			t.Fatalf("owner = %+v, want pid 4242", owner)
		}
	})

	t.Run("a lifeline needs the foreground", func(t *testing.T) {
		// The alternative is worse than refusing: a backgrounded start execs
		// again and the descriptor never arrives, so the caller would believe
		// it had armed a watch that does not exist.
		if _, err := (ownerFlags{pid: 4242, lifelineFD: 3}).resolve(false); err == nil {
			t.Fatal("--owner-lifeline-fd was accepted for a backgrounded start")
		}
	})

	t.Run("the background fork carries the owner", func(t *testing.T) {
		got := ownerFlags{pid: 4242}.args()
		want := []string{"--owner-pid", "4242"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("args = %v, want %v", got, want)
		}
		if h := (ownerFlags{headless: true}).args(); len(h) != 1 || h[0] != "--headless" {
			t.Fatalf("headless args = %v, want [--headless]", h)
		}
	})
}
