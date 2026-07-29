package downstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

func TestMain(m *testing.M) {
	fakemcp.MaybeServe() // subprocess driver: never returns in the child
	os.Exit(m.Run())
}

// inProcessDial returns a DialFunc serving scripts[min(n, len-1)] for the
// n-th dial (0-based) — a respawn factory whose behavior can differ between
// the first connection and reconnects. The returned counter reports how
// many dials happened.
func inProcessDial(scripts ...*fakemcp.Script) (downstream.DialFunc, *int) {
	n := new(int)
	var mu sync.Mutex
	return func(_ context.Context, _ downstream.Spec) (transport.Transport, error) {
		mu.Lock()
		i := *n
		*n++
		mu.Unlock()
		if i >= len(scripts) {
			i = len(scripts) - 1
		}
		return fakemcp.Connect(scripts[i])
	}, n
}

func startServer(t *testing.T, deps downstream.Deps, scripts ...*fakemcp.Script) *downstream.Server {
	t.Helper()
	if deps.Dial == nil {
		deps.Dial, _ = inProcessDial(scripts...)
	}
	s, err := downstream.Connect(context.Background(), downstream.Spec{ID: "fake"}, deps)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// echoText extracts the text of the first content item of an echo reply.
func echoText(t *testing.T, res *mcp.CallResult) string {
	t.Helper()
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(res.Content, &items); err != nil || len(items) == 0 {
		t.Fatalf("unexpected content %s (err %v)", res.Content, err)
	}
	return items[0].Text
}

func TestConnectAndCallInProcess(t *testing.T) {
	t.Parallel()
	s := startServer(t, downstream.Deps{}, fakemcp.Minimal("echo"))

	tools := s.Tools()
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("Tools() = %+v, want [echo]", tools)
	}
	res, err := s.Call(context.Background(), "echo", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := echoText(t, res); got != `{"x":1}` {
		t.Fatalf("echo = %q, want %q", got, `{"x":1}`)
	}
	if s.InitializeResult() == nil {
		t.Fatal("InitializeResult() = nil after successful handshake")
	}
}

// TestConnectAndCallSubprocess drives a real child process through the
// default stdio dialer: the fake server is this test binary re-executed
// with the script in FAKEMCP_SCRIPT (spec.Env overlay path).
func TestConnectAndCallSubprocess(t *testing.T) {
	t.Parallel()
	script := fakemcp.Minimal("echo")
	data, err := json.Marshal(script)
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	spec := downstream.Spec{
		ID:      "sub",
		Kind:    transport.Stdio,
		Command: exe,
		Env:     map[string]string{fakemcp.ScriptEnv: string(data)},
	}
	s, err := downstream.Connect(context.Background(), spec, downstream.Deps{ConnectTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("Connect (subprocess): %v", err)
	}
	defer s.Close()
	res, err := s.Call(context.Background(), "echo", json.RawMessage(`"sub"`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := echoText(t, res); got != `"sub"` {
		t.Fatalf("echo = %q, want %q", got, `"sub"`)
	}
}

func TestInitializeFailureEmbedsStderrTail(t *testing.T) {
	t.Parallel()
	const banner = "fakemcp-diagnostic-banner-xyz"
	script := fakemcp.Minimal("echo").With(fakemcp.CrashOn(mcp.MethodInitialize))
	script.StderrBanner = banner
	dial, _ := inProcessDial(script)
	_, err := downstream.Connect(context.Background(), downstream.Spec{ID: "boom"}, downstream.Deps{Dial: dial})
	if err == nil {
		t.Fatal("Connect succeeded, want handshake failure")
	}
	if !strings.Contains(err.Error(), banner) {
		t.Fatalf("error %q does not embed the stderr tail %q", err, banner)
	}
}

func TestBreakerOpenCooldownHalfOpenRespawn(t *testing.T) {
	t.Parallel()
	crashing := fakemcp.Minimal("echo").With(fakemcp.CrashOn(mcp.MethodToolsCall))
	healthy := fakemcp.Minimal("echo")
	dial, dials := inProcessDial(crashing, healthy)
	s := startServer(t, downstream.Deps{
		Dial:    dial,
		Breaker: downstream.BreakerConfig{FailureThreshold: 3, Cooldown: 150 * time.Millisecond},
	})

	// Three health failures: the crash, then two instant failures on the
	// dead connection.
	for i := 0; i < 3; i++ {
		if _, err := s.Call(context.Background(), "echo", nil); err == nil {
			t.Fatalf("call %d succeeded on a crashed server", i+1)
		} else if errors.Is(err, downstream.ErrCircuitOpen) {
			t.Fatalf("call %d hit the breaker before threshold: %v", i+1, err)
		}
	}
	// Breaker is now open: fail fast, no queueing, no dial.
	start := time.Now()
	if _, err := s.Call(context.Background(), "echo", nil); !errors.Is(err, downstream.ErrCircuitOpen) {
		t.Fatalf("during cooldown err = %v, want ErrCircuitOpen", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("fast-fail took %s", d)
	}
	if *dials != 1 {
		t.Fatalf("dials during open = %d, want 1", *dials)
	}

	// After cooldown: half-open probe fails on the dead conn, respawns via
	// the dial factory, and the probe call succeeds on the new connection.
	time.Sleep(250 * time.Millisecond)
	res, err := s.Call(context.Background(), "echo", json.RawMessage(`"probe"`))
	if err != nil {
		t.Fatalf("half-open probe: %v", err)
	}
	if got := echoText(t, res); got != `"probe"` {
		t.Fatalf("probe echo = %q", got)
	}
	if *dials != 2 {
		t.Fatalf("dials after respawn = %d, want 2", *dials)
	}
	// Breaker closed again: normal call works without further dialing.
	if _, err := s.Call(context.Background(), "echo", nil); err != nil {
		t.Fatalf("call after recovery: %v", err)
	}
	if *dials != 2 {
		t.Fatalf("dials after recovery = %d, want 2 (no extra respawn)", *dials)
	}
}

func TestSlowResponseContextCancelDoesNotWait(t *testing.T) {
	t.Parallel()
	script := fakemcp.Minimal("echo").
		With(fakemcp.SlowResponse(mcp.MethodToolsCall, 5*time.Second))
	s := startServer(t, downstream.Deps{}, script)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Call(ctx, "echo", nil)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("caller waited %s after cancellation", elapsed)
	}
	// The cancelled call is neutral for the breaker: no ErrCircuitOpen on
	// the next attempt.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if _, err := s.Call(ctx2, "echo", nil); errors.Is(err, downstream.ErrCircuitOpen) {
		t.Fatalf("breaker tripped by cancelled calls: %v", err)
	}
}

func TestRateLimited429IsRetried(t *testing.T) {
	t.Parallel()
	script := fakemcp.Minimal("echo").With(fakemcp.Rule{
		Method: mcp.MethodToolsCall,
		Call:   1,
		Actions: []fakemcp.Action{{
			Kind: fakemcp.ActError,
			Error: &mcp.Error{
				Code:    429,
				Message: "rate limited",
				Data:    json.RawMessage(`{"retryAfterMs":5}`),
			},
		}},
	})
	s := startServer(t, downstream.Deps{
		Retry: downstream.RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond},
	}, script)

	// First attempt gets 429; the retry (second tools/call) succeeds.
	res, err := s.Call(context.Background(), "echo", json.RawMessage(`"again"`))
	if err != nil {
		t.Fatalf("Call: %v (429 should have been retried)", err)
	}
	if got := echoText(t, res); got != `"again"` {
		t.Fatalf("echo = %q", got)
	}
}

func TestRateLimited429RetriesAreBounded(t *testing.T) {
	t.Parallel()
	// Every tools/call is rate-limited: retries must stop at MaxAttempts.
	script := fakemcp.Minimal("echo").With(fakemcp.Rule{
		Method: mcp.MethodToolsCall,
		Actions: []fakemcp.Action{{
			Kind:  fakemcp.ActError,
			Error: &mcp.Error{Code: 429, Message: "rate limited"},
		}},
	})
	s := startServer(t, downstream.Deps{
		Retry: downstream.RetryConfig{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
	}, script)

	_, err := s.Call(context.Background(), "echo", nil)
	var me *mcp.Error
	if !errors.As(err, &me) || me.Code != 429 {
		t.Fatalf("err = %v, want the final 429", err)
	}
	// The server was still answering: 429s must not trip the breaker.
	if _, err := s.Call(context.Background(), "echo", nil); errors.Is(err, downstream.ErrCircuitOpen) {
		t.Fatalf("breaker tripped by 429s: %v", err)
	}
}

func TestOrdinaryErrorResponseIsNotRetried(t *testing.T) {
	t.Parallel()
	// First tools/call answers an ordinary error; if it were (wrongly)
	// retried, the second attempt would succeed and Call would return nil.
	script := fakemcp.Minimal("echo").With(fakemcp.Rule{
		Method:  mcp.MethodToolsCall,
		Call:    1,
		Actions: []fakemcp.Action{{Kind: fakemcp.ActError}},
	})
	s := startServer(t, downstream.Deps{}, script)

	_, err := s.Call(context.Background(), "echo", nil)
	var me *mcp.Error
	if !errors.As(err, &me) || me.Code != mcp.CodeInternalError {
		t.Fatalf("err = %v, want the scripted internal error (no retry)", err)
	}
	// Second call succeeds — and the error response did not count toward
	// the breaker.
	if _, err := s.Call(context.Background(), "echo", nil); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

func newToolsListJSON(t *testing.T, names ...string) json.RawMessage {
	t.Helper()
	var defs []mcp.ToolDef
	for _, n := range names {
		defs = append(defs, mcp.ToolDef{Name: n, InputSchema: json.RawMessage(`{"type":"object"}`)})
	}
	raw, err := json.Marshal(mcp.ListToolsResult{Tools: defs})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func waitForTool(t *testing.T, s *downstream.Server, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, d := range s.Tools() {
			if d.Name == name {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("tool %q never appeared; have %+v", name, s.Tools())
}

func TestListChangedTriggersRefreshAndCallback(t *testing.T) {
	t.Parallel()
	script := fakemcp.Minimal("one").With(
		// tools/call emits one list_changed, then answers normally.
		fakemcp.Rule{Method: mcp.MethodToolsCall, Actions: []fakemcp.Action{
			{Kind: fakemcp.ActStorm, Count: 1},
			{Kind: fakemcp.ActRespond},
		}},
		// The refresh (second tools/list) serves a different tool set.
		fakemcp.Rule{Method: mcp.MethodToolsList, Call: 2, Actions: []fakemcp.Action{
			{Kind: fakemcp.ActResult, Result: newToolsListJSON(t, "two")},
		}},
	)
	s := startServer(t, downstream.Deps{}, script)

	masks := make(chan transport.ChangeMask, 64)
	s.OnListChanged(func(mask transport.ChangeMask) { masks <- mask })

	if _, err := s.Call(context.Background(), "one", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	select {
	case mask := <-masks:
		if !mask.Has(transport.ChangeTools) {
			t.Fatalf("mask = %v, want tools", mask)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnListChanged callback never fired")
	}
	waitForTool(t, s, "two") // the automatic refresh picked up the new list
}

// countingTransport counts Call invocations per method across respawns.
type countingTransport struct {
	transport.Transport
	c *methodCounter
}

type methodCounter struct {
	mu sync.Mutex
	m  map[string]int
}

func (c *methodCounter) inc(method string) {
	c.mu.Lock()
	c.m[method]++
	c.mu.Unlock()
}

func (c *methodCounter) get(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[method]
}

func (ct *countingTransport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ct.c.inc(method)
	return ct.Transport.Call(ctx, method, params)
}

func TestListChangedStormCoalescesRefreshes(t *testing.T) {
	t.Parallel()
	const stormSize = 50
	script := fakemcp.Minimal("one").With(
		fakemcp.Rule{Method: mcp.MethodToolsCall, Actions: []fakemcp.Action{
			{Kind: fakemcp.ActStorm, Count: stormSize},
			{Kind: fakemcp.ActRespond},
		}},
	)
	counts := &methodCounter{m: make(map[string]int)}
	dial := func(_ context.Context, _ downstream.Spec) (transport.Transport, error) {
		tr, err := fakemcp.Connect(script)
		if err != nil {
			return nil, err
		}
		return &countingTransport{Transport: tr, c: counts}, nil
	}
	s := startServer(t, downstream.Deps{Dial: dial})

	if _, err := s.Call(context.Background(), "one", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	// All storm notifications precede the response on the wire, so they all
	// land while the owner is blocked in the call — the refresh channel
	// (capacity 1) merges them into exactly one refresh: tools/list runs
	// once at connect and once after the storm.
	deadline := time.Now().Add(2 * time.Second)
	for counts.get(mcp.MethodToolsList) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // room for spurious extra refreshes
	if got := counts.get(mcp.MethodToolsList); got != 2 {
		t.Fatalf("tools/list count = %d, want 2 (storm of %d must coalesce)", got, stormSize)
	}
	// The server is still healthy after the storm.
	if _, err := s.Call(context.Background(), "one", nil); err != nil {
		t.Fatalf("call after storm: %v", err)
	}
}

func TestRefreshToolsKeepsConnection(t *testing.T) {
	t.Parallel()
	script := fakemcp.Minimal("one").With(
		fakemcp.Rule{Method: mcp.MethodToolsList, Call: 2, Actions: []fakemcp.Action{
			{Kind: fakemcp.ActResult, Result: newToolsListJSON(t, "two")},
		}},
	)
	dial, dials := inProcessDial(script)
	s := startServer(t, downstream.Deps{Dial: dial})

	if err := s.RefreshTools(context.Background()); err != nil {
		t.Fatalf("RefreshTools: %v", err)
	}
	if tools := s.Tools(); len(tools) != 1 || tools[0].Name != "two" {
		t.Fatalf("Tools after refresh = %+v, want [two]", tools)
	}
	if *dials != 1 {
		t.Fatalf("dials = %d, want 1 (RefreshTools must never respawn)", *dials)
	}
}

func TestCallAfterClose(t *testing.T) {
	t.Parallel()
	s := startServer(t, downstream.Deps{}, fakemcp.Minimal("echo"))
	s.Close()
	if _, err := s.Call(context.Background(), "echo", nil); !errors.Is(err, downstream.ErrServerClosed) {
		t.Fatalf("err = %v, want ErrServerClosed", err)
	}
	s.Close() // idempotent
}

func TestConcurrentCallsAreSerializedSafely(t *testing.T) {
	t.Parallel()
	s := startServer(t, downstream.Deps{}, fakemcp.Minimal("echo"))
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				arg := json.RawMessage(fmt.Sprintf(`"g%d-%d"`, g, i))
				res, err := s.Call(context.Background(), "echo", arg)
				if err != nil {
					errs <- err
					return
				}
				var items []struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(res.Content, &items); err != nil || len(items) == 0 || items[0].Text != string(arg) {
					errs <- fmt.Errorf("reply mismatch: got %s want %s", res.Content, arg)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
