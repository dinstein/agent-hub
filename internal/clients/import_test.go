package clients_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dinstein/agent-hub/internal/clients"
	"github.com/dinstein/agent-hub/internal/registry"
)

// TestImportConversionMatrix pins the client-entry -> registry.ServerEntry
// mapping for every spelling the ecosystem uses, plus every refusal.
func TestImportConversionMatrix(t *testing.T) {
	e := newEnv(t, "darwin")
	write(t, filepath.Join(e.project, ".mcp.json"), `{"mcpServers": {
      "stdio-plain":   {"command": "npx", "args": ["-y", "server-filesystem", "/tmp"]},
      "stdio-full":    {"command": "uvx", "args": ["x"], "env": {"A": "1"}, "cwd": "/w", "type": "stdio"},
      "http-inferred": {"url": "https://example.com/mcp"},
      "http-typed":    {"type": "streamable-http", "url": "https://example.com/mcp", "headers": {"X-A": "1"}},
      "http-alias":    {"transport": "streamableHttp", "serverUrl": "https://alias.example.com/mcp"},
      "sse-typed":     {"type": "sse", "url": "https://example.com/sse"},
      "disabled":      {"command": "x", "disabled": true},
      "My Server!":    {"command": "weird"},
      "no-shape":      {"description": "nothing actionable here"},
      "http-no-url":   {"type": "http"},
      "not-an-object": "scalar",
      "agenthub":      {"command": "/usr/local/bin/agenthub", "args": ["connect", "--client", "claude-code"]}
  }}`)

	res, err := e.tbl.Import("claude-code", e.project, nil)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	want := map[string]registry.ServerEntry{
		"stdio-plain": {
			Transport: registry.TransportStdio, Command: "npx",
			Args:    []string{"-y", "server-filesystem", "/tmp"},
			Enabled: true, Source: "imported:claude-code",
		},
		"stdio-full": {
			Transport: registry.TransportStdio, Command: "uvx", Args: []string{"x"},
			Env: map[string]string{"A": "1"}, Cwd: "/w",
			Enabled: true, Source: "imported:claude-code",
		},
		"http-inferred": {
			Transport: registry.TransportHTTP, URL: "https://example.com/mcp",
			Provenance: registry.ProvenanceRemote, Enabled: true, Source: "imported:claude-code",
		},
		"http-typed": {
			Transport: registry.TransportHTTP, URL: "https://example.com/mcp",
			Headers:    map[string]string{"X-A": "1"},
			Provenance: registry.ProvenanceRemote, Enabled: true, Source: "imported:claude-code",
		},
		"http-alias": {
			Transport: registry.TransportHTTP, URL: "https://alias.example.com/mcp",
			Provenance: registry.ProvenanceRemote, Enabled: true, Source: "imported:claude-code",
		},
		"sse-typed": {
			Transport: registry.TransportSSE, URL: "https://example.com/sse",
			Provenance: registry.ProvenanceRemote, Enabled: true, Source: "imported:claude-code",
		},
		"disabled": {
			Transport: registry.TransportStdio, Command: "x",
			Enabled: false, Source: "imported:claude-code",
		},
		"My_Server_": {
			Transport: registry.TransportStdio, Command: "weird",
			Enabled: true, Source: "imported:claude-code",
		},
	}
	if len(res.Entries) != len(want) {
		t.Fatalf("imported %d entries, want %d: %v", len(res.Entries), len(want), keysOf(res.Entries))
	}
	for name, w := range want {
		got, ok := res.Entries[name]
		if !ok {
			t.Errorf("entry %q missing", name)
			continue
		}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("entry %q =\n  %+v\nwant\n  %+v", name, got, w)
		}
	}

	// A sanitised name is reported so the user can see the rename.
	if res.Renamed["My_Server_"] != "My Server!" {
		t.Errorf("renamed = %v", res.Renamed)
	}

	// Every refusal is reported, never silently dropped.
	skipped := map[string]string{}
	for _, s := range res.Skipped {
		skipped[s.Name] = s.Reason
	}
	for _, name := range []string{"no-shape", "http-no-url", "not-an-object", "agenthub"} {
		if _, ok := skipped[name]; !ok {
			t.Errorf("entry %q was neither imported nor reported: %+v", name, res.Skipped)
		}
	}
	if !contains(skipped["agenthub"], "gateway") {
		t.Errorf("the agenthub entry must be skipped as our own gateway, got %q", skipped["agenthub"])
	}
	if len(res.Sources) != 1 {
		t.Errorf("sources = %v", res.Sources)
	}
}

// TestImportNeverOverwrites: a name already in the registry is reported as
// a conflict and kept OUT of Entries. Import proposes, it never decides.
func TestImportNeverOverwrites(t *testing.T) {
	e := newEnv(t, "darwin")
	write(t, filepath.Join(e.project, ".mcp.json"), `{"mcpServers": {
      "fs":     {"command": "npx"},
      "github": {"command": "gh-mcp"}
  }}`)

	res, err := e.tbl.Import("claude-code", e.project, []string{"github", "unrelated"})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, ok := res.Entries["github"]; ok {
		t.Error("a conflicting name must not appear in Entries")
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].Name != "github" {
		t.Fatalf("conflicts = %+v", res.Conflicts)
	}
	if res.Conflicts[0].Path == "" {
		t.Error("a conflict must name the file it came from")
	}
	if _, ok := res.Entries["fs"]; !ok {
		t.Error("a non-conflicting entry must still be imported")
	}
}

// TestImportPlacementPrecedence: the project file wins over the user file
// for the same name, and the loser is reported rather than dropped.
func TestImportPlacementPrecedence(t *testing.T) {
	e := newEnv(t, "darwin")
	write(t, filepath.Join(e.project, ".cursor", "mcp.json"),
		`{"mcpServers": {"shared": {"command": "project-one"}}}`)
	write(t, filepath.Join(e.home, ".cursor", "mcp.json"),
		`{"mcpServers": {"shared": {"command": "user-one"}, "user-only": {"command": "u"}}}`)

	res, err := e.tbl.Import("cursor", e.project, nil)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if got := res.Entries["shared"].Command; got != "project-one" {
		t.Errorf("shared command = %q, want the project definition", got)
	}
	if _, ok := res.Entries["user-only"]; !ok {
		t.Error("user-only entry missing")
	}
	if len(res.Skipped) != 1 || !contains(res.Skipped[0].Reason, "already imported") {
		t.Errorf("skipped = %+v, want the shadowed user definition reported", res.Skipped)
	}
	if len(res.Sources) != 2 {
		t.Errorf("sources = %v, want both files", res.Sources)
	}
}

// TestImportWarnsAboutLiteralSecrets: registry documents must never hold a
// credential, so an imported env/header value that looks like one is
// flagged. A ${...} placeholder is exactly the form the registry wants and
// must NOT be flagged.
func TestImportWarnsAboutLiteralSecrets(t *testing.T) {
	e := newEnv(t, "darwin")
	write(t, filepath.Join(e.project, ".mcp.json"), `{"mcpServers": {
      "leaky":       {"command": "x", "env": {"GITHUB_TOKEN": "ghp_realvalue"}},
      "placeholder": {"command": "x", "env": {"GITHUB_TOKEN": "${SECRET_GH}"}},
      "innocent":    {"command": "x", "env": {"LOG_LEVEL": "debug"}},
      "hdr":         {"url": "https://e.example/mcp", "headers": {"Authorization": "Bearer abc"}}
  }}`)
	res, err := e.tbl.Import("claude-code", e.project, nil)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	got := map[string]bool{}
	for _, n := range res.SecretWarnings {
		got[n] = true
	}
	if !got["leaky"] || !got["hdr"] {
		t.Errorf("secret warnings = %v, want leaky and hdr", res.SecretWarnings)
	}
	if got["placeholder"] || got["innocent"] {
		t.Errorf("secret warnings = %v, must not flag placeholders or plain values", res.SecretWarnings)
	}
}

// TestImportRefusals: unknown clients, probe-only shapes and unreadable or
// unparseable files fail with typed errors rather than importing a
// partial, misleading result.
func TestImportRefusals(t *testing.T) {
	e := newEnv(t, "darwin")

	var uce *clients.UnknownClientError
	if _, err := e.tbl.Import("nope", e.project, nil); !errors.As(err, &uce) {
		t.Fatalf("unknown client: err = %v", err)
	}

	var ue *clients.UnsupportedError
	if _, err := e.tbl.Import("codex", e.project, nil); !errors.As(err, &ue) {
		t.Fatalf("codex: err = %v, want *UnsupportedError", err)
	} else if ue.Op != "import" {
		t.Errorf("op = %q", ue.Op)
	}

	// Nothing installed: an empty proposal, not an error.
	res, err := e.tbl.Import("claude-code", e.project, nil)
	if err != nil || len(res.Entries) != 0 || len(res.Sources) != 0 {
		t.Errorf("import of an absent config = %+v, %v", res, err)
	}

	write(t, filepath.Join(e.project, ".mcp.json"), `{"mcpServers": {`)
	var pe *clients.ParseError
	if _, err := e.tbl.Import("claude-code", e.project, nil); !errors.As(err, &pe) {
		t.Fatalf("broken file: err = %v, want *ParseError", err)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
