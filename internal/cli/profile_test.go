package cli

import (
	"encoding/json"
	"strings"
	"testing"
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
	mustRun(t, "", "scope", "set", "--client", "claude-code", "--profile", "work")

	var change ProfileChange
	decodeInto(t, mustRun(t, "", "profile", "rename", "work", "work2", "--json"), &change)
	if len(change.Repointed) != 1 || change.Repointed[0] != "claude-code" {
		t.Fatalf("repointed = %v, want [claude-code]", change.Repointed)
	}
	var scopes ScopeList
	decodeInto(t, mustRun(t, "", "scope", "ls", "--json"), &scopes)
	if len(scopes.Bindings) != 1 || scopes.Bindings[0].Profile != "work2" || scopes.Bindings[0].Dangling {
		t.Errorf("binding = %+v, want a live reference to work2", scopes.Bindings)
	}
}

// TestProfileRemoveReportsDanglingClients: removal deliberately does NOT
// rewrite referencing clients (fail-closed), so it must warn about each one.
func TestProfileRemoveReportsDanglingClients(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "profile", "create", "work")
	mustRun(t, "", "scope", "set", "--client", "claude-code", "--profile", "work")

	env := decodeInto(t, mustRun(t, "", "profile", "rm", "work", "--json"), nil)
	joined := strings.Join(env.Warnings, " ")
	if !strings.Contains(joined, "claude-code") || !strings.Contains(joined, "EMPTY scope") {
		t.Errorf("warnings = %v, want a loud dangling-reference warning", env.Warnings)
	}
	var scopes ScopeList
	decodeInto(t, mustRun(t, "", "scope", "ls", "--json"), &scopes)
	if !scopes.Bindings[0].Dangling {
		t.Errorf("scope ls must mark the dangling binding: %+v", scopes.Bindings[0])
	}
}

// TestScopeSetAndClear covers the client-layer round trip.
func TestScopeSetAndClear(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "github", "--cmd", "gh-mcp")
	mustRun(t, "", "profile", "create", "work")

	var res ScopeSetResult
	decodeInto(t, mustRun(t, "",
		"scope", "set", "--client", "cursor", "--profile", "work",
		"--servers", "github", "--tools", "github:list_prs", "--discovery", "grouped", "--json"), &res)
	b := res.Binding
	if b.Binding != "named" || b.Profile != "work" || b.Discovery != "grouped" {
		t.Fatalf("binding = %+v", b)
	}
	if len(b.Servers) != 1 || b.Servers[0] != "github" {
		t.Errorf("servers = %v", b.Servers)
	}
	if got := b.Tools["github"].Allow; len(got) != 1 || got[0] != "list_prs" {
		t.Errorf("tool selector = %v", got)
	}

	// Amending touches only what was named.
	var amended ScopeSetResult
	decodeInto(t, mustRun(t, "", "scope", "set", "--client", "cursor", "--discovery", "lazy", "--json"), &amended)
	if amended.Binding.Profile != "work" || amended.Binding.Discovery != "lazy" {
		t.Errorf("amend dropped an unrelated field: %+v", amended.Binding)
	}
	// An empty --tools value for a server is block-all, not "all".
	var blocked ScopeSetResult
	decodeInto(t, mustRun(t, "", "scope", "set", "--client", "cursor", "--tools", "github:", "--json"), &blocked)
	if allow := blocked.Binding.Tools["github"].Allow; allow == nil || len(allow) != 0 {
		t.Errorf("empty tool list must be block-all, got %v", allow)
	}

	// Validation.
	if code, _, _ := runCLI(t, "", "scope", "set", "--client", "cursor", "--discovery", "bogus"); code != ExitUsage {
		t.Errorf("bad discovery exit = %d, want %d", code, ExitUsage)
	}
	if code, _, _ := runCLI(t, "", "scope", "set", "--client", "cursor"); code != ExitUsage {
		t.Errorf("scope set with nothing to set exit = %d, want %d", code, ExitUsage)
	}
	if code, _, _ := runCLI(t, "", "scope", "set", "--profile", "work"); code != ExitUsage {
		t.Errorf("scope set without --client exit = %d, want %d", code, ExitUsage)
	}
	if code, _, _ := runCLI(t, "", "scope", "set", "--client", "x", "--servers", "ghost"); code != ExitNotFound {
		t.Errorf("scope set with an unknown server exit = %d, want %d", code, ExitNotFound)
	}

	mustRun(t, "", "scope", "clear", "--client", "cursor")
	var list ScopeList
	decodeInto(t, mustRun(t, "", "scope", "ls", "--json"), &list)
	if len(list.Bindings) != 0 {
		t.Errorf("bindings survived clear: %+v", list.Bindings)
	}
	if code, _, _ := runCLI(t, "", "scope", "clear", "--client", "cursor"); code != ExitNotFound {
		t.Errorf("clearing an absent binding exit = %d, want %d", code, ExitNotFound)
	}
}
