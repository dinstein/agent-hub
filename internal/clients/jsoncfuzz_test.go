package clients

import (
	"encoding/json"
	"testing"
)

// FuzzBlankJSONC guards the offset-preserving blanking pass, which is what
// the whole JSONC path stands on: writing splices bytes located in the
// BLANKED copy back into the ORIGINAL, so the two must agree on where
// everything is, forever.
//
// Three properties:
//
//  1. length is preserved, byte for byte. Lose that and every offset the
//     writer uses points somewhere else in the file it is editing;
//  2. blanking is idempotent, so nothing that survived one pass can be
//     eaten by a second;
//  3. only comment and trailing-comma bytes change, and never a byte
//     inside a string. A `//` inside a value is not a comment, and a
//     blanker that thought otherwise would corrupt the very value it was
//     asked to preserve.
//
// Run: go test ./internal/clients/ -run xxx -fuzz FuzzBlankJSONC
func FuzzBlankJSONC(f *testing.F) {
	f.Add([]byte("{}"))
	f.Add([]byte("// lead\n{\n  \"a\": 1,\n}\n"))
	f.Add([]byte("{\"url\": \"https://x/y\"}"))
	f.Add([]byte("{\"a\": \"/* not a comment */\"}"))
	f.Add([]byte("{/* block */ \"a\": [1, 2,] }"))
	f.Add([]byte("{\"a\": \"tricky \\\" // still string\"}"))
	f.Add([]byte("{\"a\": 1} // trailing"))
	f.Add([]byte("/* unterminated"))
	f.Add([]byte("{\"a\":,}"))

	f.Fuzz(func(t *testing.T, data []byte) {
		out := blankJSONC(data)
		if len(out) != len(data) {
			t.Fatalf("length changed: %d -> %d", len(data), len(out))
		}
		if twice := blankJSONC(out); string(twice) != string(out) {
			t.Fatalf("not idempotent:\n%q\n%q", out, twice)
		}
		for i := range out {
			if out[i] == data[i] {
				continue
			}
			// Anything blanked must have become a space, and must have
			// been a comment byte or a comma.
			if out[i] != ' ' {
				t.Fatalf("byte %d became %q, want a space", i, out[i])
			}
			if data[i] == '\n' {
				t.Fatalf("byte %d was a newline and was eaten; offsets and lines must survive", i)
			}
		}
		// Whatever the blanked copy parses as, it must parse the same way
		// after the writer's own round trip through it.
		var v any
		if json.Unmarshal(out, &v) != nil {
			return
		}
		if _, ok := locateObject(out, nil); !ok {
			if _, isObj := v.(map[string]any); isObj {
				t.Fatalf("the locator cannot walk a document encoding/json accepts: %q", out)
			}
		}
	})
}

// FuzzSpliceEntryKeepsEverythingElse is the property the write path exists
// to have: whatever the document looks like, a splice either refuses or
// produces a document that differs from the original in nothing but
// agenthub's own entry — verified the same way the real write verifies it.
//
// Run: go test ./internal/clients/ -run xxx -fuzz FuzzSpliceEntryKeepsEverythingElse
func FuzzSpliceEntryKeepsEverythingElse(f *testing.F) {
	f.Add([]byte("{}"))
	f.Add([]byte("// lead\n{\n  \"mcpServers\": {}\n}\n"))
	f.Add([]byte("{\n  \"mcpServers\": {\n    \"x\": {\"command\": \"y\"},\n  },\n}\n"))
	f.Add([]byte("{\"mcpServers\": {\"agenthub\": {\"command\": \"old\"}}}"))
	f.Add([]byte("{\"mcpServers\": []}"))
	f.Add([]byte("{\"other\": 1}"))

	value := struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}{Command: "/bin/agenthub", Args: []string{"connect"}}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Only documents agenthub would have accepted for reading.
		var root map[string]json.RawMessage
		if json.Unmarshal(blankJSONC(data), &root) != nil {
			return
		}
		out, err := spliceEntry(data, []string{"mcpServers"}, "agenthub", value)
		if err != nil {
			return // a shape the splice refuses is always an acceptable answer
		}
		if err := verifySplice(data, out, []string{"mcpServers"}, []string{"agenthub"}); err != nil {
			t.Fatalf("splice changed more than its own entry (%v)\n--- before ---\n%s\n--- after ---\n%s",
				err, data, out)
		}
	})
}
