package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestConnectRunsGateway drives the real gateway through the CLI seam: an
// immediately-EOF stdin is a clean client disconnect (exit 0), and stdout
// stays free of anything but protocol frames (here: nothing at all).
func TestConnectRunsGateway(t *testing.T) {
	setDataDir(t)
	code, out, _ := runCLI(t, "", "connect", "--client", "claude-code")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if out != "" {
		t.Errorf("stdout must carry only protocol frames, got %q", out)
	}
}

// TestConnectAnswersInitialize proves the wired gateway actually speaks MCP
// on the CLI's stdin/stdout.
func TestConnectAnswersInitialize(t *testing.T) {
	setDataDir(t)
	stdin := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}` + "\n"
	code, out, _ := runCLI(t, stdin, "connect", "--client", "claude-code")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"protocolVersion":"2025-06-18"`) || !strings.Contains(out, `"name":"agenthub"`) {
		t.Errorf("initialize response missing from stdout: %q", out)
	}
}

func TestConnectRequiresClientFlag(t *testing.T) {
	setDataDir(t)
	code, _, _ := runCLI(t, "", "connect")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
}

func TestClientConnectDryRun(t *testing.T) {
	setDataDir(t)
	code, out, stderr := runCLI(t, "", "client", "connect", "claude-code", "--dry-run", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	env := decodeEnvelope(t, out)
	if !env.OK {
		t.Fatalf("envelope = %s", out)
	}
	var plan ConnectPlan
	if err := json.Unmarshal(env.Data, &plan); err != nil {
		t.Fatalf("data: %v", err)
	}
	if plan.Client != "claude-code" || !plan.DryRun {
		t.Errorf("plan = %+v", plan)
	}
	wantArgs := []string{"connect", "--client", "claude-code"}
	if len(plan.Entry.Args) != len(wantArgs) {
		t.Fatalf("entry args = %v, want %v", plan.Entry.Args, wantArgs)
	}
	for i, a := range wantArgs {
		if plan.Entry.Args[i] != a {
			t.Errorf("entry args = %v, want %v", plan.Entry.Args, wantArgs)
		}
	}
	if plan.Entry.Command == "" {
		t.Errorf("entry command must point at the gateway binary")
	}

	// Human mode renders the exact snippet from the same plan.
	code, humanOut, _ := runCLI(t, "", "client", "connect", "claude-code", "--dry-run")
	if code != ExitOK {
		t.Fatalf("human exit = %d", code)
	}
	for _, want := range []string{"dry-run", "nothing written", "mcpServers", "--client", "claude-code"} {
		if !strings.Contains(humanOut, want) {
			t.Errorf("human output missing %q:\n%s", want, humanOut)
		}
	}
}

// TestClientConnectRejectsProfileFlag pins that the profile binding cannot be
// smuggled into the client's own MCP configuration.
//
// The flag used to exist, was written into the entry's argv, and was then
// discarded by the gateway that received it — settable, displayable, and
// applied by nothing. Rejecting it outright is the honest replacement: a
// profile lives in clients.json, where agenthub can rebind it and push
// tools/list_changed without asking the client to restart.
func TestClientConnectRejectsProfileFlag(t *testing.T) {
	setDataDir(t)
	code, _, _ := runCLI(t, "", "client", "connect", "claude-code", "--profile", "dev", "--dry-run")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d — an unknown flag must fail loudly, never be accepted "+
			"and ignored the way the old one was", code, ExitUsage)
	}
}

// TestConnectSnippetSeam pins the shared seam that both the M0-3 dry-run
// preview and the M0-9 config writer must go through.
func TestConnectSnippetSeam(t *testing.T) {
	plan := ConnectSnippet("/usr/local/bin/agenthub", "cursor")
	if plan.Client != "cursor" || !plan.DryRun {
		t.Errorf("plan = %+v", plan)
	}
	if plan.Entry.Command != "/usr/local/bin/agenthub" {
		t.Errorf("command = %q", plan.Entry.Command)
	}
	want := []string{"connect", "--client", "cursor"}
	if len(plan.Entry.Args) != len(want) {
		t.Fatalf("args = %v, want %v (client identity only; the profile is bound in "+
			"clients.json, never in the client's own config)", plan.Entry.Args, want)
	}
	for i, a := range want {
		if plan.Entry.Args[i] != a {
			t.Errorf("args = %v, want %v", plan.Entry.Args, want)
		}
	}
}
