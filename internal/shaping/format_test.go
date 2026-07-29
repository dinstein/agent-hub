package shaping

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/shaping/toonenc"
)

// tabularResult is the shape TOON exists for: a text block holding a
// homogeneous object array, which collapses from repeated keys to one header
// plus rows.
func tabularRows(n int) string {
	rows := make([]map[string]any, n)
	for i := range rows {
		rows[i] = map[string]any{
			"id":     i + 1,
			"name":   "tool_number_" + string(rune('a'+i%26)),
			"server": "srv" + string(rune('0'+i%10)),
		}
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func textResult(texts ...string) *mcp.CallResult {
	blocks := make([]any, 0, len(texts))
	for _, s := range texts {
		blocks = append(blocks, map[string]string{"type": "text", "text": s})
	}
	block, err := json.Marshal(blocks)
	if err != nil {
		panic(err)
	}
	return &mcp.CallResult{Content: block}
}

func tabularResult() *mcp.CallResult { return textResult(tabularRows(12)) }

func firstText(t *testing.T, res *mcp.CallResult) string {
	t.Helper()
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(res.Content, &blocks); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if len(blocks) == 0 {
		t.Fatal("no content blocks")
	}
	return blocks[0].Text
}

func TestParseFormat(t *testing.T) {
	tests := map[string]Format{
		"":         FormatJSON,
		"json":     FormatJSON,
		"toon":     FormatTOON,
		"TOON":     FormatTOON,
		"  toon  ": FormatTOON,
		"yaml":     FormatJSON, // unknown values stay on the conservative side
		"tOoN":     FormatTOON,
		"toon ":    FormatTOON,
		"to on":    FormatJSON,
	}
	for in, want := range tests {
		if got := ParseFormat(in); got != want {
			t.Fatalf("ParseFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

// json is a pure passthrough: the default must not touch a single byte.
func TestFormatJSONIsPassthrough(t *testing.T) {
	src := tabularResult()
	out, sav, changed := Reformat(src, FormatJSON)
	if changed || out != src {
		t.Fatal("FormatJSON altered the result")
	}
	if sav.SavedTokens != 0 {
		t.Fatalf("FormatJSON reported savings: %+v", sav)
	}
}

func TestReformatTOON(t *testing.T) {
	src := tabularResult()
	out, sav, changed := Reformat(src, FormatTOON)
	if !changed {
		t.Fatal("a homogeneous object array must re-encode")
	}
	text := firstText(t, out)
	if !strings.HasPrefix(text, toonenc.HeaderLine+"\n") {
		t.Fatalf("first re-encoded block must carry the contract marker:\n%s", text)
	}
	if !strings.Contains(text, "[12]{id,name,server}:") {
		t.Fatalf("expected a table header:\n%s", text)
	}
	if resultBytes(out) >= resultBytes(src) {
		t.Fatalf("re-encoding grew the result: %d -> %d", resultBytes(src), resultBytes(out))
	}
	if sav.SavedTokens == 0 {
		t.Fatalf("savings not reported: %+v", sav)
	}
	if sav.BaselineBytes != resultBytes(src) || sav.ActualBytes != resultBytes(out) {
		t.Fatalf("savings do not describe the results: %+v", sav)
	}
}

// The marker is a per-RESULT contract statement, not a per-block one.
func TestContractMarkerAppearsOnce(t *testing.T) {
	rows := tabularRows(12)
	out, _, changed := Reformat(textResult(rows, rows), FormatTOON)
	if !changed {
		t.Fatal("expected re-encoding")
	}
	if n := strings.Count(string(out.Content), toonenc.HeaderLine); n != 1 {
		t.Fatalf("contract marker appears %d times, want 1", n)
	}
}

// structuredContent is the machine channel and must survive byte-identical:
// TOON has no decoder, so anything a client may parse stays JSON.
func TestStructuredContentIsNeverReencoded(t *testing.T) {
	src := tabularResult()
	src.StructuredContent = json.RawMessage(`{"rows":[{"a":1,"b":2},{"a":3,"b":4}]}`)
	out, _, changed := Reformat(src, FormatTOON)
	if !changed {
		t.Fatal("expected the text block to re-encode")
	}
	if string(out.StructuredContent) != string(src.StructuredContent) {
		t.Fatalf("structuredContent changed:\n%s", out.StructuredContent)
	}
}

// Every unexpected input delivers the original: re-encoding is an economy
// mechanism and fails open like the rest of the package.
func TestReformatFailsOpen(t *testing.T) {
	cases := map[string]*mcp.CallResult{
		"nil":                nil,
		"no content":         {},
		"content not array":  {Content: json.RawMessage(`{"type":"text"}`)},
		"malformed content":  {Content: json.RawMessage(`[`)},
		"plain text block":   {Content: json.RawMessage(`[{"type":"text","text":"not json"}]`)},
		"non-text block":     {Content: json.RawMessage(`[{"type":"image","data":"AA=="}]`)},
		"tiny json block":    {Content: json.RawMessage(`[{"type":"text","text":"{\"a\":1}"}]`)},
		"deep nesting loses": {Content: json.RawMessage(`[{"type":"text","text":"{\"a\":{\"b\":{\"c\":{\"d\":{\"e\":1}}}}}"}]`)},
	}
	for name, src := range cases {
		out, sav, changed := Reformat(src, FormatTOON)
		if changed {
			t.Fatalf("%s: re-encoded when it should not have:\n%s", name, out.Content)
		}
		if out != src {
			t.Fatalf("%s: result identity changed", name)
		}
		if sav.SavedTokens != 0 {
			t.Fatalf("%s: reported savings %+v", name, sav)
		}
	}
}

// Universal property: for any content, re-encoding never grows the payload.
func TestReformatIsNeverLarger(t *testing.T) {
	texts := []string{
		`[{"a":1,"b":2},{"a":3,"b":4},{"a":5,"b":6}]`,
		`{"a":{"b":{"c":{"d":{"e":1}}}}}`,
		`{"quoted":"a,b","dash":"- x","num":"42"}`,
		`"scalar"`, `[]`, `{}`, `null`, `12345`,
		`{"deep":[[1,2],[3,4]],"s":"x"}`,
		strings.Repeat("x", 500),
	}
	for _, text := range texts {
		block, err := json.Marshal([]any{map[string]string{"type": "text", "text": text}})
		if err != nil {
			t.Fatal(err)
		}
		src := &mcp.CallResult{Content: block}
		out, _, _ := Reformat(src, FormatTOON)
		if resultBytes(out) > resultBytes(src) {
			t.Fatalf("text %q grew from %d to %d bytes", text, resultBytes(src), resultBytes(out))
		}
	}
}

// ShapeResult reports the end-to-end saving even when nothing was truncated —
// the case Shape's three return values cannot express.
func TestShapeResultReportsReformatOnlySavings(t *testing.T) {
	src := tabularResult()
	r := ShapeResult(src, Budget{Bytes: 0}, Options{Format: FormatTOON})
	if r.Truncated || !r.Cursor.IsZero() {
		t.Fatal("no budget means no truncation")
	}
	if !r.Reformatted {
		t.Fatal("expected re-encoding")
	}
	if r.Savings.SavedTokens == 0 {
		t.Fatalf("savings not reported: %+v", r.Savings)
	}
	if r.Savings.BaselineBytes != resultBytes(src) || r.Savings.ActualBytes != resultBytes(r.Page) {
		t.Fatalf("end-to-end savings do not describe the pair: %+v", r.Savings)
	}
}

// Re-encoding happens BEFORE budgeting, so a result that fits once compacted
// is delivered whole instead of paginated — and the trailer, when there is
// one, is still the last block.
func TestShapeResultReencodesBeforeBudgeting(t *testing.T) {
	src := tabularResult()
	// A budget the JSON form misses by a wide margin (wide enough that the
	// trailer cost cannot make truncation pointless) but the TOON form clears.
	budget := Budget{Bytes: resultBytes(src) * 3 / 4}

	asJSON := ShapeResult(src, budget, Options{Owner: goldenOwner, ID: goldenID})
	if !asJSON.Truncated {
		t.Fatal("the JSON form must not fit this budget")
	}

	asTOON := ShapeResult(src, budget, Options{Owner: goldenOwner, ID: goldenID, Format: FormatTOON})
	if asTOON.Truncated {
		t.Fatalf("the TOON form fits and must be delivered whole:\n%s", asTOON.Page.Content)
	}
	if !asTOON.Reformatted {
		t.Fatal("expected re-encoding")
	}
}

// When both steps fire, the recovery trailer is still the LAST content block.
func TestShapeResultTrailerStaysLast(t *testing.T) {
	src := textResult(tabularRows(60))
	r := ShapeResult(src, Budget{Bytes: 400}, Options{Owner: goldenOwner, ID: goldenID, Format: FormatTOON})
	if !r.Truncated || !r.Reformatted {
		t.Fatalf("expected both steps to fire: %+v", r)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(r.Page.Content, &blocks); err != nil {
		t.Fatal(err)
	}
	last := blocks[len(blocks)-1]
	if last.Type != "text" || !strings.HasPrefix(last.Text, "Truncated by agenthub") {
		t.Fatalf("last block is not the trailer: %+v", last)
	}
	for _, b := range blocks[:len(blocks)-1] {
		if strings.HasPrefix(b.Text, "Truncated by agenthub") {
			t.Fatal("a trailer appears before the last block")
		}
	}
	if r.Savings.BaselineBytes != resultBytes(src) {
		t.Fatalf("savings baseline is not the original result: %+v", r.Savings)
	}
}

// A caller that never sets Format keeps byte-identical M1 behaviour.
func TestDefaultFormatMatchesLegacyShape(t *testing.T) {
	legacy, lc, lok := Shape(goldenResult(), Budget{Bytes: 256}, goldenOpts())
	r := ShapeResult(goldenResult(), Budget{Bytes: 256}, goldenOpts())
	if lok != r.Truncated || string(legacy.Content) != string(r.Page.Content) {
		t.Fatal("ShapeResult diverged from Shape on the default format")
	}
	if lc.NextOffset != r.Cursor.NextOffset || lc.Total != r.Cursor.Total {
		t.Fatalf("cursor diverged: %+v vs %+v", lc, r.Cursor)
	}
	if r.Reformatted {
		t.Fatal("the default format must not re-encode")
	}
}
