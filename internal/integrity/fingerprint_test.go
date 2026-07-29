package integrity

import (
	"encoding/json"
	"strings"
	"testing"
)

// Golden fingerprints: determinism is contract ("determinism is contract"). If any of
// these change, the fingerprint formula changed — that REQUIRES bumping
// HashSchemaVersion and wiring the content-comparison migration path, or
// every deployed pin turns into a fake rug-pull.
func TestFingerprintGolden(t *testing.T) {
	cases := []struct {
		name string
		snap ToolSnapshot
		want string
	}{
		{
			name: "full definition",
			snap: ToolSnapshot{
				Name:        "search",
				Description: "Search the corpus.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer","default":10}},"required":["query"]}`),
			},
			want: "v1:71e1fd412d6e8f1cb39ab9a25c3b0c55d75012ad26796116d4443408707346dc",
		},
		{
			name: "no schema",
			snap: ToolSnapshot{Name: "noschema", Description: "No schema at all."},
			want: "v1:665bf8d8d13631a732eb82a84f821cb21ef6cc30183d9fe8e67042850303c36b",
		},
		{
			name: "empty description",
			snap: ToolSnapshot{Name: "empty-desc", InputSchema: json.RawMessage(`{"type":"object"}`)},
			want: "v1:f386525d365eb375b653f1acb74256e80c5a1b70741beb731c4ee4d0f0021551",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Fingerprint(tc.snap)
			if err != nil {
				t.Fatalf("Fingerprint: %v", err)
			}
			if got != tc.want {
				t.Errorf("fingerprint = %s, want %s", got, tc.want)
			}
			if !strings.HasPrefix(got, HashSchemaVersion+":") {
				t.Errorf("fingerprint %q lacks version prefix %q", got, HashSchemaVersion+":")
			}
		})
	}
}

func mustFingerprint(t *testing.T, s ToolSnapshot) string {
	t.Helper()
	fp, err := Fingerprint(s)
	if err != nil {
		t.Fatalf("Fingerprint(%s): %v", s.Name, err)
	}
	return fp
}

// Key order and formatting jitter from downstream re-serialization must
// never read as drift; semantic changes always must.
func TestFingerprintCanonicalization(t *testing.T) {
	base := ToolSnapshot{
		Name:        "t",
		Description: "d",
		InputSchema: json.RawMessage(`{"b":1,"a":{"y":true,"x":null}}`),
	}
	fpBase := mustFingerprint(t, base)

	t.Run("key order and whitespace are canonicalized away", func(t *testing.T) {
		reordered := base
		reordered.InputSchema = json.RawMessage(" {\n  \"a\": {\"x\": null, \"y\": true},\t\"b\": 1 } ")
		if got := mustFingerprint(t, reordered); got != fpBase {
			t.Errorf("reordered/reformatted schema changed fingerprint: %s != %s", got, fpBase)
		}
	})

	t.Run("number source form is preserved verbatim", func(t *testing.T) {
		float := base
		float.InputSchema = json.RawMessage(`{"b":1.0,"a":{"y":true,"x":null}}`)
		if got := mustFingerprint(t, float); got == fpBase {
			t.Error("1 vs 1.0 should fingerprint differently (numbers verbatim)")
		}
	})

	t.Run("description change changes fingerprint", func(t *testing.T) {
		desc := base
		desc.Description = "d2"
		if got := mustFingerprint(t, desc); got == fpBase {
			t.Error("description change did not change fingerprint")
		}
	})

	t.Run("name change changes fingerprint", func(t *testing.T) {
		named := base
		named.Name = "t2"
		if got := mustFingerprint(t, named); got == fpBase {
			t.Error("name change did not change fingerprint")
		}
	})

	t.Run("schema semantic change changes fingerprint", func(t *testing.T) {
		schema := base
		schema.InputSchema = json.RawMessage(`{"b":2,"a":{"y":true,"x":null}}`)
		if got := mustFingerprint(t, schema); got == fpBase {
			t.Error("schema value change did not change fingerprint")
		}
	})

	t.Run("nil and empty schema are equivalent", func(t *testing.T) {
		a := ToolSnapshot{Name: "t"}
		b := ToolSnapshot{Name: "t", InputSchema: json.RawMessage("")}
		if mustFingerprint(t, a) != mustFingerprint(t, b) {
			t.Error("nil vs empty RawMessage should fingerprint identically")
		}
	})
}

// Fail direction: un-fingerprintable input errors out (callers keep the tool
// blocked) — it must never silently hash to something.
func TestFingerprintInvalidSchema(t *testing.T) {
	for name, raw := range map[string]string{
		"malformed": `{"a":`,
		"trailing":  `{"a":1} garbage`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Fingerprint(ToolSnapshot{Name: "t", InputSchema: json.RawMessage(raw)}); err == nil {
				t.Error("want error for invalid inputSchema, got nil (fail-closed violated)")
			}
		})
	}
}
