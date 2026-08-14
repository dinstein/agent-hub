package toonenc

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden regenerates testdata. Run with
//
//	go test ./internal/shaping/toonenc -update
//
// and REVIEW the diff: every .toon file is the frozen grammar (docs/conventions.md
// §7 #4), not an artefact. A drift here changes what every agent reads.
var updateGolden = flag.Bool("update", false, "rewrite testdata golden files")

// goldenCase pairs an input document with the options it is encoded under.
// The corpus is the ruling's "golden case corpus": scalars, nesting, the table
// form, every quoting trigger, the degenerate shapes, and budget truncation.
type goldenCase struct {
	name string
	in   string
	opts Options
}

var goldenCases = []goldenCase{
	{
		name: "scalars",
		in: `{"name":"agenthub","port":8080,"ratio":0.5,"big":9007199254740993,
		      "huge":123456789012345678901234567890,"ok":true,"off":false,"missing":null,
		      "note":"a plain sentence with spaces"}`,
	},
	{
		name: "nested",
		in: `{"server":{"id":"fs","transport":{"kind":"stdio","cmd":"/usr/bin/fs-mcp",
		      "env":{"PATH":"/usr/bin","HOME":"/home/u"}}},"generation":7}`,
	},
	{
		name: "table",
		in: `{"tools":[{"name":"read_file","server":"fs","calls":12,"ok":true},
		      {"name":"write_file","server":"fs","calls":3,"ok":true},
		      {"name":"commit","server":"git","calls":0,"ok":false}],"total":3}`,
	},
	{
		name: "table_root",
		in:   `[{"a":1,"b":"x"},{"a":2,"b":"y"}]`,
	},
	{
		name: "table_rejected",
		in: `{"rows":[{"a":1,"b":2},{"a":1,"c":2}],
		      "nested":[{"a":{"deep":1}},{"a":{"deep":2}}],
		      "short":[{"a":1}]}`,
	},
	{
		name: "quoting",
		in: `{"empty":"","spaced":"  padded  ","comma":"a,b","colon":"key: value",
		      "quote":"say \"hi\"","backslash":"C:\\tmp","newline":"one\ntwo",
		      "numeric":"42","floaty":"1e9","boolish":"true","nully":"null",
		      "dash":"- item","bracket":"[x]","brace":"{x}","hash":"#tag",
		      "tab":"a\tb","unicode":"路径/文件","key with space":1,"dash-ok":"a-b"}`,
	},
	{
		name: "lists",
		in: `{"words":["alpha","beta","gamma"],"mixed":[1,"two",true,null],
		      "objects":[{"a":1,"b":{"c":2}},{"a":2,"b":{"c":3}}],
		      "matrix":[[1,2],[3,4]],"empty_list":[],"empty_obj":{}}`,
	},
	{
		name: "root_scalar",
		in:   `"just a string"`,
	},
	{
		name: "header",
		in:   `{"a":1,"b":2}`,
		opts: Options{Header: true},
	},
	{
		name: "truncated",
		in: `{"rows":[{"id":1,"label":"alpha"},{"id":2,"label":"bravo"},
		      {"id":3,"label":"charlie"},{"id":4,"label":"delta"},
		      {"id":5,"label":"echo"}],"tail":"dropped"}`,
		opts: Options{Budget: 80},
	},
	{
		name: "indent_four",
		in:   `{"a":{"b":{"c":1}}}`,
		opts: Options{Indent: 4},
	},
}

func TestGoldenGrammar(t *testing.T) {
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeJSON([]byte(tc.in), tc.opts)
			if err != nil {
				t.Fatalf("EncodeJSON: %v", err)
			}
			assertGolden(t, tc.name+".toon", got)
		})
	}
}

// The encoder is a pure function: the same input must produce byte-identical
// output on every run, whatever the map iteration order happened to be.
func TestEncodeIsDeterministic(t *testing.T) {
	for _, tc := range goldenCases {
		first, err := EncodeJSON([]byte(tc.in), tc.opts)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for i := 0; i < 32; i++ {
			again, err := EncodeJSON([]byte(tc.in), tc.opts)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if again != first {
				t.Fatalf("%s: encoding is not deterministic\n--- run 1 ---\n%s\n--- run %d ---\n%s",
					tc.name, first, i+2, again)
			}
		}
	}
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	body := got
	if !strings.HasSuffix(body, "\n") {
		body += "\n" // keep the files newline-terminated for reviewability
	}
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run: go test ./internal/shaping/toonenc -update)", path, err)
	}
	if body != string(want) {
		t.Fatalf("%s drifted — determinism is contract (docs/conventions.md#engineering-conventions)\n--- got ---\n%s\n--- want ---\n%s",
			path, body, want)
	}
}
