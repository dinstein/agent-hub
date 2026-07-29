package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/registry"
)

// The detail view of `server inspect`. What these tests hold in place is not
// the layout — it is which FACTS the view is answerable for. Every one of
// them was invisible in a report that claimed to describe a whole server,
// and each was findable only by reading a file or a second command.

// TestInspectShowsTheContainerItWouldActuallyRun pins the isolation half.
// "Isolation a config claims must be delivered or refused" cannot be checked
// by an operator who is shown `docker[image] cmd` and has to trust that the
// mounts, the network and the limits they typed reached the run line.
func TestInspectShowsTheContainerItWouldActuallyRun(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "box", "--cmd", "srv", "--args", "--stdio",
		"--image", "ghcr.io/example/mcp:1", "--mount", "/host/src:/src:ro",
		"--network", "bridge", "--memory", "512m", "--cpus", "1.5")

	_, out, _ := runCLI(t, "", "server", "inspect", "box")
	for _, want := range []string{
		"image=ghcr.io/example/mcp:1", "network=bridge", "memory=512m", "cpus=1.5",
		"/host/src -> /src (ro)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect does not report %q:\n%s", want, out)
		}
	}

	// The printed line must be the SPAWNED one, not a second rendering that
	// can drift from it. It is compared against the renderer the spawn guard
	// screens, which is the same anti-drift check dockerruntime_test makes
	// for the other direction.
	var insp ServerInspect
	decodeInto(t, mustRun(t, "", "server", "inspect", "box", "--json"), &insp)
	entry := registry.ServerEntry{
		Transport: registry.TransportStdio, Command: "srv", Args: []string{"--stdio"},
		Runtime: registry.RuntimeDocker,
		Docker: &registry.DockerRuntime{
			Image: "ghcr.io/example/mcp:1", Network: "bridge", Memory: "512m", CPUs: "1.5",
			Mounts: []registry.DockerMount{{Source: "/host/src", Target: "/src"}},
		},
	}
	want, err := confops.DockerRunLine("box", entry)
	if err != nil {
		t.Fatalf("DockerRunLine: %v", err)
	}
	if strings.Join(insp.DockerRun, " ") != strings.Join(want, " ") {
		t.Errorf("printed run line differs from the spawned one:\n got %v\nwant %v",
			insp.DockerRun, want)
	}
	if !strings.Contains(out, "spawns     docker "+strings.Join(want, " ")) {
		t.Errorf("the human run line is not pasteable:\n%s", out)
	}
}

// TestInspectShowsWhereTheEnvAndHeadersLanded: the placeholders are the point
// of the fields, and nothing else printed them. Answering "did my --env
// arrive, and under which name" required reading servers.json.
func TestInspectShowsWhereTheEnvAndHeadersLanded(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "fs", "--cmd", "srv",
		"--env", "TOKEN=${SECRET_FS_TOKEN}", "--env", "ROOT=/tmp")
	_, out, _ := runCLI(t, "", "server", "inspect", "fs")
	for _, want := range []string{"TOKEN=${SECRET_FS_TOKEN}", "ROOT=/tmp"} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect does not report %q:\n%s", want, out)
		}
	}
}

// TestInspectDoesNotEchoALiteralAuthorizationHeader is the one exception to
// that rule, and it is the case where the registry's own assumption is
// already broken: header values are printable BECAUSE they are supposed to be
// ${SECRET_X} placeholders, so a literal one is a pasted token and inspect
// will not read it back out to a terminal.
func TestInspectDoesNotEchoALiteralAuthorizationHeader(t *testing.T) {
	setDataDir(t)
	const token = "Bearer sk-live-DO-NOT-ECHO-4f2b9c"
	mustRun(t, "", "server", "add", "lit", "--url", "https://mcp.example.com/mcp",
		"--header", "Authorization="+token)

	_, out, _ := runCLI(t, "", "server", "inspect", "lit")
	if strings.Contains(out, "sk-live-DO-NOT-ECHO-4f2b9c") {
		t.Fatalf("inspect echoed a literal credential:\n%s", out)
	}
	if !strings.Contains(out, "literal value, not shown") {
		t.Errorf("inspect hid the header without saying so:\n%s", out)
	}

	// A placeholder in the SAME header is printed: hiding it would remove the
	// only way to see which vault key a header refers to.
	mustRun(t, "", "server", "add", "ref", "--url", "https://mcp.example.com/mcp",
		"--header", "Authorization=Bearer ${SECRET_REF_TOKEN}")
	_, out, _ = runCLI(t, "", "server", "inspect", "ref")
	if !strings.Contains(out, "${SECRET_REF_TOKEN}") {
		t.Errorf("inspect hid a placeholder:\n%s", out)
	}
}

// TestInspectNamesTheTraceFileWhileTracingIsOn: `server ls` has a TRACE
// column and inspect said nothing at all, so the one view that describes a
// server completely omitted that it was writing unredacted payloads to disk.
func TestInspectNamesTheTraceFileWhileTracingIsOn(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "fs", "--cmd", "srv")

	_, out, _ := runCLI(t, "", "server", "inspect", "fs")
	if strings.Contains(out, "trace") {
		t.Errorf("inspect mentions tracing on a server that is not traced:\n%s", out)
	}

	mustRun(t, "", "server", "trace", "fs", "on")
	_, out, _ = runCLI(t, "", "server", "inspect", "fs")
	if !strings.Contains(out, "server-fs.log") || !strings.Contains(out, "BEFORE redaction") {
		t.Errorf("inspect does not report the trace file:\n%s", out)
	}
}

// TestInspectSeparatesAnEmptyCatalogFromAnUnvisitedOne. "cached tools: 0" was
// the same sentence for a server that offers no tools and for one nothing has
// ever connected to — the second of which is not a fact about the server at
// all.
func TestInspectSeparatesAnEmptyCatalogFromAnUnvisitedOne(t *testing.T) {
	dir := setDataDir(t)
	mustRun(t, "", "server", "add", "fs", "--cmd", "srv")

	_, out, _ := runCLI(t, "", "server", "inspect", "fs")
	if !strings.Contains(out, "no gateway has connected") {
		t.Errorf("an unvisited server is not reported as one:\n%s", out)
	}

	writeToolCache(t, dir, "fs", time.Now().Add(-90*time.Minute), []mcp.ToolDef{
		{Name: "read_file", Description: "read a file"},
	})
	_, out, _ = runCLI(t, "", "server", "inspect", "fs")
	if !strings.Contains(out, "1 tool(s), recorded 1h ago") {
		t.Errorf("inspect does not date the cached catalog:\n%s", out)
	}

	var insp ServerInspect
	decodeInto(t, mustRun(t, "", "server", "inspect", "fs", "--json"), &insp)
	if insp.CachedAt.IsZero() {
		t.Errorf("the JSON envelope dropped the cache timestamp: %+v", insp)
	}
}

// writeToolCache plants a persisted catalog the way a gateway session would
// have left one, so the offline reader can be tested without running one.
func writeToolCache(t *testing.T, dataDir, server string, savedAt time.Time, tools []mcp.ToolDef) {
	t.Helper()
	dir := filepath.Join(dataDir, "cache", "tools")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	data, err := json.Marshal(map[string]any{
		"server": server, "savedAt": savedAt, "tools": tools,
	})
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, server+".json"), data, 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}
}

// TestHumanAgeAnswersTheQuestionItIsAskedFor: a diagnostic reader wants to
// know whether a figure is current or from another era, and a clock that
// moved backwards must not become a large positive age.
func TestHumanAgeAnswersTheQuestionItIsAskedFor(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{-time.Hour, "in the future"},
		{5 * time.Second, "just now"},
		{90 * time.Second, "1m ago"},
		{90 * time.Minute, "1h ago"},
		{50 * time.Hour, "2d ago"},
	}
	for _, tc := range cases {
		if got := humanAge(tc.in); !strings.Contains(got, tc.want) {
			t.Errorf("humanAge(%s) = %q, want it to contain %q", tc.in, got, tc.want)
		}
	}
}

// TestInspectAnswersWhoCanSeeTheServer covers the join no other command
// makes. Every fact here was already on disk; getting to it meant reading
// `profile ls` and `client ls` and intersecting them per server by hand,
// which is exactly the arithmetic people get wrong when a client "cannot see
// the tools" and everything else looks healthy.
func TestInspectAnswersWhoCanSeeTheServer(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "fs", "--cmd", "srv")
	mustRun(t, "", "server", "enable", "fs", "--no-probe")
	mustRun(t, "", "profile", "create", "dev", "--servers", "fs")
	mustRun(t, "", "profile", "create", "locked", "--servers", "other")
	mustRun(t, "", "profile", "use", "dev")
	mustRun(t, "", "client", "bind", "cursor", "locked")
	mustRun(t, "", "client", "bind", "zed", "dev")

	_, out, _ := runCLI(t, "", "server", "inspect", "fs")
	for _, want := range []string{
		"seen by", "zed (dev)", "hidden", "cursor (locked)",
		`active profile "dev", which includes it`,
		"dev (active)", "not in: locked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect does not report %q:\n%s", want, out)
		}
	}

	var insp ServerInspect
	decodeInto(t, mustRun(t, "", "server", "inspect", "fs", "--json"), &insp)
	if insp.Visibility == nil || len(insp.Visibility.Clients) != 2 {
		t.Fatalf("visibility = %+v", insp.Visibility)
	}
	for _, c := range insp.Visibility.Clients {
		if want := c.Client == "zed"; c.Sees != want {
			t.Errorf("client %s sees = %v, want %v", c.Client, c.Sees, want)
		}
	}
}

// TestInspectSaysADisabledServerReachesNobody: the global switch outranks
// every profile, so a profile list that implies otherwise would be the more
// prominent falsehood.
func TestInspectSaysADisabledServerReachesNobody(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "fs", "--cmd", "srv")
	mustRun(t, "", "client", "bind", "cursor", "dev")

	_, out, _ := runCLI(t, "", "server", "inspect", "fs")
	if !strings.Contains(out, "nobody — the server is disabled") {
		t.Errorf("inspect does not report the global switch:\n%s", out)
	}
	if strings.Contains(out, "seen by    cursor") {
		t.Errorf("a disabled server is reported as visible to a client:\n%s", out)
	}
}

// TestInspectReportsABindingThatResolvesNowhere. A dangling profile
// reference fail-closes to an EMPTY scope, and from the outside that looks
// exactly like a deliberate exclusion — the two need different repairs.
func TestInspectReportsABindingThatResolvesNowhere(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "fs", "--cmd", "srv")
	mustRun(t, "", "server", "enable", "fs", "--no-probe")
	mustRun(t, "", "client", "bind", "cursor", "ghost")

	_, out, _ := runCLI(t, "", "server", "inspect", "fs")
	if !strings.Contains(out, "cursor (ghost MISSING -> empty scope)") {
		t.Errorf("inspect does not flag the dangling binding:\n%s", out)
	}
	if !strings.Contains(out, "no active profile") {
		t.Errorf("inspect does not state what an unbound client gets:\n%s", out)
	}
}

// TestInspectCountsTheLocalToolOverrides: what a client is shown differs
// from what the downstream calls its own tools, and comparing this report
// against a client's tool list without knowing that is a wild goose chase.
func TestInspectCountsTheLocalToolOverrides(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "gh", "--cmd", "srv")
	mustRun(t, "", "server", "enable", "gh", "--no-probe")
	mustRun(t, "", "tool", "override", "gh", "list_prs", "--name", "prs")

	_, out, _ := runCLI(t, "", "server", "inspect", "gh")
	if !strings.Contains(out, "1 tool(s) are exposed under a local name") {
		t.Errorf("inspect does not report the override:\n%s", out)
	}
}
