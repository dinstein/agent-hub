package toolsig

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// updateGolden regenerates testdata/signatures.golden. Run with
//
//	go test ./internal/discovery/toolsig -update
//
// and REVIEW the diff: the file IS the grammar (canonical.md §7 #4). Every
// byte in it reaches an agent's prompt.
var updateGolden = flag.Bool("update", false, "rewrite testdata golden files")

// corpus is the frozen input set. It covers every branch of the grammar:
// each scalar type, defaults, enums (short and elided), arrays (scalar,
// object, tuple, bare), objects (free-form, expanded, deep, wide), unions,
// surviving $refs, missing/garbage schemas, required-without-properties,
// outputSchema, and the length budget.
var corpus = []struct {
	name string
	def  mcp.ToolDef
	opts Options
}{
	{
		name: "the design example",
		def: def("read_file", `{"type":"object",
			"properties":{
				"path":{"type":"string"},
				"encoding":{"type":"string","default":"utf8"},
				"limit":{"type":"integer"}},
			"required":["path"]}`),
	},
	{
		name: "every scalar type",
		def: def("scalars", `{"type":"object","properties":{
			"s":{"type":"string"},"i":{"type":"integer"},"n":{"type":"number"},
			"b":{"type":"boolean"},"z":{"type":"null"},"a":{}},
			"required":["s","i","n","b","z","a"]}`),
	},
	{
		name: "required order is the schema's, optional order is sorted",
		def: def("ordering", `{"type":"object","properties":{
			"zulu":{"type":"string"},"alpha":{"type":"string"},
			"mike":{"type":"string"},"bravo":{"type":"string"}},
			"required":["zulu","mike"]}`),
	},
	{
		name: "defaults of every kind",
		def: def("defaults", `{"type":"object","properties":{
			"s":{"type":"string","default":"utf8"},
			"i":{"type":"integer","default":3600},
			"b":{"type":"boolean","default":false},
			"z":{"type":"string","default":null},
			"big":{"type":"object","default":{"a":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}}`),
	},
	{
		name: "enums",
		def: def("enums", `{"type":"object","properties":{
			"mode":{"enum":["read","write"]},
			"many":{"enum":["a","b","c","d","e","f","g","h"]},
			"mixed":{"enum":["a b",1,true,null]}},
			"required":["mode","many","mixed"]}`),
	},
	{
		name: "arrays",
		def: def("arrays", `{"type":"object","properties":{
			"names":{"type":"array","items":{"type":"string"}},
			"rows":{"type":"array","items":{"type":"object","properties":{"a":{"type":"string"}}}},
			"bare":{"type":"array"},
			"tuple":{"type":"array","items":[{"type":"string"},{"type":"integer"}]},
			"nested":{"type":"array","items":{"type":"array","items":{"type":"integer"}}}},
			"required":["names","rows","bare","tuple","nested"]}`),
	},
	{
		name: "objects fold after one level",
		def: def("objects", `{"type":"object","properties":{
			"freeform":{"type":"object"},
			"account":{"type":"object","properties":{"id":{"type":"string"},"name":{"type":"string"}}},
			"wide":{"type":"object","properties":{
				"e":{},"d":{},"c":{},"b":{},"a":{},"f":{}}},
			"deep":{"type":"object","properties":{
				"inner":{"type":"object","properties":{"x":{"type":"string"}}}}}},
			"required":["freeform","account","wide","deep"]}`),
	},
	{
		name: "unions, refs and inferred types",
		def: def("edges", `{"type":"object","properties":{
			"nullable":{"type":["string","null"]},
			"union":{"type":["string","integer"]},
			"ref":{"$ref":"#/$defs/Thing"},
			"inferred_obj":{"properties":{"k":{"type":"string"}}},
			"inferred_arr":{"items":{"type":"string"}},
			"anyof":{"anyOf":[{"type":"string"},{"type":"integer"}]},
			"weird":{"type":"decimal"}},
			"required":["nullable","union","ref","inferred_obj","inferred_arr","anyof","weird"]}`),
	},
	{
		name: "no parameters",
		def:  def("ping", `{"type":"object","properties":{},"additionalProperties":false}`),
	},
	{
		name: "no schema at all",
		def:  mcp.ToolDef{Name: "bare"},
	},
	{
		name: "unparsable schema",
		def:  def("broken", `{"type":`),
	},
	{
		name: "boolean schema",
		def:  def("anything", `true`),
	},
	{
		name: "required names missing from properties",
		def:  def("sloppy", `{"type":"object","required":["ghost","ghost","","real"],"properties":{"real":{"type":"string"}}}`),
	},
	{
		name: "declared output schema",
		def: mcp.ToolDef{
			Name:         "count",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"total":{"type":"integer"}}}`),
		},
	},
	{
		name: "output schema that is an array",
		def: mcp.ToolDef{
			Name:         "list_rows",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{}}`),
			OutputSchema: json.RawMessage(`{"type":"array","items":{"type":"string"}}`),
		},
	},
	{
		name: "over budget: optional parameters go first",
		def:  wideDef(10),
		opts: Options{MaxBytes: 80},
	},
	{
		name: "over budget: even required parameters are cut",
		def:  wideDef(10),
		opts: Options{MaxBytes: 40},
	},
	{
		name: "budget smaller than the skeleton",
		def:  wideDef(10),
		opts: Options{MaxBytes: 8},
	},
}

func def(name, schema string) mcp.ToolDef {
	return mcp.ToolDef{Name: name, InputSchema: json.RawMessage(schema)}
}

// wideDef builds a tool with n required and n optional string parameters,
// named so the elision order is readable in the golden file.
func wideDef(n int) mcp.ToolDef {
	props := make([]string, 0, 2*n)
	req := make([]string, 0, n)
	for i := 0; i < n; i++ {
		props = append(props, fmt.Sprintf(`"req%02d":{"type":"string"}`, i))
		props = append(props, fmt.Sprintf(`"opt%02d":{"type":"string"}`, i))
		req = append(req, fmt.Sprintf(`"req%02d"`, i))
	}
	return def("wide", fmt.Sprintf(`{"type":"object","properties":{%s},"required":[%s]}`,
		strings.Join(props, ","), strings.Join(req, ",")))
}

func TestGoldenSignatures(t *testing.T) {
	var b strings.Builder
	for _, tc := range corpus {
		sig := Of(tc.def, tc.opts)
		fmt.Fprintf(&b, "# %s\n", tc.name)
		if tc.opts.MaxBytes > 0 {
			fmt.Fprintf(&b, "  max     %d\n", tc.opts.MaxBytes)
		}
		fmt.Fprintf(&b, "  sig     %s\n", sig.Text)
		fmt.Fprintf(&b, "  lossy   %t\n", sig.Lossy)
		fmt.Fprintf(&b, "  params  %d shown %d\n", sig.Params, sig.Shown)
		fmt.Fprintf(&b, "  bytes   %d\n\n", len(sig.Text))
	}
	assertGolden(t, "signatures.golden", b.String())
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run: go test ./internal/discovery/toolsig -update)", path, err)
	}
	if got != string(want) {
		t.Fatalf("%s drifted — determinism is contract (canonical.md §6)\n--- got ---\n%s\n--- want ---\n%s",
			path, got, want)
	}
}

// Signatures must be a pure function of their inputs: map iteration order
// must never reach the output.
func TestSignaturesAreDeterministic(t *testing.T) {
	for _, tc := range corpus {
		first := Of(tc.def, tc.opts)
		for range 64 {
			if again := Of(tc.def, tc.opts); again != first {
				t.Fatalf("%s: %q then %q", tc.name, first.Text, again.Text)
			}
		}
	}
}

// The length budget is a post-condition, not an intention.
func TestLengthBudgetHolds(t *testing.T) {
	for _, tc := range corpus {
		for _, max := range []int{16, 24, 40, 60, 80, 120, 200, 400} {
			sig := Of(tc.def, Options{MaxBytes: max})
			skeleton := len(tc.def.Name) + len("(…+99 more) -> str")
			if len(sig.Text) > max && max >= skeleton {
				t.Fatalf("%s at max=%d produced %d bytes: %q", tc.name, max, len(sig.Text), sig.Text)
			}
			if !strings.HasPrefix(sig.Text, tc.def.Name+"(") && tc.def.Name != "" {
				t.Fatalf("%s at max=%d truncated the tool name: %q", tc.name, max, sig.Text)
			}
		}
	}
}

// Required parameters survive the budget longer than optional ones. This is
// the "required first" rule, tested as an ordering property rather than a byte
// count so it cannot be satisfied by accident.
func TestBudgetDropsOptionalFirst(t *testing.T) {
	d := wideDef(8)
	prevOptional := 1 << 30
	for _, max := range []int{300, 200, 150, 120, 100, 80, 60, 40} {
		sig := Of(d, Options{MaxBytes: max})
		optional := strings.Count(sig.Text, "opt")
		required := strings.Count(sig.Text, "req")
		if optional > prevOptional {
			t.Fatalf("max=%d kept MORE optional parameters than a looser budget: %q", max, sig.Text)
		}
		if optional > 0 && required < 8 {
			t.Fatalf("max=%d dropped a required parameter while keeping an optional one: %q", max, sig.Text)
		}
		prevOptional = optional
	}
}

// Any truncation, fold or unparsable input must set Lossy. An agent that
// trusts a non-lossy signature must be right to do so.
func TestLossyIsHonest(t *testing.T) {
	tests := []struct {
		name  string
		def   mcp.ToolDef
		opts  Options
		lossy bool
	}{
		{"plain scalars", def("a", `{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`), Options{}, false},
		{"free-form object", def("a", `{"type":"object","properties":{"x":{"type":"object"}}}`), Options{}, false},
		{"short enum", def("a", `{"type":"object","properties":{"x":{"enum":["a","b"]}}}`), Options{}, false},
		{"scalar array", def("a", `{"type":"object","properties":{"x":{"type":"array","items":{"type":"string"}}}}`), Options{}, false},
		{"unconstrained", def("a", `{"type":"object","properties":{"x":{}}}`), Options{}, false},
		{"expanded object", def("a", `{"type":"object","properties":{"x":{"type":"object","properties":{"k":{"type":"string"}}}}}`), Options{}, true},
		{"long enum", def("a", `{"type":"object","properties":{"x":{"enum":["a","b","c","d","e","f","g"]}}}`), Options{}, true},
		{"union", def("a", `{"type":"object","properties":{"x":{"type":["string","integer"]}}}`), Options{}, true},
		{"ref", def("a", `{"type":"object","properties":{"x":{"$ref":"#/$defs/T"}}}`), Options{}, true},
		{"oversized default", def("a", `{"type":"object","properties":{"x":{"type":"string","default":"`+strings.Repeat("y", 40)+`"}}}`), Options{}, true},
		{"tuple items", def("a", `{"type":"object","properties":{"x":{"type":"array","items":[{"type":"string"}]}}}`), Options{}, true},
		{"unparsable", def("a", `{`), Options{}, true},
		{"budget cut", wideDef(6), Options{MaxBytes: 40}, true},
	}
	for _, tc := range tests {
		if got := Of(tc.def, tc.opts); got.Lossy != tc.lossy {
			t.Fatalf("%s: lossy = %v, want %v (%q)", tc.name, got.Lossy, tc.lossy, got.Text)
		}
	}
}

// A signature must actually be cheaper than the schema it stands in for on
// realistic input. That is the reason the whole package exists, so it stays
// asserted even though nothing books the difference any more.
func TestSignatureIsCheaperThanItsSchema(t *testing.T) {
	// Only schemas big enough to be worth replacing: a signature naming a
	// tool is necessarily longer than a schema of `{` or `{}`, and that says
	// nothing about the claim.
	const worthReplacing = 64
	checked := 0
	for _, tc := range corpus {
		if tc.opts.MaxBytes > 0 || len(tc.def.InputSchema) < worthReplacing {
			continue
		}
		checked++
		if sig := Of(tc.def, tc.opts); len(sig.Text) >= len(tc.def.InputSchema) {
			t.Fatalf("%s: signature costs %d bytes against %d of schema",
				tc.name, len(sig.Text), len(tc.def.InputSchema))
		}
	}
	if checked == 0 {
		t.Fatal("no corpus entry carried a schema worth replacing; the test asserted nothing")
	}
}

// The cache must return exactly what Of returns, keyed on the inputs Of
// reads and nothing else.
func TestCache(t *testing.T) {
	c := NewCache(0)
	base := corpus[0].def
	want := Of(base, Options{})

	for range 4 {
		if got := c.Of(base); got != want {
			t.Fatalf("cache returned %+v, want %+v", got, want)
		}
	}
	if c.Len() != 1 {
		t.Fatalf("cache holds %d entries after 4 identical lookups", c.Len())
	}

	// The description is NOT an input: rewording docs must not churn the key.
	reworded := base
	reworded.Description = "a completely different description"
	if got := c.Of(reworded); got != want {
		t.Fatalf("description changed the signature: %+v", got)
	}
	if c.Len() != 1 {
		t.Fatalf("description changed the cache key (%d entries)", c.Len())
	}

	// Name, schema and budget ARE inputs.
	renamed := base
	renamed.Name = "other"
	c.Of(renamed)
	c.OfWith(base.Name, base, Options{MaxBytes: 40})
	changed := base
	changed.OutputSchema = json.RawMessage(`{"type":"integer"}`)
	c.Of(changed)
	if c.Len() != 4 {
		t.Fatalf("cache holds %d entries, want 4", c.Len())
	}

	// A nil cache still renders.
	if got := (*Cache)(nil).Of(base); got != want {
		t.Fatalf("nil cache returned %+v", got)
	}
	if n := (*Cache)(nil).Len(); n != 0 {
		t.Fatalf("nil cache Len = %d", n)
	}
}

// Fingerprints are length-prefixed so adjacent fields cannot be confused.
func TestFingerprintIsUnambiguous(t *testing.T) {
	a := mcp.ToolDef{Name: "a", InputSchema: json.RawMessage(`{"b":1}`)}
	b := mcp.ToolDef{Name: `a{"b":1}`, InputSchema: nil}
	if fingerprint(a.Name, a, Options{}) == fingerprint(b.Name, b, Options{}) {
		t.Fatal("concatenation collision: fields are not length-prefixed")
	}
}

// Eviction is a flush; it must not corrupt results or deadlock.
func TestCacheEviction(t *testing.T) {
	c := NewCache(4)
	for i := 0; i < 40; i++ {
		d := def(fmt.Sprintf("tool%02d", i), `{"type":"object","properties":{"x":{"type":"string"}}}`)
		if got, want := c.Of(d), Of(d, Options{}); got != want {
			t.Fatalf("entry %d: %+v != %+v", i, got, want)
		}
	}
	if c.Len() > 4 {
		t.Fatalf("cache grew past its bound: %d", c.Len())
	}
}

// Shared() is one instance, and the cache is safe under concurrent use.
func TestSharedAndConcurrency(t *testing.T) {
	first, second := Shared(), Shared()
	if first != second {
		t.Fatal("Shared() returned two instances")
	}
	c := NewCache(0)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for _, tc := range corpus {
				c.OfWith(tc.def.Name, tc.def, tc.opts)
			}
			c.Warm([]mcp.ToolDef{def(fmt.Sprintf("w%02d", i), `{"type":"object"}`)}, Options{})
			_ = c.Len()
		}(i)
	}
	wg.Wait()
}
