package downstream

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dinstein/agent-hub/internal/audit"
)

// Per-server trace log (canonical.md §6 "one log file per server"):
// <data>/logs/server-<name>.log, one JSON object per JSON-RPC frame
// as observed at the downstream boundary.
//
// Why here and not in internal/mcp/transport: the transport facade is
// standard-library only and owns no notion of a server identity or a data
// directory. The downstream layer is the first place both exist, and it is
// also where the frame is still whole (params in, raw result out).
//
// Multi-writer discipline is inherited, not reinvented: the file is an
// audit.Writer, so it is O_APPEND with one write(2) per bounded line and
// rename-based rotation — N gateways plus the daemon may hold the same
// server's log open at once (docs/architecture.md §10).
const (
	// serverLogPrefix is the frozen file-name prefix.
	serverLogPrefix = "server-"
	// serverLogExt is the frozen file-name extension.
	serverLogExt = ".log"
	// tracePayloadCap bounds one recorded payload. audit.Writer replaces an
	// oversized LINE with a marker, which would lose the frame entirely —
	// truncating the payload here keeps the method, direction and timing,
	// which is what a trace is for.
	tracePayloadCap = 1024
)

// TraceDir names the direction of a logged frame.
const (
	TraceOut = "out" // agenthub → downstream server
	TraceIn  = "in"  // downstream server → agenthub
)

// TraceFrame is one line of a per-server log. Field order is frozen: the
// file is meant to be greppable and diffable across releases.
type TraceFrame struct {
	TS     time.Time `json:"ts"`
	Server string    `json:"server"`
	Dir    string    `json:"dir"`
	Method string    `json:"method"`
	// Bytes is the payload size BEFORE truncation, so a truncated line still
	// tells the truth about how big the frame was.
	Bytes int `json:"bytes"`
	// Payload is the (possibly truncated) frame body. Truncated reports
	// whether it was cut.
	Payload   string `json:"payload,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// Error is the failure text for an inbound frame that never arrived.
	Error string `json:"error,omitempty"`
	// DurMs is the round-trip duration; set on inbound frames only.
	DurMs int64 `json:"durMs,omitempty"`
}

// ServerLog is the per-server trace sink. The zero value is not usable; a
// nil *ServerLog is, and does nothing — callers never need a nil check.
type ServerLog struct {
	serverID string
	w        *audit.Writer
	on       atomic.Bool
}

// ServerLogName returns the file name of a server's trace log. The id is
// sanitized because it becomes a path element: anything outside
// [A-Za-z0-9._-] is replaced, and the result can never escape the directory.
func ServerLogName(serverID string) string {
	return serverLogPrefix + sanitizeLogID(serverID) + serverLogExt
}

// ServerLogPath returns the absolute trace-log path for a server under
// logsDir. It is the ONE definition of that path: the CLI's `server logs`
// resolves through it too, so the writer and the reader cannot drift apart.
func ServerLogPath(logsDir, serverID string) string {
	return filepath.Join(logsDir, ServerLogName(serverID))
}

// sanitizeLogID maps a server id onto a safe file-name component. Empty and
// all-unsafe ids collapse to "_" rather than to an empty component (which
// would make the file name "server-.log" for every such server, silently
// merging their traces).
func sanitizeLogID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	// Neutralize the traversal token itself, not just the separator: a name
	// is safe when it contains neither, and ".." is worth removing outright
	// so the file name never reads like a path fragment.
	out := b.String()
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", "__")
	}
	out = strings.Trim(out, ".")
	if out == "" {
		return "_"
	}
	return out
}

// OpenServerLog opens (creating if needed) the trace log of serverID under
// logsDir. enabled is the initial state of the switch; a disabled log still
// holds the (empty) file open so `server logs` has something to point at and
// so flipping the switch needs no reopen.
func OpenServerLog(logsDir, serverID string, enabled bool) (*ServerLog, error) {
	if serverID == "" {
		return nil, fmt.Errorf("downstream: server log needs a server id")
	}
	w, err := audit.NewWriter(ServerLogPath(logsDir, serverID), audit.WriterOptions{})
	if err != nil {
		return nil, fmt.Errorf("downstream: open server log: %w", err)
	}
	l := &ServerLog{serverID: serverID, w: w}
	l.on.Store(enabled)
	return l, nil
}

// SetEnabled flips the trace switch at runtime (the frame
// log is "trace level … switchable"). Frames recorded while off are dropped, not
// buffered: a trace you did not ask for must cost nothing.
func (l *ServerLog) SetEnabled(on bool) {
	if l == nil {
		return
	}
	l.on.Store(on)
}

// Enabled reports whether frames are currently recorded.
func (l *ServerLog) Enabled() bool { return l != nil && l.on.Load() }

// Path returns the file this log writes to ("" for a nil log).
func (l *ServerLog) Path() string {
	if l == nil {
		return ""
	}
	return l.w.Path()
}

// Close flushes and closes the underlying writer. Safe on a nil log.
func (l *ServerLog) Close() error {
	if l == nil {
		return nil
	}
	return l.w.Close()
}

// out records an outbound request frame.
func (l *ServerLog) out(method string, params json.RawMessage) {
	if !l.Enabled() {
		return
	}
	l.append(TraceFrame{Dir: TraceOut, Method: method}, params)
}

// in records an inbound response frame (or the failure that replaced it).
func (l *ServerLog) in(method string, raw json.RawMessage, err error, dur time.Duration) {
	if !l.Enabled() {
		return
	}
	f := TraceFrame{Dir: TraceIn, Method: method, DurMs: dur.Milliseconds()}
	if err != nil {
		f.Error = err.Error()
	}
	l.append(f, raw)
}

// append fills the shared fields, applies the payload cap and enqueues the
// line. It never blocks (audit.Writer drops on backpressure) — a trace log
// must not be able to stall a tool call.
func (l *ServerLog) append(f TraceFrame, payload json.RawMessage) {
	f.TS = time.Now().UTC()
	f.Server = l.serverID
	f.Bytes = len(payload)
	if len(payload) > tracePayloadCap {
		f.Payload = string(payload[:tracePayloadCap])
		f.Truncated = true
	} else if len(payload) > 0 {
		f.Payload = string(payload)
	}
	line, err := json.Marshal(f)
	if err != nil {
		return
	}
	l.w.AppendLine(line)
}
