package mcp

import "testing"

// FuzzDecodeHeaderValue drives the Mcp-Name / Mcp-Param-* sentinel decoder
// with hostile header text. It is the one hand-written parser on the
// exposure side's header-validation path: whatever a caller puts in the
// header reaches it before any comparison happens, so it must never panic,
// and a value it accepts must be one that re-encodes to something a header
// can carry — otherwise validation and transmission disagree about what the
// value was.
func FuzzDecodeHeaderValue(f *testing.F) {
	for _, seed := range []string{
		"", "get_weather", "file:///a/b.json",
		"=?base64?SGVsbG8sIOS4lueVjA==?=",
		"=?base64?IHBhZGRlZCA=?=",
		"=?base64?not base64!?=",
		"=?base64?", "?=", "=?base64??=",
		"=?BASE64?aGk=?=", // wrong case: a literal, not a sentinel
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		decoded, ok := DecodeHeaderValue(header)
		if !ok {
			if decoded != "" {
				t.Fatalf("refused %q but returned %q; a refusal must carry no value", header, decoded)
			}
			return
		}
		// An accepted value must survive the round trip a client performs
		// when it mirrors the same body value into a header.
		again, ok2 := DecodeHeaderValue(EncodeHeaderValue(decoded))
		if !ok2 || again != decoded {
			t.Fatalf("header %q decoded to %q, which does not round-trip (%q, %v)",
				header, decoded, again, ok2)
		}
	})
}
