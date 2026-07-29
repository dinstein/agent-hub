package approval

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestAnswerAsRecordsDecider: AnswerAs stamps the deciding frontend into
// the snapshot and into the ErrAlreadyDecided message a late frontend sees.
func TestAnswerAsRecordsDecider(t *testing.T) {
	b := NewMemBroker(Options{})
	ch, cancel := b.Subscribe("gui")
	defer cancel()

	done := askAsync(b, context.Background(), testRequest())
	req := recvReq(t, ch)

	if err := b.AnswerAs(req.Token, true, RememberNone, "gui"); err != nil {
		t.Fatalf("AnswerAs: %v", err)
	}
	if d := recvDecision(t, done); d != Approved {
		t.Fatalf("decision = %v", d)
	}

	err := b.AnswerAs(req.Token, false, RememberNone, "cli")
	if !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("late answer error = %v", err)
	}
	if !strings.Contains(err.Error(), "by gui") {
		t.Errorf("late answer error does not name the first decider: %v", err)
	}

	sts := b.Requests()
	if len(sts) != 1 {
		t.Fatalf("Requests len = %d", len(sts))
	}
	st := sts[0]
	if st.Decision == nil || *st.Decision != Approved || st.DecidedBy != "gui" {
		t.Errorf("status = %+v", st)
	}
	if st.DecidedAt.IsZero() {
		t.Error("DecidedAt not stamped")
	}
}

// TestRequestsOrdering: pending (by deadline) sort before decided entries,
// and pending entries carry no decision.
func TestRequestsOrdering(t *testing.T) {
	b := NewMemBroker(Options{})
	ch, cancel := b.Subscribe("cli")
	defer cancel()

	first := askAsync(b, context.Background(), testRequest())
	r1 := recvReq(t, ch)
	_ = askAsync(b, context.Background(), testRequest())
	r2 := recvReq(t, ch)

	if err := b.AnswerAs(r1.Token, false, RememberNone, "cli"); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if d := recvDecision(t, first); d != Denied {
		t.Fatalf("first decision = %v", d)
	}

	sts := b.Requests()
	if len(sts) != 2 {
		t.Fatalf("Requests len = %d", len(sts))
	}
	if sts[0].Decision != nil || sts[0].Request.Token != r2.Token {
		t.Errorf("first entry should be the pending request: %+v", sts[0])
	}
	if sts[1].Decision == nil || *sts[1].Decision != Denied {
		t.Errorf("second entry should be the denied one: %+v", sts[1])
	}
	// ArgsJSON stays visible in the snapshot (memory-only view); consumers
	// are responsible for stripping it on poll surfaces (tested in ctlapi).
	if string(sts[0].Request.ArgsJSON) == "" {
		t.Error("snapshot lost ArgsJSON")
	}
}

// TestParseDecisionFailClosed: unknown wire strings never parse into an
// executable decision.
func TestParseDecisionFailClosed(t *testing.T) {
	for s, want := range map[string]Decision{
		"approved": Approved, "denied": Denied, "timedout": Timedout,
		"unreachable": Unreachable, "stale": Stale,
	} {
		got, ok := ParseDecision(s)
		if !ok || got != want {
			t.Errorf("ParseDecision(%q) = %v ok=%v", s, got, ok)
		}
	}
	if d, ok := ParseDecision("yes"); ok || d == Approved {
		t.Errorf("ParseDecision(yes) = %v ok=%v (must fail closed)", d, ok)
	}
	if _, ok := ParseRememberScope("eternal"); ok {
		t.Error("ParseRememberScope(eternal) parsed (must fail closed)")
	}
	if sc, ok := ParseRememberScope("forever"); !ok || sc != RememberForever {
		t.Errorf("ParseRememberScope(forever) = %v ok=%v", sc, ok)
	}
}
