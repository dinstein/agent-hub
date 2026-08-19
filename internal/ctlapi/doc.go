// Package ctlapi is the control-plane server: REST + SSE over a Unix
// domain socket, or a named pipe on Windows.
//
// Surface (docs/architecture.md#the-processes, docs/subsystems/controlplane.md):
//
//	GET  /v1/ping                     Hello (version, pid, registry generation)
//	GET  /v1/servers                  configured servers + runtime state + Health
//	GET  /v1/sessions                 live sessions
//	POST /v1/sessions/{id}/kill       force-disconnect one live session (handlers.go)
//	GET  /v1/events                   SSE event stream (topic filter, coalescing, Last-Event-ID)
//	POST /v1/gateway/register         stdio gateway registration (docs/architecture.md#the-processes; gateway.go)
//	GET  /v1/gateway/{sid}/link       per-gateway SSE link: registry change notifications
//	POST /v1/gateway/{sid}/servers    gateway runtime state report (gatewaystate.go)
//
// Configuration surface (docs/subsystems/controlplane.md), in admin*.go —
// the control plane's front end over internal/confops, which owns every
// rule so the CLI and the GUI cannot disagree about one operation:
//
//	GET/POST /v1/servers, GET/PATCH/DELETE /v1/servers/{id}   server CRUD
//	GET/POST /v1/profiles, PATCH/DELETE /v1/profiles/{name}   profiles, membership, selectors
//	GET/PUT/DELETE /v1/scope/{client}                         persistent client binding
//	GET /v1/config, PUT /v1/config/{key}                      governance switches
//
// This list once carried two more families — a per-tool kill switch under
// /v1/tools and an isolation set under /v1/quarantine. They were REMOVED with
// the rest of the runtime governance surface (AGENTS.md: no approval queue, no
// runtime scope change, no scanning of what a downstream returned), and no
// route, handler or store for either survives. They are named here only so
// this list is not read as an implementation gap and filled back in.
//
// Two routes above went with them and are named for the same reason:
// POST /v1/sessions/{id}/scope, the narrow-only overlay mutation, and
// POST /v1/gateway/{sid}/ack, the gateway's ack for one pushed overlay
// frame. Both were listed here long after 0bae283 deleted them, which is
// the worse half of the same mistake: a reader could go looking for the
// handler, and a frontend author could believe a live session's surface can
// still be narrowed at runtime. It cannot — a client's surface is decided by
// configuration, before the call.
//
// Every write there takes an optional expected_generation and answers a
// lost compare-and-swap with 409 + CodeStalePrecondition carrying the
// current generation (docs/flows.md#config-writes).
//
// Authentication (docs/architecture.md#the-processes): two layers, both mandatory. The socket
// directory is 0700 and the socket 0600 (first gate: file permissions), and
// every accepted connection is verified with SO_PEERCRED (Linux) /
// LOCAL_PEERCRED (macOS) to belong to the same uid as this process (second
// gate: peer credentials defeat a misconfigured directory). No tokens are
// issued — OS-level identity is sufficient for the local single-user model.
// On Windows the endpoint is a named pipe and the two layers collapse into one
// that is stronger than either: the pipe's SDDL admits the current user and
// nobody else — not Administrators, not SYSTEM — so an unauthorized process
// cannot open the pipe at all and never reaches an Accept that would have to
// reject it (pipelisten_windows.go; platform.CtlPipeSDDL renders the
// descriptor).
//
// Every request carries an X-Request-Id (echo-or-generate; the response
// header is set before the handler runs so even a panic cannot lose it) and
// error bodies carry it too (docs/conventions.md#capability-boundaries). It correlates a request
// across the daemon's own logs; there is no audit stream for it to key
// into — see docs/subsystems/controlplane.md.
//
// Constraints:
//
//   - internal/pipeline must never import this package — the data plane
//     must not depend on the control plane (enforced by depguard, proven
//     by internal/depguardtest).
//   - DTOs and the Go client live in the public api package, not here;
//     this package holds only the server side.
package ctlapi
