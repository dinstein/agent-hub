package toonenc

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// Number fidelity is a constructive guarantee: no value may pass through
// float64. An int64 above 2^53 and a 30-digit integer must come back
// byte-identical to what the server sent.
func TestNumbersKeepTheirExactText(t *testing.T) {
	literals := []string{
		"9007199254740993",                 // 2^53+1: the first int float64 loses
		"123456789012345678901234567890",   // beyond int64 entirely
		"-9007199254740993",                //
		"0.1000000000000000055511151231",   // more precision than a float64 holds
		"1e400",                            // overflows float64
		"-0",                               // sign of zero is preserved
		strconv.FormatInt(1<<62, 10),       //
		strconv.FormatUint(1<<63+1, 10),    //
		"1.7976931348623157e+308",          // math.MaxFloat64, verbatim spelling
		"0.000000000000000000000000000001", //
	}
	for _, lit := range literals {
		got, err := EncodeJSON([]byte(`{"n":`+lit+`}`), Options{})
		if err != nil {
			t.Fatalf("%s: %v", lit, err)
		}
		if want := "n: " + lit; got != want {
			t.Fatalf("number %s encoded as %q, want %q — a float64 round trip crept in", lit, got, want)
		}
	}
}

// Every string that needs quoting must round-trip through strconv.Unquote,
// and every string emitted BARE must not look like a literal to a reader.
// This is the property behind the quoting rule; the golden file pins the
// exact bytes, this test pins the reason.
func TestQuotingIsSufficientAndMinimal(t *testing.T) {
	cases := []string{
		"", " ", "x ", " x", "a,b", "a:b", `a"b`, `a\b`, "a\nb", "a\tb",
		"- item", "-", "-x", "[a]", "{a}", "#c", "42", "-1.5", "1e9",
		"true", "false", "null", "plain", "with spaces", "路径", "a-b", "x#y",
		"\x00", "\x7f",
	}
	for _, s := range cases {
		out := quoteValue(s)
		if strings.HasPrefix(out, `"`) {
			back, err := strconv.Unquote(out)
			if err != nil || back != s {
				t.Fatalf("quoted %q as %q which does not unquote back (%v)", s, out, err)
			}
			continue
		}
		if out != s {
			t.Fatalf("bare rendering of %q is %q", s, out)
		}
		if looksLikeLiteral(s) {
			t.Fatalf("%q was emitted bare but reads as a non-string literal", s)
		}
		if strings.ContainsAny(s, ",:\"\\\n\r\t") {
			t.Fatalf("%q was emitted bare but carries a structural character", s)
		}
		if strings.TrimSpace(s) != s {
			t.Fatalf("%q was emitted bare but has edge whitespace that would be lost", s)
		}
	}
}

// A key with interior whitespace is quoted even though the same string is a
// legal bare VALUE: keys sit next to the "- " marker and the ":" separator.
func TestKeyQuotingIsStricterThanValueQuoting(t *testing.T) {
	if got := quoteValue("with spaces"); got != "with spaces" {
		t.Fatalf("value %q", got)
	}
	if got := quoteKey("with spaces"); got != `"with spaces"` {
		t.Fatalf("key %q", got)
	}
}

// The table predicate is strict by design. Each rejection below has a plainer
// fallback and must not produce a header.
func TestTablePredicate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"homogeneous", `[{"a":1,"b":2},{"a":3,"b":4}]`, true},
		{"single row", `[{"a":1}]`, false},
		{"key set differs", `[{"a":1},{"b":1}]`, false},
		{"extra key", `[{"a":1},{"a":1,"b":2}]`, false},
		{"non-scalar value", `[{"a":[1]},{"a":[2]}]`, false},
		{"nested object value", `[{"a":{"b":1}},{"a":{"b":2}}]`, false},
		{"not objects", `[1,2]`, false},
		{"empty objects", `[{},{}]`, false},
		{"null cell is scalar", `[{"a":null},{"a":1}]`, true},
	}
	for _, tc := range tests {
		var v []any
		if err := json.Unmarshal([]byte(tc.in), &v); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if _, ok := tableColumns(v); ok != tc.want {
			t.Fatalf("%s: tableColumns = %v, want %v", tc.name, ok, tc.want)
		}
	}
}

// Too many columns degrades to the list form rather than emitting a row no
// reader can follow.
func TestWideRowsDegradeToLists(t *testing.T) {
	var b strings.Builder
	b.WriteString(`[{`)
	for i := 0; i <= MaxTableCols; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%03d":%d`, i, i)
	}
	b.WriteString("}]")
	one := b.String()
	doc := "[" + one[1:len(one)-1] + "," + one[1:len(one)-1] + "]"

	out, err := EncodeJSON([]byte(doc), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "}:") {
		t.Fatalf("a %d-column list produced a table:\n%s", MaxTableCols+1, out)
	}
}

// Depth past MaxDepth falls back to compact JSON on one line: bounded work,
// well-formed output, no recursion a hostile server can steer.
func TestDeepNestingIsBounded(t *testing.T) {
	doc := strings.Repeat(`{"a":`, MaxDepth+8) + "1" + strings.Repeat("}", MaxDepth+8)
	out, err := EncodeJSON([]byte(doc), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "\n") + 1; n > MaxDepth+2 {
		t.Fatalf("%d lines for a depth-%d document, want at most %d", n, MaxDepth+8, MaxDepth+2)
	}
	if !strings.Contains(out, `{"a":`) {
		t.Fatalf("expected a compact-JSON tail, got:\n%s", out)
	}
}

// Budget truncation lands on a line boundary and always announces itself.
func TestBudgetTruncation(t *testing.T) {
	doc := `{"a":"` + strings.Repeat("x", 40) + `","b":"` + strings.Repeat("y", 40) + `","c":1}`
	for _, budget := range []int{1, 10, 30, 50, 60, 90, 200} {
		out, err := EncodeJSON([]byte(doc), Options{Budget: budget})
		if err != nil {
			t.Fatal(err)
		}
		full, err := EncodeJSON([]byte(doc), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(full) <= budget {
			if out != full {
				t.Fatalf("budget %d: a document that fits was altered", budget)
			}
			continue
		}
		if !strings.Contains(out, "truncated by agenthub") {
			t.Fatalf("budget %d: silent truncation:\n%s", budget, out)
		}
		// The marker itself may exceed a tiny budget; nothing smaller is
		// honest. Above the marker's own size the bound must hold.
		marker := fmt.Sprintf(TruncationMarker, 0, 3)
		if budget >= len(marker) && len(out) > budget {
			t.Fatalf("budget %d exceeded: %d bytes\n%s", budget, len(out), out)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasSuffix(line, `"xxx`) {
				t.Fatalf("budget %d cut mid-value:\n%s", budget, out)
			}
		}
	}
}

// Degenerate shapes must not produce empty or ambiguous documents.
func TestDegenerateDocuments(t *testing.T) {
	tests := map[string]string{
		`{}`:          "{}",
		`[]`:          "[]",
		`null`:        "null",
		`true`:        "true",
		`0`:           "0",
		`""`:          `""`,
		`{"a":{}}`:    "a: {}",
		`{"a":[]}`:    "a: []",
		`[[]]`:        "- []",
		`[{}]`:        "- {}",
		`{"":1}`:      `"": 1`,
		`[null,null]`: "- null\n- null",
	}
	for in, want := range tests {
		got, err := EncodeJSON([]byte(in), Options{})
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Fatalf("EncodeJSON(%s) = %q, want %q", in, got, want)
		}
	}
}

// Invalid input is an error, never a partial document.
func TestInvalidInput(t *testing.T) {
	for _, in := range []string{``, `{`, `{"a":}`, `{} {}`, `nope`} {
		if out, err := EncodeJSON([]byte(in), Options{}); err == nil {
			t.Fatalf("EncodeJSON(%q) = %q, want an error", in, out)
		}
	}
}

// Encode accepts any Go value with a JSON form and agrees with EncodeJSON.
func TestEncodeMatchesEncodeJSON(t *testing.T) {
	v := map[string]any{"b": []any{1, 2}, "a": "x"}
	viaValue, err := Encode(v, Options{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	viaJSON, err := EncodeJSON(raw, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if viaValue != viaJSON {
		t.Fatalf("Encode = %q, EncodeJSON = %q", viaValue, viaJSON)
	}
	if _, err := Encode(make(chan int), Options{}); err == nil {
		t.Fatal("Encode(chan) must fail")
	}
}

// Indent below MinIndent is raised, not honoured: "- " needs two columns.
func TestIndentFloor(t *testing.T) {
	for _, n := range []int{-4, 0, 1, 2} {
		out, err := EncodeJSON([]byte(`{"a":{"b":1}}`), Options{Indent: n})
		if err != nil {
			t.Fatal(err)
		}
		if out != "a:\n  b: 1" {
			t.Fatalf("indent %d produced %q", n, out)
		}
	}
}
