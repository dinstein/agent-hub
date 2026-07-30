package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
	"github.com/dinstein/agent-hub/internal/scope"
)

// scopeWith builds an EffectiveScope where server "srv" exposes the given
// (sorted) raw tools.
func scopeWith(tools []string) *scope.EffectiveScope {
	return &scope.EffectiveScope{
		Servers: map[string]scope.ToolView{"srv": {Tools: tools}},
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
	ok := pipeline.New(pipeline.Options{Scope: scopeOf(scopeWith([]string{"tool"}))})
	if _, err := execute(t, ok, req); err != nil {
		t.Fatalf("visible route: %v", err)
	}
	// Tool outside the view denies.
	hiddenTool := pipeline.New(pipeline.Options{Scope: scopeOf(scopeWith([]string{"other"}))})
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
