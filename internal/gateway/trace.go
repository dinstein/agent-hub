package gateway

import (
	"log/slog"
	"sync"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
)

// traceLogs owns this gateway's per-server frame logs: one *downstream.ServerLog
// per server id, handed to downstream.Connect through Deps.TraceFor.
//
// It exists because the two ends have different cardinality. Deps is one
// struct shared by every server and every derived instance, while a
// ServerLog is bound to the server id it was opened with — so the mapping
// from one to the other has to live somewhere, and this is it.
//
// A log is opened for every server the registry lists, traced or not, and the
// registry's `trace` flag is applied as the log's enabled state. That is what
// makes `agenthub server trace <id> on` reach a client that is already
// running: Server.trace is captured once at Connect, so a nil handed out at
// connect time could never be filled in later, whereas a disabled log can be
// enabled in place (OpenServerLog: "a disabled log still holds the file open
// … so flipping the switch needs no reopen").
//
// Failure direction: every failure degrades to no tracing. A gateway that
// cannot open a trace log still serves tools — this is a debugging aid, and
// the one thing it must never do is take the data plane down with it.
type traceLogs struct {
	dir string
	log *slog.Logger

	mu   sync.Mutex
	logs map[string]*downstream.ServerLog
	// want is the desired enabled state per server id, from the most recent
	// snapshot. It is consulted when a log is opened, so a server that
	// connects AFTER the snapshot was applied starts in the right state
	// instead of inheriting the default.
	want map[string]bool
}

// newTraceLogs resolves the logs directory once. A nil return means tracing
// is unavailable for this process; every caller treats that as "no tracing".
func newTraceLogs(resolver *platform.Resolver, log *slog.Logger) *traceLogs {
	dir, err := resolver.LogsDir()
	if err == nil {
		err = platform.EnsureDir(dir)
	}
	if err != nil {
		log.Warn("frame tracing unavailable; server logs will not be recorded", "error", err)
		return nil
	}
	return &traceLogs{
		dir:  dir,
		log:  log,
		logs: map[string]*downstream.ServerLog{},
		want: map[string]bool{},
	}
}

// logFor returns the frame log of one server, opening it on first use. It is
// the Deps.TraceFor implementation, so it runs on the connect path of every
// server and every derived instance of one — instances of the same server
// share one log, which is why TraceFrame carries the DeriveKey.
func (t *traceLogs) logFor(spec downstream.Spec) *downstream.ServerLog {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if l, ok := t.logs[spec.ID]; ok {
		return l
	}
	l, err := downstream.OpenServerLog(t.dir, spec.ID, t.want[spec.ID])
	if err != nil {
		// Remember the failure as a nil entry: retrying on every reconnect
		// would turn one unwritable path into a warning per dial.
		t.log.Warn("frame trace log unavailable for this server",
			logx.Server(spec.ID), "error", err)
		t.logs[spec.ID] = nil
		return nil
	}
	t.logs[spec.ID] = l
	return l
}

// apply pushes a registry snapshot's `trace` flags onto the open logs. It is
// called on load and on every hot reload, which is what lets the switch take
// effect on a running client without a reconnect.
//
// A server the snapshot no longer mentions is switched OFF rather than
// dropped: the connection may still be draining, and the safe direction for
// a switch that writes raw payloads to disk is off.
func (t *traceLogs) apply(snap *registry.Snapshot) {
	if t == nil || snap == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.want = make(map[string]bool, len(snap.Servers.V.Servers))
	for id, doc := range snap.Servers.V.Servers {
		t.want[id] = doc.V.Trace
	}
	for id, l := range t.logs {
		l.SetEnabled(t.want[id])
	}
}

// close closes every open log. Errors are logged, not returned: shutdown
// must not fail because a debugging log could not be flushed.
func (t *traceLogs) close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, l := range t.logs {
		if err := l.Close(); err != nil {
			t.log.Warn("closing frame trace log", logx.Server(id), "error", err)
		}
	}
	t.logs = map[string]*downstream.ServerLog{}
}
