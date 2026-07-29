package discovery

import (
	"encoding/json"
	"strings"
	"testing"
)

// THE privacy test: a search trace must record measurements and tool names,
// never a byte of the query. It marshals a trace for a query stuffed with
// distinctive tokens and asserts none of them survive. Adding a field that
// carries query text to Trace breaks this test — which is the point.
func TestTraceNeverCarriesQueryText(t *testing.T) {
	const secret = "hunter2-swordfish-AKIAIOSFODNN7EXAMPLE"
	queries := []string{
		"read file " + secret,
		secret,
		strings.Repeat(secret, 40), // over the byte limit: rejected path
		"",                         // empty: rejected path
	}
	s := New(Options{Mode: ModeLazy, Tools: corpus()})
	for _, q := range queries {
		payload, err := json.Marshal(struct {
			Query string `json:"query"`
		}{q})
		if err != nil {
			t.Fatal(err)
		}
		_, res := s.HandleSearch(payload, NewSearchGuard())
		raw, err := json.Marshal(res.Trace)
		if err != nil {
			t.Fatal(err)
		}
		for _, frag := range []string{secret, "hunter2", "swordfish", "AKIA"} {
			if strings.Contains(string(raw), frag) {
				t.Fatalf("trace leaked query text (%q): %s", frag, raw)
			}
		}
	}
}

// The trace's measurements and result list are exact.
func TestTraceContents(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: corpus()})
	const q = "read file now"
	_, res := s.HandleSearch(json.RawMessage(`{"query":"read file now","limit":1}`), nil)

	if res.Trace.QueryBytes != len(q) {
		t.Errorf("QueryBytes = %d, want %d", res.Trace.QueryBytes, len(q))
	}
	if res.Trace.QueryWords != 3 {
		t.Errorf("QueryWords = %d, want 3", res.Trace.QueryWords)
	}
	if len(res.Trace.Results) != len(res.Hits) {
		t.Fatalf("Results = %v, hits = %v", res.Trace.Results, hitNames(res.Hits))
	}
	for i, name := range res.Trace.Results {
		if name != res.Hits[i].Tool {
			t.Errorf("Results[%d] = %q, want %q", i, name, res.Hits[i].Tool)
		}
	}
	if res.Trace.Matched != res.Matched || res.Trace.Matched < len(res.Hits) {
		t.Errorf("Matched = %d, hits = %d", res.Trace.Matched, len(res.Hits))
	}
	if !res.Trace.Truncated {
		t.Error("Truncated must be recorded when Matched exceeds the limit")
	}
	if res.Trace.TopScore != res.Hits[0].Score {
		t.Errorf("TopScore = %d, want %d", res.Trace.TopScore, res.Hits[0].Score)
	}
	if res.Trace.Rejected != "" || res.Trace.Escalated {
		t.Errorf("unexpected flags: %+v", res.Trace)
	}
}

// A rejected query still produces a usable trace: an agent hammering the
// limits is exactly the pattern audit needs to see.
func TestTraceOfRejection(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: corpus()})
	long := strings.Repeat("x", MaxQueryBytes+10)
	payload, err := json.Marshal(struct {
		Query string `json:"query"`
	}{long})
	if err != nil {
		t.Fatal(err)
	}
	_, res := s.HandleSearch(payload, nil)
	if res.Trace.Rejected != CodeQueryTooLong {
		t.Fatalf("Rejected = %q, want %q", res.Trace.Rejected, CodeQueryTooLong)
	}
	if res.Trace.QueryBytes != len(long) {
		t.Fatalf("QueryBytes = %d, want %d", res.Trace.QueryBytes, len(long))
	}
	if len(res.Trace.Results) != 0 {
		t.Fatalf("a rejected query produced results: %v", res.Trace.Results)
	}
}
