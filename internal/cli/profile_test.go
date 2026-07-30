package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/discovery"
)

// decodeInto unmarshals a success envelope's data into v.
func decodeInto(t *testing.T, out string, v any) envelope {
	t.Helper()
	env := decodeEnvelope(t, out)
	if !env.OK {
		t.Fatalf("envelope not ok: %s", out)
	}
	if v != nil {
		if err := json.Unmarshal(env.Data, v); err != nil {
			t.Fatalf("data does not decode: %v\n%s", err, env.Data)
		}
	}
	return env
}

// mustRun runs a command expecting exit 0 and returns stdout.
func mustRun(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	code, out, stderr := runCLI(t, stdin, args...)
	if code != ExitOK {
		t.Fatalf("agenthub %s: exit %d\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), code, out, stderr)
	}
	return out
}

// TestProfileLifecycle exercises the whole offline round trip: create, list,
// bind servers, set the three-state tool selector, rename (with repointing),
// use/clear and remove.
func TestProfileLifecycle(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "github", "--cmd", "gh-mcp")
	mustRun(t, "", "server", "add", "linear", "--cmd", "linear-mcp")

	mustRun(t, "", "profile", "create", "work", "--json")

	// Creating twice is an error, not a silent overwrite.
	if code, _, _ := runCLI(t, "", "profile", "create", "work"); code != ExitGeneral {
		t.Errorf("duplicate create exit = %d, want %d", code, ExitGeneral)
	}
	// An unknown profile is exit 3.
	if code, _, _ := runCLI(t, "", "profile", "rm", "nope"); code != ExitNotFound {
		t.Errorf("rm unknown exit = %d, want %d", code, ExitNotFound)
	}

	// A fresh profile has NO server narrowing: null, not [].
	var list ProfileList
	decodeInto(t, mustRun(t, "", "profile", "ls", "--json"), &list)
	if len(list.Profiles) != 1 || list.Profiles[0].Servers != nil {
		t.Fatalf("fresh profile = %+v, want one entry with null servers", list.Profiles)
	}

	// Naming one server turns "no narrowing" into an explicit set.
	mustRun(t, "", "profile", "server", "add", "work", "github")
	var change ProfileChange
	decodeInto(t, mustRun(t, "", "profile", "server", "add", "work", "linear", "--json"), &change)
	if got := change.Profile.Servers; len(got) != 2 || got[0] != "github" || got[1] != "linear" {
		t.Errorf("servers = %v, want [github linear] sorted", got)
	}
	// A server that is not in the registry cannot be added.
	if code, _, _ := runCLI(t, "", "profile", "server", "add", "work", "ghost"); code != ExitNotFound {
		t.Errorf("adding an unknown server exit = %d, want %d", code, ExitNotFound)
	}
	mustRun(t, "", "profile", "server", "rm", "work", "linear")

	// Three-state tool selector. Each decode uses a FRESH value: unmarshal
	// merges into an existing map, which would let a previous assertion's
	// data survive into the next one.
	var onlyChange ProfileChange
	decodeInto(t, mustRun(t, "", "profile", "tools", "work", "github", "--only", "list_prs,create_pr", "--json"), &onlyChange)
	if sel := onlyChange.Profile.Tools["github"]; len(sel.Allow) != 2 || sel.Allow[0] != "create_pr" {
		t.Errorf("--only selector = %+v", sel)
	}
	var noneChange ProfileChange
	decodeInto(t, mustRun(t, "", "profile", "tools", "work", "github", "--none", "--json"), &noneChange)
	if sel := noneChange.Profile.Tools["github"]; sel.Allow == nil || len(sel.Allow) != 0 {
		t.Errorf("--none must store the EMPTY allow list (block-all), got %+v", sel)
	}
	var allChange ProfileChange
	decodeInto(t, mustRun(t, "", "profile", "tools", "work", "github", "--all", "--json"), &allChange)
	if _, ok := allChange.Profile.Tools["github"]; ok {
		t.Errorf("--all must drop the (now inert) rule, got %+v", allChange.Profile.Tools)
	}
	// Exactly one of the three is required.
	if code, _, _ := runCLI(t, "", "profile", "tools", "work", "github"); code != ExitUsage {
		t.Errorf("tools with no mode exit = %d, want %d", code, ExitUsage)
	}
	if code, _, _ := runCLI(t, "", "profile", "tools", "work", "github", "--all", "--none"); code != ExitUsage {
		t.Errorf("tools with two modes exit = %d, want %d", code, ExitUsage)
	}

	// use / clear.
	mustRun(t, "", "profile", "use", "work")
	decodeInto(t, mustRun(t, "", "profile", "ls", "--json"), &list)
	if list.ActiveProfile != "work" || !list.Profiles[0].Active {
		t.Errorf("active profile not reported: %+v", list)
	}
	mustRun(t, "", "profile", "use", "-")
	decodeInto(t, mustRun(t, "", "profile", "ls", "--json"), &list)
	if list.ActiveProfile != "" {
		t.Errorf("profile use - did not clear the active profile: %+v", list)
	}
	if code, _, _ := runCLI(t, "", "profile", "use", "ghost"); code != ExitNotFound {
		t.Errorf("use unknown exit = %d, want %d", code, ExitNotFound)
	}
}

// TestProfileRenameRepointsClients: leaving a client pointed at the old name
// would fail-close it to an EMPTY scope, so rename must follow the
// references.
func TestProfileRenameRepointsClients(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "profile", "create", "work")
	mustRun(t, "", "client", "bind", "claude-code", "work")

	var change ProfileChange
	decodeInto(t, mustRun(t, "", "profile", "rename", "work", "work2", "--json"), &change)
	if len(change.Repointed) != 1 || change.Repointed[0] != "claude-code" {
		t.Fatalf("repointed = %v, want [claude-code]", change.Repointed)
	}
	var list ClientList
	decodeInto(t, mustRun(t, "", "client", "ls", "--json"), &list)
	if len(list.Clients) != 1 || list.Clients[0].Profile != "work2" || list.Clients[0].Dangling {
		t.Errorf("binding = %+v, want a live reference to work2", list.Clients)
	}
}

// TestProfileRemoveReportsDanglingClients: removal deliberately does NOT
// rewrite referencing clients (fail-closed), so it must warn about each one.
func TestProfileRemoveReportsDanglingClients(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "profile", "create", "work")
	mustRun(t, "", "client", "bind", "claude-code", "work")

	env := decodeInto(t, mustRun(t, "", "profile", "rm", "work", "--json"), nil)
	joined := strings.Join(env.Warnings, " ")
	if !strings.Contains(joined, "claude-code") || !strings.Contains(joined, "EMPTY scope") {
		t.Errorf("warnings = %v, want a loud dangling-reference warning", env.Warnings)
	}
	var list ClientList
	decodeInto(t, mustRun(t, "", "client", "ls", "--json"), &list)
	if len(list.Clients) != 1 || !list.Clients[0].Dangling {
		t.Errorf("client ls must mark the dangling binding: %+v", list.Clients)
	}
}

// TestClientBindAndUnbind covers the binding round trip.
func TestClientBindAndUnbind(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "github", "--cmd", "gh-mcp")
	mustRun(t, "", "profile", "create", "work")
	mustRun(t, "", "profile", "create", "personal")

	var res ClientBindResult
	decodeInto(t, mustRun(t, "", "client", "bind", "cursor", "work", "--json"), &res)
	if res.Binding.Binding != "named" || res.Binding.Profile != "work" {
		t.Fatalf("binding = %+v", res.Binding)
	}

	// Rebinding replaces the reference outright.
	var rebound ClientBindResult
	decodeInto(t, mustRun(t, "", "client", "bind", "cursor", "personal", "--json"), &rebound)
	if rebound.Binding.Profile != "personal" {
		t.Errorf("rebind = %+v, want personal", rebound.Binding)
	}

	// Binding to a profile nobody created is ACCEPTED but fail-closes, and
	// says so: refusing would stop an operator binding before creating, while
	// silence would show an empty tool list as though it were working.
	var ghost ClientBindResult
	env := decodeInto(t, mustRun(t, "", "client", "bind", "cursor", "ghost", "--json"), &ghost)
	if !ghost.Binding.Dangling {
		t.Errorf("binding to a missing profile not marked dangling: %+v", ghost.Binding)
	}
	if !strings.Contains(strings.Join(env.Warnings, " "), "EMPTY scope") {
		t.Errorf("warnings = %v, want the fail-closed warning", env.Warnings)
	}

	// Validation: both arguments are required.
	if code, _, _ := runCLI(t, "", "client", "bind", "cursor"); code != ExitUsage {
		t.Errorf("bind without a profile exit = %d, want %d", code, ExitUsage)
	}
	if code, _, _ := runCLI(t, "", "client", "bind"); code != ExitUsage {
		t.Errorf("bind with no arguments exit = %d, want %d", code, ExitUsage)
	}
	mustRun(t, "", "client", "unbind", "cursor")
	var list ClientList
	decodeInto(t, mustRun(t, "", "client", "ls", "--json"), &list)
	for _, c := range list.Clients {
		if c.Binding == "named" {
			t.Errorf("binding survived unbind: %+v", c)
		}
	}
	if code, _, _ := runCLI(t, "", "client", "unbind", "cursor"); code != ExitNotFound {
		t.Errorf("unbinding an absent binding exit = %d, want %d", code, ExitNotFound)
	}
}

// Discovery is a profile field: it describes the tool set it ships with.
func TestProfileDiscovery(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "profile", "create", "work")

	var change ProfileChange
	decodeInto(t, mustRun(t, "", "profile", "discovery", "work", "grouped", "--json"), &change)
	if change.Profile.Discovery != "grouped" {
		t.Fatalf("discovery = %q, want grouped", change.Profile.Discovery)
	}

	var cleared ProfileChange
	decodeInto(t, mustRun(t, "", "profile", "discovery", "work", "-", "--json"), &cleared)
	if cleared.Profile.Discovery != "" {
		t.Errorf("discovery = %q, want it cleared", cleared.Profile.Discovery)
	}

	if code, _, _ := runCLI(t, "", "profile", "discovery", "work", "telepathy"); code != ExitUsage {
		t.Errorf("bad discovery exit = %d, want %d", code, ExitUsage)
	}
	if code, _, _ := runCLI(t, "", "profile", "discovery", "ghost", "lazy"); code != ExitNotFound {
		t.Errorf("unknown profile exit = %d, want %d", code, ExitNotFound)
	}
}

// TestProfileToolsWarnsOnUnknownTool pins a fail-closed-but-invisible case:
// an `--only` name the server does not have stores a rule that lets NOTHING
// through, because an allow-list is an intersection.
//
// Both halves are asserted, and the second is the one that keeps the fix
// honest: the warning must not have become a refusal. The rule is still
// stored, and the command still succeeds.
func TestProfileToolsWarnsOnUnknownTool(t *testing.T) {
	dir := setDataDir(t)
	mustRun(t, "", "server", "add", "fs", "--cmd", "fs-server")
	mustRun(t, "", "profile", "create", "work")
	seedToolCache(t, dir, "fs", []map[string]any{
		{"name": "read_file", "inputSchema": map[string]any{"type": "object"}},
		{"name": "write_file", "inputSchema": map[string]any{"type": "object"}},
	})

	var change ProfileChange
	env := decodeInto(t, mustRun(t, "", "profile", "tools", "work", "fs",
		"--only", "read_file,raed_file", "--json"), &change)

	warning := strings.Join(env.Warnings, "\n")
	if !strings.Contains(warning, `"raed_file"`) {
		t.Errorf("the typo was not named in the warnings: %q", warning)
	}
	if strings.Contains(warning, `"read_file"`) {
		t.Errorf("a tool the server does have was reported as unknown: %q", warning)
	}
	// Stored, not refused: a cold cache must never block a rule, so the
	// check may only ever add a warning to a write that already happened.
	if got := change.Profile.Tools["fs"].Allow; !slices.Equal(got, []string{"raed_file", "read_file"}) {
		t.Errorf("selector = %v, want the rule stored exactly as typed", got)
	}
}

// TestProfileToolsSilentWithoutCatalog holds the other direction: a server no
// gateway has ever connected to has no catalog to check against, and a rule
// written ahead of that first connection is legitimate. Guessing "unknown"
// from an empty cache would warn about every correctly-spelled name.
func TestProfileToolsSilentWithoutCatalog(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "fs", "--cmd", "fs-server")
	mustRun(t, "", "profile", "create", "work")

	env := decodeInto(t, mustRun(t, "", "profile", "tools", "work", "fs",
		"--only", "read_file", "--json"), nil)
	for _, w := range env.Warnings {
		if strings.Contains(w, "no recorded tool") {
			t.Errorf("warned against a cache that was never written: %q", w)
		}
	}
}

// TestProfileDiscoveryHelpMarksTheRealDefault keeps `profile discovery --help`
// and discovery.DefaultMode in step. The help used to mark `full` as the
// default while the gateway had always fallen back to lazy — a wrong default
// in a help text is the failure that survives longest, because it is what a
// reader consults instead of running the command.
func TestProfileDiscoveryHelpMarksTheRealDefault(t *testing.T) {
	setDataDir(t)
	out := mustRun(t, "", "profile", "discovery", "--help")

	marked := make([]string, 0, 1)
	for _, mode := range []discovery.Mode{discovery.ModeFull, discovery.ModeGrouped, discovery.ModeLazy} {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), string(mode)+" ") &&
				strings.Contains(line, "(the default)") {
				marked = append(marked, string(mode))
			}
		}
	}
	want := []string{string(discovery.DefaultMode)}
	if !slices.Equal(marked, want) {
		t.Errorf("help marks %v as default, want exactly %v\n%s", marked, want, out)
	}
}
