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

// TestPrecheckGateMatrix covers the argument prevalidation tiers: object
// shape, required fields, shallow top-level types, and the deliberate
// fail-open on absent/unparsable schemas.
func TestPrecheckGateMatrix(t *testing.T) {
	t.Parallel()
	p := pipeline.New(pipeline.Options{})
	schema := json.RawMessage(`{
		"type": "object",
		"required": ["path"],
		"properties": {
			"path":  {"type": "string"},
			"depth": {"type": "integer"},
			"flags": {"type": ["array", "null"]}
		}
	}`)
	base := pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool",
		InputSchema: schema, Annotations: readOnlyAnnotations,
	}
	run := func(args string) error {
		req := base
		if args != "" {
			req.Args = json.RawMessage(args)
		}
		_, err := execute(t, p, req)
		return err
	}

	// Non-object arguments deny regardless of schema.
	for _, bad := range []string{`[1,2]`, `"str"`, `42`, `{broken`} {
		wantBlocked(t, run(bad), pipeline.GatePrecheck, pipeline.CodeArgsInvalid)
	}
	// Missing required field denies; empty args count as an empty object.
	wantBlocked(t, run(`{"depth":3}`), pipeline.GatePrecheck, pipeline.CodeArgsInvalid)
	wantBlocked(t, run(""), pipeline.GatePrecheck, pipeline.CodeArgsInvalid)
	// Top-level shallow type mismatches deny.
	wantBlocked(t, run(`{"path":123}`), pipeline.GatePrecheck, pipeline.CodeArgsInvalid)
	wantBlocked(t, run(`{"path":"p","depth":1.5}`), pipeline.GatePrecheck, pipeline.CodeArgsInvalid)
	wantBlocked(t, run(`{"path":"p","flags":"x"}`), pipeline.GatePrecheck, pipeline.CodeArgsInvalid)
	// Valid shapes pass, including type unions and undeclared extras.
	for _, good := range []string{
		`{"path":"p"}`,
		`{"path":"p","depth":7}`,
		`{"path":"p","flags":null}`,
		`{"path":"p","flags":["a"]}`,
		`{"path":"p","extra":"ignored"}`,
	} {
		if err := run(good); err != nil {
			t.Fatalf("args %s: %v", good, err)
		}
	}

	// Absent or unparsable schema: only the object check applies (fail-open
	// toward delivery; the downstream validates authoritatively).
	noSchema := base
	noSchema.InputSchema = nil
	if _, err := execute(t, p, noSchema); err != nil {
		t.Fatalf("absent schema: %v", err)
	}
	badSchema := base
	badSchema.InputSchema = json.RawMessage(`{not json`)
	if _, err := execute(t, p, badSchema); err != nil {
		t.Fatalf("unparsable schema: %v", err)
	}
}

// fakeAsker returns a scripted decision (or error) and records the request.
type fakeAsker struct {
	dec  pipeline.Decision
	err  error
	last pipeline.ApprovalRequest
	n    int
}

func (a *fakeAsker) Ask(_ context.Context, req pipeline.ApprovalRequest) (pipeline.Decision, error) {
	a.n++
	a.last = req
	return a.dec, a.err
}

// TestHITLGateDecisions covers the asker decision mapping and the trigger
// predicate combinations.
func TestHITLGateDecisions(t *testing.T) {
	t.Parallel()
	view := []string{"tool"}
	req := pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool",
		Args:        json.RawMessage(`{"a":1}`),
		Annotations: readOnlyAnnotations,
	}

	humanApproval := scope.EffectiveApproval{HumanApproval: true}
	cases := []struct {
		name     string
		dec      pipeline.Decision
		err      error
		wantCode string // "" = allowed
	}{
		{"approved", pipeline.DecisionApproved, nil, ""},
		{"denied", pipeline.DecisionDenied, nil, pipeline.CodeHITLDenied},
		{"timeout", pipeline.DecisionTimeout, nil, pipeline.CodeHITLTimeout},
		{"unavailable", pipeline.DecisionUnavailable, nil, pipeline.CodeHITLUnavailable},
		{"unknown-decision", pipeline.Decision("perhaps"), nil, pipeline.CodeHITLUnavailable},
		{"broker-error", pipeline.DecisionApproved, errors.New("broker down"), pipeline.CodeHITLUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asker := &fakeAsker{dec: tc.dec, err: tc.err}
			p := pipeline.New(pipeline.Options{
				Scope: scopeOf(scopeWith(view, humanApproval)),
				Asker: asker,
			})
			_, err := execute(t, p, req)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("want allow, got %v", err)
				}
			} else {
				wantBlocked(t, err, pipeline.GateHITL, tc.wantCode)
			}
			if asker.n != 1 {
				t.Fatalf("asker called %d times, want 1", asker.n)
			}
			// The approval is bound to these exact arguments.
			if asker.last.ArgsHash != pipeline.HashArgs(req.Args) {
				t.Errorf("ArgsHash = %q, want the sha256 binding", asker.last.ArgsHash)
			}
		})
	}
}

// TestHITLGateTriggers covers when the gate consults the broker at all,
// the destructive fail-closed default, DenyDestructive enforcement without
// a broker, and the nil-asker M1 baseline.
func TestHITLGateTriggers(t *testing.T) {
	t.Parallel()
	view := []string{"tool"}
	readReq := pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool",
		Annotations: readOnlyAnnotations,
	}
	bareReq := readReq
	bareReq.Annotations = nil // missing annotations ⇒ destructive (fail-closed)
	explicitSafe := readReq
	explicitSafe.Annotations = json.RawMessage(`{"destructiveHint":false}`)

	t.Run("no-approval-needed-never-asks", func(t *testing.T) {
		asker := &fakeAsker{dec: pipeline.DecisionDenied}
		p := pipeline.New(pipeline.Options{
			Scope: scopeOf(scopeWith(view, scope.EffectiveApproval{})),
			Asker: asker,
		})
		if _, err := execute(t, p, bareReq); err != nil {
			t.Fatalf("no switches set: %v", err)
		}
		if asker.n != 0 {
			t.Fatalf("asker consulted %d times without a trigger", asker.n)
		}
	})
	t.Run("missing-annotations-classify-as-destructive", func(t *testing.T) {
		asker := &fakeAsker{dec: pipeline.DecisionDenied}
		p := pipeline.New(pipeline.Options{
			Scope: scopeOf(scopeWith(view, scope.EffectiveApproval{HumanApproval: true})),
			Asker: asker,
		})
		_, err := execute(t, p, bareReq)
		wantBlocked(t, err, pipeline.GateHITL, pipeline.CodeHITLDenied)
		if !asker.last.Destructive {
			t.Error("missing annotations must classify as destructive")
		}
		// The annotation decides how a request is CLASSIFIED to the approver,
		// not whether one is asked: humanApproval gates every call, so these
		// reach the broker too — flagged non-destructive.
		for _, r := range []pipeline.CallRequest{readReq, explicitSafe} {
			asker.n = 0
			if _, err := execute(t, p, r); err == nil {
				t.Fatalf("%q: humanApproval must gate non-destructive calls too", r.RawTool)
			}
			if asker.n != 1 {
				t.Fatalf("%q: broker consulted %d times, want 1", r.RawTool, asker.n)
			}
			if asker.last.Destructive {
				t.Errorf("%q classified as destructive", r.RawTool)
			}
		}
	})
	t.Run("deny-destructive-blocks-without-broker", func(t *testing.T) {
		p := pipeline.New(pipeline.Options{
			Scope: scopeOf(scopeWith(view, scope.EffectiveApproval{DenyDestructive: true})),
		})
		_, err := execute(t, p, bareReq)
		wantBlocked(t, err, pipeline.GateHITL, pipeline.CodeDestructiveDenied)
		// A read-only tool is unaffected.
		if _, err := execute(t, p, readReq); err != nil {
			t.Fatalf("read-only under denyDestructive: %v", err)
		}
	})
	t.Run("nil-asker-fails-closed", func(t *testing.T) {
		// An assembly that requires human approval but wires no broker cannot
		// obtain one, so the call is blocked rather than auto-approved. This
		// used to pass (the M1 baseline, before any broker existed); nothing
		// in the type system requires an Asker, so the old default meant a
		// forgotten field silently disabled the whole HITL gate.
		p := pipeline.New(pipeline.Options{
			Scope: scopeOf(scopeWith(view, scope.EffectiveApproval{HumanApproval: true})),
		})
		_, err := execute(t, p, readReq)
		wantBlocked(t, err, pipeline.GateHITL, pipeline.CodeHITLUnavailable)
	})

	t.Run("nil-asker-does-not-block-what-no-scope-requires", func(t *testing.T) {
		// The flip must not turn into "no broker means nothing works": a call
		// whose scope asks for no approval never reaches the asker at all.
		p := pipeline.New(pipeline.Options{
			Scope: scopeOf(scopeWith(view, scope.EffectiveApproval{})),
		})
		if _, err := execute(t, p, readReq); err != nil {
			t.Fatalf("a call needing no approval was blocked without a broker: %v", err)
		}
	})
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

// TestDestructiveDefault pins the fail-closed annotation classifier.
func TestDestructiveDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		annotations string
		want        bool
	}{
		{"", true},                           // missing ⇒ destructive
		{`{broken`, true},                    // unparsable ⇒ destructive
		{`{}`, true},                         // no hints ⇒ destructive (spec default)
		{`{"readOnlyHint":true}`, false},     // read-only wins
		{`{"destructiveHint":false}`, false}, // explicit opt-out
		{`{"destructiveHint":true}`, true},   // explicit destructive
		{`{"readOnlyHint":false}`, true},     // not read-only, hint absent
	}
	for _, tc := range cases {
		var raw json.RawMessage
		if tc.annotations != "" {
			raw = json.RawMessage(tc.annotations)
		}
		if got := pipeline.DefaultDestructive(raw); got != tc.want {
			t.Errorf("DefaultDestructive(%q) = %v, want %v", tc.annotations, got, tc.want)
		}
	}
}
