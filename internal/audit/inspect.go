package audit

import (
	"strings"
	"sync"
	"time"
)

// Inspect stream bounds (docs/architecture.md §10, inherited toolport semantics:
// "opt-in, 50-entry ring, individual bodies truncated at 4KB, cleared on
// disable").
const (
	// InspectCapacity is the ring size.
	InspectCapacity = 50
	// InspectMaxBody bounds one entry body in bytes; longer bodies are
	// truncated and flagged.
	InspectMaxBody = 4096
)

// InspectEntry is one in-memory inspect entry. Unlike the audit Record it
// DOES carry a body — which is exactly why it never touches disk and only
// exists while inspection is explicitly enabled.
type InspectEntry struct {
	// TS is the capture time (UTC).
	TS time.Time `json:"ts"`
	// Seq is a monotonically increasing sequence number, assigned on Add.
	// It survives ring eviction, so ctlapi pollers can detect gaps.
	Seq uint64 `json:"seq"`
	// Kind labels the payload direction/type (e.g. "request", "response").
	Kind string `json:"kind"`
	// Server, Tool, Session identify the call (optional).
	Server  string `json:"server,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Session string `json:"session,omitempty"`
	// Body is the captured payload, truncated at InspectMaxBody.
	Body string `json:"body"`
	// Truncated is set when Body was cut.
	Truncated bool `json:"truncated,omitempty"`
	// OrigBytes is the pre-truncation body length.
	OrigBytes int `json:"origBytes"`
}

// InspectRing is the memory-only inspect stream: an opt-in ring of at most
// InspectCapacity entries served to ctlapi via Snapshot.
//
// Failure direction: capture is OFF by default and Add is a no-op while
// disabled — payloads are never retained unless a human explicitly turned
// inspection on (fail-closed with respect to data retention). Disabling
// clears every buffered entry immediately.
type InspectRing struct {
	mu      sync.Mutex
	enabled bool
	seq     uint64
	buf     []InspectEntry // ring storage, len <= InspectCapacity
	start   int            // index of the oldest entry when len(buf) == cap
	clock   func() time.Time
}

// NewInspectRing returns a disabled, empty ring.
func NewInspectRing() *InspectRing {
	return &InspectRing{clock: time.Now}
}

// SetEnabled toggles capture. Disabling clears the buffer (the "cleared on disable"
// invariant): no payload outlives the inspection session.
func (r *InspectRing) SetEnabled(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = on
	if !on {
		r.buf = nil
		r.start = 0
	}
}

// Enabled reports whether capture is on.
func (r *InspectRing) Enabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enabled
}

// Add captures one entry and reports whether it was retained (false while
// disabled). Bodies longer than InspectMaxBody are truncated on a UTF-8
// boundary and flagged; OrigBytes always records the original length.
// A zero TS is filled with the ring clock.
func (r *InspectRing) Add(e InspectEntry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled {
		return false
	}
	if e.TS.IsZero() {
		e.TS = r.clock()
	}
	e.TS = e.TS.UTC()
	e.OrigBytes = len(e.Body)
	if len(e.Body) > InspectMaxBody {
		// Byte-cut then repair to valid UTF-8 so the entry stays JSON-safe.
		e.Body = strings.ToValidUTF8(e.Body[:InspectMaxBody], "")
		e.Truncated = true
	}
	r.seq++
	e.Seq = r.seq
	if len(r.buf) < InspectCapacity {
		r.buf = append(r.buf, e)
	} else {
		r.buf[r.start] = e
		r.start = (r.start + 1) % InspectCapacity
	}
	return true
}

// Snapshot returns the buffered entries oldest-first. The slice is a copy;
// mutating it does not affect the ring.
func (r *InspectRing) Snapshot() []InspectEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]InspectEntry, 0, len(r.buf))
	for i := 0; i < len(r.buf); i++ {
		out = append(out, r.buf[(r.start+i)%len(r.buf)])
	}
	return out
}

// Len reports the number of buffered entries.
func (r *InspectRing) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buf)
}
