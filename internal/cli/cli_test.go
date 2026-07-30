package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/platform"
)

// runCLI executes one CLI invocation hermetically and returns exit code,
// stdout and stderr. Callers must point AGENTHUB_DATA_DIR at a temp dir
// first (see setDataDir) so no test touches the real user registry.
func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	return runCLIWithTimeout(t, stdin, 0, args...)
}

func runCLIWithTimeout(t *testing.T, stdin string, lockTimeout time.Duration, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Main(Options{
		Version:     "1.2.3-test",
		Args:        args,
		Stdin:       strings.NewReader(stdin),
		Stdout:      &out,
		Stderr:      &errOut,
		LockTimeout: lockTimeout,
	})
	return code, out.String(), errOut.String()
}

// runCLIReleaseHelp runs one invocation the way a release build's Main
// does, so the "hidden commands still run" assertion goes through the real
// Main rather than a hand-built tree.
func runCLIReleaseHelp(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Main(Options{
		Version:     "1.2.3-test",
		Args:        args,
		Stdin:       strings.NewReader(stdin),
		Stdout:      &out,
		Stderr:      &errOut,
		ReducedHelp: true,
	})
	return code, out.String(), errOut.String()
}

// setDataDir points AGENTHUB_DATA_DIR at a fresh temp dir (proving the
// override is honored end-to-end via internal/platform) and neutralizes any
// ambient AGENTHUB_REGISTRY override.
//
// It also moves $HOME somewhere empty. `client ls` reads the client
// configuration files it finds, and the developer running these tests has
// real ones: without this, the same test passes or fails depending on whose
// laptop it runs on, and reads that machine's private files to do it. Tests
// that want client files put them under their own HOME afterwards.
func setDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(platform.EnvDataDir, dir)
	t.Setenv(platform.EnvRegistry, "")
	t.Setenv("HOME", t.TempDir())
	// And forbid running another application's configuration CLI. Connect
	// on a client agenthub does not rewrite DELEGATES to that client's own
	// tool, and a test that does it runs the developer's real codex. Tests
	// that want the delegated path put a fake one on PATH and clear this.
	t.Setenv(platform.EnvNoClientCLI, "1")
	return dir
}

// envelope mirrors the --json output contract for test decoding.
type envelope struct {
	OK       bool            `json:"ok"`
	Data     json.RawMessage `json:"data"`
	Warnings []string        `json:"warnings"`
	Error    *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Hint    string `json:"hint"`
	} `json:"error"`
}

func decodeEnvelope(t *testing.T, s string) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		t.Fatalf("output is not a JSON envelope: %v\n%s", err, s)
	}
	return env
}

func TestVersionFlag(t *testing.T) {
	setDataDir(t)
	code, out, _ := runCLI(t, "", "--version")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if out != "agenthub 1.2.3-test\n" {
		t.Errorf("output = %q", out)
	}
}

func TestBareInvocationShowsHelp(t *testing.T) {
	setDataDir(t)
	code, out, _ := runCLI(t, "")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"Usage:", "server", "client", "connect", "doctor"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q", want)
		}
	}
}

// TestUsageErrorsExit2 pins the "cobra parse error => exit 2" contract for
// every class of usage error: unknown command, unknown subcommand, unknown
// flag, wrong arg count, and missing required combinations.
func TestUsageErrorsExit2(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"unknown command", []string{"bogus"}},
		{"unknown subcommand", []string{"server", "bogus"}},
		// Asking for HELP about a name that does not exist. cobra answers a
		// help flag before RunE, so these used to print the group's page and
		// exit 0 — the same answer a real subcommand gives, which is what made
		// `secret get` look like a command that exists.
		{"unknown subcommand with --help", []string{"secret", "get", "--help"}},
		{"unknown subcommand with -h", []string{"server", "bogus", "-h"}},
		{"unknown nested subcommand with --help", []string{"profile", "server", "bogus", "--help"}},
		{"unknown flag", []string{"server", "ls", "--nope"}},
		{"rm missing arg", []string{"server", "rm"}},
		{"rm extra args", []string{"server", "rm", "a", "b"}},
		{"add without --cmd", []string{"server", "add", "x"}},
		{"add without name", []string{"server", "add", "--cmd", "foo"}},
		{"add stdin plus flags", []string{"server", "add", "x", "--stdin", "--cmd", "foo"}},
		{"connect without --client", []string{"connect"}},
		{"connect positional arg", []string{"connect", "claude-code"}},
		{"client connect missing arg", []string{"client", "connect"}},
		{"doctor extra arg", []string{"doctor", "x"}},
		{"bad env flag", []string{"server", "add", "x", "--cmd", "foo", "--env", "NOEQUALS"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setDataDir(t)
			code, _, stderr := runCLI(t, "", tc.args...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitUsage, stderr)
			}
			if !strings.Contains(stderr, "agenthub: ") {
				t.Errorf("stderr should carry the error line, got %q", stderr)
			}
		})
	}
}

func TestUsageErrorJSONEnvelope(t *testing.T) {
	setDataDir(t)
	// Unknown flag: parsing fails before the bound --json variable is set,
	// exercising the raw-args fallback in Main.
	code, out, _ := runCLI(t, "", "server", "ls", "--nope", "--json")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
	env := decodeEnvelope(t, out)
	if env.OK || env.Error == nil || env.Error.Code != CodeUsage {
		t.Errorf("envelope = %+v, want ok:false code E_USAGE", env)
	}

	// Args-validation failure: parsing succeeded, the bound flag drives it.
	code, out, _ = runCLI(t, "", "server", "rm", "--json")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
	env = decodeEnvelope(t, out)
	if env.OK || env.Error == nil || env.Error.Code != CodeUsage {
		t.Errorf("envelope = %+v, want ok:false code E_USAGE", env)
	}
	if env.Error.Hint == "" {
		t.Errorf("usage errors should hint at --help")
	}
}

func TestGroupAliases(t *testing.T) {
	setDataDir(t)
	// Plural aliases resolve to the singular canonical groups.
	code, out, _ := runCLI(t, "", "servers", "ls", "--json")
	if code != ExitOK {
		t.Fatalf("servers ls exit = %d", code)
	}
	if env := decodeEnvelope(t, out); !env.OK {
		t.Errorf("servers ls envelope not ok: %s", out)
	}
	code, _, _ = runCLI(t, "", "clients", "connect", "claude-code", "--dry-run")
	if code != ExitOK {
		t.Fatalf("clients connect exit = %d", code)
	}
}

// TestHelpForEveryRealCommandStillExits0 is the other direction of the
// unknown-subcommand-with-help refusal, and the one that matters more: the
// check runs before cobra sees the args, so an over-broad version of it would
// refuse `--help` on real commands — turning a fix for one misleading page
// into a CLI whose help does not work.
//
// It walks the whole tree rather than sampling, because the failure would be
// per-command: a group whose args are shaped unlike the others is exactly what
// would slip through a handful of cases written by hand.
func TestHelpForEveryRealCommandStillExits0(t *testing.T) {
	setDataDir(t)
	app := &App{version: "1.2.3-test"}
	checked := 0
	var walk func(cmd *cobra.Command, path []string)
	walk = func(cmd *cobra.Command, path []string) {
		if len(path) > 0 {
			checked++
			for _, flag := range []string{"--help", "-h"} {
				args := append(append([]string{}, path...), flag)
				code, out, stderr := runCLI(t, "", args...)
				if code != ExitOK {
					t.Errorf("agenthub %s: exit %d, want 0\nstderr: %s",
						strings.Join(args, " "), code, stderr)
				}
				if !strings.Contains(out, "Usage:") {
					t.Errorf("agenthub %s printed no usage block:\n%s",
						strings.Join(args, " "), out)
				}
			}
		}
		for _, sub := range cmd.Commands() {
			if sub.Name() == "help" {
				continue // cobra's own, and it takes a command name as an arg
			}
			walk(sub, append(append([]string{}, path...), sub.Name()))
		}
	}
	walk(app.newRoot(), nil)
	// A walk that visited nothing passes every assertion inside it. The tree
	// is ~90 commands; the floor only has to be high enough that an empty or
	// top-level-only traversal cannot masquerade as a clean run.
	if checked < 50 {
		t.Fatalf("walked %d commands; the traversal is not reaching the tree", checked)
	}
}
