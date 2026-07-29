package mcp

import (
	"bytes"
	"encoding/json"
	"testing"
)

// InputSchema and call results must round-trip byte-for-byte (modulo
// compaction): the facade passes downstream JSON through verbatim.
func TestToolDefSchemaPassthrough(t *testing.T) {
	in := []byte(`{
		"name": "read_file",
		"description": "Read a file",
		"inputSchema": {
			"type": "object",
			"properties": {"path": {"type": "string", "x-vendor": [1, 2.5, null]}},
			"required": ["path"],
			"unknownKeyword": {"nested": true}
		}
	}`)
	var def ToolDef
	if err := json.Unmarshal(in, &def); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	var again ToolDef
	if err := json.Unmarshal(out, &again); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, def.InputSchema, again.InputSchema) {
		t.Fatalf("inputSchema mutated:\n%s\n%s", def.InputSchema, again.InputSchema)
	}
	var schema map[string]any
	if err := json.Unmarshal(again.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema["unknownKeyword"]; !ok {
		t.Fatal("unknown schema keyword dropped")
	}
}

func TestCallResultPassthrough(t *testing.T) {
	in := []byte(`{"content":[{"type":"text","text":"hi"}],"structuredContent":{"k":1},"isError":true}`)
	var res CallResult
	if err := json.Unmarshal(in, &res); err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("isError lost")
	}
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, in, out) {
		t.Fatalf("call result mutated:\n%s\n%s", in, out)
	}
}

func TestInitializeResultCapabilitiesRaw(t *testing.T) {
	in := []byte(`{"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":true},"experimental":{"x":1}},"serverInfo":{"name":"s","version":"1"},"instructions":"be nice"}`)
	var res InitializeResult
	if err := json.Unmarshal(in, &res); err != nil {
		t.Fatal(err)
	}
	if res.ProtocolVersion != "2025-06-18" || res.ServerInfo.Name != "s" || res.Instructions != "be nice" {
		t.Fatalf("decoded %+v", res)
	}
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, in, out) {
		t.Fatalf("initialize result mutated:\n%s\n%s", in, out)
	}
}

// jsonEqual compares two JSON documents structurally (compaction-safe).
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var ca, cb bytes.Buffer
	if err := json.Compact(&ca, a); err != nil {
		t.Fatal(err)
	}
	if err := json.Compact(&cb, b); err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}
