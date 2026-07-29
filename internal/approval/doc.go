// Package approval implements the HITL (human-in-the-loop) approval broker
// (docs/flows.md §3): the daemon-resident Broker that queues gated tool
// calls for a human decision, the gateway-side Asker face, and the
// fingerprint-keyed remember-forever allowlist.
//
// Invariants (frozen, "the full fail-closed set"):
//
//   - Only Approved permits execution. Denied, Timedout, Unreachable and
//     Stale are all terminal rejections — a caller that cannot distinguish
//     them must still block the call.
//   - Ask with zero subscribed frontends returns Unreachable immediately
//     (headless semantics inherited from toolport): never wait for a human
//     who cannot possibly see the request.
//   - The deadline is stamped by the broker; when it passes the request is
//     auto-denied as Timedout. Late answers get typed ErrExpired.
//   - Request.ArgsJSON lives only in memory and on the (authenticated) SSE /
//     control channel. It is never persisted anywhere — the allowlist and
//     any audit record bind arguments through ArgsHash only ("what was approved is what runs").
//   - The allowlist is keyed by tool fingerprint, and callers pass the
//     fingerprint of the LIVE router definition. A drifted tool therefore
//     produces a different fingerprint, misses the allowlist, and must be
//     re-approved. An empty fingerprint never matches anything.
//
// The broker runs inside the daemon; stdio gateways reach it over the UDS
// control connection (Stage 2 wiring) through the Asker interface, whose
// remote implementation degrades to Unreachable on any transport failure.
package approval
