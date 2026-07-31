package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
)

// countingGate stands in for the frozen 7.3 chain in these tests: it counts
// and allows (or denies, when deny is set).
type countingGate struct {
	name string
	deny bool
	n    int
}

func (g *countingGate) Name() string { return g.name }

func (g *countingGate) Check(context.Context, *pipeline.CallRequest) error {
	g.n++
	if g.deny {
		return pipeline.Blockedf(g.name, "E_TEST_DENIED", "denied by test gate")
	}
	return nil
}

func okResult() *mcp.CallResult {
	return &mcp.CallResult{Content: json.RawMessage(`[{"type":"text","text":"ok"}]`)}
}

func TestGuardRunsAfterEveryGateAndBeforeTheCall(t *testing.T) {
	tl := newTestLimiter(t, Config{Rules: []Rule{{Limit: 1, Window: Duration(time.Minute)}}})
	gate := &countingGate{name: pipeline.GateTokenTier}
	p := pipeline.NewWithGates([]pipeline.Gate{gate}, nil)

	calls := 0
	exec := func() (*mcp.CallResult, error) {
		req := pipeline.CallRequest{
			Exposed: "gh__search", ServerID: "gh", RawTool: "search",
			Call: func(context.Context) (*mcp.CallResult, error) { calls++; return okResult(), nil },
		}
		tl.Guard(Key{Client: "c", Server: req.ServerID, Tool: req.RawTool}, &req)
		return p.Execute(context.Background(), req)
	}

	if _, err := exec(); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := exec()
	if err == nil {
		t.Fatal("second call must be rejected by the quota")
	}
	if calls != 1 {
		t.Fatalf("downstream call ran %d times, want 1 — the quota must stop the call itself", calls)
	}
	if gate.n != 2 {
		t.Fatalf("gate ran %d times, want 2 — quota enforcement happens AFTER the gates", gate.n)
	}
}

// A call the HITL gate denied must not spend a token: charging a human's
// "no" would let denied calls starve approved ones.
func TestDeniedCallSpendsNoToken(t *testing.T) {
	tl := newTestLimiter(t, Config{Rules: []Rule{{Limit: 1, Window: Duration(time.Minute)}}})
	deny := &countingGate{name: pipeline.GateTokenTier, deny: true}
	denyPipe := pipeline.NewWithGates([]pipeline.Gate{deny}, nil)
	allowPipe := pipeline.NewWithGates(nil, nil)

	build := func() pipeline.CallRequest {
		req := pipeline.CallRequest{
			Exposed: "gh__search", ServerID: "gh", RawTool: "search",
			Call: func(context.Context) (*mcp.CallResult, error) { return okResult(), nil },
		}
		tl.Guard(Key{Client: "c", Server: "gh", Tool: "search"}, &req)
		return req
	}
	for range 5 {
		if _, err := denyPipe.Execute(context.Background(), build()); err == nil {
			t.Fatal("gate must deny")
		}
	}
	if _, err := allowPipe.Execute(context.Background(), build()); err != nil {
		t.Fatalf("the quota was spent by gate-denied calls: %v", err)
	}
}

// The rejection must present as BOTH a pipeline rejection (for classifiers)
// and a JSON-RPC error carrying the retry hint (for the client), so the
// existing gateway error mapping needs no change.
func TestExceededErrorHasBothFaces(t *testing.T) {
	tl := newTestLimiter(t, Config{Rules: []Rule{{Server: "gh", Limit: 1, Window: Duration(time.Minute)}}})
	key := Key{Client: "c", Server: "gh", Tool: "search"}
	tl.Allow(key)
	dec := tl.Allow(key)
	if dec.Allowed {
		t.Fatal("setup: expected a denial")
	}
	err := error(newExceeded(key, dec))

	if !errors.Is(err, pipeline.ErrBlocked) {
		t.Fatal("a quota rejection must satisfy errors.Is(err, pipeline.ErrBlocked)")
	}
	var be *pipeline.BlockedError
	if !errors.As(err, &be) {
		t.Fatal("a quota rejection must unwrap to *pipeline.BlockedError")
	}
	if be.Code != CodeRateLimited || be.Gate != StageName {
		t.Fatalf("blocked error = %+v", be)
	}
	var me *mcp.Error
	if !errors.As(err, &me) {
		t.Fatal("a quota rejection must unwrap to *mcp.Error so the client gets a JSON-RPC error")
	}
	if me.Code != JSONRPCCode {
		t.Fatalf("JSON-RPC code = %d, want %d", me.Code, JSONRPCCode)
	}
	var data struct {
		Rule         string `json:"rule"`
		RetryAfterMs int64  `json:"retryAfterMs"`
	}
	if err := json.Unmarshal(me.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.RetryAfterMs <= 0 {
		t.Fatalf("retryAfterMs = %d, must be > 0", data.RetryAfterMs)
	}
	if data.Rule != "*/gh/*" {
		t.Fatalf("rule = %q", data.Rule)
	}
}

func TestGuardIsANoOpWithoutRules(t *testing.T) {
	tl := newTestLimiter(t, Config{})
	called := false
	req := pipeline.CallRequest{
		Exposed: "gh__search", ServerID: "gh", RawTool: "search",
		Call: func(context.Context) (*mcp.CallResult, error) { called = true; return okResult(), nil },
	}
	tl.Guard(Key{Server: "gh", Tool: "search"}, &req)
	if _, err := pipeline.NewWithGates(nil, nil).Execute(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("a limiter with no rules must leave the call untouched")
	}
}
