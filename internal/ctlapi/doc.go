// Package ctlapi is the control-plane server: REST + SSE over a Unix
// domain socket (named pipe on Windows, M2).
//
// Surface (docs/architecture.md §2, docs/modules/controlplane.md):
//
//	GET  /v1/ping                     Hello (version, pid, registry generation)
//	GET  /v1/servers                  configured servers + runtime state + Health
//	GET  /v1/sessions                 live sessions
//	POST /v1/sessions/{id}/scope      narrow-only overlay mutation via SessionManager.Mutate
//	GET  /v1/events                   SSE event stream (topic filter, coalescing, Last-Event-ID)
//	POST /v1/gateway/register         stdio gateway registration (docs/architecture.md §2; gateway.go)
//	GET  /v1/gateway/{sid}/link       per-gateway SSE link: overlay pushes + registry events
//	POST /v1/gateway/{sid}/ack        gateway ack for one pushed overlay frame
//	POST /v1/gateway/{sid}/servers    gateway runtime state report (gatewaystate.go)
//
// Configuration surface (docs/modules/controlplane.md), in admin*.go —
// the control plane's front end over internal/confops, which owns every
// rule so the CLI and the GUI cannot disagree about one operation:
//
//	GET/POST /v1/servers, GET/PATCH/DELETE /v1/servers/{id}   server CRUD
//	GET/POST /v1/profiles, PATCH/DELETE /v1/profiles/{name}   profiles, membership, selectors
//	GET/PUT/DELETE /v1/scope/{client}                         persistent client binding
//	GET /v1/config, PUT /v1/config/{key}                      governance switches
//	GET /v1/tools, PUT /v1/tools/{server}/{tool}              kill switch + local override
//	GET /v1/quarantine, DELETE /v1/quarantine/{exposed}       isolation set and release
//	GET /v1/audit, GET /v1/security                           JSONL stream tails (auditread.go)
//
// Every write there takes an optional expected_generation and answers a
// lost compare-and-swap with 409 + CodeStalePrecondition carrying the
// current generation (docs/flows.md §4).
//
// Authentication (docs/architecture.md §2): two layers, both mandatory. The socket
// directory is 0700 and the socket 0600 (first gate: file permissions), and
// every accepted connection is verified with SO_PEERCRED (Linux) /
// LOCAL_PEERCRED (macOS) to belong to the same uid as this process (second
// gate: peer credentials defeat a misconfigured directory). No tokens are
// issued — OS-level identity is sufficient for the local single-user model.
// Windows returns ErrUnsupportedPlatform until the named-pipe implementation
// lands in M2.
//
// Every request carries an X-Request-Id (echo-or-generate; the response
// header is set before the handler runs so even a panic cannot lose it),
// error bodies carry it too, and every control-plane WRITE is appended to
// the audit stream with the same id (canonical.md §4).
//
// Constraints:
//
//   - internal/pipeline must never import this package — the data plane
//     must not depend on the control plane (enforced by depguard, proven
//     by internal/depguardtest).
//   - DTOs and the Go client live in the public api package, not here;
//     this package holds only the server side.
package ctlapi
