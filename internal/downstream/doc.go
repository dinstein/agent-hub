// Package downstream owns the connection lifecycle of one downstream MCP
// server: spawn/dial, handshake, the serialized call queue, the circuit
// breaker, retry semantics, and tool-list caching (docs/architecture.md#the-processes,
// docs/subsystems/downstream.md).
//
// Concurrency model (docs/subsystems/downstream.md): every server has exactly
// one owner goroutine consuming a calls channel of capacity 1 —
// serialization by communication, not by mutex. Callers block in Call; the
// owner performs the transport round trip, so a sleeping retry or a slow
// downstream never occupies a caller's goroutine beyond its own call.
//
// Invariants:
//
//   - The circuit breaker verdict happens BEFORE the request is posted to
//     the calls channel: during cooldown, callers fail fast and never queue.
//   - Only health failures (transport.ClassUnavailable) count toward the
//     breaker. Ordinary error responses (ClassFatal) prove liveness and
//     reset the failure streak; context cancellation is neutral.
//   - Half-open admits a single probe. If the probe fails with a health
//     failure, the connection is rebuilt once via the dial factory and the
//     probe call is retried on the fresh connection.
//   - A call that fails PRE-SEND on an already-dead connection
//     (transport.ErrDeadConnection) also rebuilds the connection once and
//     replays, probe or not. Sound because the request never reached the
//     wire; a post-send failure is never replayed.
//   - Retries are limited to errors that provably never reached the server
//     plus rate-limit responses (ClassRetry / JSON-RPC code 429, honoring a
//     RetryAfter hint with jitter). Post-send I/O errors and ordinary error
//     responses are never retried: tools/call is not idempotent.
//   - RefreshTools re-queries tools/list on the live connection and never
//     respawns the process.
//   - A caller whose context is cancelled stops waiting immediately;
//     forwarding notifications/cancelled downstream is the transport
//     layer's job.
//
// Connection supervision (probe.go / refresh.go / moved.go):
//
//   - Health is probed with MCP ping and is SEPARATE from the breaker: the
//     breaker gates tool calls, the probe observes the connection. Three
//     consecutive transient failures flip Health to ConnError; one hard
//     failure (connection refused and friends, or a 410 Gone) flips it at
//     once. A JSON-RPC error ANSWER counts as alive — the round trip
//     completed, which is all a liveness probe may conclude.
//   - Concurrent tools/list refreshes are merged leader/waiter: one round
//     trip serves every caller. tools/call is never merged (not idempotent).
//   - The reconnect counter SURVIVES a successful respawn, so a flapping
//     server climbs the backoff ladder; only Reconnect (a user action)
//     resets it.
//   - HTTP 410 Gone is terminal: ErrEndpointMoved is never retried and
//     never respawned, and its error carries the "change the URL" hint.
//   - Every JSON-RPC frame can be traced into the call ledger, carrying the
//     id of the call that caused it (frames.go, off by default), and an
//     initialization failure embeds the child's last 20 stderr LINES so a
//     handshake crash is not reported as a bare deadline.
//
// Derived instances (docs/subsystems/downstream.md, derive.go / pool.go):
//
//   - One server may run as SEVERAL instances, keyed by (serverID,
//     DeriveKey), when its registry entry asks for per-root or per-session
//     connection parameters. A derived instance is a full *Server, so it
//     already owns its circuit breaker, its call queue and its connection
//     state — isolation needs no new mechanism, only a lifecycle, which is
//     what Pool provides (lazy dial, reference counting with a delayed
//     idle close, a per-server cap that degrades to the base instance).
//   - Spec.ID NEVER changes across a derivation: routing, visibility and
//     audit all keep naming the server the operator configured. Only
//     Spec.ScopeName follows the key, so a derivation can hold its own
//     vault entries — with a documented fallback to the "_global" ones.
//
// Transports: all three read-side transports are wired (docs/conventions.md#mcp-protocol-scope) —
// stdio (spawned child), streamable-http and legacy HTTP+SSE. The two HTTP
// kinds add three responsibilities this package owns because the transport
// facade is standard-library only and must not know about them:
//
//   - SSRF screening — netguard.DialControl on the RESOLVED address, with
//     one carve-out for a LITERAL loopback endpoint an operator declared
//     local (Spec.Provenance);
//   - ${SECRET_X} resolution in Env and Headers, fail-closed: an unresolved
//     placeholder is an error, never literal text on the wire;
//   - the bearer credential — read from the (serverID, "_global",
//     "__http_auth__") vault entry and, on a 401/403, refreshed ONCE and the
//     request replayed ONCE (docs/status/oauth.md). That is the only place a
//     non-idempotent call is ever repeated, and it is sound because an
//     authorization rejection happens before the server dispatches the call.
package downstream
