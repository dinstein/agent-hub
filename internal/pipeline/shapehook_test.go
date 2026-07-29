package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/guard/injection"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
	"github.com/dinstein/agent-hub/internal/scope"
)

// The ResultShaper seam is where internal/shaping joins the single execute
// path. Three properties are pinned here because getting any of them wrong
// is silent: the shaper must run for every allowed call, it must run AFTER
// the injection verdict (so a payload cannot hide beyond the budget), and it
// must NOT touch a withheld result (whose recovery trailer must stay the
// last, untruncated block).

// shapeRecorder is a ResultShaper that records what it was handed.
type shapeRecorder struct {
	calls int
	saw   []string
	out   *mcp.CallResult
}

func (r *shapeRecorder) fn(_ context.Context, _ *pipeline.CallRequest, res *mcp.CallResult) *mcp.CallResult {
	r.calls++
	r.saw = append(r.saw, string(res.Content))
	if r.out != nil {
		return r.out
	}
	return res
}

func TestResultShaperRunsOnEveryAllowedCall(t *testing.T) {
	t.Parallel()
	rec := &shapeRecorder{out: &mcp.CallResult{Content: json.RawMessage(`[{"type":"text","text":"page 1"}]`)}}
	p := pipeline.New(pipeline.Options{ResultShaper: rec.fn})
	res, err := p.Execute(context.Background(), pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool", Annotations: readOnlyAnnotations,
		Call: func(context.Context) (*mcp.CallResult, error) {
			return &mcp.CallResult{Content: json.RawMessage(`[{"type":"text","text":"the whole payload"}]`)}, nil
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("shaper ran %d times, want exactly 1", rec.calls)
	}
	if string(res.Content) != `[{"type":"text","text":"page 1"}]` {
		t.Errorf("delivered content = %s, want the shaped page", res.Content)
	}
}

func TestResultShaperSeesTheScannedResult(t *testing.T) {
	t.Parallel()
	// Label mode rewrites the content (warning block prepended). The shaper
	// must see THAT result: scanning first, budgeting second.
	rec := &shapeRecorder{}
	p := pipeline.New(pipeline.Options{
		Scanner:         testScanner(t),
		InjectionPolicy: polOf(injection.Policy{}), // label mode
		ResultShaper:    rec.fn,
	})
	if _, err := p.Execute(context.Background(), pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool", Annotations: readOnlyAnnotations,
		Call: func(context.Context) (*mcp.CallResult, error) {
			return &mcp.CallResult{Content: json.RawMessage(`[{"type":"text","text":"evil payload"}]`)}, nil
		},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("shaper ran %d times, want 1", rec.calls)
	}
	if !strings.Contains(rec.saw[0], "injection guard") {
		t.Errorf("shaper saw %q, want the LABELLED result (scan runs first)", rec.saw[0])
	}
}

func TestResultShaperSkipsWithheldAndErrorBranches(t *testing.T) {
	t.Parallel()
	t.Run("blocked", func(t *testing.T) {
		rec := &shapeRecorder{}
		p := pipeline.New(pipeline.Options{
			Scanner:         testScanner(t),
			InjectionPolicy: polOf(injection.Policy{Mode: injection.ModeBlock}),
			ResultShaper:    rec.fn,
		})
		res, err := p.Execute(context.Background(), pipeline.CallRequest{
			Exposed: "srv__tool", ServerID: "srv", RawTool: "tool", Annotations: readOnlyAnnotations,
			Call: func(context.Context) (*mcp.CallResult, error) {
				return &mcp.CallResult{Content: json.RawMessage(`[{"type":"text","text":"evil payload"}]`)}, nil
			},
		})
		if err != nil || !res.IsError {
			t.Fatalf("want a withheld isError result, got (%+v, %v)", res, err)
		}
		if rec.calls != 0 {
			t.Errorf("shaper ran %d times on a withheld result; its recovery trailer must never be truncated", rec.calls)
		}
	})

	t.Run("call error", func(t *testing.T) {
		rec := &shapeRecorder{}
		p := pipeline.New(pipeline.Options{ResultShaper: rec.fn})
		want := errors.New("downstream is down")
		_, err := p.Execute(context.Background(), pipeline.CallRequest{
			Exposed: "srv__tool", ServerID: "srv", RawTool: "tool", Annotations: readOnlyAnnotations,
			Call: func(context.Context) (*mcp.CallResult, error) { return nil, want },
		})
		if !errors.Is(err, want) {
			t.Fatalf("Execute error = %v, want %v", err, want)
		}
		if rec.calls != 0 {
			t.Errorf("shaper ran %d times on the error branch, want 0", rec.calls)
		}
	})

	t.Run("gate rejection", func(t *testing.T) {
		// An empty scope hides everything: the scope gate rejects before the
		// downstream call, so there is no result to shape.
		rec := &shapeRecorder{}
		p := pipeline.New(pipeline.Options{
			Scope:        func() *scope.EffectiveScope { return &scope.EffectiveScope{Servers: map[string]scope.ToolView{}} },
			ResultShaper: rec.fn,
		})
		called := false
		if _, err := p.Execute(context.Background(), pipeline.CallRequest{
			Exposed: "srv__tool", ServerID: "srv", RawTool: "tool", Annotations: readOnlyAnnotations,
			Call: func(context.Context) (*mcp.CallResult, error) {
				called = true
				return &mcp.CallResult{Content: json.RawMessage(`[]`)}, nil
			},
		}); err == nil {
			t.Fatal("Execute succeeded, want the scope gate rejection")
		}
		if called {
			t.Error("the downstream call ran despite the rejection")
		}
		if rec.calls != 0 {
			t.Errorf("shaper ran %d times on a rejected call, want 0", rec.calls)
		}
	})
}

// A nil ResultShaper is the documented no-shaping assembly: results are
// delivered whole.
func TestNilResultShaperDeliversWholeResult(t *testing.T) {
	t.Parallel()
	whole := `[{"type":"text","text":"the whole payload"}]`
	res, err := pipeline.New(pipeline.Options{}).Execute(context.Background(), pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool", Annotations: readOnlyAnnotations,
		Call: func(context.Context) (*mcp.CallResult, error) {
			return &mcp.CallResult{Content: json.RawMessage(whole)}, nil
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(res.Content) != whole {
		t.Errorf("content = %s, want it delivered whole", res.Content)
	}
}
