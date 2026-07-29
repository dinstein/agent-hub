package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/dinstein/agent-hub/internal/platform"
)

// DefaultDedupWindow is the cross-process security-event dedup window
// (docs/architecture.md §10: "10-minute time window", inherited toolport semantics).
const DefaultDedupWindow = 10 * time.Minute

// Severity levels for security events. Severity is part of the dedup key:
// the same event at a higher severity must not be swallowed by an earlier
// lower-severity emission.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// SecurityEvent is one security.jsonl line. Like the audit Record it never
// carries call arguments or payloads — Detail is a short machine-readable
// reason code / summary, not content.
type SecurityEvent struct {
	// TS is the event time (UTC).
	TS time.Time `json:"ts"`
	// Event is the event type, dot-namespaced by the emitting guard
	// (e.g. "injection.blocked", "integrity.drift", "ssrf.denied").
	Event string `json:"event"`
	// Severity is one of the Severity* constants.
	Severity string `json:"severity"`
	// Server, Tool, Client identify the subject (optional).
	Server string `json:"server,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Client string `json:"client,omitempty"`
	// Detail is a short reason code or summary. Never raw args/results.
	Detail string `json:"detail,omitempty"`
	// RequestID correlates with the audit record when applicable.
	RequestID string `json:"requestID,omitempty"`
}

// SecurityOptions configures a SecurityStream.
type SecurityOptions struct {
	// Window is the dedup window (0 = DefaultDedupWindow).
	Window time.Duration
	// DedupDir holds the per-key marker files and the lock file. Default:
	// "<dir of path>/security-dedup". It is shared by every process
	// writing the same security.jsonl.
	DedupDir string
	// Writer configures the underlying JSONL writer.
	Writer WriterOptions
}

// SecurityStream appends deduplicated SecurityEvents to security.jsonl.
//
// Cross-process dedup: a marker file per dedup key lives in DedupDir; its
// mtime is the last emission time. Check-and-refresh runs under an
// exclusive flock on DedupDir/lock, so concurrent emitters in different
// processes agree on exactly one emission per window.
//
// Failure direction: the dedup machinery FAILS OPEN — any lock or
// filesystem error results in the event being emitted (possibly
// duplicated). Deduplication is a noise reducer, never a gate: it must
// not be able to suppress a security signal.
type SecurityStream struct {
	w          *Writer
	window     time.Duration
	dedupDir   string
	clock      func() time.Time
	suppressed atomic.Uint64
}

// NewSecurityStream opens (creating if needed) the security stream at
// path and its dedup directory.
func NewSecurityStream(path string, opts SecurityOptions) (*SecurityStream, error) {
	w, err := NewWriter(path, opts.Writer)
	if err != nil {
		return nil, err
	}
	s := &SecurityStream{
		w:        w,
		window:   opts.Window,
		dedupDir: opts.DedupDir,
		clock:    w.clock,
	}
	if s.window <= 0 {
		s.window = DefaultDedupWindow
	}
	if s.dedupDir == "" {
		s.dedupDir = filepath.Join(filepath.Dir(path), "security-dedup")
	}
	if err := platform.EnsureDir(s.dedupDir); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("audit: security dedup dir: %w", err)
	}
	return s, nil
}

// Emit appends the event unless an identical one (same dedup key) was
// emitted within the window by any process. It reports whether the event
// was emitted. A zero TS is filled with the stream clock; an empty
// Severity defaults to info.
func (s *SecurityStream) Emit(ev SecurityEvent) bool {
	if ev.TS.IsZero() {
		ev.TS = s.clock()
	}
	ev.TS = ev.TS.UTC()
	if ev.Severity == "" {
		ev.Severity = SeverityInfo
	}
	if !s.shouldEmit(dedupKey(ev), ev.TS) {
		s.suppressed.Add(1)
		return false
	}
	s.w.AppendLine(marshalLine(ev))
	return true
}

// Suppressed reports how many events were swallowed by deduplication.
func (s *SecurityStream) Suppressed() uint64 { return s.suppressed.Load() }

// Dropped reports records discarded by writer backpressure.
func (s *SecurityStream) Dropped() uint64 { return s.w.Dropped() }

// Close flushes and closes the underlying writer.
func (s *SecurityStream) Close() error { return s.w.Close() }

// dedupKey hashes the identity fields into a filename-safe key. Severity
// is deliberately included (docs/architecture.md §10): the same event type at a new
// severity is a new signal.
func dedupKey(ev SecurityEvent) string {
	h := sha256.New()
	for _, part := range []string{ev.Event, ev.Severity, ev.Server, ev.Tool, ev.Client} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// shouldEmit performs the locked check-and-refresh of the marker for key.
//
// Failure direction: FAIL OPEN — every error path returns true (emit).
// Only a fresh marker observed under the lock suppresses.
func (s *SecurityStream) shouldEmit(key string, now time.Time) bool {
	lf, err := os.OpenFile(filepath.Join(s.dedupDir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return true
	}
	defer func() { _ = lf.Close() }()
	if err := flockExclusive(lf); err != nil {
		return true
	}
	defer func() { _ = flockUnlock(lf) }()

	marker := filepath.Join(s.dedupDir, key)
	if fi, err := os.Stat(marker); err == nil {
		age := now.Sub(fi.ModTime())
		// Suppress inside the window in EITHER direction.
		//
		// now is the event's own timestamp, stamped before this function
		// could take the lock, so across processes the marker is regularly
		// a few microseconds NEWER than the event being checked against it:
		// whichever emitter wins the lock is not necessarily the one
		// holding the earliest timestamp. Requiring age >= 0 therefore let
		// ordinary interleaving through — the observable symptom was a
		// burst of identical security lines whose timestamps ran backwards.
		// Out-of-order arrival is the common case here, not the exotic one.
		//
		// A marker far in the future is still the exotic case the original
		// guard was written for (clock skew, a restored backup), and it
		// still emits: bounding the tolerance at one window keeps that
		// protection while closing the microsecond hole. Failure direction
		// is unchanged — this can only ever suppress a duplicate inside the
		// window, never a distinct signal.
		if age > -s.window && age < s.window {
			return false
		}
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		return true
	}
	// Align the marker mtime with the (possibly injected) clock so the
	// window is measured on one timeline.
	_ = os.Chtimes(marker, now, now)
	s.pruneLocked(now)
	return true
}

// pruneLocked removes markers stale beyond 2x the window. Called under the
// dedup lock; security events are rare, so a directory scan per emission
// is acceptable.
func (s *SecurityStream) pruneLocked(now time.Time) {
	entries, err := os.ReadDir(s.dedupDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == "lock" {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(fi.ModTime()) > 2*s.window {
			_ = os.Remove(filepath.Join(s.dedupDir, e.Name()))
		}
	}
}
