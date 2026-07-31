// Package ratelimit implements the cooperative call quotas
// (`rate_limits.rs` → `internal/ratelimit` in the migration table).
//
// # What it is, and what it is not
//
// A quota here is RESOURCE GOVERNANCE, not a security control. It exists so
// one runaway agent loop cannot burn a paid API budget or trip a downstream
// server's own limiter for everybody else. It is deliberately NOT part of
// the frozen gate chain (scope → token tier): those two decide whether a
// call is ALLOWED AT ALL, and both fail closed and decide from configuration
// alone. A quota decides whether an allowed call happens NOW
// or a few seconds from now, and it fails OPEN (see "Failure direction").
// Mixing the two would put a fail-open stage inside a fail-closed chain,
// which is how a limiter becomes a bypass.
//
// # Where it runs in the pipeline
//
// Immediately BEFORE the downstream call and AFTER every gate. That position
// is not achieved by adding a third gate — it is
// achieved structurally, by wrapping pipeline.CallRequest.Call (and its
// self-heal twin CallWithArgs) through Admission:
//
//	adm := lim.Admit(ratelimit.Key{Client: c, Server: s, Tool: t})
//	req.Call = adm.Wrap(req.Call)
//	req.CallWithArgs = adm.WrapArgs(req.CallWithArgs)
//
// Two consequences are load-bearing:
//
//   - A call a gate denied never spends a token. Charging a refusal against
//     the agent's quota would let denied calls starve allowed ones.
//   - The 7.2 argument self-heal retry is charged ONCE, not twice: both
//     wrappers share one Admission and the token is spent on first
//     admission only. One agent intent = one token.
//
// A rejection is an *ExceededError, which unwraps to BOTH
// pipeline.ErrBlocked (so errors.Is keeps working for anything that
// classifies gate rejections) and an *mcp.Error carrying
// `data.retryAfterMs` (so the assembling gateway's existing
// errors.As(*mcp.Error) path answers the client a JSON-RPC error with a
// retry hint, with no change to the gateway).
//
// # Multi-process correctness
//
// N gateway processes (one per connected client) plus the daemon share one
// counter file, <data>/state/ratelimits.json. The reference implementation
// (toolport `rate_limits.rs`) read the file, decided, and wrote its own
// in-memory copy back — so two processes racing would each write a state
// that never saw the other's increment, and the quota silently doubled.
// The fix here is the whole point of the package:
//
//  1. A DEDICATED lock file, <data>/state/ratelimits.lock, is flock'd
//     exclusively for the entire read-decide-write cycle. It is a separate
//     file because the data file is replaced by rename: locking an inode
//     that a concurrent writer is about to swap out protects nothing.
//  2. The state is re-read from disk INSIDE the lock every time. No
//     decision is ever made against a cached copy, so the merge is
//     read-modify-write, never last-writer-wins.
//  3. The data file is written atomically (temp file in the same directory,
//     fsync, rename, parent fsync), so a reader outside the lock — or a
//     crash mid-write — never sees a half file.
//
// Counters are integer milli-tokens (tokenScale), never floats: the on-disk
// bytes are then identical on every platform and the file is golden-testable.
//
// # Failure direction
//
// The split is between STARTUP (fail closed) and the CALL PATH (fail open,
// never silently).
//
// Fail closed, at New:
//
//   - an invalid rule set (Validate) — silently dropping an unparsable rule
//     presents as "the quota is not working" with no evidence anywhere;
//   - rules configured on a build with no cross-process file lock, where the
//     counters would silently multiply by the number of gateway processes
//     (flock_stub.go);
//   - rules configured over a counter file that cannot be locked, read or
//     replaced right now — probed once at assembly rather than rediscovered
//     per call.
//
// All three are the same rule: a configuration that CLAIMS a quota must
// enforce it or refuse to run, never silently ignore it. None of them fire
// when no rule is configured — an empty rule set is a no-op that never
// touches the filesystem.
//
// Fail open on the call path, LOUDLY: a counter file that becomes corrupt,
// unreadable or unwritable while calls are in flight allows the call, sets
// Decision.Degraded, logs a warning, emits an Event, and (for corruption)
// quarantines the bad file once. Rationale: the limiter is not a security
// boundary, so a counter that dies at 03:00 must not become an outage of
// every agent on the machine.
//
// "Loudly" is not decoration, it is the property that makes fail-open
// defensible. What an attacker wants from a limiter is a SILENT admission:
// counters unreadable, calls flowing, nothing anywhere saying the quota
// stopped applying. So every uncounted admission is BOTH logged and reported
// as an Event, and an assembly is expected to wire Logger and OnEvent — a
// quota that never fires and a quota that is not running must never look
// alike. A degraded limiter is a visible incident, not an invisible bypass.
//
// # Wiring
//
// The assembling gateway needs two things (internal/gateway/ratelimit.go is
// the production wiring):
//
//	// 1. governance.json `rateLimits` (absent = no quotas), translated by
//	//    ConfigFromGovernance and built once against <data>/state. A build
//	//    error is FATAL to the assembly — see the failure direction above.
//	cfg, err := ratelimit.ConfigFromGovernance(snap.Governance.V)
//	lim, err := ratelimit.New(ratelimit.Options{
//	    Config: cfg, StateDir: stateDir, Logger: log,
//	    OnEvent: func(ev ratelimit.Event) { /* log + audit; identifiers only */ },
//	})
//	// 2. every assembled CallRequest is guarded, right where it is built.
//	lim.Guard(ratelimit.Key{Client: clientID, Server: route.ServerID, Tool: route.RawTool}, &req)
package ratelimit
