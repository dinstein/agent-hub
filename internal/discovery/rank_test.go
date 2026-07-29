package discovery

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// rankQueries is the frozen query set. Each exercises one ranker property:
// exact name hit, multi-term coverage, prefix weighting, server-name
// routing, description-only recall, a tie that must break on the name, and
// a miss.
var rankQueries = []string{
	"read file",
	"read",
	"file",
	"fs",
	"git commit",
	"repository",
	"searching the web",
	"list",
	"disk",
	"wr",           // short prefix: matches write_file
	"a",            // below minPrefixLen: matches nothing by prefix
	"quantum flux", // no match at all
}

// The ranking order — and the scores that produce it — is a contract:
// agents are prompted against "the first result is the one to call", and
// SearchGuard's confidence threshold is calibrated on these numbers.
func TestGoldenRanking(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: corpus()})
	var b strings.Builder
	for _, q := range rankQueries {
		res, err := s.Search(SearchRequest{Query: q, Limit: MaxSearchLimit}, nil)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		fmt.Fprintf(&b, "query=%q matched=%d\n", q, res.Matched)
		if len(res.Hits) == 0 {
			b.WriteString("  (no match)\n")
		}
		for _, h := range res.Hits {
			fmt.Fprintf(&b, "  %d. %s score=%d call_with=%s\n", h.Rank, h.Tool, h.Score, h.CallWith)
		}
	}
	assertGolden(t, "ranking.txt", []byte(b.String()))
}

// Ranking must not depend on the order tools were handed to New.
func TestRankingIsOrderIndependent(t *testing.T) {
	forward := corpus()
	reversed := make([]Tool, len(forward))
	for i, t := range forward {
		reversed[len(forward)-1-i] = t
	}
	a := New(Options{Mode: ModeLazy, Tools: forward})
	b := New(Options{Mode: ModeLazy, Tools: reversed})
	for _, q := range rankQueries {
		ra, err := a.Search(SearchRequest{Query: q, Limit: MaxSearchLimit}, nil)
		if err != nil {
			t.Fatal(err)
		}
		rb, err := b.Search(SearchRequest{Query: q, Limit: MaxSearchLimit}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(ra.Hits) != len(rb.Hits) {
			t.Fatalf("query %q: %d vs %d hits", q, len(ra.Hits), len(rb.Hits))
		}
		for i := range ra.Hits {
			if ra.Hits[i].Tool != rb.Hits[i].Tool || ra.Hits[i].Score != rb.Hits[i].Score {
				t.Fatalf("query %q rank %d: %v vs %v", q, i+1, ra.Hits[i], rb.Hits[i])
			}
		}
	}
}

// Equal scores break on the exposed name ascending — never on map order.
func TestTiesBreakOnExposedName(t *testing.T) {
	tools := []Tool{
		{Exposed: "z__widget", ServerID: "z", RawTool: "widget", Def: defWithDesc("a widget")},
		{Exposed: "a__widget", ServerID: "a", RawTool: "widget", Def: defWithDesc("a widget")},
		{Exposed: "m__widget", ServerID: "m", RawTool: "widget", Def: defWithDesc("a widget")},
	}
	s := New(Options{Mode: ModeLazy, Tools: tools})
	res, err := s.Search(SearchRequest{Query: "widget"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a__widget", "m__widget", "z__widget"}
	if len(res.Hits) != 3 {
		t.Fatalf("got %d hits", len(res.Hits))
	}
	for i, w := range want {
		if res.Hits[i].Tool != w {
			t.Fatalf("tie order = %v, want %v", hitNames(res.Hits), want)
		}
		if res.Hits[i].Score != res.Hits[0].Score {
			t.Fatalf("expected equal scores, got %d vs %d", res.Hits[i].Score, res.Hits[0].Score)
		}
	}
}

// Repeating a term must not inflate a score: occurrence counts are not a
// ranking signal (they would be trivially gameable by a hostile server).
func TestRepetitionIsNotASignal(t *testing.T) {
	honest := Tool{Exposed: "s__a", ServerID: "s", RawTool: "a", Def: defWithDesc("fetch a page")}
	spammy := Tool{Exposed: "s__b", ServerID: "s", RawTool: "b",
		Def: defWithDesc("fetch fetch fetch fetch fetch fetch fetch fetch")}
	s := New(Options{Mode: ModeLazy, Tools: []Tool{honest, spammy}})
	res, err := s.Search(SearchRequest{Query: "fetch"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Hits[0].Score != res.Hits[1].Score {
		t.Fatalf("repetition changed the score: %d vs %d", res.Hits[0].Score, res.Hits[1].Score)
	}
}

// A duplicated query term must not inflate the score either.
func TestDuplicateQueryTermsCollapse(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: corpus()})
	one, err := s.Search(SearchRequest{Query: "file"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	many, err := s.Search(SearchRequest{Query: "file file file"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if one.Hits[0].Score != many.Hits[0].Score {
		t.Fatalf("duplicate query terms changed the score: %d vs %d",
			one.Hits[0].Score, many.Hits[0].Score)
	}
}

// A description longer than MaxDescriptionTokens is indexed only up to the
// bound: a hostile server cannot make every search expensive.
func TestDescriptionIndexIsBounded(t *testing.T) {
	var b strings.Builder
	for range MaxDescriptionTokens {
		b.WriteString("pad ")
	}
	b.WriteString("needle")
	s := New(Options{Mode: ModeLazy, Tools: []Tool{
		{Exposed: "s__big", ServerID: "s", RawTool: "big", Def: defWithDesc(b.String())},
	}})
	res, err := s.Search(SearchRequest{Query: "needle"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Fatal("a token past the index bound was still matched")
	}
}

// An exact tool-name match must clear the confidence threshold; weaker
// evidence must not (this is what keeps SearchGuard from forcing a guess).
func TestConfidenceCalibration(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: corpus()})
	strong, err := s.Search(SearchRequest{Query: "commit"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strong.Hits[0].Score < ConfidenceThreshold {
		t.Fatalf("exact name match scored %d, below the confidence threshold %d",
			strong.Hits[0].Score, ConfidenceThreshold)
	}
	weak, err := s.Search(SearchRequest{Query: "entries"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(weak.Hits) == 0 {
		t.Fatal("expected a description-only match")
	}
	if weak.Hits[0].Score >= ConfidenceThreshold {
		t.Fatalf("description-only match scored %d, at or above the threshold %d",
			weak.Hits[0].Score, ConfidenceThreshold)
	}
}

func TestSearchLimitAndTruncation(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: corpus()})
	res, err := s.Search(SearchRequest{Query: "file", Limit: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || !res.Truncated {
		t.Fatalf("limit not applied: %d hits, truncated=%v", len(res.Hits), res.Truncated)
	}
	if res.Matched < 2 {
		t.Fatalf("Matched must count pre-limit candidates, got %d", res.Matched)
	}
	// Limit 0 falls back to the default; an oversized limit is clamped.
	a, _ := ParseSearch([]byte(`{"query":"x"}`))
	if a.Limit != DefaultSearchLimit {
		t.Fatalf("default limit = %d", a.Limit)
	}
	b, _ := ParseSearch([]byte(`{"query":"x","limit":9999}`))
	if b.Limit != MaxSearchLimit {
		t.Fatalf("clamped limit = %d", b.Limit)
	}
}

// defWithDesc builds a minimal definition carrying only a description.
func defWithDesc(desc string) mcp.ToolDef {
	return mcp.ToolDef{Description: desc, InputSchema: []byte(`{"type":"object"}`)}
}
