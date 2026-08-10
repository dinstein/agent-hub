package downstream

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dinstein/agent-hub/internal/calllog"
	"github.com/dinstein/agent-hub/internal/logx"
)

// Frame recording, as it exists now that the wire trace lives in the call
// ledger rather than in a file of its own.
//
// What changed, and why it had to: the per-server log recorded the same
// frames, with no call id in any of them. So one client call that retried
// twice appeared as three exchanges belonging to nobody, and the ledger's own
// record of that call said only that it took 1.2 seconds. Neither stream
// could answer "what did this call actually do", because the join key existed
// in neither.
//
// Everything else is deliberately unchanged: the switch is still per server
// and still flips at runtime without reconnecting anything (FrameLog holds
// the atomic, exactly as ServerLog did), and recording is still off by
// default and free when off.

// Origin says which call, if any, a frame belongs to. It travels down from
// the caller rather than being read out of a context, so every call site has
// to name its own cause — a frame that belongs to nobody is a real category
// (a probe, a refresh) and must be spelled, not inferred from an absence.
type Origin struct {
	// CallID joins this frame to one client call. Empty for the traffic no
	// client asked for.
	CallID string
	// Cause is why the frame crossed the boundary. The zero value is
	// CauseProbe's opposite number: it means the caller did not say, and
	// callTransport refuses to guess.
	Cause calllog.Cause
}

// CallOrigin is the origin of a client's tools/call.
func CallOrigin(callID string) Origin {
	return Origin{CallID: callID, Cause: calllog.CauseCall}
}

// FrameSink receives frame records. *calllog.Store implements it; a nil sink
// records nothing, which is what a gateway with no ledger hands over.
//
// It is an interface so this package depends on the shape of the ledger and
// not on its assembly — the gateway owns the store's lifetime, the key and
// the capture policy, and none of that belongs in a connection.
type FrameSink interface {
	AppendFrame(e calllog.Event, body []byte, capture bool)
}

// FrameLog is one server's frame switch plus the identity every one of its
// frames carries. The zero value is not usable; a nil *FrameLog is, and
// records nothing — callers never need a nil check.
type FrameLog struct {
	// sink is settable AFTER construction, and that is not a convenience: a
	// connection captures its FrameLog once, at Connect, so a log handed out
	// before the ledger was open could never be filled in later — the exact
	// shape of the bug that made the old per-server file be opened eagerly
	// for every server, traced or not. Servers connect while the policy is
	// still being applied, and those connections must not be silent for the
	// rest of their lives.
	sinkMu   sync.RWMutex
	sink     FrameSink
	serverID string
	inst     string
	on       atomic.Bool
	// capture says whether frame BODIES are stored. It follows the evidence
	// tier: without a key there is nothing to seal them with, and metadata
	// alone is what makes the switch cheap enough to leave on.
	capture atomic.Bool
}

// NewFrameLog binds a sink to one server (and derived instance). enabled is
// the initial state of the switch.
func NewFrameLog(sink FrameSink, serverID, inst string, enabled, capture bool) *FrameLog {
	l := &FrameLog{sink: sink, serverID: serverID, inst: inst}
	l.on.Store(enabled)
	l.capture.Store(capture)
	return l
}

// SetEnabled flips the switch at runtime. Frames arriving while off are
// dropped, not buffered: a trace nobody asked for must cost nothing.
//
// This is what makes `agenthub server trace <id> on` reach a client that is
// already running — the flip is a change to the entry, not to the connection,
// so nothing reconnects.
func (l *FrameLog) SetEnabled(on bool) {
	if l == nil {
		return
	}
	l.on.Store(on)
}

// SetSink points this log at a ledger store, or at nothing. It is what lets
// a connection made before the ledger was ready start recording once it is.
func (l *FrameLog) SetSink(sink FrameSink) {
	if l == nil {
		return
	}
	l.sinkMu.Lock()
	l.sink = sink
	l.sinkMu.Unlock()
}

func (l *FrameLog) currentSink() FrameSink {
	if l == nil {
		return nil
	}
	l.sinkMu.RLock()
	defer l.sinkMu.RUnlock()
	return l.sink
}

// SetCapture flips body storage, which follows the evidence policy rather
// than the per-server switch.
func (l *FrameLog) SetCapture(on bool) {
	if l == nil {
		return
	}
	l.capture.Store(on)
}

// Enabled reports whether frames are currently recorded: the switch is on
// AND there is somewhere to record them.
func (l *FrameLog) Enabled() bool { return l.active() != nil }

// active returns the sink to record to, or nil when this log is off or has
// nowhere to write. One accessor rather than an Enabled() check followed by a
// second read: between the two, a policy change could take the sink away, and
// the caller would be holding a nil it had just been told was safe.
func (l *FrameLog) active() FrameSink {
	if l == nil || !l.on.Load() {
		return nil
	}
	return l.currentSink()
}

// sent records an outbound request frame.
func (l *FrameLog) sent(o Origin, seq int, method string, params json.RawMessage) {
	sink := l.active()
	if sink == nil {
		return
	}
	sink.AppendFrame(l.event(calllog.EventSent, o, seq, method), params, l.capture.Load())
}

// recv records an inbound response frame, or the failure that replaced one.
func (l *FrameLog) recv(o Origin, seq int, method string, raw json.RawMessage, err error, dur time.Duration) {
	sink := l.active()
	if sink == nil {
		return
	}
	e := l.event(calllog.EventRecv, o, seq, method)
	e.DurationMs = dur.Milliseconds()
	if err != nil {
		// The error can embed a downstream HTTP body snippet or a peer's
		// JSON-RPC message, which may quote a credential. Unlike the payload,
		// this metadata line is written to the plaintext frame ledger even when
		// capture is off and no evidence key exists, so scrub it before it is
		// persisted.
		e.Error = logx.ScrubString(err.Error())
		e.Outcome = "error"
	} else {
		e.Outcome = "success"
	}
	sink.AppendFrame(e, raw, l.capture.Load())
}

func (l *FrameLog) event(kind calllog.EventKind, o Origin, seq int, method string) calllog.Event {
	return calllog.Event{
		Kind: kind, CallID: o.CallID, Cause: o.Cause, Seq: seq,
		Server: l.serverID, Instance: l.inst, Method: method,
	}
}
