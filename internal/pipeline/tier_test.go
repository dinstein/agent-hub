package pipeline_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
)

// ToolTier is the derivation the token tier gate, the intent variants and
// every allowlist UI downstream agree on, so each branch is pinned — in
// particular the two that look alike and are not: NO annotations object
// (destructive, fail-closed) versus an EMPTY one (write, docs/architecture.md §9).
func TestToolTierDerivation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		annotations string
		want        pipeline.CallerTier
	}{
		{"absent", ``, pipeline.TierDestructive},
		{"null", `null`, pipeline.TierDestructive},
		{"unparsable", `{"readOnlyHint":`, pipeline.TierDestructive},
		{"not an object", `"readOnlyHint"`, pipeline.TierDestructive},
		{"empty object", `{}`, pipeline.TierWrite},
		{"unrelated fields only", `{"title":"Read a file"}`, pipeline.TierWrite},
		{"readOnly true", `{"readOnlyHint":true}`, pipeline.TierRead},
		{"readOnly false", `{"readOnlyHint":false}`, pipeline.TierWrite},
		{"destructive true", `{"destructiveHint":true}`, pipeline.TierDestructive},
		{"destructive false", `{"destructiveHint":false}`, pipeline.TierWrite},
		{"readOnly wins over destructive", `{"readOnlyHint":true,"destructiveHint":true}`, pipeline.TierRead},
		{"readOnly false plus destructive true", `{"readOnlyHint":false,"destructiveHint":true}`, pipeline.TierDestructive},
	}
	for _, tc := range cases {
		var raw json.RawMessage
		if tc.annotations != "" {
			raw = json.RawMessage(tc.annotations)
		}
		if got := pipeline.ToolTier(raw); got != tc.want {
			t.Errorf("%s: ToolTier(%s) = %q, want %q", tc.name, tc.annotations, got, tc.want)
		}
	}
}

// TestTokenTierGateMatrix is the gate's own truth table: no tier passes
// everything, each tier reaches exactly its rung and below, and the tool's
// tier comes from its annotations (absent = destructive).
func TestTokenTierGateMatrix(t *testing.T) {
	t.Parallel()
	p := pipeline.New(pipeline.Options{})
	cases := []struct {
		name        string
		caller      pipeline.CallerTier
		annotations string
		blocked     bool
	}{
		{"no tier authority reaches an unannotated tool", "", ``, false},
		{"read reaches a read tool", pipeline.TierRead, `{"readOnlyHint":true}`, false},
		{"read cannot write", pipeline.TierRead, `{"destructiveHint":false}`, true},
		{"read cannot destroy", pipeline.TierRead, `{"destructiveHint":true}`, true},
		{"read cannot reach an unannotated tool", pipeline.TierRead, ``, true},
		{"write reaches a read tool", pipeline.TierWrite, `{"readOnlyHint":true}`, false},
		{"write reaches a write tool", pipeline.TierWrite, `{}`, false},
		{"write cannot destroy", pipeline.TierWrite, `{"destructiveHint":true}`, true},
		{"write cannot reach an unannotated tool", pipeline.TierWrite, ``, true},
		{"destructive reaches everything", pipeline.TierDestructive, ``, false},
		{"an unknown tier reaches nothing", pipeline.CallerTier("root"), `{"readOnlyHint":true}`, true},
	}
	for _, tc := range cases {
		var raw json.RawMessage
		if tc.annotations != "" {
			raw = json.RawMessage(tc.annotations)
		}
		_, err := execute(t, p, pipeline.CallRequest{
			Exposed: "srv__tool", ServerID: "srv", RawTool: "tool",
			Annotations: raw, CallerTier: tc.caller,
		})
		if tc.blocked {
			wantBlocked(t, err, pipeline.GateTokenTier, pipeline.CodeTokenTierDenied)
			continue
		}
		if err != nil {
			t.Errorf("%s: err = %v, want nil", tc.name, err)
		}
	}
}

// A blocked call must never reach the downstream: the tier gate runs before
// the call, not after it.
func TestTokenTierDenialNeverCallsDownstream(t *testing.T) {
	t.Parallel()
	called := false
	_, err := pipeline.New(pipeline.Options{}).Execute(context.Background(), pipeline.CallRequest{
		Exposed: "srv__rm", ServerID: "srv", RawTool: "rm",
		CallerTier: pipeline.TierRead,
		Call: func(context.Context) (*mcp.CallResult, error) {
			called = true
			return &mcp.CallResult{}, nil
		},
	})
	wantBlocked(t, err, pipeline.GateTokenTier, pipeline.CodeTokenTierDenied)
	if called {
		t.Fatal("the downstream was called despite a tier denial")
	}
}

// TestDefenceLinesAreDistinguishable is the docs/architecture.md §9 stack:
// scope and token tier each block, in that order, and each rejection carries
// its own gate and code so a caller can tell them apart.
func TestDefenceLinesAreDistinguishable(t *testing.T) {
	t.Parallel()
	const (
		srv  = "srv"
		tool = "rm"
	)
	destructive := json.RawMessage(`{"destructiveHint":true}`)

	// Line 1: the tool is not visible at all. Scope wins even though the
	// caller's tier would also fail — the chain short-circuits in order.
	visibleNothing := pipeline.New(pipeline.Options{
		Scope: scopeOf(scopeWith(nil)),
	})
	_, err := execute(t, visibleNothing, pipeline.CallRequest{
		Exposed: "srv__rm", ServerID: srv, RawTool: tool,
		Annotations: destructive, CallerTier: pipeline.TierRead,
	})
	wantBlocked(t, err, pipeline.GateScope, pipeline.CodeScopeDenied)

	// Line 2: visible, but the credential does not reach this tier. The HITL
	// The tier decision is made from the credential and the annotations alone.
	visible := pipeline.New(pipeline.Options{
		Scope: scopeOf(scopeWith([]string{tool})),
	})
	_, err = execute(t, visible, pipeline.CallRequest{
		Exposed: "srv__rm", ServerID: srv, RawTool: tool,
		Annotations: destructive, CallerTier: pipeline.TierRead,
	})
	wantBlocked(t, err, pipeline.GateTokenTier, pipeline.CodeTokenTierDenied)

	// Both lines open: the call runs.
	permissive := pipeline.New(pipeline.Options{
		Scope: scopeOf(scopeWith([]string{tool})),
	})
	if _, err := execute(t, permissive, pipeline.CallRequest{
		Exposed: "srv__rm", ServerID: srv, RawTool: tool,
		Annotations: destructive, CallerTier: pipeline.TierDestructive,
	}); err != nil {
		t.Fatalf("all gates open: err = %v, want nil", err)
	}
}

// The codes must stay distinct values — callers key off them.
func TestRejectionCodesAreDistinct(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, code := range []string{
		pipeline.CodeScopeDenied, pipeline.CodeTokenTierDenied, pipeline.CodeArgsInvalid,
		pipeline.CodeHITLDenied, pipeline.CodeHITLTimeout, pipeline.CodeHITLUnavailable,
		pipeline.CodeDestructiveDenied,
	} {
		if seen[code] {
			t.Fatalf("duplicate rejection code %q", code)
		}
		seen[code] = true
	}
}

// The tier-denial wording is a contract: it names the caller's tier, the
// tool and the tier that tool requires, so an agent (or an operator reading
// an audit line) can act on it without a second lookup.
func TestGoldenTokenTierDenialWording(t *testing.T) {
	t.Parallel()
	p := pipeline.New(pipeline.Options{})
	var b strings.Builder
	for _, tc := range []struct {
		caller      pipeline.CallerTier
		annotations string
	}{
		{pipeline.TierRead, `{"destructiveHint":true}`},
		{pipeline.TierRead, `{"destructiveHint":false}`},
		{pipeline.TierWrite, `{"destructiveHint":true}`},
		{pipeline.TierWrite, ``},
	} {
		var raw json.RawMessage
		if tc.annotations != "" {
			raw = json.RawMessage(tc.annotations)
		}
		_, err := execute(t, p, pipeline.CallRequest{
			Exposed: "srv__rm", ServerID: "srv", RawTool: "rm",
			Annotations: raw, CallerTier: tc.caller,
		})
		if err == nil {
			t.Fatalf("caller %q was not blocked", tc.caller)
		}
		b.WriteString(err.Error())
		b.WriteByte('\n')
	}
	assertPipelineGolden(t, "token_tier_denied.txt", []byte(b.String()))
}

// assertPipelineGolden compares got against testdata/<name>.
func assertPipelineGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with UPDATE_GOLDEN=1)", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s drifted — error wording is contract (canonical.md §6)\n--- got ---\n%s\n--- want ---\n%s",
			path, got, want)
	}
}
