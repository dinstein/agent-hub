package toonenc

import "testing"

// FuzzEncodeJSON drives the TOON encoder with arbitrary bytes.
//
// The input is a downstream tool's RESULT, which agenthub re-encodes before
// handing it to the agent. A tool chooses those bytes, so a panic here is a
// gateway crash on the response path — after the call has already run, which
// is the worst moment to lose the connection.
//
// Consider is fuzzed alongside Encode because it is the gate that decides
// whether encoding happens at all, and a disagreement between "this is
// encodable" and "this encodes" is a crash rather than a fallback.
func FuzzEncodeJSON(f *testing.F) {
	for _, s := range []string{
		`{"a":1}`, `[{"a":1},{"a":2}]`, `[]`, `{}`, `null`, `[[[[1]]]]`,
		`{"a":{"b":{"c":[1,2,3]}}}`, `[{"a":null},{"b":""}]`,
		`{"":""}`, `[1,"a",null,true]`, `{"a":" "}`,
		`[{"a":1},{"b":2}]`, // ragged rows: not a uniform table
		"{\"a\":\"\\n\\t\"}", "{\"a\":\"\\u0000\"}",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = EncodeJSON(raw, Options{})
		_, _ = Consider(string(raw), Options{})
	})
}
