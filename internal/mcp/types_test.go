package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// TestRequestStateIsThreeState pins the shape the MRTR client rules need:
// present-and-echoed, present-but-empty, and absent are three different
// answers, and a plain string collapses the last two. The spec makes the
// distinction load-bearing — "echo back the exact value" when there is one,
// "MUST NOT include one" when there is not — so absent has to survive a
// round trip as absent.
func TestRequestStateIsThreeState(t *testing.T) {
	empty := ""
	blob := "AEAD-protected"
	tests := []struct {
		name  string
		state *RequestState
		wire  string
	}{
		{name: "absent", state: nil, wire: `{"name":"t"}`},
		{name: "present but empty", state: &empty, wire: `{"name":"t","requestState":""}`},
		{name: "present", state: &blob, wire: `{"name":"t","requestState":"AEAD-protected"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(CallToolParams{Name: "t", RequestState: tt.state})
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tt.wire {
				t.Fatalf("encoded %s, want %s", raw, tt.wire)
			}
			var back CallToolParams
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			switch {
			case tt.state == nil && back.RequestState != nil:
				t.Fatalf("absent decoded as %q", *back.RequestState)
			case tt.state != nil && back.RequestState == nil:
				t.Fatalf("%q decoded as absent", *tt.state)
			case tt.state != nil && *back.RequestState != *tt.state:
				t.Fatalf("round trip changed %q to %q", *tt.state, *back.RequestState)
			}
		})
	}
}

// StripReservedMetaKeys removes the specification's namespace and nothing
// else. The rule is that a hop owns its own protocol namespace: a reserved
// key describes the exchange it travels on, so relaying one across a hop
// restates it about a different exchange.
func TestStripReservedMetaKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"nothing to strip", `{"com.example/x":1}`, `{"com.example/x":1}`},
		{
			// The one reserved key that can legitimately reach a
			// tools/call result, and the reason this function exists.
			"serverInfo alone leaves nothing",
			`{"io.modelcontextprotocol/serverInfo":{"name":"down","version":"9"}}`, "",
		},
		{
			"reserved removed, the rest kept",
			`{"io.modelcontextprotocol/serverInfo":{"name":"d"},"com.example/x":1}`,
			`{"com.example/x":1}`,
		},
		{
			// The namespace is stripped, not a list of known keys: a
			// reserved key allocated later is wrong in the same way.
			"a reserved key this tree has never heard of",
			`{"io.modelcontextprotocol/somethingNew":true,"a":1}`, `{"a":1}`,
		},
		{
			// W3C trace keys are reserved by the specification but carry
			// no io.modelcontextprotocol prefix, and are not this hop's to
			// claim. See docs/mcp-2026-07-28.md §7.13.
			"trace keys are not in this namespace", `{"traceparent":"00-x-y-01"}`, `{"traceparent":"00-x-y-01"}`,
		},
		{"absent", "", ""},
		{"empty object", `{}`, ""},
		// Fail closed: the schema requires an object, so anything else is
		// not something this hub can vouch for.
		{"not an object", `[1,2]`, ""},
		{"malformed", `{`, ""},
		{"json null", `null`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := StripReservedMetaKeys(json.RawMessage(tc.in))
			if tc.want == "" {
				if got != nil {
					t.Fatalf("got %s, want nil so the member is omitted entirely", got)
				}
				return
			}
			var gotObj, wantObj map[string]any
			if err := json.Unmarshal(got, &gotObj); err != nil {
				t.Fatalf("result %s is not an object: %v", got, err)
			}
			if err := json.Unmarshal([]byte(tc.want), &wantObj); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(gotObj) != fmt.Sprint(wantObj) {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
