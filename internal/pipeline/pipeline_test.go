package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
)

// recordingGate appends its name to a shared trace on every Check.
type recordingGate struct {
	name  string
	trace *[]string
	deny  *pipeline.BlockedError // non-nil: reject every call
}

func (g *recordingGate) Name() string { return g.name }

func (g *recordingGate) Check(_ context.Context, _ *pipeline.CallRequest) error {
	*g.trace = append(*g.trace, g.name)
	if g.deny != nil {
		return g.deny
	}
	return nil
}

// recordingShaper appends its name to the trace and passes through.
type recordingShaper struct {
	name  string
	trace *[]string
}

func (s *recordingShaper) Name() string { return s.name }

func (s *recordingShaper) Shape(_ context.Context, _ *pipeline.CallRequest, res *mcp.CallResult, callErr error) (*mcp.CallResult, error) {
	*s.trace = append(*s.trace, s.name)
	return res, callErr
}

func okCall(trace *[]string) pipeline.CallFunc {
	return func(context.Context) (*mcp.CallResult, error) {
		*trace = append(*trace, "call")
		return &mcp.CallResult{Content: json.RawMessage(`[]`)}, nil
	}
}

// TestFrozenGateChainOrder pins the production chain to the docs/architecture.md §9
// order: scope → token tier → precheck → HITL.
func TestFrozenGateChainOrder(t *testing.T) {
	t.Parallel()
	got := pipeline.New(pipeline.Options{}).GateNames()
	want := []string{pipeline.GateScope, pipeline.GateTokenTier, pipeline.GatePrecheck, pipeline.GateHITL}
	if len(got) != len(want) {
		t.Fatalf("GateNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("GateNames() = %v, want %v", got, want)
		}
	}
}

// TestExecutionOrder proves gates run in chain order, before the call, with
// the shaper after the call.
func TestExecutionOrder(t *testing.T) {
	t.Parallel()
	var trace []string
	p := pipeline.NewWithGates([]pipeline.Gate{
		&recordingGate{name: "a", trace: &trace},
		&recordingGate{name: "b", trace: &trace},
		&recordingGate{name: "c", trace: &trace},
	}, &recordingShaper{name: "shape", trace: &trace})

	if _, err := p.Execute(context.Background(), pipeline.CallRequest{Exposed: "x", Call: okCall(&trace)}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := []string{"a", "b", "c", "call", "shape"}
	if fmt.Sprint(trace) != fmt.Sprint(want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

// TestM0GatesCountAndAllow proves every M0 no-op stage counts each call and
// none of them blocks.
func TestM0GatesCountAndAllow(t *testing.T) {
	t.Parallel()
	p := pipeline.New(pipeline.Options{})
	var trace []string
	for i := 0; i < 3; i++ {
		res, err := p.Execute(context.Background(), pipeline.CallRequest{
			Exposed: "srv__tool", ServerID: "srv", RawTool: "tool",
			Args: json.RawMessage(`{"x":1}`),
			Call: okCall(&trace),
		})
		if err != nil {
			t.Fatalf("Execute #%d: %v", i, err)
		}
		if res == nil {
			t.Fatalf("Execute #%d: nil result", i)
		}
	}
	counters := p.Counters()
	for _, stage := range []string{
		pipeline.GateScope, pipeline.GateTokenTier, pipeline.GatePrecheck,
		pipeline.GateHITL, pipeline.StageDefendAndShape,
	} {
		if counters[stage] != 3 {
			t.Errorf("counter[%s] = %d, want 3 (all %v)", stage, counters[stage], counters)
		}
	}
}

// TestDenyShortCircuits proves rejection semantics with a fake denying
// gate: later gates never run, the downstream call never happens, and the
// typed error propagates with its code intact.
func TestDenyShortCircuits(t *testing.T) {
	t.Parallel()
	var trace []string
	deny := pipeline.Blockedf("denygate", "E_TEST_DENY", "computer says no")
	p := pipeline.NewWithGates([]pipeline.Gate{
		&recordingGate{name: "first", trace: &trace},
		&recordingGate{name: "denygate", trace: &trace, deny: deny},
		&recordingGate{name: "after", trace: &trace},
	}, &recordingShaper{name: "shape", trace: &trace})

	res, err := p.Execute(context.Background(), pipeline.CallRequest{Exposed: "x", Call: okCall(&trace)})
	if res != nil {
		t.Fatalf("result must be nil on rejection, got %+v", res)
	}
	if !errors.Is(err, pipeline.ErrBlocked) {
		t.Fatalf("errors.Is(err, ErrBlocked) = false, err = %v", err)
	}
	var be *pipeline.BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("errors.As(*BlockedError) = false, err = %v", err)
	}
	if be.Gate != "denygate" || be.Code != "E_TEST_DENY" {
		t.Errorf("BlockedError = %+v", be)
	}
	// Short circuit: chain stops at the denying gate; neither the call nor
	// the shaper runs (nothing to shape — the call never happened).
	want := []string{"first", "denygate"}
	if fmt.Sprint(trace) != fmt.Sprint(want) {
		t.Errorf("trace = %v, want %v", trace, want)
	}
}

// TestShaperSeesErrorBranch proves defend_and_shape runs on the error
// branch too (docs/flows.md: error responses must not bypass the hook).
func TestShaperSeesErrorBranch(t *testing.T) {
	t.Parallel()
	p := pipeline.New(pipeline.Options{})
	callErr := errors.New("downstream exploded")
	res, err := p.Execute(context.Background(), pipeline.CallRequest{
		Exposed: "x",
		Call:    func(context.Context) (*mcp.CallResult, error) { return nil, callErr },
	})
	if res != nil || !errors.Is(err, callErr) {
		t.Fatalf("Execute = (%v, %v), want (nil, %v)", res, err, callErr)
	}
	if got := p.Counters()[pipeline.StageDefendAndShape]; got != 1 {
		t.Errorf("defend_and_shape counter = %d, want 1", got)
	}
}

// TestNilCallRejected pins the misuse guard.
func TestNilCallRejected(t *testing.T) {
	t.Parallel()
	if _, err := pipeline.New(pipeline.Options{}).Execute(context.Background(), pipeline.CallRequest{Exposed: "x"}); err == nil {
		t.Fatal("Execute with nil Call must fail")
	}
}
