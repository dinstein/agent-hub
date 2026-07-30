package fakemcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// TestMain is the subprocess seam: when this test binary is re-executed by
// (*Script).StdioConfig with FAKEMCP_SCRIPT set, MaybeServe turns it into
// the fake server and exits before any test runs.
func TestMain(m *testing.M) {
	fakemcp.MaybeServe()
	os.Exit(m.Run())
}

var clientInfo = mcp.Implementation{Name: "agenthub-test", Version: "0"}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// connect starts the in-process driver.
func connect(t *testing.T, script *fakemcp.Script) transport.Transport {
	t.Helper()
	tr, err := fakemcp.Connect(script)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// spawn starts the subprocess driver via a real transport.SpawnStdio.
func spawn(t *testing.T, script *fakemcp.Script) transport.Transport {
	t.Helper()
	cfg, err := script.StdioConfig()
	if err != nil {
		t.Fatal(err)
	}
	tr, err := transport.SpawnStdio(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// smoke runs the shared happy path: full handshake, tools/list, one
// tools/call against the echo tool.
func smoke(t *testing.T, tr transport.Transport) {
	t.Helper()
	ctx := testCtx(t)
	res, err := transport.Handshake(ctx, tr, clientInfo)
	if err != nil {
		t.Fatalf("handshake: %v (stderr: %q)", err, tr.Stderr())
	}
	if res.Version != mcp.ProtocolVersion {
		t.Fatalf("negotiated %q, want %q", res.Version, mcp.ProtocolVersion)
	}
	if res.ServerInfo.Name != "fakemcp" {
		t.Fatalf("serverInfo %+v", res.ServerInfo)
	}

	raw, err := tr.Call(ctx, mcp.MethodToolsList, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var list mcp.ListToolsResult
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, want one tool named echo", list.Tools)
	}

	args := json.RawMessage(`{"x":1}`)
	raw, err = tr.Call(ctx, mcp.MethodToolsCall, mcp.CallToolParams{Name: "echo", Arguments: args})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	var cr mcp.CallResult
	if err := json.Unmarshal(raw, &cr); err != nil {
		t.Fatal(err)
	}
	if cr.IsError {
		t.Fatalf("echo reported IsError: %s", cr.Content)
	}
	if !strings.Contains(string(cr.Content), `{\"x\":1}`) {
		t.Fatalf("echo content %s does not carry the arguments", cr.Content)
	}
}

func TestInProcessSmoke(t *testing.T) {
	smoke(t, connect(t, fakemcp.Minimal()))
}

func TestSubprocessSmoke(t *testing.T) {
	smoke(t, spawn(t, fakemcp.Minimal()))
}

// Slow response: the server sleeps far past the caller's deadline; the
// call must end with the context error, not hang.
func TestInProcessSlowResponseTimesOut(t *testing.T) {
	tr := connect(t, fakemcp.Minimal().With(fakemcp.SlowResponse(mcp.MethodPing, 30*time.Second)))
	if _, err := transport.Handshake(testCtx(t), tr, clientInfo); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := tr.Call(ctx, mcp.MethodPing, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took %v — the fake blocked the caller", time.Since(start))
	}
}

// Never responding is per-request: the hung call times out, and the
// connection still serves later requests.
func TestInProcessNeverRespond(t *testing.T) {
	tr := connect(t, fakemcp.Minimal().With(fakemcp.NeverRespond(mcp.MethodPing)))
	if _, err := transport.Handshake(testCtx(t), tr, clientInfo); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := tr.Call(ctx, mcp.MethodPing, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if _, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil); err != nil {
		t.Fatalf("server dead after hung request: %v", err)
	}
}

// Mid-handshake crash: initialize is consumed, then the stream closes.
func TestInProcessCrashOnInitialize(t *testing.T) {
	tr := connect(t, fakemcp.Minimal().With(fakemcp.CrashOn(mcp.MethodInitialize)))
	_, err := transport.Handshake(testCtx(t), tr, clientInfo)
	var te *transport.Error
	if !errors.As(err, &te) || te.Class != transport.ClassUnavailable {
		t.Fatalf("err = %v, want ClassUnavailable", err)
	}
}

// Version mismatch: the handshake must fail with the typed sentinel.
func TestInProcessVersionMismatch(t *testing.T) {
	script := fakemcp.Minimal()
	script.ProtocolVersion = "1999-01-01"
	tr := connect(t, script)
	_, err := transport.Handshake(testCtx(t), tr, clientInfo)
	if !errors.Is(err, mcp.ErrUnsupportedVersion) {
		t.Fatalf("err = %v, want ErrUnsupportedVersion", err)
	}
	var te *transport.Error
	if !errors.As(err, &te) || te.Class != transport.ClassFatal {
		t.Fatalf("err = %v, want ClassFatal (retrying the handshake cannot help)", err)
	}
}

// Oversized payload: a >16 MiB frame must hit the bounded read and poison
// the connection with the typed sentinel.
func TestInProcessHugeResponse(t *testing.T) {
	tr := connect(t, fakemcp.Minimal().With(fakemcp.HugeResponse(mcp.MethodToolsList, 0)))
	if _, err := transport.Handshake(testCtx(t), tr, clientInfo); err != nil {
		t.Fatal(err)
	}
	_, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil)
	if !errors.Is(err, mcp.ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	var te *transport.Error
	if !errors.As(err, &te) || te.Class != transport.ClassUnavailable {
		t.Fatalf("err = %v, want ClassUnavailable", err)
	}
}

// Protocol violations and broken frames, table-driven over the in-process
// driver.
func TestInProcessProtocolViolations(t *testing.T) {
	tests := []struct {
		name string
		rule fakemcp.Rule
		// wantSentinel: the poisoned-connection sentinel; nil means the
		// violation starves the call instead (context deadline).
		wantSentinel error
	}{
		{"malformed json frame", fakemcp.MalformedResponse(mcp.MethodPing), mcp.ErrMalformedFrame},
		{"half frame then close", fakemcp.HalfFrameResponse(mcp.MethodPing, 10, true), mcp.ErrMalformedFrame},
		{"wrong response id", fakemcp.WrongIDResponse(mcp.MethodPing), nil},
		{"notification instead of response", fakemcp.NotificationInsteadOfResponse(mcp.MethodPing), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := connect(t, fakemcp.Minimal().With(tt.rule))
			if _, err := transport.Handshake(testCtx(t), tr, clientInfo); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, err := tr.Call(ctx, mcp.MethodPing, nil)
			if tt.wantSentinel == nil {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("err = %v, want DeadlineExceeded (call starved)", err)
				}
				return
			}
			if !errors.Is(err, tt.wantSentinel) {
				t.Fatalf("err = %v, want sentinel %v", err, tt.wantSentinel)
			}
			var te *transport.Error
			if !errors.As(err, &te) || te.Class != transport.ClassUnavailable {
				t.Fatalf("err = %v, want ClassUnavailable", err)
			}
		})
	}
}

// list_changed storm: N notifications, each surfacing as a ChangeTools
// callback, then the triggering request is still answered.
func TestInProcessListChangedStorm(t *testing.T) {
	const n = 7
	tr := connect(t, fakemcp.Minimal().With(fakemcp.ListChangedStorm(mcp.MethodPing, n, time.Millisecond)))
	got := make(chan transport.ChangeMask, n+1)
	tr.OnListChanged(func(mask transport.ChangeMask) { got <- mask })
	if _, err := transport.Handshake(testCtx(t), tr, clientInfo); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Call(testCtx(t), mcp.MethodPing, nil); err != nil {
		t.Fatalf("ping after storm: %v", err)
	}
	for i := 0; i < n; i++ {
		select {
		case mask := <-got:
			if !mask.Has(transport.ChangeTools) {
				t.Fatalf("mask %v, want tools", mask)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d/%d storm callbacks", i, n)
		}
	}
}

// Stderr injection through the in-process driver: only the last 4 KiB of
// an 8 KiB banner is retained.
func TestInProcessStderrTailWindow(t *testing.T) {
	script := fakemcp.Minimal()
	script.StderrBanner = strings.Repeat("n", 8<<10) + "TAIL-MARKER"
	tr := connect(t, script)
	if _, err := transport.Handshake(testCtx(t), tr, clientInfo); err != nil {
		t.Fatal(err)
	}
	tail := tr.Stderr()
	if !strings.HasSuffix(tail, "TAIL-MARKER") {
		t.Fatalf("stderr tail %q lost the trailing marker", tail[max(0, len(tail)-32):])
	}
	if len(tail) > 4<<10 {
		t.Fatalf("tail is %d bytes, want <= 4 KiB", len(tail))
	}
}

// Nth-call scripting: the first ping succeeds, the second hits the fault.
func TestInProcessNthCallRule(t *testing.T) {
	rule := fakemcp.MalformedResponse(mcp.MethodPing)
	rule.Call = 2
	tr := connect(t, fakemcp.Minimal().With(rule))
	ctx := testCtx(t)
	if _, err := transport.Handshake(ctx, tr, clientInfo); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Call(ctx, mcp.MethodPing, nil); err != nil {
		t.Fatalf("first ping: %v", err)
	}
	if _, err := tr.Call(ctx, mcp.MethodPing, nil); !errors.Is(err, mcp.ErrMalformedFrame) {
		t.Fatalf("second ping err = %v, want ErrMalformedFrame", err)
	}
}

// --- subprocess fault injection (real SpawnStdio against the re-executed
// test binary) ---

func TestSubprocessCrashOnInitialize(t *testing.T) {
	tr := spawn(t, fakemcp.Minimal().With(fakemcp.CrashOn(mcp.MethodInitialize)))
	_, err := transport.Handshake(testCtx(t), tr, clientInfo)
	var te *transport.Error
	if !errors.As(err, &te) || te.Class != transport.ClassUnavailable {
		t.Fatalf("err = %v, want ClassUnavailable", err)
	}
	// Terminal: the transport stays failed.
	if _, err := tr.Call(testCtx(t), mcp.MethodPing, nil); err == nil {
		t.Fatal("call succeeded after child crash")
	}
}

func TestSubprocessHugeResponse(t *testing.T) {
	tr := spawn(t, fakemcp.Minimal().With(fakemcp.HugeResponse(mcp.MethodToolsList, 0)))
	if _, err := transport.Handshake(testCtx(t), tr, clientInfo); err != nil {
		t.Fatal(err)
	}
	_, err := tr.Call(testCtx(t), mcp.MethodToolsList, nil)
	if !errors.Is(err, mcp.ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

// Stderr injection against the real 4 KiB tail window of SpawnStdio.
func TestSubprocessStderrTailWindow(t *testing.T) {
	script := fakemcp.Minimal()
	script.StderrBanner = strings.Repeat("n", 8<<10) + "TAIL-MARKER"
	tr := spawn(t, script)
	if _, err := transport.Handshake(testCtx(t), tr, clientInfo); err != nil {
		t.Fatal(err)
	}
	// The exec stderr copier is asynchronous; poll briefly.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.HasSuffix(tr.Stderr(), "TAIL-MARKER") {
		if time.Now().After(deadline) {
			t.Fatalf("stderr tail %q never showed the marker", tr.Stderr())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(tr.Stderr()); n > 4<<10 {
		t.Fatalf("tail is %d bytes, want <= 4 KiB", n)
	}
}

// The script must survive the env-var round trip byte-exactly — it is the
// entire contract between test and subprocess.
func TestScriptJSONRoundTrip(t *testing.T) {
	script := fakemcp.Minimal("echo", "probe")
	script.ProtocolVersion = "2025-06-18"
	script.Instructions = "be fake"
	script.StderrBanner = "noise"
	script.Tools[1].Result = &mcp.CallResult{
		Content: json.RawMessage(`[{"type":"text","text":"fixed"}]`),
		IsError: true,
	}
	script.With(
		fakemcp.SlowResponse(mcp.MethodPing, 150*time.Millisecond),
		fakemcp.HugeResponse(mcp.MethodToolsList, 123),
		fakemcp.Rule{Method: mcp.MethodToolsCall, Call: 3, Actions: []fakemcp.Action{
			{Kind: fakemcp.ActError, Error: &mcp.Error{Code: 429, Message: "slow down"}},
			{Kind: fakemcp.ActStorm, Count: 2, Delay: fakemcp.Duration(time.Second), Method: mcp.NotificationPromptsListChanged},
		}},
	)
	data, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fakemcp.ParseScript(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, script) {
		t.Fatalf("round trip mismatch:\n got  %#v\n want %#v", got, script)
	}
	// Duration must serialize human-readable, not as bare nanoseconds.
	if !strings.Contains(string(data), `"150ms"`) {
		t.Fatalf("script JSON %s does not use duration strings", data)
	}
}

// A configured (non-echo) tool result and the unknown-tool error path.
func TestConfiguredToolResultAndUnknownTool(t *testing.T) {
	script := fakemcp.Minimal("echo", "fixed")
	script.Tools[1].Result = &mcp.CallResult{
		Content: json.RawMessage(`[{"type":"text","text":"canned"}]`),
	}
	tr := connect(t, script)
	ctx := testCtx(t)
	if _, err := transport.Handshake(ctx, tr, clientInfo); err != nil {
		t.Fatal(err)
	}
	raw, err := tr.Call(ctx, mcp.MethodToolsCall, mcp.CallToolParams{Name: "fixed"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "canned") {
		t.Fatalf("result %s, want the canned content", raw)
	}
	_, err = tr.Call(ctx, mcp.MethodToolsCall, mcp.CallToolParams{Name: "no-such-tool"})
	var je *mcp.Error
	if !errors.As(err, &je) || je.Code != mcp.CodeInvalidParams {
		t.Fatalf("err = %v, want JSON-RPC invalid-params for unknown tool", err)
	}
}
