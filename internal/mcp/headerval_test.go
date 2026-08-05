package mcp

import "testing"

// TestHeaderValueRoundTrip pins both directions against the specification's
// own worked examples plus the cases Go's net/http silently changes.
func TestHeaderValueRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		encoded string
	}{
		{name: "plain ascii", value: "get_weather", encoded: "get_weather"},
		{name: "empty", value: "", encoded: ""},
		{name: "uri", value: "file:///projects/myapp/config.json",
			encoded: "file:///projects/myapp/config.json"},
		{name: "non-ascii", value: "Hello, 世界",
			encoded: "=?base64?SGVsbG8sIOS4lueVjA==?="},
		{name: "padded", value: " padded ", encoded: "=?base64?IHBhZGRlZCA=?="},
		{name: "newline", value: "line1\nline2", encoded: "=?base64?bGluZTEKbGluZTI=?="},
		{name: "sentinel shaped", value: "=?base64?literal?=",
			encoded: "=?base64?PT9iYXNlNjQ/bGl0ZXJhbD89?="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EncodeHeaderValue(tt.value); got != tt.encoded {
				t.Fatalf("EncodeHeaderValue(%q) = %q, want %q", tt.value, got, tt.encoded)
			}
			got, ok := DecodeHeaderValue(tt.encoded)
			if !ok || got != tt.value {
				t.Fatalf("DecodeHeaderValue(%q) = %q, %v; want %q, true", tt.encoded, got, ok, tt.value)
			}
		})
	}
}

// TestDecodeHeaderValueRefusesMalformedSentinel: a value that opens the
// sentinel and does not decode is malformed, never a literal. Comparing the
// raw text there is how a crafted header slips past validation.
func TestDecodeHeaderValueRefusesMalformedSentinel(t *testing.T) {
	for _, v := range []string{
		"=?base64?not base64!?=",
		"=?base64?dHJ1bmNhdGVk", // never closed
		"=?base64?",
	} {
		if got, ok := DecodeHeaderValue(v); ok {
			t.Fatalf("DecodeHeaderValue(%q) = %q, true; want refusal", v, got)
		}
	}
}

// TestEncodeHeaderValueIsHeaderSafe is the property that matters on the
// wire: whatever the body value, the encoded form is transmissible.
func TestEncodeHeaderValueIsHeaderSafe(t *testing.T) {
	for _, v := range []string{"世界", " x ", "a\r\nb", "\x00", "=?base64?x?=", "plain"} {
		enc := EncodeHeaderValue(v)
		for i := range len(enc) {
			if c := enc[i]; c < 0x20 || c > 0x7e {
				t.Fatalf("EncodeHeaderValue(%q) = %q has byte %#x at %d", v, enc, c, i)
			}
		}
		if enc != "" && (isHeaderSpace(enc[0]) || isHeaderSpace(enc[len(enc)-1])) {
			t.Fatalf("EncodeHeaderValue(%q) = %q has edge whitespace net/http would trim", v, enc)
		}
	}
}
