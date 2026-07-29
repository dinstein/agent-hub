package discovery

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// The escalation wording is a contract: it is the single instruction an
// agent stuck in a search loop receives.
func TestEscalationMessageIsFrozen(t *testing.T) {
	if got := escalationMessage("fs__read_file"); got != "you already found fs__read_file; call it" {
		t.Fatalf("escalation message = %q", got)
	}
}

// The state machine, walked end to end.
func TestSearchGuardStateMachine(t *testing.T) {
	const confident = ConfidenceThreshold + 10
	const weak = ConfidenceThreshold - 1

	t.Run("escalates on the third identical confident top", func(t *testing.T) {
		g := NewSearchGuard()
		for i := 1; i <= 2; i++ {
			e := g.ObserveSearch("fs__read_file", confident)
			if e.Fire {
				t.Fatalf("escalated after %d searches, want %d", i, EscalateAfter)
			}
			if e.Streak != i {
				t.Fatalf("streak = %d, want %d", e.Streak, i)
			}
		}
		e := g.ObserveSearch("fs__read_file", confident)
		if !e.Fire || e.Streak != EscalateAfter {
			t.Fatalf("no escalation on search %d: %+v", EscalateAfter, e)
		}
		if e.Message != "you already found fs__read_file; call it" {
			t.Fatalf("message = %q", e.Message)
		}
		// Searching again after being told keeps telling: only doing
		// something else ends the loop.
		if again := g.ObserveSearch("fs__read_file", confident); !again.Fire {
			t.Fatal("escalation must persist while the loop persists")
		}
	})

	t.Run("a different top restarts the streak", func(t *testing.T) {
		g := NewSearchGuard()
		g.ObserveSearch("a", confident)
		g.ObserveSearch("a", confident)
		e := g.ObserveSearch("b", confident)
		if e.Fire || e.Streak != 1 {
			t.Fatalf("streak did not restart: %+v", e)
		}
		if e := g.ObserveSearch("b", confident); e.Fire {
			t.Fatal("escalated too early after a top change")
		}
		if e := g.ObserveSearch("b", confident); !e.Fire {
			t.Fatal("did not escalate on the new top's third search")
		}
	})

	t.Run("any non-search action resets", func(t *testing.T) {
		for _, reset := range []struct {
			name string
			fn   func(*SearchGuard)
		}{
			{"call_tool / fetch_result / status", (*SearchGuard).ObserveOther},
			{"scope change", (*SearchGuard).Reset},
		} {
			g := NewSearchGuard()
			g.ObserveSearch("a", confident)
			g.ObserveSearch("a", confident)
			reset.fn(g)
			if top, streak := g.State(); top != "" || streak != 0 {
				t.Fatalf("%s: state = %q/%d, want empty", reset.name, top, streak)
			}
			if e := g.ObserveSearch("a", confident); e.Fire || e.Streak != 1 {
				t.Fatalf("%s: streak survived the reset: %+v", reset.name, e)
			}
		}
	})

	t.Run("low confidence never escalates", func(t *testing.T) {
		g := NewSearchGuard()
		for i := 0; i < EscalateAfter+3; i++ {
			e := g.ObserveSearch("fs__read_file", weak)
			if e.Fire {
				t.Fatalf("escalated on a low-confidence top (score %d < %d)", weak, ConfidenceThreshold)
			}
			if e.Confident {
				t.Fatal("a below-threshold score must not be reported as confident")
			}
		}
		// The streak was still accumulating: the moment the same top
		// becomes confident, the guard fires immediately.
		if e := g.ObserveSearch("fs__read_file", confident); !e.Fire {
			t.Fatal("an accumulated streak must escalate as soon as the top is confident")
		}
	})

	t.Run("exactly at the threshold is confident", func(t *testing.T) {
		g := NewSearchGuard()
		g.ObserveSearch("a", ConfidenceThreshold)
		g.ObserveSearch("a", ConfidenceThreshold)
		if e := g.ObserveSearch("a", ConfidenceThreshold); !e.Fire {
			t.Fatal("score == ConfidenceThreshold must count as confident")
		}
	})

	t.Run("an empty result clears the streak", func(t *testing.T) {
		g := NewSearchGuard()
		g.ObserveSearch("a", confident)
		g.ObserveSearch("a", confident)
		if e := g.ObserveSearch("", 0); e.Fire || e.Streak != 0 {
			t.Fatalf("empty result did not clear: %+v", e)
		}
		if e := g.ObserveSearch("a", confident); e.Fire {
			t.Fatal("streak survived an empty result")
		}
	})

	t.Run("nil guard is inert", func(t *testing.T) {
		var g *SearchGuard
		if e := g.ObserveSearch("a", confident); e.Fire {
			t.Fatal("nil guard escalated")
		}
		g.ObserveOther()
		g.Reset()
		if top, streak := g.State(); top != "" || streak != 0 {
			t.Fatalf("nil guard state = %q/%d", top, streak)
		}
	})
}

// End to end through the handler: the escalated reply is ONE line, carries
// no result list, and is recorded as such in the trace.
func TestSearchGuardTruncatesTheReply(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: corpus()})
	g := NewSearchGuard()
	args := json.RawMessage(`{"query":"commit"}`)

	for i := 0; i < EscalateAfter-1; i++ {
		out, sr := s.HandleSearch(args, g)
		if sr.Escalation.Fire || out.IsError {
			t.Fatalf("search %d escalated early", i+1)
		}
	}
	out, sr := s.HandleSearch(args, g)
	if !sr.Escalation.Fire {
		t.Fatal("no escalation on the third identical search")
	}
	if len(sr.Hits) != 0 {
		t.Fatalf("escalated reply carried %d hits", len(sr.Hits))
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(out.Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("escalated reply has %d content blocks, want 1", len(blocks))
	}
	if strings.Count(blocks[0].Text, "\n") != 0 {
		t.Fatalf("escalated reply is not a single line: %q", blocks[0].Text)
	}
	if blocks[0].Text != "you already found git__commit; call it" {
		t.Fatalf("escalated reply = %q", blocks[0].Text)
	}
	if !sr.Trace.Escalated || len(sr.Trace.Results) != 1 || sr.Trace.Results[0] != "git__commit" {
		t.Fatalf("trace = %+v", sr.Trace)
	}

	// A non-search action clears it; the next search answers normally again.
	g.ObserveOther()
	out, sr = s.HandleSearch(args, g)
	if sr.Escalation.Fire || out.IsError {
		t.Fatal("guard did not reset after a non-search action")
	}
}

func TestSearchGuardIsRaceSafe(t *testing.T) {
	g := NewSearchGuard()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				g.ObserveSearch("a", ConfidenceThreshold)
				if j%7 == 0 {
					g.ObserveOther()
				}
				g.State()
			}
		}(i)
	}
	wg.Wait()
}
