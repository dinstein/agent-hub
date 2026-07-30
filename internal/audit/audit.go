// Package audit implements the four governance streams (docs/architecture.md §6
// observability flow, docs/modules/security.md):
//
//   - audit.jsonl    — one record per tool-call decision. Never contains
//     call arguments or results: the Record type has no fields for them,
//     only a canonical-JSON SHA-256 (argsHash). This is a type-level
//     guarantee, not a runtime filter.
//   - security.jsonl — guard/integrity events with cross-process
//     deduplication over a 10-minute window (severity is part of the
//     dedup key).
//   - savings.jsonl  — token-savings estimates consumed by `agenthub
//     activity`.
//   - inspect        — an opt-in, memory-only ring of at most 50 entries
//     (bodies truncated at 4 KiB, cleared on disable) pulled by ctlapi.
//
// Multi-writer discipline (docs/architecture.md §10 "multi-writer discipline", docs/modules/security.md): N gateways
// plus the daemon append to the same JSONL files concurrently. Every file
// is opened O_APPEND, every record is exactly one write(2) of one line,
// and rotation renames the active file to a new segment — the active file
// is never read back and truncated. Within a process all appends funnel
// through a single writer goroutine behind a buffered channel; overflow is
// dropped and counted, never blocked on.
//
// Dependency budget: standard library plus the zero-dependency foundations
// internal/platform and internal/logx only.
package audit

import (
	"time"
)

// Decision is the audit outcome of one tool call. Values are frozen: they
// appear in audit.jsonl and are matched by `agenthub audit tail --held /
// --errors` filters.
type Decision string

const (
	// DecisionAllowed — the call passed every gate and was executed.
	DecisionAllowed Decision = "allowed"
	// DecisionDenied — a gate rejected the call (guard block, approval
	// denied, quarantine, ...).
	DecisionDenied Decision = "denied"
	// DecisionHeld — the call is waiting on human approval (HITL).
	DecisionHeld Decision = "held"
	// DecisionError — the call was allowed but failed downstream.
	DecisionError Decision = "error"
)

// Record is one audit.jsonl line.
//
// Invariant (docs/architecture.md §10, inherited from toolport's audit discipline):
// the record NEVER carries call arguments or results — there are no fields
// to put them in. ArgsHash binds the record to the exact arguments
// ("what was approved is what runs") without retaining them.
//
// Field order is frozen: golden tests assert the serialized byte layout,
// and `agenthub audit export --csv` derives its columns from it. All
// fields are always serialized (no omitempty) so every line has the same
// shape for line-oriented consumers.
type Record struct {
	// TS is the decision time (UTC, RFC 3339 with nanoseconds).
	TS time.Time `json:"ts"`
	// Actor identifies who initiated the call (e.g. "client", "system").
	Actor string `json:"actor"`
	// Client is the client identifier (AGENTHUB_CLIENT_ID).
	Client string `json:"client"`
	// Session is the gateway session identifier.
	Session string `json:"session"`
	// Server is the downstream MCP server name.
	Server string `json:"server"`
	// Tool is the raw per-server tool name (not the namespaced form).
	Tool string `json:"tool"`
	// ArgsHash is the SHA-256 (hex) of the canonical-JSON encoding of the
	// call arguments; see ArgsHash.
	ArgsHash string `json:"argsHash"`
	// Decision is the outcome.
	Decision Decision `json:"decision"`
	// DurMs is the wall-clock duration of the call in milliseconds
	// (0 while held).
	DurMs int64 `json:"durMs"`
	// RequestID is the X-Request-Id correlating this record with the
	// response and error body (canonical.md §4).
	RequestID string `json:"requestID"`
}

// AuditStream appends Records to audit.jsonl through a Writer.
type AuditStream struct {
	w     *Writer
	clock func() time.Time
}

// NewAuditStream opens (creating if needed) the audit stream at path.
func NewAuditStream(path string, opts WriterOptions) (*AuditStream, error) {
	w, err := NewWriter(path, opts)
	if err != nil {
		return nil, err
	}
	return &AuditStream{w: w, clock: w.clock}, nil
}

// Append enqueues one record. A zero TS is filled with the stream clock;
// TS is always normalized to UTC. Append never blocks: on backpressure the
// record is dropped and counted (see Dropped).
func (s *AuditStream) Append(r Record) {
	if r.TS.IsZero() {
		r.TS = s.clock()
	}
	r.TS = r.TS.UTC()
	s.w.AppendLine(marshalLine(r))
}

// Dropped reports how many records were discarded due to backpressure.
func (s *AuditStream) Dropped() uint64 { return s.w.Dropped() }

// Close flushes and closes the underlying writer.
func (s *AuditStream) Close() error { return s.w.Close() }
