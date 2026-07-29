package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/guard/injection"
	"github.com/dinstein/agent-hub/internal/guard/leakguard"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
)

// The leakguard stage of defend_and_shape (docs/modules/security.md). Four properties
// are pinned here because each one is silent when broken: the three
// governance dispositions behave differently, the audit hook never carries
// content, inline redaction reaches BOTH branches, and leakguard runs before
// shaping so the shaping cache can never see an unredacted secret.

const leakToken = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"

// leakSink collects LeakEvents from the pipeline's own goroutine.
type leakSink struct {
	mu     sync.Mutex
	events []pipeline.LeakEvent
	ch     chan struct{}
}

func newLeakSink() *leakSink { return &leakSink{ch: make(chan struct{}, 8)} }

func (s *leakSink) fn(_ context.Context, ev pipeline.LeakEvent) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

// wait blocks for one event; the hook is asynchronous by design, so tests
// synchronise on it instead of assuming it already ran.
func (s *leakSink) wait(t *testing.T) pipeline.LeakEvent {
	t.Helper()
	select {
	case <-s.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the leak audit hook")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events[len(s.events)-1]
}

func (s *leakSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func leakPolicyOf(p leakguard.Policy) func() leakguard.Policy {
	return func() leakguard.Policy { return p }
}

func textResult(text string) *mcp.CallResult {
	blocks, err := json.Marshal([]any{map[string]string{"type": "text", "text": text}})
	if err != nil {
		panic(err)
	}
	return &mcp.CallResult{Content: blocks}
}

func leakRequest(res *mcp.CallResult, callErr error) pipeline.CallRequest {
	return pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool", Annotations: readOnlyAnnotations,
		Call: func(context.Context) (*mcp.CallResult, error) { return res, callErr },
	}
}

func TestLeakguardDispositions(t *testing.T) {
	t.Parallel()

	t.Run("off: no scan, no report", func(t *testing.T) {
		t.Parallel()
		sink := newLeakSink()
		p := pipeline.New(pipeline.Options{
			LeakScanner: leakguard.NewDefault(),
			LeakPolicy:  leakPolicyOf(leakguard.Policy{Mode: leakguard.ModeOff}),
			OnLeak:      sink.fn,
		})
		res, err := p.Execute(context.Background(), leakRequest(textResult("token "+leakToken), nil))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(string(res.Content), leakToken) {
			t.Fatalf("off mode rewrote the result: %s", res.Content)
		}
		// Nothing to synchronise on: assert the hook stayed silent.
		time.Sleep(50 * time.Millisecond)
		if n := sink.count(); n != 0 {
			t.Fatalf("off mode reported %d events", n)
		}
	})

	t.Run("audit: reports, delivers untouched", func(t *testing.T) {
		t.Parallel()
		sink := newLeakSink()
		p := pipeline.New(pipeline.Options{
			LeakScanner: leakguard.NewDefault(),
			// Zero Policy == audit: the default-on half of ruling #17.
			OnLeak: sink.fn,
		})
		res, err := p.Execute(context.Background(), leakRequest(textResult("token "+leakToken), nil))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(string(res.Content), leakToken) {
			t.Fatalf("audit mode rewrote the result: %s", res.Content)
		}
		ev := sink.wait(t)
		if ev.ServerID != "srv" || ev.RawTool != "tool" || ev.Exposed != "srv__tool" {
			t.Errorf("event identity = %+v", ev)
		}
		if len(ev.Records) != 1 || ev.Records[0].Rule != "github-token" {
			t.Fatalf("records = %+v", ev.Records)
		}
		if ev.Redacted != 0 {
			t.Errorf("audit mode redacted %d spans", ev.Redacted)
		}
		// The audit red line: the record carries no content.
		raw, err := json.Marshal(ev.Records)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), leakToken) || strings.Contains(string(raw), "ghp_") {
			t.Fatalf("audit record carries content: %s", raw)
		}
	})

	t.Run("inline: redacts before delivery", func(t *testing.T) {
		t.Parallel()
		sink := newLeakSink()
		p := pipeline.New(pipeline.Options{
			LeakScanner: leakguard.NewDefault(),
			LeakPolicy:  leakPolicyOf(leakguard.Policy{Mode: leakguard.ModeInline}),
			OnLeak:      sink.fn,
		})
		res, err := p.Execute(context.Background(), leakRequest(textResult("token "+leakToken), nil))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		body := string(res.Content)
		if strings.Contains(body, leakToken) {
			t.Fatalf("inline mode delivered the secret: %s", body)
		}
		if !strings.Contains(body, leakguard.Label("github-token")) {
			t.Fatalf("no redaction label in %s", body)
		}
		if !strings.Contains(body, "agenthub leak guard") {
			t.Fatalf("no notice block in %s", body)
		}
		ev := sink.wait(t)
		if ev.Redacted != 1 {
			t.Errorf("event reported %d redactions, want 1", ev.Redacted)
		}
	})
}

func TestLeakguardScansBothBranches(t *testing.T) {
	t.Parallel()
	// A hostile or careless server must not be able to smuggle a secret out
	// inside a JSON-RPC error message.
	sink := newLeakSink()
	p := pipeline.New(pipeline.Options{
		LeakScanner: leakguard.NewDefault(),
		LeakPolicy:  leakPolicyOf(leakguard.Policy{Mode: leakguard.ModeInline}),
		OnLeak:      sink.fn,
	})
	sentinel := errors.New("upstream failed while using " + leakToken)
	_, err := p.Execute(context.Background(), leakRequest(nil, sentinel))
	if err == nil {
		t.Fatal("Execute returned no error")
	}
	if strings.Contains(err.Error(), leakToken) {
		t.Fatalf("error message leaked the secret: %v", err)
	}
	if !strings.Contains(err.Error(), leakguard.Label("github-token")) {
		t.Fatalf("error message not redacted: %v", err)
	}
	// The typed downstream error must stay reachable: redaction rewrites the
	// rendering, never the error chain (code passthrough depends on it).
	if !errors.Is(err, sentinel) {
		t.Fatalf("redaction broke the error chain: %v", err)
	}
	if ev := sink.wait(t); len(ev.Records) != 1 {
		t.Fatalf("records = %+v", ev.Records)
	}
}

func TestLeakguardRedactsStructuredContent(t *testing.T) {
	t.Parallel()
	p := pipeline.New(pipeline.Options{
		LeakScanner: leakguard.NewDefault(),
		LeakPolicy:  leakPolicyOf(leakguard.Policy{Mode: leakguard.ModeInline}),
	})
	res := textResult("nothing here")
	res.StructuredContent = json.RawMessage(`{"creds":{"token":"` + leakToken + `"}}`)
	out, err := p.Execute(context.Background(), leakRequest(res, nil))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(string(out.StructuredContent), leakToken) {
		t.Fatalf("structuredContent leaked: %s", out.StructuredContent)
	}
	if !json.Valid(out.StructuredContent) {
		t.Fatalf("redaction produced invalid JSON: %s", out.StructuredContent)
	}
}

func TestLeakguardWithholdsUnrewritablePayload(t *testing.T) {
	t.Parallel()
	// Content that is not a JSON block array cannot be rewritten safely. The
	// closed direction is to withhold it: inline mode was chosen explicitly,
	// so "could not rewrite" must not degrade to "sent it anyway".
	p := pipeline.New(pipeline.Options{
		LeakScanner: leakguard.NewDefault(),
		LeakPolicy:  leakPolicyOf(leakguard.Policy{Mode: leakguard.ModeInline}),
	})
	raw := &mcp.CallResult{Content: json.RawMessage(`{"not":"an array","token":"` + leakToken + `"}`)}
	out, err := p.Execute(context.Background(), leakRequest(raw, nil))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(string(out.Content), leakToken) {
		t.Fatalf("unrewritable payload was delivered: %s", out.Content)
	}
	if !strings.Contains(string(out.Content), "withheld") {
		t.Fatalf("no withholding notice: %s", out.Content)
	}
}

func TestLeakguardLeavesUntouchedSegmentsAlone(t *testing.T) {
	t.Parallel()
	p := pipeline.New(pipeline.Options{
		LeakScanner: leakguard.NewDefault(),
		LeakPolicy:  leakPolicyOf(leakguard.Policy{Mode: leakguard.ModeInline}),
	})
	clean := `{"annotations":{"audience":["user"]},"text":"nothing to hide","type":"text"}`
	secret := `{"text":"token ` + leakToken + `","type":"text"}`
	res := &mcp.CallResult{Content: json.RawMessage(`[` + clean + `,` + secret + `]`)}
	out, err := p.Execute(context.Background(), leakRequest(res, nil))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// A block the scanner did not touch must come back byte for byte —
	// including fields (annotations) this stage knows nothing about.
	if !strings.Contains(string(out.Content), clean) {
		t.Fatalf("the untouched block was rewritten: %s", out.Content)
	}
	if strings.Contains(string(out.Content), leakToken) {
		t.Fatalf("the secret survived: %s", out.Content)
	}
}

func TestLeakguardInlineIgnoresLowConfidenceOnly(t *testing.T) {
	t.Parallel()
	// An entropy-only result is reported but never rewritten: enabling inline
	// mode must not turn a heuristic into a mutation.
	sink := newLeakSink()
	p := pipeline.New(pipeline.Options{
		LeakScanner: leakguard.NewDefault(),
		LeakPolicy:  leakPolicyOf(leakguard.Policy{Mode: leakguard.ModeInline}),
		OnLeak:      sink.fn,
	})
	opaque := "aG7fQ2mZx9Kw3PLbTs01VuYr8NdEjHi5XcOgAqWvRt4B"
	in := textResult("opaque " + opaque)
	out, err := p.Execute(context.Background(), leakRequest(in, nil))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(out.Content) != string(in.Content) {
		t.Fatalf("low-confidence finding rewrote the result: %s", out.Content)
	}
	if ev := sink.wait(t); len(ev.Records) != 1 || ev.Redacted != 0 {
		t.Fatalf("event = %+v, want one record and no redaction", ev)
	}
}

func TestLeakguardRunsBeforeShaping(t *testing.T) {
	t.Parallel()
	// docs/modules/security.md: the shaping cache must never hold an unredacted secret,
	// which is only true if the shaper sees the ALREADY redacted result.
	rec := &shapeRecorder{}
	p := pipeline.New(pipeline.Options{
		LeakScanner:  leakguard.NewDefault(),
		LeakPolicy:   leakPolicyOf(leakguard.Policy{Mode: leakguard.ModeInline}),
		ResultShaper: rec.fn,
	})
	if _, err := p.Execute(context.Background(), leakRequest(textResult("token "+leakToken), nil)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("shaper ran %d times, want 1", rec.calls)
	}
	if strings.Contains(rec.saw[0], leakToken) {
		t.Fatalf("the shaper saw the unredacted secret: %s", rec.saw[0])
	}
}

func TestLeakguardSkipsBlockedResults(t *testing.T) {
	t.Parallel()
	// A result withheld by the injection guard has no downstream payload
	// left, so the leak scan has nothing to do — and must not report on the
	// guard's own notice text.
	sink := newLeakSink()
	p := pipeline.New(pipeline.Options{
		Scanner:         testScanner(t),
		InjectionPolicy: polOf(injection.Policy{Mode: injection.ModeBlock}),
		LeakScanner:     leakguard.NewDefault(),
		OnLeak:          sink.fn,
	})
	hostile := "evil payload; also " + leakToken
	res, err := p.Execute(context.Background(), leakRequest(textResult(hostile), nil))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError || strings.Contains(string(res.Content), leakToken) {
		t.Fatalf("blocked result = %s", res.Content)
	}
	time.Sleep(50 * time.Millisecond)
	if n := sink.count(); n != 0 {
		t.Fatalf("leak hook fired %d times for a withheld result", n)
	}
}

func TestLeakguardUnwiredIsPassThrough(t *testing.T) {
	t.Parallel()
	// The M0/M1-compatible assembly: no scanner, no stage.
	p := pipeline.New(pipeline.Options{})
	res, err := p.Execute(context.Background(), leakRequest(textResult("token "+leakToken), nil))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(string(res.Content), leakToken) {
		t.Fatalf("unwired pipeline rewrote the result: %s", res.Content)
	}
}

func TestLeakguardConcurrentCalls(t *testing.T) {
	t.Parallel()
	sink := newLeakSink()
	p := pipeline.New(pipeline.Options{
		LeakScanner: leakguard.NewDefault(),
		LeakPolicy:  leakPolicyOf(leakguard.Policy{Mode: leakguard.ModeInline}),
		OnLeak:      sink.fn,
	})
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := p.Execute(context.Background(), leakRequest(textResult("token "+leakToken), nil))
			if err != nil {
				t.Errorf("Execute: %v", err)
				return
			}
			if strings.Contains(string(res.Content), leakToken) {
				t.Errorf("secret survived: %s", res.Content)
			}
		}()
	}
	wg.Wait()
	// Drain what the async hook produced; the point of the test is the race
	// detector, not the count.
	sink.wait(t)
}
