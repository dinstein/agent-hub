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

// scopeWith builds an EffectiveScope where server "srv" exposes the given
// (sorted) raw tools, with the given approval switches.
func scopeWith(tools []string, ap scope.EffectiveApproval) *scope.EffectiveScope {
	return &scope.EffectiveScope{
		Servers:  map[string]scope.ToolView{"srv": {Tools: tools}},
		Approval: ap,
	}
}

func scopeOf(es *scope.EffectiveScope) func() *scope.EffectiveScope {
	return func() *scope.EffectiveScope { return es }
}

// execute runs one canned call through p and returns the outcome.
func execute(t *testing.T, p *pipeline.Pipeline, req pipeline.CallRequest) (*mcp.CallResult, error) {
	t.Helper()
	if req.Call == nil {
		req.Call = func(context.Context) (*mcp.CallResult, error) {
			return &mcp.CallResult{Content: json.RawMessage(`[{"type":"text","text":"ok"}]`)}, nil
		}
	}
	return p.Execute(context.Background(), req)
}

// wantBlocked asserts err is a BlockedError from gate with code.
func wantBlocked(t *testing.T, err error, gate, code string) {
	t.Helper()
	var be *pipeline.BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v, want *BlockedError %s/%s", err, gate, code)
	}
	if be.Gate != gate || be.Code != code {
		t.Fatalf("BlockedError = %+v, want gate %s code %s", be, gate, code)
	}
}

// readOnlyAnnotations marks a tool read-only (not destructive).
var readOnlyAnnotations = json.RawMessage(`{"readOnlyHint":true}`)

// TestScopeGateMatrix covers the scope gate: no authority allows; a hidden
// server, a hidden tool, and an empty scope all deny with E_SCOPE_DENIED;
// a visible route passes.
func TestScopeGateMatrix(t *testing.T) {
	t.Parallel()
	req := pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool",
		Annotations: readOnlyAnnotations,
	}

	// nil provider: no scope authority — allow (M0-compat assembly).
	if _, err := execute(t, pipeline.New(pipeline.Options{}), req); err != nil {
		t.Fatalf("nil provider: %v", err)
	}
	// provider returning nil: registry unavailable — allow.
	if _, err := execute(t, pipeline.New(pipeline.Options{Scope: scopeOf(nil)}), req); err != nil {
		t.Fatalf("nil scope: %v", err)
	}
	// Visible route passes.
	ok := pipeline.New(pipeline.Options{Scope: scopeOf(scopeWith([]string{"tool"}, scope.EffectiveApproval{}))})
	if _, err := execute(t, ok, req); err != nil {
		t.Fatalf("visible route: %v", err)
	}
	// Tool outside the view denies.
	hiddenTool := pipeline.New(pipeline.Options{Scope: scopeOf(scopeWith([]string{"other"}, scope.EffectiveApproval{}))})
	_, err := execute(t, hiddenTool, req)
	wantBlocked(t, err, pipeline.GateScope, pipeline.CodeScopeDenied)
	// Server invisible denies.
	hiddenSrv := pipeline.New(pipeline.Options{Scope: scopeOf(&scope.EffectiveScope{
		Servers: map[string]scope.ToolView{"elsewhere": {Tools: []string{"tool"}}},
	})})
	_, err = execute(t, hiddenSrv, req)
	wantBlocked(t, err, pipeline.GateScope, pipeline.CodeScopeDenied)
	// Empty scope (fail-closed resolution) denies.
	empty := pipeline.New(pipeline.Options{Scope: scopeOf(&scope.EffectiveScope{Servers: map[string]scope.ToolView{}})})
	_, err = execute(t, empty, req)
	wantBlocked(t, err, pipeline.GateScope, pipeline.CodeScopeDenied)
}

// testScanner builds a scanner with a single deterministic phrase rule.
func testScanner(t *testing.T) *injection.Scanner {
	t.Helper()
	s, err := injection.New(injection.Config{Rules: []injection.Rule{
		{ID: "test-evil", Phrase: "evil payload", Severity: injection.SeverityHigh},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func polOf(p injection.Policy) func() injection.Policy {
	return func() injection.Policy { return p }
}

// TestDefendAndShapeLabel: label mode injects the warning block BEFORE the
// original content and never blocks.

// TestDefendAndShapeLabel: label mode injects the warning block BEFORE the
// original content and never blocks.
func TestDefendAndShapeLabel(t *testing.T) {
	t.Parallel()
	p := pipeline.New(pipeline.Options{
		Scanner:         testScanner(t),
		InjectionPolicy: polOf(injection.Policy{Mode: injection.ModeLabel}),
	})
	req := pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool", Annotations: readOnlyAnnotations,
		Call: func(context.Context) (*mcp.CallResult, error) {
			return &mcp.CallResult{Content: json.RawMessage(`[{"type":"text","text":"the evil payload speaks"}]`)}, nil
		},
	}
	res, err := p.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatal("label mode must not turn the result into an error")
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(res.Content, &blocks); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want warning + original", len(blocks))
	}
	if !strings.Contains(blocks[0].Text, "injection guard") || !strings.Contains(blocks[0].Text, "test-evil") {
		t.Errorf("warning block = %q, want the guard label naming the rule", blocks[0].Text)
	}
	if blocks[1].Text != "the evil payload speaks" {
		t.Errorf("original content altered: %q", blocks[1].Text)
	}
	// Clean content stays untouched.
	clean := req
	clean.Call = func(context.Context) (*mcp.CallResult, error) {
		return &mcp.CallResult{Content: json.RawMessage(`[{"type":"text","text":"benign"}]`)}, nil
	}
	res, err = p.Execute(context.Background(), clean)
	if err != nil || string(res.Content) != `[{"type":"text","text":"benign"}]` {
		t.Fatalf("clean result = (%s, %v), want untouched", res.Content, err)
	}
}

// TestDefendAndShapeBlock: block mode replaces the hostile result with an
// isError result carrying the recovery trailer LAST — on the success AND
// the error branch (#421: a JSON-RPC error must not dodge the scan).
func TestDefendAndShapeBlock(t *testing.T) {
	t.Parallel()
	p := pipeline.New(pipeline.Options{
		Scanner:         testScanner(t),
		InjectionPolicy: polOf(injection.Policy{Mode: injection.ModeBlock}),
	})
	base := pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool", Annotations: readOnlyAnnotations,
	}

	checkBlocked := func(t *testing.T, res *mcp.CallResult, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("blocked outcome must be an isError result, got err %v", err)
		}
		if !res.IsError {
			t.Fatal("IsError = false, want true")
		}
		if strings.Contains(string(res.Content), "evil payload") {
			t.Fatal("hostile payload leaked into the blocked result")
		}
		var blocks []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(res.Content, &blocks); err != nil {
			t.Fatalf("decode content: %v", err)
		}
		if len(blocks) < 2 || !strings.Contains(blocks[len(blocks)-1].Text, "Recovery:") {
			t.Fatalf("recovery trailer must be the final block, got %+v", blocks)
		}
	}

	t.Run("success-branch", func(t *testing.T) {
		req := base
		req.Call = func(context.Context) (*mcp.CallResult, error) {
			return &mcp.CallResult{Content: json.RawMessage(`[{"type":"text","text":"evil payload"}]`)}, nil
		}
		res, err := p.Execute(context.Background(), req)
		checkBlocked(t, res, err)
	})
	t.Run("error-branch", func(t *testing.T) {
		req := base
		req.Call = func(context.Context) (*mcp.CallResult, error) {
			return nil, &mcp.Error{Code: mcp.CodeInternalError, Message: "evil payload in an error"}
		}
		res, err := p.Execute(context.Background(), req)
		checkBlocked(t, res, err)
	})
	t.Run("exempt-server-passes", func(t *testing.T) {
		exempt := pipeline.New(pipeline.Options{
			Scanner: testScanner(t),
			InjectionPolicy: polOf(injection.Policy{
				Mode: injection.ModeBlock, PerServerExempt: []string{"srv"},
			}),
		})
		req := base
		req.Call = func(context.Context) (*mcp.CallResult, error) {
			return &mcp.CallResult{Content: json.RawMessage(`[{"type":"text","text":"evil payload"}]`)}, nil
		}
		res, err := exempt.Execute(context.Background(), req)
		if err != nil || res.IsError {
			t.Fatalf("exempt server result = (%+v, %v), want pass-through", res, err)
		}
	})
}
