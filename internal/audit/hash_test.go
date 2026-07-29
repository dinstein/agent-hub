package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestCanonicalJSONGolden(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"sorted keys", `{"b":1,"a":"x"}`, `{"a":"x","b":1}`},
		{"whitespace collapsed", " { \"a\" : [ 1 , 2 ] } ", `{"a":[1,2]}`},
		{"nested objects sorted", `{"z":{"b":1,"a":2},"a":0}`, `{"a":0,"z":{"a":2,"b":1}}`},
		{"numbers preserved lexically", `{"a":1.0,"b":1,"c":1e0}`, `{"a":1.0,"b":1,"c":1e0}`},
		{"array order kept", `[3,1,2]`, `[3,1,2]`},
		{"scalars", `true`, `true`},
		{"null", `null`, `null`},
		{"empty input is null", ``, `null`},
		{"whitespace-only input is null", "  \n\t ", `null`},
		{"duplicate keys last wins", `{"a":1,"a":2}`, `{"a":2}`},
		// The input spells é as a JSON \\u escape; the canonical form
		// re-encodes it as the literal rune, so both spellings converge.
		{"escape spellings converge", "{\"k\":\"\\u00e9\"}", `{"k":"é"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CanonicalJSON([]byte(tt.in))
			if err != nil {
				t.Fatalf("CanonicalJSON(%q): %v", tt.in, err)
			}
			if string(got) != tt.want {
				t.Errorf("CanonicalJSON(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCanonicalJSONErrors(t *testing.T) {
	for _, in := range []string{`{`, `{"a":}`, `null garbage`, `{} {}`} {
		if _, err := CanonicalJSON([]byte(in)); err == nil {
			t.Errorf("CanonicalJSON(%q): expected error", in)
		}
	}
}

func TestArgsHashStable(t *testing.T) {
	// Layout variants of the same document must collide; the digest is
	// the SHA-256 of the canonical form.
	a, err := ArgsHash([]byte(`{"b":1,"a":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ArgsHash([]byte("{ \"a\" : \"x\",\n\t\"b\" : 1 }"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("layout variants hash differently: %s vs %s", a, b)
	}
	sum := sha256.Sum256([]byte(`{"a":"x","b":1}`))
	if want := hex.EncodeToString(sum[:]); a != want {
		t.Errorf("ArgsHash = %s, want sha256(canonical) = %s", a, want)
	}
	// No-args calls hash to the digest of "null".
	empty, err := ArgsHash(nil)
	if err != nil {
		t.Fatal(err)
	}
	nullSum := sha256.Sum256([]byte("null"))
	if want := hex.EncodeToString(nullSum[:]); empty != want {
		t.Errorf("ArgsHash(nil) = %s, want %s", empty, want)
	}
	if len(a) != 64 || strings.ToLower(a) != a {
		t.Errorf("hash %q is not lowercase 64-char hex", a)
	}
}
