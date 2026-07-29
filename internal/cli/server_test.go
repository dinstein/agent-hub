package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func decodeRows(t *testing.T, env envelope) []ServerRow {
	t.Helper()
	var rows []ServerRow
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		t.Fatalf("data is not a server list: %v\n%s", err, env.Data)
	}
	return rows
}

// TestServerRoundtrip is the M0-3 acceptance path: add -> ls -> rm against a
// temp data directory selected via AGENTHUB_DATA_DIR.
func TestServerRoundtrip(t *testing.T) {
	dir := setDataDir(t)

	code, out, stderr := runCLI(t, "", "server", "add", "github",
		"--cmd", "npx",
		"--args", "-y,@modelcontextprotocol/server-github",
		"--env", "GITHUB_TOKEN=${SECRET_GITHUB_TOKEN}",
		"--env", "LOG_LEVEL=debug",
		"--cwd", "/tmp")
	if code != ExitOK {
		t.Fatalf("add exit = %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(out, "added: github (stdio, source=manual, disabled)") {
		t.Errorf("add output = %q", out)
	}

	// The write landed under the overridden data dir (offline direct write).
	if _, err := os.Stat(filepath.Join(dir, "registry", "servers.json")); err != nil {
		t.Errorf("servers.json not under AGENTHUB_DATA_DIR: %v", err)
	}

	code, out, _ = runCLI(t, "", "server", "ls", "--json")
	if code != ExitOK {
		t.Fatalf("ls exit = %d", code)
	}
	env := decodeEnvelope(t, out)
	if !env.OK {
		t.Fatalf("ls envelope not ok: %s", out)
	}
	rows := decodeRows(t, env)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want 1 entry", rows)
	}
	r := rows[0]
	if r.ID != "github" || r.Transport != "stdio" || r.Command != "npx" ||
		len(r.Args) != 2 || r.Args[0] != "-y" || r.Args[1] != "@modelcontextprotocol/server-github" ||
		r.Env["GITHUB_TOKEN"] != "${SECRET_GITHUB_TOKEN}" || r.Env["LOG_LEVEL"] != "debug" ||
		r.Cwd != "/tmp" || r.Enabled || r.Source != "manual" {
		t.Errorf("row = %+v", r)
	}

	code, out, _ = runCLI(t, "", "server", "rm", "github")
	if code != ExitOK {
		t.Fatalf("rm exit = %d", code)
	}
	if !strings.Contains(out, "removed: github") {
		t.Errorf("rm output = %q", out)
	}

	code, out, _ = runCLI(t, "", "server", "ls", "--json")
	if code != ExitOK {
		t.Fatalf("ls exit = %d", code)
	}
	if rows := decodeRows(t, decodeEnvelope(t, out)); len(rows) != 0 {
		t.Errorf("rows after rm = %+v, want empty", rows)
	}
	// Empty list still marshals as [] (not null).
	if !strings.Contains(out, `"data":[]`) {
		t.Errorf("empty list must serialize as [], got %s", out)
	}
}

func TestServerRmNotFoundExit3(t *testing.T) {
	setDataDir(t)
	code, _, stderr := runCLI(t, "", "server", "rm", "ghost")
	if code != ExitNotFound {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitNotFound, stderr)
	}
	code, out, _ := runCLI(t, "", "server", "rm", "ghost", "--json")
	if code != ExitNotFound {
		t.Fatalf("exit = %d, want %d", code, ExitNotFound)
	}
	env := decodeEnvelope(t, out)
	if env.OK || env.Error == nil || env.Error.Code != CodeServerNotFound {
		t.Errorf("envelope = %s", out)
	}
}

func TestServerAddDuplicate(t *testing.T) {
	setDataDir(t)
	if code, _, _ := runCLI(t, "", "server", "add", "x", "--cmd", "foo"); code != ExitOK {
		t.Fatalf("first add failed")
	}
	code, out, _ := runCLI(t, "", "server", "add", "x", "--cmd", "bar", "--json")
	if code != ExitGeneral {
		t.Fatalf("duplicate add exit = %d, want 1", code)
	}
	env := decodeEnvelope(t, out)
	if env.OK || env.Error == nil || env.Error.Code != CodeServerExists {
		t.Errorf("envelope = %s", out)
	}
}

// TestHumanAndJSONSameSource proves the two output modes render from one
// data structure: every value surfaced in the JSON data must appear in the
// human table.
func TestHumanAndJSONSameSource(t *testing.T) {
	setDataDir(t)
	mustAdd := func(args ...string) {
		t.Helper()
		if code, _, stderr := runCLI(t, "", append([]string{"server", "add"}, args...)...); code != ExitOK {
			t.Fatalf("add failed: %s", stderr)
		}
	}
	mustAdd("github", "--cmd", "npx", "--args", "-y,@modelcontextprotocol/server-github")
	mustAdd("linear", "--cmd", "linear-mcp")

	_, jsonOut, _ := runCLI(t, "", "server", "ls", "--json")
	rows := decodeRows(t, decodeEnvelope(t, jsonOut))
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}

	_, humanOut, _ := runCLI(t, "", "server", "ls")
	for _, r := range rows {
		for _, val := range append([]string{r.ID, r.Transport, r.Command, r.Source, fmt.Sprint(r.Enabled)}, r.Args...) {
			if !strings.Contains(humanOut, val) {
				t.Errorf("human output missing %q from JSON data\nhuman: %s\njson: %s", val, humanOut, jsonOut)
			}
		}
	}
}

func TestServerAddStdinWrapper(t *testing.T) {
	setDataDir(t)
	stdin := `{"mcpServers":{
		"linear":{"command":"npx","args":["-y","linear-mcp"],"env":{"API_KEY":"${SECRET_LINEAR}"}},
		"gh":{"type":"stdio","command":"gh-mcp","cwd":"/srv"}
	}}`
	code, out, stderr := runCLI(t, stdin, "server", "add", "--stdin")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	// Deterministic (sorted) order.
	if !strings.Contains(out, "added: gh (stdio, source=manual, disabled)") ||
		!strings.Contains(out, "added: linear (stdio, source=manual, disabled)") {
		t.Errorf("output = %q", out)
	}

	_, lsOut, _ := runCLI(t, "", "server", "ls", "--json")
	rows := decodeRows(t, decodeEnvelope(t, lsOut))
	if len(rows) != 2 || rows[0].ID != "gh" || rows[1].ID != "linear" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Cwd != "/srv" || rows[1].Env["API_KEY"] != "${SECRET_LINEAR}" {
		t.Errorf("normalized fields lost: %+v", rows)
	}
}

func TestServerAddStdinSingleEntry(t *testing.T) {
	setDataDir(t)
	code, out, stderr := runCLI(t, `{"command":"npx","args":["-y","x-mcp"]}`,
		"server", "add", "xsrv", "--stdin")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(out, "added: xsrv (stdio, source=manual, disabled)") {
		t.Errorf("output = %q", out)
	}
}

func TestServerAddStdinSingleEntryRequiresName(t *testing.T) {
	setDataDir(t)
	code, _, _ := runCLI(t, `{"command":"npx"}`, "server", "add", "--stdin")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
}

func TestServerAddStdinBareMap(t *testing.T) {
	setDataDir(t)
	code, out, stderr := runCLI(t, `{"foo":{"command":"foo-bin"}}`, "server", "add", "--stdin")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(out, "added: foo") {
		t.Errorf("output = %q", out)
	}
}

func TestServerAddStdinWrapperRenamedByArg(t *testing.T) {
	setDataDir(t)
	code, out, _ := runCLI(t, `{"mcpServers":{"orig":{"command":"bin"}}}`,
		"server", "add", "renamed", "--stdin")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "added: renamed") {
		t.Errorf("output = %q", out)
	}
}

func TestServerAddStdinErrors(t *testing.T) {
	cases := []struct {
		name     string
		stdin    string
		args     []string
		wantExit int
		wantCode string
	}{
		{"malformed json", `{"mcpServers":{`, []string{"server", "add", "--stdin", "--json"}, ExitGeneral, CodeInvalidJSON},
		{"empty stdin", ``, []string{"server", "add", "--stdin", "--json"}, ExitGeneral, CodeInvalidJSON},
		{"private url refused", `{"mcpServers":{"r":{"url":"http://127.0.0.1:9/mcp"}}}`, []string{"server", "add", "--stdin", "--json"}, ExitUsage, CodeUsage},
		{"http entry without url", `{"mcpServers":{"r":{"type":"http","command":"c"}}}`, []string{"server", "add", "--stdin", "--json"}, ExitGeneral, CodeInvalidJSON},
		{"unknown transport", `{"mcpServers":{"r":{"type":"grpc","url":"https://x/mcp"}}}`, []string{"server", "add", "--stdin", "--json"}, ExitGeneral, CodeUnsupportedTransport},
		{"missing command", `{"mcpServers":{"r":{"args":["a"]}}}`, []string{"server", "add", "--stdin", "--json"}, ExitGeneral, CodeInvalidJSON},
		{"name vs multi-entry", `{"mcpServers":{"a":{"command":"c"},"b":{"command":"c"}}}`, []string{"server", "add", "n", "--stdin", "--json"}, ExitUsage, CodeUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setDataDir(t)
			code, out, _ := runCLI(t, tc.stdin, tc.args...)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d\n%s", code, tc.wantExit, out)
			}
			env := decodeEnvelope(t, out)
			if env.OK || env.Error == nil || env.Error.Code != tc.wantCode {
				t.Errorf("envelope = %s, want error code %s", out, tc.wantCode)
			}
		})
	}
}

// TestQuarantinedRegistryBecomesWarning: a corrupt document that the
// registry can self-heal (quarantine + reset) must not fail the command —
// it surfaces as a warning on the success envelope. Exit 7 is reserved for
// unhealable corruption.
func TestQuarantinedRegistryBecomesWarning(t *testing.T) {
	dir := setDataDir(t)
	if code, _, _ := runCLI(t, "", "server", "add", "x", "--cmd", "foo"); code != ExitOK {
		t.Fatalf("add failed")
	}
	serversPath := filepath.Join(dir, "registry", "servers.json")
	if err := os.WriteFile(serversPath, []byte("{{{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, _ := runCLI(t, "", "server", "ls", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (healed quarantine is a warning)\n%s", code, out)
	}
	env := decodeEnvelope(t, out)
	if !env.OK || len(env.Warnings) == 0 {
		t.Fatalf("envelope = %s, want ok with warnings", out)
	}
	if !strings.Contains(env.Warnings[0], "unreadable") {
		t.Errorf("warning = %q", env.Warnings[0])
	}
	// The healed document reset to defaults: the entry is gone.
	if rows := decodeRows(t, env); len(rows) != 0 {
		t.Errorf("rows = %+v", rows)
	}
}

// TestNormalizeStdinKeepsOAuthHints pins that pasted JSON's "oauth" block
// survives into the stored entry.
//
// Regression: stdinEntry did not model the field, so encoding/json dropped
// it without a word — `server add --stdin` reported success while the
// pinned issuer was silently absent, and the loss only surfaced later as an
// unrelated-looking discovery failure at `auth login`.
func TestNormalizeStdinKeepsOAuthHints(t *testing.T) {
	in := []byte(`{"mcpServers":{"elk":{"transport":"sse",` +
		`"url":"https://elk.example.com/sse",` +
		`"oauth":{"issuer":"https://as.example.com","scopes":["openid","read"]}}}}`)
	got, err := normalizeStdin(in, "")
	if err != nil {
		t.Fatalf("normalizeStdin: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	oa := got[0].Entry.OAuth
	if oa == nil {
		t.Fatal("oauth hints were dropped")
	}
	if oa.Issuer != "https://as.example.com" {
		t.Errorf("issuer = %q", oa.Issuer)
	}
	if !slices.Equal(oa.Scopes, []string{"openid", "read"}) {
		t.Errorf("scopes = %v", oa.Scopes)
	}
}

// TestNormalizeStdinRejectsUnknownFields is the general form of the bug
// above: a key we do not model must be an error, never a silent drop.
func TestNormalizeStdinRejectsUnknownFields(t *testing.T) {
	in := []byte(`{"mcpServers":{"x":{"transport":"sse",` +
		`"url":"https://x.example.com/sse","notAField":1}}}`)
	if _, err := normalizeStdin(in, ""); err == nil {
		t.Fatal("unknown field was accepted; it would have been dropped silently")
	}
}

// TestServerAddOAuthFlags: an issuer had to be pasted as JSON before these
// flags existed (`auth login --issuer` applies to one invocation and is
// never stored), so the one setting a remote server most often needs was
// also the only one with no flag.
//
// The hints are transport-independent, which is why the stdio case is here
// too: an stdio child may proxy to a remote authorization server.
func TestServerAddOAuthFlags(t *testing.T) {
	dir := setDataDir(t)

	code, out, stderr := runCLI(t, "", "server", "add", "elk",
		"--url", "https://elk.example.com/mcp",
		"--oauth-issuer", "https://as.example.com",
		"--oauth-scope", "openid,mcp:read",
		"--oauth-scope", "offline",
		"--oauth-resource-metadata", "https://elk.example.com/.well-known/oauth-protected-resource",
		"--json")
	if code != ExitOK {
		t.Fatalf("add exit = %d\n%s\n%s", code, out, stderr)
	}
	if code, out, _ = runCLI(t, "", "server", "add", "local-proxy",
		"--cmd", "proxy-mcp", "--oauth-issuer", "https://as.example.com", "--json"); code != ExitOK {
		t.Fatalf("stdio add exit = %d\n%s", code, out)
	}

	code, out, _ = runCLI(t, "", "server", "ls", "--json")
	if code != ExitOK {
		t.Fatalf("ls exit = %d\n%s", code, out)
	}
	byID := map[string]ServerRow{}
	for _, r := range decodeRows(t, decodeEnvelope(t, out)) {
		byID[r.ID] = r
	}

	oa := byID["elk"].OAuth
	if oa == nil {
		t.Fatal("elk: the oauth hints did not reach the registry")
	}
	if oa.Issuer != "https://as.example.com" {
		t.Errorf("issuer = %q", oa.Issuer)
	}
	// Repeated and comma-separated forms accumulate, in the order given.
	if !slices.Equal(oa.Scopes, []string{"openid", "mcp:read", "offline"}) {
		t.Errorf("scopes = %v", oa.Scopes)
	}
	if oa.ResourceMetadataURL != "https://elk.example.com/.well-known/oauth-protected-resource" {
		t.Errorf("resourceMetadataUrl = %q", oa.ResourceMetadataURL)
	}
	if got := byID["local-proxy"].OAuth; got == nil || got.Issuer != "https://as.example.com" {
		t.Errorf("stdio entry oauth = %+v", got)
	}
	// An entry given no hints must not grow an empty block on disk.
	if code, out, _ := runCLI(t, "", "server", "add", "plain", "--cmd", "plain-mcp", "--json"); code != ExitOK {
		t.Fatalf("plain add exit = %d\n%s", code, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "registry", "servers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"oauth":{}`) {
		t.Errorf("an entry without hints grew an empty oauth block:\n%s", raw)
	}
}

// TestServerAddOAuthFlagsRefuseBadValues: the flags go through confops'
// screen like every other writer does. Failure direction is refusal — a
// pinned issuer that discovery can never use must not be stored, because
// the loss surfaces much later as an unrelated-looking `auth login` error.
func TestServerAddOAuthFlagsRefuseBadValues(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"issuer is not a URL", []string{"--oauth-issuer", "as.example.com"}},
		{"plaintext issuer", []string{"--oauth-issuer", "http://as.example.com"}},
		{"private issuer", []string{"--oauth-issuer", "https://10.1.2.3"}},
		{"issuer with a query", []string{"--oauth-issuer", "https://as.example.com/?t=1"}},
		{"scope holding two scopes", []string{"--oauth-scope", "openid profile"}},
		{"resource metadata is not a URL", []string{"--oauth-resource-metadata", "/prm"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setDataDir(t)
			args := append([]string{"server", "add", "x", "--url", "https://x.example.com/mcp", "--json"}, tc.args...)
			code, out, _ := runCLI(t, "", args...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d\n%s", code, ExitUsage, out)
			}
			if code, out, _ := runCLI(t, "", "server", "ls", "--json"); code != ExitOK ||
				len(decodeRows(t, decodeEnvelope(t, out))) != 0 {
				t.Errorf("a rejected entry was stored anyway: %s", out)
			}
		})
	}
}

// TestServerAddOAuthFlagsRejectStdin: --stdin carries its own oauth block,
// so combining the two forms would leave two sources for one field.
func TestServerAddOAuthFlagsRejectStdin(t *testing.T) {
	setDataDir(t)
	code, out, _ := runCLI(t, `{"mcpServers":{"x":{"url":"https://x.example.com/mcp"}}}`,
		"server", "add", "--stdin", "--oauth-issuer", "https://as.example.com", "--json")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitUsage, out)
	}
}

// The trace switch round-trips through the CLI, and the listing surfaces it.
// A trace writes raw downstream payloads to disk, so "I forgot it was on"
// has to be answerable from the listing an operator already reads.
func TestServerTraceRoundTrip(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "fake", "--cmd", "fake-mcp")

	// Off by default, and the column is absent while nothing is traced.
	out := mustRun(t, "", "server", "ls")
	if strings.Contains(out, "TRACE") {
		t.Errorf("the TRACE column is shown when nothing is traced:\n%s", out)
	}

	var row struct {
		Trace bool   `json:"trace"`
		Path  string `json:"path"`
	}
	decodeInto(t, mustRun(t, "", "server", "trace", "fake", "on", "--json"), &row)
	if !row.Trace {
		t.Error("trace on did not report the switch as on")
	}
	if !strings.HasSuffix(row.Path, "server-fake.log") {
		t.Errorf("path = %q, want the per-server log the reader looks for", row.Path)
	}

	// Now it shows, so a forgotten trace is visible where servers are listed.
	out = mustRun(t, "", "server", "ls")
	if !strings.Contains(out, "TRACE") {
		t.Errorf("a traced server does not show the TRACE column:\n%s", out)
	}

	decodeInto(t, mustRun(t, "", "server", "trace", "fake", "off", "--json"), &row)
	if row.Trace {
		t.Error("trace off did not report the switch as off")
	}
	if out = mustRun(t, "", "server", "ls"); strings.Contains(out, "TRACE") {
		t.Errorf("the column outlived the last trace:\n%s", out)
	}
}

// A misspelled state is a usage error, never a silent "off". Guessing here
// would quietly leave a server untraced while the operator believes it is
// recording, which is the one failure this command cannot afford.
func TestServerTraceRejectsUnknownState(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "fake", "--cmd", "fake-mcp")
	for _, bad := range []string{"true", "yes", "1", "ON"} {
		if code, out, _ := runCLI(t, "", "server", "trace", "fake", bad); code != ExitUsage {
			t.Errorf("state %q: exit = %d, want %d\n%s", bad, code, ExitUsage, out)
		}
	}
	if code, out, _ := runCLI(t, "", "server", "trace", "ghost", "on"); code != ExitNotFound {
		t.Errorf("tracing an unknown server: exit = %d, want %d\n%s", code, ExitNotFound, out)
	}
}
