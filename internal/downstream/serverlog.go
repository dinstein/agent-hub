package downstream

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/dinstein/agent-hub/internal/jsonl"
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
// jsonl.Writer, so it is O_APPEND with one write(2) per bounded line and
// rename-based rotation — N gateways plus the daemon may hold the same
// server's log open at once (docs/architecture.md §10).
const (
	// serverLogPrefix is the frozen file-name prefix.
	serverLogPrefix = "server-"
	// serverLogExt is the frozen file-name extension.
	serverLogExt = ".log"
	// tracePayloadCap is the FIRST, cheap cut of a recorded payload: it caps
	// how much of a body is worth keeping at all. It matches
	// tracePayloadCapBytes deliberately — both answer the same question, and
	// two different answers would mean whichever stream you happened to read
	// told you a different story about the same frame.
	//
	// It is NOT what keeps the line writable. See traceLineBudget.
	tracePayloadCap = tracePayloadCapBytes
	// tracePayloadCapBytes bounds one recorded payload. It is the same 4 KiB
	// the retired inspect ring used, for the same reason: enough of a frame
	// to diagnose it, little enough that one hostile result cannot fill a
	// disk.
	tracePayloadCapBytes = 4096

	// traceLineBudget bounds the SERIALIZED line, which is the bound that
	// actually exists: jsonl.Writer replaces an over-long line with a marker
	// and drops the record.
	//
	// Capping the raw payload cannot honour it, and quietly did not. A
	// payload cut to tracePayloadCap goes into a JSON string, where every
	// quote and backslash doubles and every control byte becomes six
	// characters — so 4 KiB of JSON body serializes to well over 4 KiB, plus
	// the envelope. With both numbers set to 4096 the arithmetic could not
	// work out, and the frames it failed on were exactly the ones a trace is
	// opened for: a 64 KB tools/list, a large tools/call result. They were
	// replaced by markers, and `server logs` rendered those as blank rows.
	//
	// The bound cannot simply be raised, either. audit's 4096 is PIPE_BUF:
	// it is what makes a line from one of N gateway processes appending to
	// the same file atomic. Trading it for a bigger payload would trade
	// "some frames are dropped" for "frames from two processes tear into
	// each other", which is worse and much harder to recognise.
	//
	// So the payload is fitted to the serialized size instead (see append),
	// which yields the largest body that can actually be written and never
	// produces a marker. -1 leaves room for the newline jsonl.Writer adds.
	traceLineBudget = jsonl.DefaultMaxLineBytes - 1
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
	// Inst names the derived instance a frame belongs to (Spec.DeriveKey),
	// and is empty for the base connection. One server's derived instances
	// share one log file — the file is named for the server, not for a
	// connection — so without this field the frames of two instances
	// interleave into something that reads like one conversation.
	//
	// It is last and omitempty so the frozen field order above is untouched
	// and a line from a base connection is byte-identical to one written
	// before this field existed.
	Inst string `json:"inst,omitempty"`
	// PID is the process that wrote the frame. It is Inst's argument one
	// level up: a server's log file is named for the SERVER, so every
	// gateway process tracing it appends to the same file, and a user
	// normally has several running at once. Without it the frames of two
	// clients interleave into what reads like one conversation contradicting
	// itself — the same request issued twice, a response arriving before its
	// request.
	//
	// Unlike Inst it is never empty, so it carries no omitempty: a frame with
	// no pid would mean "written by no process", which is not a state that
	// exists.
	PID int `json:"pid"`
}

// ServerLog is the per-server trace sink. The zero value is not usable; a
// nil *ServerLog is, and does nothing — callers never need a nil check.
type ServerLog struct {
	serverID string
	w        *jsonl.Writer
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
	// Retention matters here for the same reason it does for the process
	// logs: a traced server can produce 32 MiB segments indefinitely, and
	// nobody goes looking for a debugging file that grew for a year.
	w, err := jsonl.NewWriter(ServerLogPath(logsDir, serverID),
		jsonl.WriterOptions{KeepSegments: jsonl.DefaultKeepSegments})
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

// out records an outbound request frame. inst is the caller's DeriveKey, or
// "" for the base connection.
func (l *ServerLog) out(inst, method string, params json.RawMessage) {
	if !l.Enabled() {
		return
	}
	l.append(TraceFrame{Dir: TraceOut, Method: method, Inst: inst}, params)
}

// in records an inbound response frame (or the failure that replaced it).
func (l *ServerLog) in(inst, method string, raw json.RawMessage, err error, dur time.Duration) {
	if !l.Enabled() {
		return
	}
	f := TraceFrame{Dir: TraceIn, Method: method, DurMs: dur.Milliseconds(), Inst: inst}
	if err != nil {
		f.Error = err.Error()
	}
	l.append(f, raw)
}

// append fills the shared fields, applies the payload cap and enqueues the
// line. It never blocks (jsonl.Writer drops on backpressure) — a trace log
// must not be able to stall a tool call.
func (l *ServerLog) append(f TraceFrame, payload json.RawMessage) {
	f.TS = time.Now().UTC()
	f.Server = l.serverID
	f.PID = os.Getpid()
	f.Bytes = len(payload)
	if len(payload) > tracePayloadCap {
		f.Payload, f.Truncated = trimValidUTF8(string(payload), tracePayloadCap), true
	} else if len(payload) > 0 {
		f.Payload = string(payload)
	}
	line, err := json.Marshal(f)
	if err != nil {
		return
	}
	// Fit the SERIALIZED line, not the raw payload (traceLineBudget). Each
	// pass cuts by the overflow measured in serialized bytes, so it converges
	// in a couple of rounds even though escaping makes the relationship
	// between the two sizes non-linear. The loop is bounded regardless:
	// every pass removes at least one byte of payload, and it stops when the
	// payload is empty rather than spinning on an envelope that cannot fit.
	for len(line) > traceLineBudget && f.Payload != "" {
		keep := len(f.Payload) - (len(line) - traceLineBudget)
		if keep >= len(f.Payload) {
			keep = len(f.Payload) - 1 // guarantee progress
		}
		if keep < 0 {
			keep = 0
		}
		f.Payload, f.Truncated = trimValidUTF8(f.Payload, keep), true
		if line, err = json.Marshal(f); err != nil {
			return
		}
	}
	l.w.AppendLine(line)
}

// trimValidUTF8 cuts s to at most n bytes without leaving a partial rune.
// A half rune would be re-encoded by json.Marshal as U+FFFD — three bytes
// where one was cut — which is the wrong direction for a function whose job
// is to make the line smaller.
func trimValidUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
