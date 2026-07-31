package gateway

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// Until these lines existed, a tools/call that did not produce a result was
// answered upstream and written nowhere. A downstream error, a dead
// transport, an open circuit, exhausted retries and a gate rejection all
// travelled the same silent path, so the first question after "the tool
// failed" — which server, which tool, why — had no answer in the log file at
// all, and the only way to get one was to enable frame tracing and reproduce
// it. The one outcome that WAS recorded was cancellation, which is the one
// that is not a failure.

// callLog is the sink; callHandler is the per-WithAttrs view that writes
// into it. They are two types because the gateway binds client and pid on
// its logger at construction, so a handler that returned itself from
// WithAttrs — the shortcut the other log tests take — would drop exactly the
// bound fields, while one that kept them privately would hide the records
// from the assertions.
type callLog struct {
	mu   sync.Mutex
	recs []map[string]string
	seen []string
}

type callHandler struct {
	sink  *callLog
	attrs []slog.Attr
}

func (h *callHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *callHandler) Handle(_ context.Context, r slog.Record) error {
	fields := map[string]string{"msg": r.Message, "level": r.Level.String()}
	for _, a := range h.attrs {
		fields[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.String()
		return true
	})
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()
	h.sink.recs = append(h.sink.recs, fields)
	h.sink.seen = append(h.sink.seen, r.Message)
	return nil
}

func (h *callHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &callHandler{sink: h.sink, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h *callHandler) WithGroup(string) slog.Handler { return h }

func newCallLog() (*slog.Logger, *callLog) {
	sink := &callLog{}
	return slog.New(&callHandler{sink: sink}), sink
}

func (h *callLog) find(t *testing.T, msg string) map[string]string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r["msg"] == msg {
			return r
		}
	}
	t.Fatalf("no %q record; logged: %v", msg, h.seen)
	return nil
}

func (h *callLog) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	var n int
	for _, r := range h.recs {
		if r["msg"] == msg {
			n++
		}
	}
	return n
}

// assertCallIdentity pins what every outcome line must carry: the ROUTED
// server and tool (never the exposed name — a rename must not become a
// different tool in the log), the upstream request id (two concurrent calls
// are otherwise one interleaved sequence), and a duration.
func assertCallIdentity(t *testing.T, rec map[string]string, server, tool string) {
	t.Helper()
	if rec[logx.FieldServer] != server {
		t.Errorf("server = %q, want %q: %v", rec[logx.FieldServer], server, rec)
	}
	if rec[logx.FieldTool] != tool {
		t.Errorf("tool = %q, want %q: %v", rec[logx.FieldTool], tool, rec)
	}
	if rec["id"] == "" {
		t.Errorf("no request id, so two concurrent calls cannot be told apart: %v", rec)
	}
	if _, ok := rec["dur_ms"]; !ok {
		t.Errorf("no duration: %v", rec)
	}
}

func TestFailedToolCallIsLogged(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "alpha")
	log, sink := newCallLog()
	_, c, _ := startGateway(t, Config{
		ClientID: "calllog", Resolver: resolver, Log: log,
		Dial: scriptedDial(map[string]*fakemcp.Script{
			"alpha": fakemcp.Minimal("echo").With(fakemcp.Rule{
				Method:  mcp.MethodToolsCall,
				Actions: []fakemcp.Action{{Kind: fakemcp.ActError, Error: &mcp.Error{Code: -32000, Message: "boom"}}},
			}),
		}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "alpha__echo")

	resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "alpha__echo", Arguments: []byte(`{}`)})
	if resp.Error == nil {
		t.Fatal("the downstream answered an error; the gateway must not report success")
	}

	rec := sink.find(t, "tools/call failed")
	if rec["level"] != slog.LevelWarn.String() {
		t.Errorf("a failed call logged at %s, want WARN — Info is the level a reader filters", rec["level"])
	}
	assertCallIdentity(t, rec, "alpha", "echo")
	if rec["error"] == "" {
		t.Errorf("the failure line carries no error: %v", rec)
	}
}

// A gate rejection is reported apart from a failure because it is not one:
// nothing broke, the call was refused by configuration written down before
// the client connected. internal/ratelimit's rejection line makes the same
// argument in its own comment — a governance decision that never fires and
// one that is not running must not look alike from outside — and the scope
// gate is the one whose silence made that indistinguishable.
func TestDeniedToolCallIsLoggedWithItsGateAndCode(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "s1", "s2")
	ext := externalRegistry(t, resolver)
	updateRegistry(t, ext, func(tx *registry.Tx) {
		tx.Profiles.V.Profiles["team"] = registry.Doc[registry.Profile]{V: registry.Profile{
			Servers: []string{"s1"},
		}}
		tx.Clients.V.Clients["denylog"] = registry.Doc[registry.ClientEntry]{V: registry.ClientEntry{
			Profile: "team",
		}}
	})
	log, sink := newCallLog()
	_, c, _ := startGateway(t, Config{
		ClientID: "denylog", Resolver: resolver, Log: log,
		Dial: scriptedDial(map[string]*fakemcp.Script{
			"s1": fakemcp.Minimal("echo"),
			"s2": fakemcp.Minimal("echo"),
		}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "s1__echo")

	// s2 is routable but invisible: the call still enters the pipeline and is
	// refused by the scope gate (docs/architecture.md §9 — the gate is the
	// enforcement point).
	callBlockedWithCode(t, c, "s2__echo", "E_SCOPE_DENIED")

	rec := sink.find(t, "tools/call denied")
	if rec["level"] != slog.LevelWarn.String() {
		t.Errorf("a denial logged at %s, want WARN", rec["level"])
	}
	assertCallIdentity(t, rec, "s2", "echo")
	if rec["code"] != "E_SCOPE_DENIED" {
		t.Errorf("code = %q, want E_SCOPE_DENIED: %v", rec["code"], rec)
	}
	if rec["gate"] == "" {
		t.Errorf("no gate named, so the line cannot say which decision refused: %v", rec)
	}
	if sink.count("tools/call failed") != 0 {
		t.Error("a denial must not also be reported as a failure: nothing broke")
	}
}

// The success line exists so a call can be followed end to end when a reader
// asks for it, and sits at Debug so an agent making hundreds of calls does
// not bury the failures at the level everyone reads.
func TestServedToolCallIsLoggedAtDebug(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "alpha")
	log, sink := newCallLog()
	_, c, _ := startGateway(t, Config{
		ClientID: "okcalllog", Resolver: resolver, Log: log,
		Dial: scriptedDial(map[string]*fakemcp.Script{"alpha": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "alpha__echo")
	callToolOK(t, c, "alpha__echo")

	rec := sink.find(t, "tools/call served")
	if rec["level"] != slog.LevelDebug.String() {
		t.Errorf("a served call logged at %s, want DEBUG", rec["level"])
	}
	assertCallIdentity(t, rec, "alpha", "echo")
}

// "downstream connected" had no counterpart at this layer either. A server
// that leaves the catalog because the operator deleted or edited its entry
// produced no line naming that decision, so the config change and the
// disappearance could not be connected to each other.
//
// The reason belongs here and nowhere lower: downstream.Close reports that a
// connection ended and cannot know whether the entry was removed or
// rewritten, which is exactly the distinction an operator is checking after
// an edit.
func TestClosingADownstreamNamesTheReason(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "alpha", "beta")
	log, sink := newCallLog()
	_, c, _ := startGateway(t, Config{
		ClientID: "closelog", Resolver: resolver, Log: log,
		Dial: scriptedDial(map[string]*fakemcp.Script{
			"alpha": fakemcp.Minimal("echo"),
			"beta":  fakemcp.Minimal("echo"),
		}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "alpha__echo", "beta__echo")

	ext := externalRegistry(t, resolver)
	updateRegistry(t, ext, func(tx *registry.Tx) {
		delete(tx.Servers.V.Servers, "beta")
	})
	waitForTools(t, c, "alpha__echo")

	rec := sink.find(t, "closing a downstream connection")
	if rec[logx.FieldServer] != "beta" {
		t.Errorf("server = %q, want beta: %v", rec[logx.FieldServer], rec)
	}
	if rec["reason"] != "removed from the configuration" {
		t.Errorf("reason = %q, want the removal: %v", rec["reason"], rec)
	}
	// The pair is the point: this line is written before Close, which blocks
	// on the owner goroutine, so its counterpart arriving proves the teardown
	// finished rather than wedged.
	waitFor(t, "the connection to report itself closed", func() bool {
		return sink.count("downstream connection closed") > 0
	})
}
