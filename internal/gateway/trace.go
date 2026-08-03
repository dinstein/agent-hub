package gateway

import (
	"log/slog"
	"sync"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/registry"
)

// traceLogs owns this gateway's per-server frame switches: one
// *downstream.FrameLog per server id, handed to downstream.Connect through
// Deps.FramesFor.
//
// It exists because the two ends have different cardinality. Deps is one
// struct shared by every server and every derived instance, while a FrameLog
// is bound to the server id it was created with — so the mapping from one to
// the other has to live somewhere, and this is it.
//
// A log is created for every server the registry lists, traced or not, and
// the registry's `trace` flag is applied as its enabled state. That is what
// makes `agenthub server trace <id> on` reach a client that is already
// running: Server.trace is captured once at Connect, so a nil handed out at
// connect time could never be filled in later, whereas a disabled log can be
// enabled in place.
//
// What changed when the frames moved into the call ledger: there is no file
// here any more. The sink is the ledger store the gateway already owns, so a
// server's frames land beside the lifecycle records of the calls that caused
// them, carrying the call id that joins the two. What did NOT change is
// everything above — the per-server switch, the runtime flip, and the rule
// that every failure degrades to no tracing rather than to a failing call.
type traceLogs struct {
	log *slog.Logger

	mu   sync.Mutex
	logs map[string]*downstream.FrameLog
	// sink is the ledger store frames are written to. It changes when the
	// policy is re-applied, so existing logs are re-pointed rather than left
	// writing into a store nobody reads any more.
	sink downstream.FrameSink
	// capture says whether frame BODIES are stored. It follows the evidence
	// tier, not the per-server switch: metadata is what makes the switch
	// cheap, and payloads are what needs a key.
	capture bool
	// want is the desired enabled state per server id, from the most recent
	// snapshot. It is consulted when a log is created, so a server that
	// connects AFTER the snapshot was applied starts in the right state
	// instead of inheriting the default.
	want map[string]bool
}

// newTraceLogs builds the manager. Unlike its predecessor it cannot fail:
// there is no directory to resolve and no file to open, because the ledger
// the frames go to is opened once by the gateway itself.
func newTraceLogs(log *slog.Logger) *traceLogs {
	return &traceLogs{
		log:  log,
		logs: map[string]*downstream.FrameLog{},
		want: map[string]bool{},
	}
}

// setSink points every frame log, existing and future, at the current ledger
// store. A nil sink means frames are recorded nowhere, which is what a
// gateway whose ledger could not be opened hands over.
func (t *traceLogs) setSink(sink downstream.FrameSink, capture bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sink, t.capture = sink, capture
	for _, l := range t.logs {
		l.SetSink(sink)
		l.SetCapture(capture)
	}
}

// logFor returns the frame log of one server, creating it on first use. It is
// the Deps.FramesFor implementation, so it runs on the connect path of every
// server and every derived instance of one.
//
// Instances of one server share a log, and the derive key therefore travels
// on the FRAME rather than on the log: four instances are one conversation to
// somebody asking about that server, and four to somebody asking which
// connection a failure happened on.
func (t *traceLogs) logFor(spec downstream.Spec) *downstream.FrameLog {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if l, ok := t.logs[spec.ID]; ok {
		return l
	}
	// A log is created even when the ledger is not open yet, and the sink is
	// filled in when it is. A connection captures its log once, at Connect,
	// so handing out a nil here would silence that server for the rest of the
	// process — and servers do connect while the policy is still being
	// applied.
	l := downstream.NewFrameLog(t.sink, spec.ID, string(spec.DeriveKey), t.want[spec.ID], t.capture)
	t.logs[spec.ID] = l
	return l
}

// apply pushes a registry snapshot's `trace` flags onto the open logs. It is
// called on load and on every hot reload, which is what lets the switch take
// effect on a running client without a reconnect.
//
// A server the snapshot no longer mentions is switched OFF rather than
// dropped: the connection may still be draining, and the safe direction for a
// switch that records what a client sent is off.
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

// close drops every log. There is nothing to flush — the ledger store owns
// the file and the gateway closes that — so this only stops the switches
// pointing at a store that is about to go away.
func (t *traceLogs) close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, l := range t.logs {
		l.SetEnabled(false)
		l.SetSink(nil)
	}
	t.logs = map[string]*downstream.FrameLog{}
	t.sink = nil
}
