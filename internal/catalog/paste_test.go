package catalog

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/registry"
)

// The five shapes docs/subsystems/controlplane.md names, plus the naked forms people actually
// paste. Each case asserts the whole preview, because "it parsed" is not the
// contract — "it parsed into exactly this definition" is.
func TestParseClientConfigShapes(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		shape   Shape
		section []string
		want    map[string]registry.ServerEntry
	}{
		{
			name:  "claude / cursor mcpServers",
			shape: ShapeWrapped, section: []string{"mcpServers"},
			text: `{"mcpServers":{"fs":{"command":"npx","args":["-y","pkg"],"env":{"A":"1"}}}}`,
			want: map[string]registry.ServerEntry{
				"fs": {
					Transport: "stdio", Command: "npx", Args: []string{"-y", "pkg"},
					Env: map[string]string{"A": "1"}, Enabled: true, Source: SourcePasted,
				},
			},
		},
		{
			name:  "vscode workspace servers",
			shape: ShapeWrapped, section: []string{"servers"},
			text: `{"servers":{"remote":{"type":"http","url":"https://mcp.example.com/mcp"}}}`,
			want: map[string]registry.ServerEntry{
				"remote": {
					Transport: "http", URL: "https://mcp.example.com/mcp",
					Provenance: registry.ProvenanceRemote, Enabled: true, Source: SourcePasted,
				},
			},
		},
		{
			name:  "vscode user settings mcp.servers",
			shape: ShapeWrapped, section: []string{"mcp", "servers"},
			text: `{"editor.fontSize":13,"mcp":{"servers":{"fs":{"command":"npx"}}}}`,
			want: map[string]registry.ServerEntry{
				"fs": {Transport: "stdio", Command: "npx", Enabled: true, Source: SourcePasted},
			},
		},
		{
			name:  "zed context_servers",
			shape: ShapeWrapped, section: []string{"context_servers"},
			text: `{"context_servers":{"fs":{"command":"npx","args":["-y","pkg"]}}}`,
			want: map[string]registry.ServerEntry{
				"fs": {
					Transport: "stdio", Command: "npx", Args: []string{"-y", "pkg"},
					Enabled: true, Source: SourcePasted,
				},
			},
		},
		{
			name:  "bare name to entry map",
			shape: ShapeEntryMap,
			text:  `{"fs":{"command":"npx"},"web":{"url":"https://x.example/mcp"}}`,
			want: map[string]registry.ServerEntry{
				"fs": {Transport: "stdio", Command: "npx", Enabled: true, Source: SourcePasted},
				"web": {
					Transport: "http", URL: "https://x.example/mcp",
					Provenance: registry.ProvenanceRemote, Enabled: true, Source: SourcePasted,
				},
			},
		},
		{
			name:  "single unnamed entry (claude mcp add-json)",
			shape: ShapeSingleEntry,
			text:  `{"command":"npx","args":["-y","pkg"],"cwd":"/tmp"}`,
			want: map[string]registry.ServerEntry{
				"": {
					Transport: "stdio", Command: "npx", Args: []string{"-y", "pkg"},
					Cwd: "/tmp", Enabled: true, Source: SourcePasted,
				},
			},
		},
		{
			name:  "sse alias and headers",
			shape: ShapeWrapped, section: []string{"mcpServers"},
			text: `{"mcpServers":{"s":{"type":"sse","serverUrl":"https://x/sse","headers":{"X-A":"1"}}}}`,
			want: map[string]registry.ServerEntry{
				"s": {
					Transport: "sse", URL: "https://x/sse", Headers: map[string]string{"X-A": "1"},
					Provenance: registry.ProvenanceRemote, Enabled: true, Source: SourcePasted,
				},
			},
		},
		{
			name:  "streamable-http alias",
			shape: ShapeWrapped, section: []string{"mcpServers"},
			text: `{"mcpServers":{"s":{"type":"streamable-http","url":"https://x/mcp"}}}`,
			want: map[string]registry.ServerEntry{
				"s": {
					Transport: "http", URL: "https://x/mcp",
					Provenance: registry.ProvenanceRemote, Enabled: true, Source: SourcePasted,
				},
			},
		},
		{
			name:  "disabled entry stays disabled",
			shape: ShapeWrapped, section: []string{"mcpServers"},
			text: `{"mcpServers":{"s":{"command":"npx","disabled":true}}}`,
			want: map[string]registry.ServerEntry{
				"s": {Transport: "stdio", Command: "npx", Enabled: false, Source: SourcePasted},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ParseClientConfig(tc.text)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if res.Shape != tc.shape {
				t.Errorf("shape = %q, want %q", res.Shape, tc.shape)
			}
			if !slices.Equal(res.Section, tc.section) {
				t.Errorf("section = %v, want %v", res.Section, tc.section)
			}
			if len(res.Servers) != len(tc.want) {
				t.Fatalf("got %d servers, want %d: %+v", len(res.Servers), len(tc.want), res.Servers)
			}
			for _, p := range res.Servers {
				want, ok := tc.want[p.Name]
				if !ok {
					t.Fatalf("unexpected server %q", p.Name)
				}
				if !entryEqual(p.Entry, want) {
					t.Errorf("%q entry =\n %+v\nwant\n %+v", p.Name, p.Entry, want)
				}
			}
		})
	}
}

func entryEqual(a, b registry.ServerEntry) bool {
	return a.Transport == b.Transport && a.Command == b.Command &&
		slices.Equal(a.Args, b.Args) && mapEqual(a.Env, b.Env) && a.Cwd == b.Cwd &&
		a.URL == b.URL && mapEqual(a.Headers, b.Headers) && a.Provenance == b.Provenance &&
		a.Enabled == b.Enabled && a.Source == b.Source
}

func mapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Sorted output: a preview list that reorders itself between two identical
// pastes is a list a user cannot check.
func TestParseOrdersServersByName(t *testing.T) {
	text := `{"mcpServers":{"zulu":{"command":"a"},"alpha":{"command":"b"},"mike":{"command":"c"}}}`
	for range 5 {
		res, err := ParseClientConfig(text)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, p := range res.Servers {
			names = append(names, p.Name)
		}
		if !slices.Equal(names, []string{"alpha", "mike", "zulu"}) {
			t.Fatalf("names = %v", names)
		}
	}
}

// A preview must SAY what it dropped. Silently discarding a key is the worst
// failure shape available here: the user sees "added" and the setting they
// pasted is simply absent.
func TestParseReportsIgnoredFields(t *testing.T) {
	res, err := ParseClientConfig(`{"mcpServers":{"s":{"command":"npx","alwaysAllow":["x"],"timeout":60}}}`)
	if err != nil {
		t.Fatal(err)
	}
	warn := strings.Join(res.Servers[0].Warnings, " | ")
	if !strings.Contains(warn, "alwaysAllow") || !strings.Contains(warn, "timeout") {
		t.Errorf("warnings = %q, want both ignored keys named", warn)
	}
}

func TestParseWarnsAboutLiteralCredentials(t *testing.T) {
	res, err := ParseClientConfig(
		`{"mcpServers":{"s":{"command":"npx","env":{"GITHUB_TOKEN":"ghp_literal","SAFE":"1"}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	warn := strings.Join(res.Servers[0].Warnings, " | ")
	if !strings.Contains(warn, "GITHUB_TOKEN") || !strings.Contains(warn, "secret set") {
		t.Errorf("warnings = %q, want the credential named and a remedy", warn)
	}
	if strings.Contains(warn, "SAFE") {
		t.Errorf("warnings = %q, must not flag a non-credential key", warn)
	}
	// The VALUE must never travel in the warning.
	if strings.Contains(warn, "ghp_literal") {
		t.Error("the warning echoed the credential value")
	}
}

// A placeholder value is exactly the shape the registry is designed to hold,
// so it must not be flagged.
func TestParsePlaceholderCredentialIsNotFlagged(t *testing.T) {
	res, err := ParseClientConfig(
		`{"mcpServers":{"s":{"command":"npx","env":{"GITHUB_TOKEN":"${SECRET_GITHUB_TOKEN}"}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Servers[0].Warnings) != 0 {
		t.Errorf("warnings = %v, want none", res.Servers[0].Warnings)
	}
}

func TestParseWarnsThatASingleEntryHasNoName(t *testing.T) {
	res, err := ParseClientConfig(`{"command":"npx"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Servers[0].Name != "" {
		t.Errorf("name = %q, want empty", res.Servers[0].Name)
	}
	if !strings.Contains(strings.Join(res.Servers[0].Warnings, " "), "name") {
		t.Errorf("warnings = %v, want a note that a name is required", res.Servers[0].Warnings)
	}
}

// agenthub's own gateway entry is in every adapted client config. Importing
// it would point agenthub at itself.
func TestParseSkipsTheGatewayEntry(t *testing.T) {
	res, err := ParseClientConfig(`{"mcpServers":{
		"agenthub":{"command":"/usr/local/bin/agenthub","args":["connect","--client","cursor"]},
		"fs":{"command":"npx"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Servers) != 1 || res.Servers[0].Name != "fs" {
		t.Fatalf("servers = %+v", res.Servers)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Name != "agenthub" {
		t.Fatalf("skipped = %+v", res.Skipped)
	}
	if !strings.Contains(res.Skipped[0].Reason, "itself") {
		t.Errorf("reason = %q", res.Skipped[0].Reason)
	}
}

// One unusable entry is reported; the rest of the paste stays usable.
func TestParseSkipsUnusableEntriesWithoutSinkingTheBatch(t *testing.T) {
	res, err := ParseClientConfig(`{"mcpServers":{"good":{"command":"npx"},"bad":{"note":"nothing here"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Servers) != 1 || res.Servers[0].Name != "good" {
		t.Fatalf("servers = %+v", res.Servers)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Name != "bad" {
		t.Fatalf("skipped = %+v", res.Skipped)
	}
}

func TestParseFailsWhenNothingIsUsable(t *testing.T) {
	_, err := ParseClientConfig(`{"mcpServers":{"bad":{"type":"http"}}}`)
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %T %v, want *ParseError", err, err)
	}
	if !strings.Contains(pe.Reason, "url") {
		t.Errorf("reason = %q, want it to name the missing url", pe.Reason)
	}
}

// TOML and YAML are RECOGNIZED and refused with instructions — not folded
// into "this is not a configuration", which has no next step.
func TestParseUnsupportedFormats(t *testing.T) {
	cases := []struct {
		name, text, format string
	}{
		{"codex toml", "[mcp_servers.fs]\ncommand = \"npx\"\nargs = [\"-y\", \"pkg\"]\n", "TOML"},
		{"toml with a leading key", "model = \"gpt\"\n\n[mcp_servers.fs]\ncommand = \"npx\"\n", "TOML"},
		{"toml table only", "[tool.agenthub]\nx = 1\n", "TOML"},
		{"continue yaml", "name: my-config\nmcpServers:\n  - name: fs\n", "YAML"},
		{"yaml list", "- name: fs\n  command: npx\n", "YAML"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseClientConfig(tc.text)
			var ue *UnsupportedError
			if !errors.As(err, &ue) {
				t.Fatalf("error = %T %v, want *UnsupportedError", err, err)
			}
			if ue.Format != tc.format {
				t.Errorf("format = %q, want %q", ue.Format, tc.format)
			}
			if !strings.Contains(ue.Hint, "agenthub server add") {
				t.Errorf("hint does not offer the manual route: %q", ue.Hint)
			}
		})
	}
}

// Everything the parser cannot place must fail LOUDLY, never with a
// plausible-looking empty result.
func TestParseRejectsUnrecognizedInput(t *testing.T) {
	cases := []struct {
		name, text, wantIn string
	}{
		{"empty", "   ", "nothing to parse"},
		{"not json", "hello world", "not a JSON object"},
		{"json array", `[{"command":"npx"}]`, "not a JSON object"},
		{"unrelated object", `{"editor.fontSize":13,"theme":"dark"}`, "does not look like"},
		{"empty section", `{"mcpServers":{}}`, "no servers"},
		{"oversized", "{\"mcpServers\":{}}" + strings.Repeat(" ", maxPasteBytes), "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseClientConfig(tc.text)
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("error = %T %v, want *ParseError", err, err)
			}
			if !strings.Contains(pe.Error(), tc.wantIn) {
				t.Errorf("error = %q, want it to mention %q", pe.Error(), tc.wantIn)
			}
			if pe.Hint == "" {
				t.Error("a refusal must say what was expected instead")
			}
		})
	}
}

// The wrapper keys come from the internal/clients table, so the four shapes
// the design names must all be present without this package listing them.
func TestSectionCandidatesCoverTheClientTable(t *testing.T) {
	got := SectionNames()
	for _, want := range []string{"mcpServers", "servers", "mcp.servers", "context_servers"} {
		if !slices.Contains(got, want) {
			t.Errorf("section candidates %v missing %q", got, want)
		}
	}
	// Longest first: "mcp.servers" must be tried before "servers", or a
	// VS Code settings document would never reach its nested section.
	nested := slices.Index(got, "mcp.servers")
	shallow := slices.Index(got, "servers")
	if nested > shallow {
		t.Errorf("candidate order %v tries the shallow key first", got)
	}
}

// An explicit but unknown transport marker must not fall back to inference:
// a typo must not silently become stdio.
func TestParseRefusesUnknownTransportMarker(t *testing.T) {
	res, err := ParseClientConfig(`{"mcpServers":{"s":{"type":"htpp","command":"npx"}}}`)
	if err == nil && len(res.Servers) > 0 {
		t.Fatalf("accepted an unknown transport: %+v", res.Servers)
	}
}
