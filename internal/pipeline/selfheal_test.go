package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
)

// healEnv assembles a pipeline with no gate authority (the gates are pinned
// elsewhere) plus a downstream that rejects the FIRST argument shape and
// accepts anything else, so a retry is observable as a success.
type healEnv struct {
	mu      sync.Mutex
	seen    []string // the raw arguments of every attempt, in order
	reject  func(args json.RawMessage) bool
	events  []pipeline.SelfHealEvent
	rejects json.RawMessage // content of the rejection result
}

func (e *healEnv) call(_ context.Context, args json.RawMessage) (*mcp.CallResult, error) {
	e.mu.Lock()
	e.seen = append(e.seen, string(args))
	reject := e.reject(args)
	content := e.rejects
	e.mu.Unlock()
	if reject {
		if content == nil {
			content = json.RawMessage(`[{"type":"text","text":"invalid params: field \"count\" is not of type integer"}]`)
		}
		return &mcp.CallResult{IsError: true, Content: content}, nil
	}
	return &mcp.CallResult{Content: json.RawMessage(`[{"type":"text","text":"ok"}]`)}, nil
}

func (e *healEnv) onHeal(_ context.Context, ev pipeline.SelfHealEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func (e *healEnv) attempts() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.seen...)
}

func (e *healEnv) audit() []pipeline.SelfHealEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]pipeline.SelfHealEvent(nil), e.events...)
}

func healPipeline(e *healEnv) *pipeline.Pipeline {
	return pipeline.New(pipeline.Options{OnSelfHeal: e.onHeal})
}

func healRequest(e *healEnv, args, schema string) pipeline.CallRequest {
	return pipeline.CallRequest{
		Exposed:      "srv__tool",
		ServerID:     "srv",
		RawTool:      "tool",
		Args:         json.RawMessage(args),
		InputSchema:  json.RawMessage(schema),
		CallWithArgs: e.call,
	}
}

const countSchema = `{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`

// The headline case: "5" where the schema wants an integer. Our own
// precheck sees the mismatch, repairs it losslessly and lets the call
// through — the downstream is never bothered with the broken shape, and
// there is no wasted round trip to retry.
func TestSelfHealCoercesLosslessStringBeforeTheCall(t *testing.T) {
	t.Parallel()
	e := &healEnv{reject: func(args json.RawMessage) bool { return strings.Contains(string(args), `"5"`) }}
	res, err := healPipeline(e).Execute(context.Background(), healRequest(e, `{"count":"5"}`, countSchema))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("result still an error after a successful heal: %s", res.Content)
	}
	attempts := e.attempts()
	if len(attempts) != 1 {
		t.Fatalf("attempts = %v, want exactly 1 (repaired before the call)", attempts)
	}
	if !strings.Contains(attempts[0], `"count":5`) {
		t.Fatalf("attempt = %s, want count as an integer", attempts[0])
	}
	evs := e.audit()
	if len(evs) != 1 {
		t.Fatalf("audit events = %d, want 1", len(evs))
	}
	if evs[0].Retried || !evs[0].Recovered {
		t.Fatalf("audit event = %+v, want a pre-call repair (Retried=false, Recovered=true)", evs[0])
	}
	if len(evs[0].Fixes) != 1 || !strings.HasPrefix(evs[0].Fixes[0], "count: ") {
		t.Fatalf("audit fixes = %v, want one fix naming count", evs[0].Fixes)
	}
}

// A missing required field is filled from the SCHEMA's own default — never
// from a guess.
func TestSelfHealFillsSchemaDefault(t *testing.T) {
	t.Parallel()
	const schema = `{"type":"object","properties":{"limit":{"type":"integer","default":10}},"required":["limit"]}`
	e := &healEnv{
		reject: func(args json.RawMessage) bool { return !strings.Contains(string(args), "limit") },
		rejects: json.RawMessage(
			`[{"type":"text","text":"missing required parameter: limit"}]`),
	}
	res, err := healPipeline(e).Execute(context.Background(), healRequest(e, `{}`, schema))
	if err != nil || res.IsError {
		t.Fatalf("Execute = (%v, %v), want a recovered call", res, err)
	}
	attempts := e.attempts()
	if len(attempts) != 1 || !strings.Contains(attempts[0], `"limit":10`) {
		t.Fatalf("attempts = %v, want a single call carrying the schema default", attempts)
	}
}

// No default, no repair: a required field we cannot derive is REJECTED by
// the precheck gate. Inventing a value would be a side effect the caller
// never asked for; a stable rejection code is the honest answer.
func TestSelfHealRefusesToInventMissingValue(t *testing.T) {
	t.Parallel()
	e := &healEnv{reject: func(json.RawMessage) bool { return true }}
	_, err := healPipeline(e).Execute(context.Background(), healRequest(e, `{}`, countSchema))
	if !errors.Is(err, pipeline.ErrBlocked) {
		t.Fatalf("err = %v, want a precheck rejection", err)
	}
	var be *pipeline.BlockedError
	if !errors.As(err, &be) || be.Code != pipeline.CodeArgsInvalid {
		t.Fatalf("err = %v, want %s", err, pipeline.CodeArgsInvalid)
	}
	if n := len(e.attempts()); n != 0 {
		t.Fatalf("attempts = %d, want 0 (nothing provably fixable ⇒ never call)", n)
	}
	if n := len(e.audit()); n != 0 {
		t.Fatalf("audit events = %d, want 0 (no heal happened)", n)
	}
}

// Lossy conversions are not conversions. "5.5" → integer, "007" → 7 and
// [1] → 5 all change what the caller sent, so none may be healed; the
// precheck rejection stands.
func TestSelfHealRefusesLossyConversions(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{`{"count":"5.5"}`, `{"count":"007"}`, `{"count":"abc"}`, `{"count":[1]}`} {
		t.Run(arg, func(t *testing.T) {
			e := &healEnv{reject: func(json.RawMessage) bool { return true }}
			_, err := healPipeline(e).Execute(context.Background(), healRequest(e, arg, countSchema))
			if !errors.Is(err, pipeline.ErrBlocked) {
				t.Fatalf("err = %v for %s, want a precheck rejection", err, arg)
			}
			if n := len(e.attempts()); n != 0 {
				t.Fatalf("attempts = %d for %s, want 0 (not losslessly fixable)", n, arg)
			}
			if n := len(e.audit()); n != 0 {
				t.Fatalf("audit events = %d for %s, want 0", n, arg)
			}
		})
	}
}

// movedSchema is the schema a downstream attaches to its own rejection; it
// declares a field the CACHED schema (countSchema) never mentioned, which is
// exactly the case the precheck could not have repaired.
const rejectionSchema = `{"type":"object","properties":{"count":{"type":"integer"},` +
	`"mode":{"type":"string","default":"fast"}},"required":["count","mode"]}`

// rejectWithSchema builds the docs/modules/dataplane.md rejection shape: an isError
// result whose structuredContent carries the server's own input_schema.
func rejectWithSchema(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"error_type":   "invalid_params",
		"tool":         "tool",
		"hint":         "mode is required",
		"input_schema": json.RawMessage(rejectionSchema),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The POST-CALL half: our precheck accepted the arguments, the downstream
// rejected them and handed back its OWN schema. That schema is the only new
// evidence in the system, so the call is repaired against it and re-issued
// exactly once.
func TestSelfHealRetriesAgainstTheRejectionSchema(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []string
	var events []pipeline.SelfHealEvent
	structured := rejectWithSchema(t)

	p := pipeline.New(pipeline.Options{OnSelfHeal: func(_ context.Context, ev pipeline.SelfHealEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}})
	res, err := p.Execute(context.Background(), pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool",
		Args:        json.RawMessage(`{"count":1}`),
		InputSchema: json.RawMessage(countSchema),
		CallWithArgs: func(_ context.Context, args json.RawMessage) (*mcp.CallResult, error) {
			mu.Lock()
			seen = append(seen, string(args))
			n := len(seen)
			mu.Unlock()
			if n == 1 {
				return &mcp.CallResult{
					IsError:           true,
					Content:           json.RawMessage(`[{"type":"text","text":"invalid params: missing required parameter mode"}]`),
					StructuredContent: structured,
				}, nil
			}
			return &mcp.CallResult{Content: json.RawMessage(`[{"type":"text","text":"ok"}]`)}, nil
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("result still an error after the retry: %s", res.Content)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("attempts = %v, want exactly 2 (original + one healed retry)", seen)
	}
	if !strings.Contains(seen[1], `"mode":"fast"`) {
		t.Fatalf("retry args = %s, want the rejection schema's default filled in", seen[1])
	}
	if len(events) != 1 || !events[0].Retried || !events[0].Recovered {
		t.Fatalf("audit = %+v, want one retried+recovered event", events)
	}
}

// The retry happens AT MOST ONCE: a downstream that keeps rejecting must
// not produce a ladder, and the ORIGINAL outcome is what the agent sees
// (its remediation hint describes the call the agent actually made).
func TestSelfHealRetriesExactlyOnce(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []string
	var events []pipeline.SelfHealEvent
	structured := rejectWithSchema(t)

	p := pipeline.New(pipeline.Options{OnSelfHeal: func(_ context.Context, ev pipeline.SelfHealEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}})
	res, err := p.Execute(context.Background(), pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool",
		Args:        json.RawMessage(`{"count":1}`),
		InputSchema: json.RawMessage(countSchema),
		CallWithArgs: func(_ context.Context, args json.RawMessage) (*mcp.CallResult, error) {
			mu.Lock()
			seen = append(seen, string(args))
			mu.Unlock()
			return &mcp.CallResult{
				IsError:           true,
				Content:           json.RawMessage(`[{"type":"text","text":"invalid params: missing required parameter mode"}]`),
				StructuredContent: structured,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("result = %+v, want the original rejection", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("attempts = %v, want exactly 2 (no ladder)", seen)
	}
	if len(events) != 1 || !events[0].Retried || events[0].Recovered {
		t.Fatalf("audit = %+v, want one retried-but-not-recovered event", events)
	}
}

// docs/modules/dataplane.md deny-list: an authentication or transport failure that
// happens to mention a parameter must NEVER be reclassified as
// invalid_params — retrying a 401 only burns the agent's turn.
func TestSelfHealDenyListsAuthAndTransportSmells(t *testing.T) {
	t.Parallel()
	structured := rejectWithSchema(t)
	for _, text := range []string{
		"401 Unauthorized: invalid parameter signature",
		"403 Forbidden: missing required parameter scope",
		"request timeout while validating parameters",
		"connection refused: invalid params",
		"429 rate limit: invalid arguments",
	} {
		t.Run(text, func(t *testing.T) {
			var mu sync.Mutex
			var seen int
			body, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
			p := pipeline.New(pipeline.Options{})
			if _, err := p.Execute(context.Background(), pipeline.CallRequest{
				Exposed: "srv__tool", ServerID: "srv", RawTool: "tool",
				Args:        json.RawMessage(`{"count":1}`),
				InputSchema: json.RawMessage(countSchema),
				CallWithArgs: func(context.Context, json.RawMessage) (*mcp.CallResult, error) {
					mu.Lock()
					seen++
					mu.Unlock()
					return &mcp.CallResult{IsError: true, Content: body, StructuredContent: structured}, nil
				},
			}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if seen != 1 {
				t.Fatalf("attempts = %d for %q, want 1 (deny-listed, never healed)", seen, text)
			}
		})
	}
}

// The JSON-RPC code path: -32602 is the unambiguous signal and needs no
// text classification at all.
func TestSelfHealOnJSONRPCInvalidParamsCode(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []string
	var healed []pipeline.SelfHealEvent
	data, err := json.Marshal(map[string]any{"input_schema": json.RawMessage(rejectionSchema)})
	if err != nil {
		t.Fatal(err)
	}

	p := pipeline.New(pipeline.Options{OnSelfHeal: func(_ context.Context, ev pipeline.SelfHealEvent) {
		mu.Lock()
		healed = append(healed, ev)
		mu.Unlock()
	}})
	res, err := p.Execute(context.Background(), pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool",
		Args:        json.RawMessage(`{"count":7}`),
		InputSchema: json.RawMessage(countSchema),
		CallWithArgs: func(_ context.Context, args json.RawMessage) (*mcp.CallResult, error) {
			mu.Lock()
			seen = append(seen, string(args))
			n := len(seen)
			mu.Unlock()
			if n == 1 {
				return nil, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "bad params", Data: data}
			}
			if !strings.Contains(string(args), `"mode":"fast"`) {
				return nil, fmt.Errorf("healed args did not fill the default: %s", args)
			}
			return &mcp.CallResult{Content: json.RawMessage(`[]`)}, nil
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatalf("result = %+v, want the recovered success", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("attempts = %v, want 2", seen)
	}
	if len(healed) != 1 || !healed[0].Recovered {
		t.Fatalf("audit = %+v, want one recovered event", healed)
	}
}

// Without CallWithArgs the pipeline cannot carry repaired arguments to the
// downstream, so it must not pretend it can: the precheck rejection stands
// and nothing is healed. This is the pre-wiring behaviour of every assembly
// that still uses the plain Call closure.
func TestSelfHealInertWithoutArgSeam(t *testing.T) {
	t.Parallel()
	var calls, heals int
	p := pipeline.New(pipeline.Options{OnSelfHeal: func(context.Context, pipeline.SelfHealEvent) { heals++ }})
	_, err := p.Execute(context.Background(), pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool",
		Args:        json.RawMessage(`{"count":"5"}`),
		InputSchema: json.RawMessage(countSchema),
		Call: func(context.Context) (*mcp.CallResult, error) {
			calls++
			return nil, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "bad params"}
		},
	})
	if !errors.Is(err, pipeline.ErrBlocked) {
		t.Fatalf("err = %v, want the precheck rejection (no seam to heal through)", err)
	}
	if calls != 0 || heals != 0 {
		t.Fatalf("calls = %d, heals = %d; want 0 and 0", calls, heals)
	}
}

// defend_and_shape must run ONCE over the FINAL outcome: shaping an
// intermediate invalid_params that is about to be superseded would
// double-charge the shaping cursor and could leave a truncation banner
// pointing at a result nobody receives.
func TestSelfHealShapesFinalOutcomeOnce(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var shaped, seen int
	structured := rejectWithSchema(t)

	p := pipeline.New(pipeline.Options{
		ResultShaper: func(_ context.Context, _ *pipeline.CallRequest, res *mcp.CallResult) *mcp.CallResult {
			mu.Lock()
			shaped++
			mu.Unlock()
			return res
		},
	})
	res, err := p.Execute(context.Background(), pipeline.CallRequest{
		Exposed: "srv__tool", ServerID: "srv", RawTool: "tool",
		Args:        json.RawMessage(`{"count":1}`),
		InputSchema: json.RawMessage(countSchema),
		CallWithArgs: func(context.Context, json.RawMessage) (*mcp.CallResult, error) {
			mu.Lock()
			seen++
			n := seen
			mu.Unlock()
			if n == 1 {
				return &mcp.CallResult{
					IsError:           true,
					Content:           json.RawMessage(`[{"type":"text","text":"invalid params: missing required parameter mode"}]`),
					StructuredContent: structured,
				}, nil
			}
			return &mcp.CallResult{Content: json.RawMessage(`[{"type":"text","text":"ok"}]`)}, nil
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("Execute = (%+v, %v), want the recovered success", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != 2 {
		t.Fatalf("attempts = %d, want 2", seen)
	}
	if shaped != 1 {
		t.Fatalf("shaper ran %d times, want exactly 1 (final outcome only)", shaped)
	}
}

// A successful call is never classified, never healed — the classifier only
// ever looks at failures.
func TestSelfHealIgnoresSuccessfulResults(t *testing.T) {
	t.Parallel()
	e := &healEnv{reject: func(json.RawMessage) bool { return false }}
	if _, err := healPipeline(e).Execute(context.Background(), healRequest(e, `{"count":5}`, countSchema)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := len(e.attempts()); n != 1 {
		t.Fatalf("attempts = %d on a successful call, want 1", n)
	}
	if n := len(e.audit()); n != 0 {
		t.Fatalf("audit events = %d on a successful call, want 0", n)
	}
}
