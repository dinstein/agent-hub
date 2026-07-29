package discovery

import (
	"encoding/json"
	"strings"
	"testing"
)

// The validation matrix. Codes AND messages are frozen: an agent keys its
// recovery on the code, a human reads the message, and both appear in
// golden-tested audit records.
func TestQueryValidationMatrix(t *testing.T) {
	long := strings.Repeat("a", MaxQueryBytes+1)
	exact := strings.Repeat("a", MaxQueryBytes)
	manyWords := strings.TrimSpace(strings.Repeat("w ", MaxQueryWords+1))
	exactWords := strings.TrimSpace(strings.Repeat("w ", MaxQueryWords))

	cases := []struct {
		name    string
		in      string
		code    string
		message string
		tokens  int
	}{
		{name: "plain", in: "read file", tokens: 2},
		{name: "empty", in: "", code: CodeQueryEmpty, message: "query must not be empty"},
		{name: "blank", in: "   \t\n ", code: CodeQueryEmpty, message: "query must not be empty"},
		{name: "punctuation only", in: "!!! ??? ---", code: CodeQueryEmpty, message: "query must not be empty"},
		{name: "at the byte limit", in: exact, tokens: 1},
		{name: "over the byte limit", in: long, code: CodeQueryTooLong,
			message: "query exceeds 512 bytes (got 513)"},
		{name: "at the word limit", in: exactWords, tokens: 1}, // deduplicated
		{name: "over the word limit", in: manyWords, code: CodeQueryTooManyWords,
			message: "query exceeds 64 words"},
		// Byte length is checked BEFORE the word count, so a query that
		// violates both always reports the same code.
		{name: "over both limits", in: strings.Repeat("word ", 200), code: CodeQueryTooLong,
			message: "query exceeds 512 bytes (got 1000)"},
		{name: "unicode is counted in bytes", in: strings.Repeat("字", 200), code: CodeQueryTooLong,
			message: "query exceeds 512 bytes (got 600)"},
		{name: "mixed case folds", in: "READ File", tokens: 2},
		{name: "punctuation splits", in: "read-file/now", tokens: 3},
	}

	for _, tc := range cases {
		q, err := Validate(tc.in)
		if tc.code == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
				continue
			}
			if len(q.Tokens) != tc.tokens {
				t.Errorf("%s: %d tokens (%v), want %d", tc.name, len(q.Tokens), q.Tokens, tc.tokens)
			}
			if q.Bytes != len(tc.in) {
				t.Errorf("%s: Bytes = %d, want %d", tc.name, q.Bytes, len(tc.in))
			}
			continue
		}
		var e *Error
		if !asError(err, &e) {
			t.Errorf("%s: got %v, want a typed *Error", tc.name, err)
			continue
		}
		if e.Code != tc.code || e.Message != tc.message {
			t.Errorf("%s: got %q/%q, want %q/%q", tc.name, e.Code, e.Message, tc.code, tc.message)
		}
		if e.Error() != tc.code+": "+tc.message {
			t.Errorf("%s: Error() = %q", tc.name, e.Error())
		}
		// A rejection message must never quote the query back.
		if len(tc.in) > 8 && strings.Contains(e.Message, tc.in[:8]) {
			t.Errorf("%s: rejection message echoes the query: %q", tc.name, e.Message)
		}
	}
}

func TestTokenizeMinimalStemming(t *testing.T) {
	got := tokenize("Read_The-File.NOW 42", 0)
	want := []string{"read", "the", "file", "now", "42"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tokenize = %v, want %v", got, want)
	}
	// No suffix stripping: "files" is NOT folded to "file" (prefix
	// weighting covers that case instead).
	if tokenize("files", 0)[0] != "files" {
		t.Fatal("tokenizer must not stem")
	}
}

// Meta-tool argument decoding: unknown fields are rejected rather than
// silently dropped, and every failure is typed.
func TestParseSearchArgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		code string
	}{
		{"ok", `{"query":"read"}`, ""},
		{"ok with limit", `{"query":"read","limit":3}`, ""},
		{"unknown field", `{"query":"read","lmit":3}`, CodeInvalidArgs},
		{"wrong type", `{"query":42}`, CodeInvalidArgs},
		{"not an object", `"read"`, CodeInvalidArgs},
		{"negative limit", `{"query":"read","limit":-1}`, CodeInvalidArgs},
		{"absent payload", ``, ""},
		{"null payload", `null`, ""},
	}
	for _, tc := range cases {
		_, err := ParseSearch(json.RawMessage(tc.in))
		if tc.code == "" && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if tc.code != "" && codeOf(err) != tc.code {
			t.Errorf("%s: code = %q, want %q (err=%v)", tc.name, codeOf(err), tc.code, err)
		}
	}
}

func TestParseCallTool(t *testing.T) {
	cases := []struct {
		name string
		in   string
		code string
	}{
		{"ok", `{"tool":"fs__read_file","arguments":{"path":"/tmp"}}`, ""},
		{"ok without arguments", `{"tool":"fs__read_file"}`, ""},
		{"missing tool", `{"arguments":{}}`, CodeInvalidArgs},
		{"blank tool", `{"tool":"   "}`, CodeInvalidArgs},
		{"arguments not an object", `{"tool":"x","arguments":[1,2]}`, CodeInvalidArgs},
		{"unknown field", `{"tool":"x","args":{}}`, CodeInvalidArgs},
	}
	for _, tc := range cases {
		_, err := ParseCallTool(json.RawMessage(tc.in))
		if tc.code == "" && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if tc.code != "" && codeOf(err) != tc.code {
			t.Errorf("%s: code = %q, want %q", tc.name, codeOf(err), tc.code)
		}
	}

	// Arguments travel verbatim.
	a, err := ParseCallTool(json.RawMessage(`{"tool":"t","arguments":{"b":1,"a":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Arguments) != `{"b":1,"a":2}` {
		t.Fatalf("arguments were rewritten: %s", a.Arguments)
	}
}

// An unresolvable call_tool target reports the SAME message whether the
// tool does not exist or is merely invisible: no probing oracle.
func TestResolveCallIsAntiProbing(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: corpus()[:1]}) // only fs__read_file
	_, _, errHidden := s.ResolveCall(json.RawMessage(`{"tool":"git__log"}`))
	_, _, errAbsent := s.ResolveCall(json.RawMessage(`{"tool":"git__nonexistent"}`))
	if errHidden == nil || errAbsent == nil {
		t.Fatal("both lookups must fail")
	}
	var a, b *Error
	asError(errHidden, &a)
	asError(errAbsent, &b)
	if a.Code != CodeUnknownTool || b.Code != CodeUnknownTool {
		t.Fatalf("codes = %q / %q", a.Code, b.Code)
	}
	if strings.Contains(a.Message, "visible") || strings.Contains(a.Message, "scope") {
		t.Fatalf("message leaks the reason: %q", a.Message)
	}

	tool, args, err := s.ResolveCall(json.RawMessage(`{"tool":"fs__read_file","arguments":{"path":"/x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if tool.ServerID != "fs" || tool.RawTool != "read_file" || string(args) != `{"path":"/x"}` {
		t.Fatalf("resolved = %+v args=%s", tool, args)
	}
}

func TestParseFetchResult(t *testing.T) {
	cases := []struct {
		name string
		in   string
		code string
	}{
		{"ok", `{"cursor":"c1"}`, ""},
		{"ok with paging", `{"cursor":"c1","offset":10,"limit":100}`, ""},
		{"missing cursor", `{"offset":1}`, CodeInvalidArgs},
		{"blank cursor", `{"cursor":" "}`, CodeInvalidArgs},
		{"negative offset", `{"cursor":"c","offset":-1}`, CodeInvalidArgs},
		{"negative limit", `{"cursor":"c","limit":-1}`, CodeInvalidArgs},
		{"unknown field", `{"cursor":"c","page":2}`, CodeInvalidArgs},
	}
	for _, tc := range cases {
		_, err := ParseFetchResult(json.RawMessage(tc.in))
		if tc.code == "" && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if tc.code != "" && codeOf(err) != tc.code {
			t.Errorf("%s: code = %q, want %q", tc.name, codeOf(err), tc.code)
		}
	}
}

func TestErrorResultShape(t *testing.T) {
	res := ErrorResult(newError(CodeQueryTooLong, "query exceeds 512 bytes (got 513)"))
	if !res.IsError {
		t.Fatal("meta-tool failures must be isError results, not protocol errors")
	}
	want := `[{"type":"text","text":"query_too_long: query exceeds 512 bytes (got 513)"}]`
	if string(res.Content) != want {
		t.Fatalf("content = %s, want %s", res.Content, want)
	}
	// An untyped error still produces a well-formed reply.
	if r := ErrorResult(errString("boom")); !r.IsError || !json.Valid(r.Content) {
		t.Fatal("untyped error produced a malformed result")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
