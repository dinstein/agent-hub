package clients_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// decodeObj parses a JSON object into raw members, so a comparison can look at
// one key without re-encoding (and therefore without hiding a re-encode bug).
func decodeObj(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not a JSON object: %v\n%s", err, s)
	}
	return m
}

// TestConnectPreservesEverythingItDoesNotOwn is the property with the worst
// failure in this package: agenthub edits a file the USER owns, and for
// several clients that file is their entire editor configuration.
//
// The existing round-trip test starts from an empty file, and the ownership
// test covers sibling entries INSIDE the servers section. Neither covers the
// keys beside it. For VS Code the servers live at mcp.servers inside
// settings.json, so "beside it" means every setting the person has.
//
// Both levels are checked for the nested client: a sibling of the section
// (editor.fontSize) and a sibling INSIDE the parent of the section
// (mcp.autostart). A writer that rebuilt either object from a typed struct
// would pass a top-level-only test and still erase the other.
func TestConnectPreservesEverythingItDoesNotOwn(t *testing.T) {
	cases := []struct {
		client string
		file   string
		body   string
		// siblings that must survive verbatim, as JSON pointers of depth 1 or 2
		top    map[string]string
		nested map[string]map[string]string
	}{
		{
			client: "claude-code",
			file:   ".mcp.json",
			body: `{
  "$schema": "https://example.com/s.json",
  "mcpServers": {"other": {"command": "npx", "args": ["-y", "pkg"]}},
  "telemetry": false,
  "extensions": ["a", "b"],
  "editor": {"fontSize": 14, "nested": {"deep": [1, 2, 3]}}
}`,
			top: map[string]string{
				"$schema":    `"https://example.com/s.json"`,
				"telemetry":  `false`,
				"extensions": `["a","b"]`,
				"editor":     `{"fontSize":14,"nested":{"deep":[1,2,3]}}`,
			},
		},
		{
			client: "vscode",
			file:   "settings.json",
			body: `{
  "editor.fontSize": 14,
  "workbench.colorTheme": "Dark+",
  "mcp": {"autostart": true, "servers": {"other": {"command": "npx"}}},
  "files.exclude": {"**/.git": true}
}`,
			top: map[string]string{
				"editor.fontSize":      `14`,
				"workbench.colorTheme": `"Dark+"`,
				"files.exclude":        `{"**/.git":true}`,
			},
			nested: map[string]map[string]string{
				"mcp": {"autostart": `true`},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.client, func(t *testing.T) {
			e := newEnv(t, "darwin")
			path := filepath.Join(e.project, tc.file)
			write(t, path, tc.body)
			f := e.format(t, tc.client)

			check := func(stage, content string) {
				t.Helper()
				got := decodeObj(t, content)
				for k, want := range tc.top {
					raw, ok := got[k]
					if !ok {
						t.Errorf("%s: top-level key %q was dropped", stage, k)
						continue
					}
					if !sameJSON(t, string(raw), want) {
						t.Errorf("%s: key %q = %s, want %s", stage, k, raw, want)
					}
				}
				for parent, kids := range tc.nested {
					raw, ok := got[parent]
					if !ok {
						t.Errorf("%s: parent object %q was dropped", stage, parent)
						continue
					}
					obj := decodeObj(t, string(raw))
					for k, want := range kids {
						sub, ok := obj[k]
						if !ok {
							t.Errorf("%s: %s.%s was dropped — the section's parent was rebuilt", stage, parent, k)
							continue
						}
						if !sameJSON(t, string(sub), want) {
							t.Errorf("%s: %s.%s = %s, want %s", stage, parent, k, sub, want)
						}
					}
				}
			}

			if _, err := f.Connect(path, entry(tc.client)); err != nil {
				t.Fatalf("connect: %v", err)
			}
			check("after connect", read(t, path))

			if _, err := f.Disconnect(path); err != nil {
				t.Fatalf("disconnect: %v", err)
			}
			// Disconnect is the more dangerous direction: it DELETES, so a
			// bug there removes more than it should.
			check("after disconnect", read(t, path))
		})
	}
}

// sameJSON compares two JSON fragments by value, so a difference in
// whitespace or key order is not reported as data loss.
func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		t.Fatalf("test expectation is not valid JSON: %s", b)
	}
	ja, err1 := json.Marshal(x)
	jb, err2 := json.Marshal(y)
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}
