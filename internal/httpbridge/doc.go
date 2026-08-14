// Package httpbridge is the daemon's DATA-plane exposure face: MCP
// Streamable HTTP for upstream clients, the ingress hard limits that guard
// it, and the agent-token credential layer that grades what a caller may do
// (docs/architecture.md#the-processes and §9, docs/subsystems/docs/subsystems/controlplane.md).
//
// It is deliberately NOT the control plane. Management traffic goes over the
// UDS control socket (internal/ctlapi) where OS peer credentials are the
// identity and no token exists; this package speaks MCP only.
//
// # What it exposes
//
// Streamable HTTP, and nothing else. canonical.md §5b freezes the transport
// asymmetry: agenthub READS legacy HTTP+SSE downstreams but never grows a new
// SSE exposure face. That is a rule about the 2024-11-05 two-endpoint
// transport, the one whose `endpoint` event says where to POST — NOT about
// streamable HTTP's own server→client channel, which this face does serve:
//
//   - POST carries one JSON-RPC message and gets one JSON-RPC answer, except
//     subscriptions/listen (2026-07-28), whose response IS the stream;
//   - GET opens the ≤ 2025-11-25 notification stream as text/event-stream. A
//     GET whose Accept rules that out is still a 405.
//
// This paragraph said GET was answered 405 outright and that one POST always
// meant one answer, and cited §5b for it — while §5b had already RETIRED that
// stance in the same words it uses to record it. Server's own header
// (server.go) has described the three verbs correctly throughout, so the
// package's front door disagreed with the type behind it.
//
// # Fail-closed bindings
//
// Binding the listener is itself an authorization decision (docs/architecture.md#the-processes,
// inherited from toolport http_bind_is_authorized): with no admin token, no
// active agent token and no registered client there is nobody who could
// legitimately connect, so the bind is REFUSED. --insecure-loopback is the
// single documented escape hatch and it only excuses loopback addresses —
// a non-loopback bind always needs a token. See AuthorizeBind.
//
// # Credentials
//
// Two kinds of bearer token, dispatched by prefix (Authenticator):
//
//   - the admin token (AGENTHUB_HTTP_TOKEN semantics): full tier, every
//     server, no profile pin — the operator's own credential;
//   - agent tokens, "agt_" + 64 hex, minted by Store. Each carries an
//     operation tier (read | write | destructive), an optional server
//     allowlist, an optional profile pin and an optional expiry, and can be
//     revoked. Only the HMAC of the token is ever stored.
//
// The tier travels into internal/pipeline as CallRequest.CallerTier, where
// the token tier gate compares it against the tool's annotation-derived
// tier. That is the SECOND of the two defence lines of docs/architecture.md#what-a-call-passes-through
// (scope → token tier); this package mints the input, it
// does not re-implement the decision.
//
// # Ordering invariant
//
// Per request: ingress limits → authentication → session binding →
// dispatch. Every stage is fail-closed and each rejection is distinguishable
// (413/401/403/404/503), so an operator reading an access log can tell a
// too-large body from a revoked token from a foreign session.
package httpbridge
