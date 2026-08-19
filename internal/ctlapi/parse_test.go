package ctlapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/catalog"
)

// parseBody mirrors catalog.ParseResult for decoding.
type parseBody struct {
	Shape   string   `json:"shape"`
	Section []string `json:"section"`
	Servers []struct {
		Name  string `json:"name"`
		Entry struct {
			Transport string            `json:"transport"`
			Command   string            `json:"command"`
			Args      []string          `json:"args"`
			Env       map[string]string `json:"env"`
			URL       string            `json:"url"`
			Enabled   bool              `json:"enabled"`
			Source    string            `json:"source"`
		} `json:"entry"`
		Warnings []string `json:"warnings"`
	} `json:"servers"`
	Skipped []struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	} `json:"skipped"`
}

func parseConfig(t *testing.T, sock, text string) adminResp {
	t.Helper()
	return doAdmin(t, sock, http.MethodPost, "/v1/parse/client-config", map[string]any{"text": text})
}

// The shapes docs/subsystems/controlplane.md names, end to end over the socket.
func TestParseClientConfigShapesOverTheWire(t *testing.T) {
	env, _, _ := adminServer(t, nil)

	cases := []struct {
		name    string
		text    string
		shape   string
		section []string
		server  string
		command string
		url     string
	}{
		{
			name: "mcpServers", shape: "wrapped", section: []string{"mcpServers"},
			text:   `{"mcpServers":{"fs":{"command":"npx","args":["-y","pkg"]}}}`,
			server: "fs", command: "npx",
		},
		{
			name: "vscode servers", shape: "wrapped", section: []string{"servers"},
			text:   `{"servers":{"web":{"type":"http","url":"https://mcp.example.com/mcp"}}}`,
			server: "web", url: "https://mcp.example.com/mcp",
		},
		{
			name: "vscode settings", shape: "wrapped", section: []string{"mcp", "servers"},
			text:   `{"mcp":{"servers":{"fs":{"command":"npx"}}}}`,
			server: "fs", command: "npx",
		},
		{
			name: "zed", shape: "wrapped", section: []string{"context_servers"},
			text:   `{"context_servers":{"fs":{"command":"npx"}}}`,
			server: "fs", command: "npx",
		},
		{
			name: "single entry", shape: "single-entry",
			text:   `{"command":"npx","args":["-y","pkg"]}`,
			server: "", command: "npx",
		},
		{
			name: "bare map", shape: "entry-map",
			text:   `{"fs":{"command":"npx"}}`,
			server: "fs", command: "npx",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := parseConfig(t, env.sock, tc.text)
			if res.status != http.StatusOK {
				t.Fatalf("parse: %d %s", res.status, res.raw)
			}
			var got parseBody
			res.decode(t, &got)
			if got.Shape != tc.shape {
				t.Errorf("shape = %q, want %q", got.Shape, tc.shape)
			}
			if strings.Join(got.Section, ".") != strings.Join(tc.section, ".") {
				t.Errorf("section = %v, want %v", got.Section, tc.section)
			}
			if len(got.Servers) != 1 {
				t.Fatalf("servers = %+v", got.Servers)
			}
			s := got.Servers[0]
			if s.Name != tc.server || s.Entry.Command != tc.command || s.Entry.URL != tc.url {
				t.Errorf("server = %+v", s)
			}
			if s.Entry.Source != catalog.SourcePasted {
				t.Errorf("source = %q, want %q", s.Entry.Source, catalog.SourcePasted)
			}
		})
	}
}

// Parsing writes NOTHING: no registry entry, no ledger record. A preview is a
// read, exactly like the dry-run of a client connect.
func TestParseWritesNothing(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	before := env.reg.Snapshot().Generation

	res := parseConfig(t, env.sock, `{"mcpServers":{"fs":{"command":"npx"}}}`)
	if res.status != http.StatusOK {
		t.Fatalf("parse: %d %s", res.status, res.raw)
	}
	if got := env.reg.Snapshot().Generation; got != before {
		t.Errorf("generation moved from %d to %d", before, got)
	}
	if len(env.reg.Snapshot().Servers.V.Servers) != 0 {
		t.Error("parsing added a server")
	}
}

func TestParseReportsWarningsAndSkips(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	res := parseConfig(t, env.sock, `{"mcpServers":{
		"agenthub":{"command":"agenthub","args":["connect","--client","cursor"]},
		"fs":{"command":"npx","timeout":60,"env":{"API_TOKEN":"literal-value"}}}}`)
	if res.status != http.StatusOK {
		t.Fatalf("parse: %d %s", res.status, res.raw)
	}
	var got parseBody
	res.decode(t, &got)
	if len(got.Servers) != 1 || got.Servers[0].Name != "fs" {
		t.Fatalf("servers = %+v", got.Servers)
	}
	warn := strings.Join(got.Servers[0].Warnings, " | ")
	if !strings.Contains(warn, "timeout") || !strings.Contains(warn, "API_TOKEN") {
		t.Errorf("warnings = %q", warn)
	}
	if strings.Contains(warn, "literal-value") {
		t.Error("the warning echoed the credential value")
	}
	if len(got.Skipped) != 1 || got.Skipped[0].Name != "agenthub" {
		t.Errorf("skipped = %+v", got.Skipped)
	}
}

// TOML and YAML get the distinct code plus the manual route: "we do not read
// this" has a next step, and burying it in a generic parse failure would
// hide the only useful part of the answer.
func TestParseUnsupportedFormatsOverTheWire(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	for _, text := range []string{
		"[mcp_servers.fs]\ncommand = \"npx\"\n",
		"mcpServers:\n  - name: fs\n",
	} {
		res := parseConfig(t, env.sock, text)
		res.wantErr(t, http.StatusBadRequest, CodeUnsupportedFormat)
		if !strings.Contains(res.Error.Hint, "agenthub server add") {
			t.Errorf("hint does not offer the manual route: %q", res.Error.Hint)
		}
	}
}

func TestParseRejectsUnusableInput(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	for _, tc := range []struct{ name, text string }{
		{"empty", "   "},
		{"not json", "hello"},
		{"unrelated json", `{"editor.fontSize":13}`},
		{"empty section", `{"mcpServers":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parseConfig(t, env.sock, tc.text)
			res.wantErr(t, http.StatusBadRequest, CodeBadRequest)
			if res.Error.Hint == "" {
				t.Error("a refusal must say what was expected instead")
			}
		})
	}
	// A body that is not even the request shape.
	doAdmin(t, env.sock, http.MethodPost, "/v1/parse/client-config", "not json").
		wantErr(t, http.StatusBadRequest, CodeBadRequest)
	doAdmin(t, env.sock, http.MethodPost, "/v1/parse/client-config", nil).
		wantErr(t, http.StatusBadRequest, CodeBadRequest)
	// Wrong method is the uniform 404, like every other miss.
	doAdmin(t, env.sock, http.MethodGet, "/v1/parse/client-config", nil).
		wantErr(t, http.StatusNotFound, CodeNotFound)
}
