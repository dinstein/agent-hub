package downstream_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// Spec.ID deliberately survives derivation — it is what RouteOf, the scope
// intersection and the operator's config all name — so four derivations of
// one server write four connections' worth of lines under one `server`
// value. The frame log solved this with `inst`; the record log did not have
// it, and a respawn or an opening circuit therefore could not be attributed
// to the connection it happened on.
//
// boundLog is the handler the other log tests in this package cannot be:
// theirs return themselves from WithAttrs and so drop exactly the bound
// attributes under test here.
type boundLog struct {
	mu   sync.Mutex
	recs []map[string]string
}

type boundHandler struct {
	sink  *boundLog
	attrs []slog.Attr
}

func (h *boundHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *boundHandler) Handle(_ context.Context, r slog.Record) error {
	fields := map[string]string{"msg": r.Message}
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
	return nil
}

func (h *boundHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &boundHandler{sink: h.sink, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
	return next
}

func (h *boundHandler) WithGroup(string) slog.Handler { return h }

func (l *boundLog) records(msg string) []map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]string
	for _, r := range l.recs {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

func connectForLog(t *testing.T, spec downstream.Spec) (*downstream.Server, *boundLog) {
	t.Helper()
	sink := &boundLog{}
	srv, err := downstream.Connect(context.Background(), spec, downstream.Deps{
		Log: slog.New(&boundHandler{sink: sink}),
		Dial: func(context.Context, downstream.Spec) (transport.Transport, error) {
			return fakemcp.Connect(fakemcp.Minimal("echo"))
		},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv, sink
}

func TestDerivedInstanceLogLinesNameTheInstance(t *testing.T) {
	t.Parallel()
	spec := downstream.Spec{ID: "fs", Command: "srv", DeriveKey: downstream.SessionDeriveKey("proj-a")}
	srv, sink := connectForLog(t, spec)

	// Reconnect is the cheapest line this connection can be made to write.
	if err := srv.Reconnect(context.Background()); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	recs := sink.records("respawned")
	if len(recs) == 0 {
		t.Fatal("the reconnect produced no respawned record, so nothing was verified")
	}
	for _, r := range recs {
		if r[logx.FieldServer] != "fs" {
			t.Fatalf("record names server %q, want fs: %v", r[logx.FieldServer], r)
		}
		if got := r[logx.FieldInstance]; got != string(spec.DeriveKey) {
			t.Fatalf("record names instance %q, want %q: %v", got, spec.DeriveKey, r)
		}
	}
}

// The base connection must stay byte-identical to what it logged before the
// field existed: an empty `inst` on every line of every non-derived server
// would be a column of nothing, and would read as "instance: none" rather
// than "this question does not apply".
func TestBaseConnectionLogLinesCarryNoInstance(t *testing.T) {
	t.Parallel()
	srv, sink := connectForLog(t, downstream.Spec{ID: "fs", Command: "srv"})

	if err := srv.Reconnect(context.Background()); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	recs := sink.records("respawned")
	if len(recs) == 0 {
		t.Fatal("the reconnect produced no respawned record, so nothing was verified")
	}
	for _, r := range recs {
		if _, ok := r[logx.FieldInstance]; ok {
			t.Fatalf("a base connection's record carries %q: %v", logx.FieldInstance, r)
		}
	}
}
