package shaping

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// goldenNow / goldenOwner / goldenID fix every variable input so the golden
// output below is a pure function of the shaping rules.
var (
	goldenNow   = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	goldenOwner = Owner("claude-code:1")
)

const goldenID = "rc-000001"

// goldenResult is the frozen shaping input: a long text block that must be
// split mid-block, a structured (resource) block that must NOT be split, a
// short trailing text block, and structuredContent.
func goldenResult() *mcp.CallResult {
	return &mcp.CallResult{
		Content: json.RawMessage(`[` +
			`{"type":"text","text":"` + strings.Repeat("line of tool output. ", 40) + `"},` +
			`{"type":"resource","resource":{"uri":"file:///tmp/report.txt","text":"embedded payload"}},` +
			`{"type":"text","text":"tail"}` +
			`]`),
		StructuredContent: json.RawMessage(`{"rows":[1,2,3],"ok":true}`),
	}
}

func goldenOpts() Options {
	return Options{Owner: goldenOwner, ID: goldenID, Now: goldenNow, TTL: 30 * time.Minute}
}

// Frozen wire output of Shape(goldenResult(), Budget{256}). Determinism is
// contract (docs/conventions.md#engineering-conventions): the split point, the trailer wording and the
// cursor shape are all agent-visible, so a change here is an ABI change and
// must be made deliberately.
const goldenPage1 = `[{"type":"text","text":"line of tool output. line of tool output. line of tool output. ` +
	`line of tool output. line of tool output. line of tool output. line of tool output. ` +
	`line of tool output. line of tool output. line of tool output. line of tool output"},` +
	`{"type":"text","text":"Truncated by agenthub to fit the result budget: 229 of 962 characters delivered. ` +
	`Use fetch_result with cursor=rc-000001 offset=229 to continue."}]`

const goldenPage2 = `[{"type":"text","text":". line of tool output. line of tool output. line of tool output. ` +
	`line of tool output. line of tool output. line of tool output. line of tool output. ` +
	`line of tool output. line of tool output. line of tool output. line of tool outp"},` +
	`{"type":"text","text":"Truncated by agenthub to fit the result budget: 458 of 962 characters delivered. ` +
	`Use fetch_result with cursor=rc-000001 offset=458 to continue."}]`

func TestShapeGolden(t *testing.T) {
	out, c, ok := Shape(goldenResult(), Budget{Bytes: 256}, goldenOpts())
	if !ok {
		t.Fatal("expected truncation")
	}
	if got := string(out.Content); got != goldenPage1 {
		t.Errorf("page 1 drifted:\n got %s\nwant %s", got, goldenPage1)
	}
	// structuredContent sits after every content block in the linearized
	// payload, so a truncated result never carries it: it is deferred whole.
	// This is the behaviour docs/status/mcp-2026-07-28.md §7.14 records as a
	// conformance cost — a page whose tool declared an outputSchema does not
	// satisfy it — so a change here is a decision, not a fix.
	if out.StructuredContent != nil {
		t.Errorf("structuredContent must be deferred when the result is truncated, got %s", out.StructuredContent)
	}
	if c.ID != goldenID || c.Owner != goldenOwner {
		t.Errorf("cursor identity = %q/%q", c.ID, c.Owner)
	}
	if c.NextOffset != 229 || c.Total != 962 {
		t.Errorf("cursor offsets = next %d total %d, want 229/962", c.NextOffset, c.Total)
	}
	if c.CreatedAt != goldenNow || c.TTL != 30*time.Minute {
		t.Errorf("cursor lifetime = %v +%v", c.CreatedAt, c.TTL)
	}
}

// The cursor id shape is frozen: "rc-" plus a zero-padded sequence.
func TestCursorIDShape(t *testing.T) {
	s := NewMemStore(0)
	for i, want := range []string{"rc-000001", "rc-000002", "rc-000003"} {
		if got := s.NextID(); got != want {
			t.Fatalf("NextID #%d = %q, want %q", i, got, want)
		}
	}
	if got := formatID(1234567); got != "rc-1234567" {
		t.Errorf("wide sequence = %q", got)
	}
	for _, bad := range []string{"", "rc-", "rc-0001", "rc-00000a", "../etc/passwd", "rc-000001/../x", "RC-000001"} {
		if validID(bad) {
			t.Errorf("validID(%q) must be false", bad)
		}
	}
}

// Paging from the trailer offset must reproduce the retained payload
// exactly, block boundaries and all — a round trip that loses or duplicates
// one character silently corrupts every large tool result.
func TestPaginationRoundTrip(t *testing.T) {
	src := goldenResult()
	out, c, ok := Shape(src, Budget{Bytes: 256}, goldenOpts())
	if !ok {
		t.Fatal("expected truncation")
	}
	store := NewMemStore(0)
	store.Clock = func() time.Time { return goldenNow }
	if err := Retain(t.Context(), store, c); err != nil {
		t.Fatal(err)
	}

	// Page 1 delivered exactly NextOffset runes of the payload.
	var rebuilt strings.Builder
	rebuilt.WriteString(c.full[:runeIndexToByte(c.full, c.NextOffset)])
	if got := blockText(t, out.Content, 0); !strings.HasPrefix(c.full, got) {
		t.Fatal("page 1 text is not a prefix of the retained payload")
	}

	offset := c.NextOffset
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatal("pagination did not terminate")
		}
		res, found := Fetch(t.Context(), store, goldenOwner, c.ID, offset)
		if !found {
			t.Fatalf("fetch at offset %d missed", offset)
		}
		if i == 0 {
			if got := string(res.Content); got != goldenPage2 {
				t.Errorf("page 2 drifted:\n got %s\nwant %s", got, goldenPage2)
			}
		}
		var blocks []json.RawMessage
		if err := json.Unmarshal(res.Content, &blocks); err != nil {
			t.Fatal(err)
		}
		if len(blocks) == 0 {
			t.Fatalf("empty page before the end (offset %d of %d)", offset, c.Total)
		}
		text := blockText(t, res.Content, 0)
		rebuilt.WriteString(text)
		offset += utf8.RuneCountInString(text)
		if len(blocks) == 1 {
			break // no trailer: last page
		}
	}
	if rebuilt.String() != c.full {
		t.Errorf("round trip lost data:\n got %q\nwant %q", rebuilt.String(), c.full)
	}
	if offset != c.Total {
		t.Errorf("final offset = %d, want %d", offset, c.Total)
	}
}

// The trailer is the last block, is exempt from the budget, and is never
// itself truncated — a recovery hint the agent cannot read is not a
// recovery hint (docs/flows.md, same rule as pipeline's injection trailer).
func TestTrailerIsLastAndWhole(t *testing.T) {
	// A budget far below the trailer's own size still yields a whole
	// trailer as the final block.
	out, c, ok := Shape(goldenResult(), Budget{Bytes: 64}, goldenOpts())
	if !ok {
		t.Fatal("expected truncation")
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(out.Content, &blocks); err != nil {
		t.Fatal(err)
	}
	last := blockText(t, out.Content, len(blocks)-1)
	if !strings.HasPrefix(last, "Truncated by agenthub") ||
		!strings.Contains(last, "Use fetch_result with cursor="+c.ID) ||
		!strings.HasSuffix(last, "to continue.") {
		t.Fatalf("trailer block malformed: %q", last)
	}
	if len(out.Content) <= 64 {
		t.Fatal("expected the page to exceed the budget by the exempt trailer")
	}
}

// Structured (non-text) blocks are all-or-nothing: the walk stops at one
// that does not fit rather than splitting it into invalid JSON.
func TestStructuredBlockDeferredWhole(t *testing.T) {
	big := strings.Repeat("x", 900)
	res := &mcp.CallResult{Content: json.RawMessage(
		`[{"type":"text","text":"` + strings.Repeat("a", 200) + `"},` +
			`{"type":"resource","resource":{"uri":"file:///r","text":"` + big + `"}},` +
			`{"type":"text","text":"after"}]`)}
	out, c, ok := Shape(res, Budget{Bytes: 300}, goldenOpts())
	if !ok {
		t.Fatal("expected truncation")
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(out.Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 { // the 200-char text block + trailer
		t.Fatalf("blocks = %d, want 2 (whole first block + trailer)", len(blocks))
	}
	if got := blockText(t, out.Content, 0); got != strings.Repeat("a", 200) {
		t.Errorf("first block was altered: %q", got)
	}
	// The deferred resource block must survive intact in the payload.
	if !strings.Contains(c.full, big) {
		t.Error("deferred resource payload was lost")
	}
	if c.NextOffset != 201 { // 200 runes + the "\n" separator
		t.Errorf("next offset = %d, want 201", c.NextOffset)
	}
}

// Every unexpected input fails OPEN: budgeting is token economy, never a
// security boundary, so the caller's data is delivered rather than dropped.
func TestShapeFailsOpen(t *testing.T) {
	big := &mcp.CallResult{Content: json.RawMessage(
		`[{"type":"text","text":"` + strings.Repeat("z", 4000) + `"}]`)}
	cases := []struct {
		name   string
		res    *mcp.CallResult
		budget Budget
		opts   Options
	}{
		{"nil result", nil, Budget{Bytes: 10}, goldenOpts()},
		{"unbounded budget", big, Budget{Bytes: 0}, goldenOpts()},
		{"negative budget", big, Budget{Bytes: -1}, goldenOpts()},
		{"no cursor id", big, Budget{Bytes: 100}, Options{Owner: goldenOwner}},
		{"no owner", big, Budget{Bytes: 100}, Options{ID: goldenID}},
		{"content not an array", &mcp.CallResult{Content: json.RawMessage(`"` + strings.Repeat("q", 4000) + `"`)},
			Budget{Bytes: 100}, goldenOpts()},
		{"content not JSON", &mcp.CallResult{Content: json.RawMessage(strings.Repeat("{", 4000))},
			Budget{Bytes: 100}, goldenOpts()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, c, ok := Shape(tc.res, tc.budget, tc.opts)
			if ok || !c.IsZero() {
				t.Fatalf("expected pass-through, got ok=%v cursor=%q", ok, c.ID)
			}
			if out != tc.res {
				t.Fatal("pass-through must return the original result value")
			}
		})
	}
}

// Never-larger: the trailer is not free, so on a result that only just
// exceeds its budget the shaped page would cost more than the original.
// Shaping stands down instead.
func TestNeverLargerThanBaseline(t *testing.T) {
	res := &mcp.CallResult{Content: json.RawMessage(
		`[{"type":"text","text":"` + strings.Repeat("m", 120) + `"}]`)}
	out, c, ok := Shape(res, Budget{Bytes: 100}, goldenOpts())
	if ok {
		t.Fatalf("shaping produced a %d-byte page for a %d-byte result",
			len(out.Content), len(res.Content))
	}
	if !c.IsZero() {
		t.Error("no cursor may be minted when shaping stands down")
	}
}

// IsError travels with the shaped page: a truncated error result is still
// an error result.
func TestShapePreservesIsError(t *testing.T) {
	res := goldenResult()
	res.IsError = true
	out, _, ok := Shape(res, Budget{Bytes: 256}, goldenOpts())
	if !ok || !out.IsError {
		t.Fatalf("ok=%v isError=%v", ok, out.IsError)
	}
}

// The result's own _meta travels with the shaped page, for the reason
// IsError does: a page is still the downstream's result.
//
// This is the path a field added to mcp.CallResult could most easily be lost
// on anyway, and the hardest to notice: `shape` returns the original
// untouched when nothing is cut, so small results would have carried the
// member and only large ones dropped it.
func TestShapePreservesMeta(t *testing.T) {
	res := goldenResult()
	res.Meta = json.RawMessage(`{"com.example.tools/traceId":"abc123"}`)
	out, _, ok := Shape(res, Budget{Bytes: 256}, goldenOpts())
	if !ok {
		t.Fatal("this result was expected to be truncated")
	}
	if string(out.Meta) != string(res.Meta) {
		t.Fatalf("_meta = %s, want %s", out.Meta, res.Meta)
	}
}

// escapedRuneLen must agree with encoding/json byte for byte; the split
// point is computed from it, so a divergence would silently push pages over
// the budget.
func TestEscapedRuneLenMatchesStdlib(t *testing.T) {
	samples := []string{
		"plain ascii", `quote " backslash \\ `, "control\x00\x01\x07\b\f\n\r\t",
		"html <tag> & amp", "unicode \u00e9 \u4e2d\u6587 \U0001F600",
		"line sep \u2028 para sep \u2029", "invalid \xff\xfe bytes", "", "\x7f\u00a0",
	}
	for _, s := range samples {
		want, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		got := 2 // the enclosing quotes
		for i := 0; i < len(s); {
			r, size := utf8.DecodeRuneInString(s[i:])
			got += escapedRuneLen(r, size)
			i += size
		}
		if got != len(want) {
			t.Errorf("escaped length of %q = %d, stdlib = %d (%s)", s, got, len(want), want)
		}
	}
}

// A split must never land inside a UTF-8 sequence.
func TestSplitStaysOnRuneBoundary(t *testing.T) {
	text := strings.Repeat("中文\U0001F600", 200)
	res := &mcp.CallResult{Content: json.RawMessage(
		`[` + string(textBlock(text)) + `]`)}
	for budget := 40; budget < 400; budget += 7 {
		out, c, ok := Shape(res, Budget{Bytes: budget}, goldenOpts())
		if !ok {
			continue
		}
		var blocks []json.RawMessage
		if err := json.Unmarshal(out.Content, &blocks); err != nil {
			t.Fatal(err)
		}
		if len(blocks) == 1 {
			// Too small to be worth a partial block: the whole block is
			// deferred and only the trailer is delivered.
			if c.NextOffset != 0 {
				t.Fatalf("budget %d: trailer-only page must defer from offset 0, got %d", budget, c.NextOffset)
			}
			continue
		}
		got := blockText(t, out.Content, 0)
		if !utf8.ValidString(got) {
			t.Fatalf("budget %d produced invalid UTF-8", budget)
		}
		if !strings.HasPrefix(text, got) {
			t.Fatalf("budget %d produced a non-prefix page", budget)
		}
		if utf8.RuneCountInString(got) != c.NextOffset {
			t.Fatalf("budget %d: delivered %d runes but cursor says %d",
				budget, utf8.RuneCountInString(got), c.NextOffset)
		}
	}
}

// blockText returns the "text" field of content block i.
func blockText(t *testing.T, content json.RawMessage, i int) string {
	t.Helper()
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if i < 0 || i >= len(blocks) {
		t.Fatalf("block %d out of range (%d blocks)", i, len(blocks))
	}
	return blocks[i].Text
}
