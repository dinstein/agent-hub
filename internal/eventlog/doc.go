// Package eventlog is the CONTROL-plane event stream: what happened to a
// downstream server, a gateway or the daemon, as a bounded closed-vocabulary
// record rather than as prose.
//
// It is the third of three observability streams and the only one that did
// not exist. The other two answer different questions and neither could be
// stretched to answer this one:
//
//   - internal/calllog (`agenthub calls`) records who called which tool.
//     It is encrypted, opt-in, and STRICT — a write failure refuses the call.
//   - the process logs (`agenthub logs`) record what a process was doing, in
//     free-text slog records meant for a human to read.
//
// The lifecycle facts were already being logged, into the second one. That
// is the wrong shape for anything but reading: `msg` is prose, it is not a
// closed set, and nothing may depend on its wording. A UI timeline, a
// `--json` consumer or an alert needs a vocabulary that is allowed to be
// matched on, which is what Kind is.
//
// # Failure direction: OPEN
//
// An event that cannot be written is DROPPED. Never a blocked connection,
// never a failed call. Refusing to serve a client because a note about the
// service could not be filed would be strictly worse than the gap it
// prevents, and the state itself is still observable. calllog reaches the
// same answer from the other end: its writes are synchronous and a failure
// is reported rather than merely counted, but it too costs the history a
// line and never a call.
//
// A nil *Stream is usable and does nothing, so callers hold one without a
// nil check. That is what makes "the switch is off" and "the file would not
// open" the same code path at every call site.
//
// # One file, three scopes
//
// Server, gateway and daemon events share <data>/logs/events.jsonl rather
// than being split by scope. The question an operator asks is a TIMELINE —
// "the daemon restarted at 11:03 and six servers dropped two seconds later"
// — and splitting the file makes re-assembling that story the reader's job.
//
// # Multi-writer discipline
//
// Inherited from internal/jsonl and not reinvented: O_APPEND, one write(2)
// per bounded line, rename-based rotation. Every gateway process plus the
// daemon may hold this file open at once, which is exactly why PID is a
// mandatory field — without it two processes' lines read as one process
// contradicting itself (the lesson internal/logx's FieldPID records, and
// the one internal/downstream's trace log had to learn twice).
//
// Dependency budget: standard library plus internal/jsonl.
package eventlog
