package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/discovery"
)

// pointActiveProfileAt rewrites the active-profile marker in governance.json
// to a name the CLI would refuse to set, which is the only way to reach a
// dangling active profile: `profile use` rejects an unknown name and
// `profile rm` clears the marker along with the profile. The marker lives in
// the registry rather than a state file because scope resolution is pure.
func pointActiveProfileAt(t *testing.T, dataDir, name string) {
	t.Helper()
	path := filepath.Join(dataDir, "registry", "governance.json")
	doc := map[string]any{}
	// The document is written on first governance edit, so on a fresh data
	// directory there is nothing to read yet — an absent file is the same
	// starting point as an empty one, but a read error of any other kind is
	// a real failure and must not be swallowed into it.
	switch raw, err := os.ReadFile(path); {
	case err == nil:
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
	case !os.IsNotExist(err):
		t.Fatal(err)
	default:
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	doc["activeProfile"] = name
	edited, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
}

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
	decodeInto(t, mustRun(t, "", "profile", "tool", "allow", "work", "github", "--only", "list_prs,create_pr", "--json"), &onlyChange)
	if sel := onlyChange.Profile.Tools["github"]; len(sel.Allow) != 2 || sel.Allow[0] != "create_pr" {
		t.Errorf("--only selector = %+v", sel)
	}
	var noneChange ProfileChange
	decodeInto(t, mustRun(t, "", "profile", "tool", "allow", "work", "github", "--none", "--json"), &noneChange)
	if sel := noneChange.Profile.Tools["github"]; sel.Allow == nil || len(sel.Allow) != 0 {
		t.Errorf("--none must store the EMPTY allow list (block-all), got %+v", sel)
	}
	var allChange ProfileChange
	decodeInto(t, mustRun(t, "", "profile", "tool", "allow", "work", "github", "--all", "--json"), &allChange)
	if _, ok := allChange.Profile.Tools["github"]; ok {
		t.Errorf("--all must drop the (now inert) rule, got %+v", allChange.Profile.Tools)
	}
	// Exactly one of the three is required.
	if code, _, _ := runCLI(t, "", "profile", "tool", "allow", "work", "github"); code != ExitUsage {
		t.Errorf("tools with no mode exit = %d, want %d", code, ExitUsage)
	}
	if code, _, _ := runCLI(t, "", "profile", "tool", "allow", "work", "github", "--all", "--none"); code != ExitUsage {
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
	env := decodeInto(t, mustRun(t, "", "profile", "tool", "allow", "work", "fs",
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

	env := decodeInto(t, mustRun(t, "", "profile", "tool", "allow", "work", "fs",
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

// TestProfileLsDefaultRow pins the row that made `client ls` legible: the
// PROFILE cell there says "(default)", and this table has to contain
// something by that name. It also has to say what (default) RESOLVES to,
// which is the active profile's own content — a reader should not have to
// join two rows by hand.
func TestProfileLsDefaultRow(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "github", "--cmd", "gh-mcp")
	mustRun(t, "", "profile", "create", "work", "--servers", "github")

	// No active profile: (default) is in force and takes nothing away.
	var list ProfileList
	decodeInto(t, mustRun(t, "", "profile", "ls", "--json"), &list)
	if list.Default.Profile != "" || list.Default.Servers != nil || list.Default.Dangling {
		t.Errorf("default with no active profile = %+v, want the empty, non-narrowing one", list.Default)
	}
	// The synthetic row must not leak into the array a script feeds back to
	// 'profile rm'.
	for _, p := range list.Profiles {
		if strings.HasPrefix(p.Name, "(") {
			t.Errorf("profiles[] carries the display token %q", p.Name)
		}
	}
	out := mustRun(t, "", "profile", "ls")
	if !strings.Contains(out, "(default)") {
		t.Errorf("the human table has no (default) row:\n%s", out)
	}

	// With one: (default) points at it and repeats its content.
	mustRun(t, "", "profile", "use", "work")
	decodeInto(t, mustRun(t, "", "profile", "ls", "--json"), &list)
	if list.Default.Profile != "work" || len(list.Default.Servers) != 1 || list.Default.Servers[0] != "github" {
		t.Errorf("default = %+v, want it resolved to work's own servers", list.Default)
	}
	if out := mustRun(t, "", "profile", "ls"); !strings.Contains(out, "(default) -> work") {
		t.Errorf("the human table does not point (default) at the active profile:\n%s", out)
	}
}

// A (default) that resolves nowhere must be as loud as a client bound to a
// missing profile: both fail-close to an empty scope, and the server column
// has to show the empty set rather than "(all registered)" — the reader is
// otherwise told the opposite of what those clients get.
func TestProfileLsDefaultRowShowsADanglingActiveProfile(t *testing.T) {
	dir := setDataDir(t)
	// Neither `profile use` nor `profile rm` can produce this state — the
	// first refuses an unknown name, the second clears the marker with the
	// profile — so it is reached the only way a user reaches it: a stale or
	// hand-edited governance document.
	pointActiveProfileAt(t, dir, "ghost")

	var list ProfileList
	env := decodeInto(t, mustRun(t, "", "profile", "ls", "--json"), &list)
	if !list.Default.Dangling {
		t.Fatalf("default = %+v, want it marked dangling", list.Default)
	}
	if list.Default.Servers == nil || len(list.Default.Servers) != 0 {
		t.Errorf("dangling default servers = %v, want the EMPTY block-all list", list.Default.Servers)
	}
	if len(env.Warnings) == 0 {
		t.Error("a dangling active profile produced no warning")
	}
	out := mustRun(t, "", "profile", "ls")
	if !strings.Contains(out, "(default) -> ghost") || !strings.Contains(out, "MISSING") {
		t.Errorf("the human table does not flag the dangling default:\n%s", out)
	}
}

// TestProfileLsDiscoveryIsResolved: the column used to print the configured
// value, so "-" meant both "no mode here" and "decided elsewhere". It now
// prints the mode that will be used, and marks the ones the profile does not
// own — while the configured value stays in its own JSON field, or a
// consumer round-tripping it would pin what was inherited.
func TestProfileLsDiscoveryIsResolved(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "profile", "create", "pinned")
	mustRun(t, "", "profile", "discovery", "pinned", "full")
	mustRun(t, "", "profile", "create", "inherits")

	byName := func(list ProfileList, name string) ProfileRow {
		t.Helper()
		for _, p := range list.Profiles {
			if p.Name == name {
				return p
			}
		}
		t.Fatalf("profile %q not listed", name)
		return ProfileRow{}
	}

	var list ProfileList
	decodeInto(t, mustRun(t, "", "profile", "ls", "--json"), &list)
	if got := byName(list, "pinned"); got.EffectiveDiscovery != "full" || got.DiscoverySource != "profile" {
		t.Errorf("pinned = %+v, want full from the profile", got)
	}
	// Nothing set anywhere: the built-in default, named as such.
	if got := byName(list, "inherits"); got.Discovery != "" ||
		got.EffectiveDiscovery != string(discovery.DefaultMode) || got.DiscoverySource != "builtin" {
		t.Errorf("inherits = %+v, want the built-in %s", got, discovery.DefaultMode)
	}
	if list.DefaultDiscovery != string(discovery.DefaultMode) || list.DefaultDiscoverySource != "builtin" {
		t.Errorf("default discovery = %q/%q, want the built-in",
			list.DefaultDiscovery, list.DefaultDiscoverySource)
	}

	// A global default moves every inheriting profile and nothing else.
	mustRun(t, "", "config", "set", "discovery", "grouped")
	decodeInto(t, mustRun(t, "", "profile", "ls", "--json"), &list)
	if got := byName(list, "inherits"); got.EffectiveDiscovery != "grouped" || got.DiscoverySource != "global" {
		t.Errorf("inherits = %+v, want grouped from governance.json", got)
	}
	if got := byName(list, "pinned"); got.EffectiveDiscovery != "full" {
		t.Errorf("pinned = %+v, want its own mode to survive the global default", got)
	}
	out := mustRun(t, "", "profile", "ls")
	if !strings.Contains(out, "grouped (inherited)") || !strings.Contains(out, "\tfull\t") &&
		!strings.Contains(out, "full  ") {
		t.Errorf("the human table does not distinguish inherited from owned:\n%s", out)
	}
}

// The one-release `profile tools <profile> <server>` shim shipped in v0.14.0
// and is gone; `tools` is now the plural alias of the `profile tool` group,
// like `profile server`/`servers`. Both halves matter: the alias must reach
// the same command, and the retired leaf form must be REFUSED — the shim was
// hidden precisely because an alias would have swallowed it, so if this
// spelling ever answers again, the group has grown a leaf it should not have.
func TestProfileToolsIsTheGroupAliasAndTheOldLeafIsGone(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "github", "--cmd", "gh-mcp")
	mustRun(t, "", "profile", "create", "work")

	out := mustRun(t, "", "profile", "tools", "allow", "work", "github", "--only", "list_prs", "--json")
	if env := decodeEnvelope(t, out); !env.OK {
		t.Fatalf("the plural alias did not produce a clean envelope: %s", out)
	}
	var row ProfileList
	decodeInto(t, mustRun(t, "", "profile", "ls", "--json"), &row)
	found := false
	for _, p := range row.Profiles {
		if p.Name == "work" && len(p.Tools["github"].Allow) == 1 && p.Tools["github"].Allow[0] == "list_prs" {
			found = true
		}
	}
	if !found {
		t.Errorf("the plural alias did not write the rule: %+v", row.Profiles)
	}

	code, _, stderr := runCLI(t, "", "profile", "tools", "work", "github")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d: the retired leaf spelling must be refused, stderr = %q",
			code, ExitUsage, stderr)
	}
}
