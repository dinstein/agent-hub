# Data Plane

Everything on the path from "a `tools/call` arrives from an upstream client" to "a result comes back
from a downstream MCP server". Nine packages, one layer each, with the boundaries enforced by types and
dependency direction rather than by convention:

- `internal/downstream` owns **connections**: process/HTTP lifecycle, the serialized call queue, the
  circuit breaker, retries, health probing, the derived instance pool. It knows nothing about the name a
  tool is exposed under.
- `internal/router` owns **naming**: it aggregates several servers into one namespaced catalog and
  provides the single reverse-provenance lookup `RouteOf`. It knows nothing about who may see what.
- `internal/pipeline` owns **the execute path**: two gates in frozen order, the call, the shaping post
  hook.
- `internal/gateway` does only **assembly**: it wires the three layers above to visibility
  (`internal/scope`), the discovery face and the budget face, and handles the upstream MCP protocol,
  startup ordering, hot reload and the control link. It implements no governance decision of its own.
- `internal/discovery` (+ `toolsig`) owns **exposure**: which names the visible tool set is presented
  under across the full / grouped / lazy modes, and lazy mode's five meta-tools. It never executes.
- `internal/shaping` (+ `toonenc`) owns **budget**: trim a result to a byte budget, hand the remainder
  back through a `fetch_result` cursor. A cost-saving mechanism, not a security boundary.
- `internal/ratelimit` is quotas on calls. It adds **no** governance interface: it wraps the
  `CallRequest.Call` closure rather than joining the gate chain.

Two disciplines run through the whole collaboration:

1. **An exposed name is an opaque handle.** Names are `sanitize(serverID) + "__" + sanitize(rawTool)`,
   but a server id or a tool name may itself contain `__`, so splitting is ambiguous and banned
   repository-wide. Every reverse mapping goes through `router.RouteOf`.
2. **Failure directions are layered.** The gate chain (scope → token tier) is always fail-closed; the
   cost-saving mechanisms (shaping, toonenc, ratelimit) are always fail-open and must be loud (log it,
   set `Degraded`). Stuffing a fail-open stage into a fail-closed chain is how a rate limiter becomes a
   bypass — which is why `ratelimit` is not a third gate.

The call path itself, and the two rules that hold it together (frozen gate order, one execution path),
are drawn in [../architecture.md §5](../architecture.md#5-what-a-tool-call-passes-through); runtime
sequences and their failure branches are in [flows.md](../flows.md).

---

## internal/downstream

**Responsibility in one sentence**: own the entire lifecycle of one downstream MCP server connection —
spawn/dial, handshake, serialized call queue, circuit breaker, retries, tool table cache, health probing,
and the pool that runs one server as several derived instances.

`SpecFromEntry` is the **only** translation from a `registry.ServerEntry` to a runtime `Spec`, so a new
transport cannot land in one caller and be silently dropped in another. `Connect` (dial + `initialize` +
first `tools/list`) is bounded by `DefaultConnectTimeout` = 120s, generous because a cold-cache npx/uvx
start can take minutes.

**A connection now says how it was made and what it agreed to**, at Info, in three places that were
silent:

| Where | Records | Because |
|---|---|---|
| `dialStdio` | runtime, command, args, cwd | Minutes of honest "connecting" said nothing about what had been launched |
| `dialHTTP` | transport, auth source, endpoint host | 401 and hang are the two reports, and this is which of two credentials and which of two protocols |
| `dialAndInit` | protocol version, peer name/version, capability keys | The terms the connection runs under, on every reconnect |

**`runtime` is the load-bearing one.** AGENTS.md requires that isolation a config claims is delivered or
refused, never silently degraded to a host spawn, and this is the only place recording which of the two a
connection actually got.

**The child ENVIRONMENT is never logged, at any level**, because it is the one input holding expanded
secrets — never writing them beats redacting them. Command and args go in as one string so `ScrubString`
runs over them: `slog` passes a `[]string` through as an opaque value that no pattern ever sees. On the
host path they are public anyway (`ps(1)`), which is exactly why the docker path routes env through the
CLI's own environment instead of argv.

**The handshake line spells the peer's name `server_name`, not `server`.** The bound key is the registry
id, `slog`'s JSON handler does not deduplicate, and a reader taking the last of two identical keys —
`encoding/json` included — would join on the peer's self-report. Same split as `client` / `client_name`
(foundation.md); the test asserts on the **serialized** line, because a decode is what hides it.
Capabilities are reported as sorted top-level **keys**: `InitializeResult` keeps them raw precisely
because this facade does not interpret them, and a log line is neither where that starts nor where an
unbounded server-controlled object belongs.

### Invariants and failure directions

**The owner goroutine plus the `calls` channel is the entire concurrency model.** One owner goroutine per
`Server` consumes a capacity-1 `calls chan callReq` — serialization by communication, not by mutex. The
caller blocks on a buffered(1) `reply` channel, so the owner never blocks writing a reply and a sleeping
retry costs the owner's time, not the caller's. Three `callKind`s share the queue: `kindCall`
(breaker-governed), `kindRefresh`, `kindPing` (breaker-exempt).

**The MRTR retry loop lives below the pipeline, and `requestState` never leaves this package.** A
2026-07-28 server may answer `tools/call` with `input_required`; `Server.Call` collects the requested
inputs and re-issues the original params, up to `maxInputRounds` (4), so callers only ever see a complete
result and the gate chain — which ran once on the original call — is never re-entered by a retry. Each
round re-enters the owner queue separately, so a slow collection (a human at the other end of roots or
elicitation) does not block other calls to this server. The collection itself is `mrtr.Resolve`, which is
handed only the `InputRequests` map and answers it through the `mrtr.Handler` seam: `requestState` is
echoed verbatim by `Server.Call` and **never passed in**, which makes "the coordinator cannot inspect or
modify it" a property of the package boundary rather than of reviewer vigilance. `Resolve` fails closed in
both directions — the first failing input aborts the round rather than returning a partial map, and
`sampling/createMessage` is refused outright (`ErrSamplingUnsupported`), since AgentHub does not proxy LLM
calls and never declares that capability.

**The circuit breaker decides before enqueueing**, so during cooldown the caller fails immediately with
`ErrCircuitOpen` and never occupies a queue slot. `FailureThreshold` (3) consecutive health failures opens
it; after `Cooldown` (20s) exactly one half-open probe is admitted. A straggler failure arriving while
already open does **not** refresh `openedAt`, which would otherwise extend the outage indefinitely.

**Only `transport.ClassUnavailable` counts as a health failure.** An ordinary error response
(`ClassFatal`) proves the connection works and **resets** the streak; context cancellation is neutral.

**Retries cover exactly two classes**: `transport.ClassRetry` (errors proving the request never reached
the server) and JSON-RPC code 429 (`codeRateLimited` — some stdio servers wrap an HTTP 429 into a JSON-RPC
error). I/O errors *after* the send, and ordinary error responses, are **never** retried: `tools/call` is
not idempotent. A `RetryAfter` hint is only jittered upward, never shortened.

**A failed half-open probe rebuilds the connection once** and retries the probe on the new one. The
residual window is accepted: a process that dies mid-call may execute twice.

**A respawn names which of its three causes fired** (`respawnCause`): `half-open-probe` points at the
downstream, `dead-connection` (a call the transport rejected pre-send) at the network in between, and
`manual` (`Reconnect()`) at nobody. The fix goes elsewhere for each.

**The reconnect counter survives a successful respawn; a connection that ANSWERED resets it.** Dialing and
handshaking is exactly what a crash loop does successfully every time, so success alone must not reset the
backoff exponent. What resets it is `Server.served` — the replaced connection completed at least one round
trip (`isAnswered`; a JSON-RPC error *response* counts). A crash loop therefore climbs unchanged, while a
long-lived HTTP/SSE stream reaped for idleness starts over instead of reaching the 30s `MaxDelay` and
charging every later call ~22s of sleep. Both directions have a test.
`Reconnect()` resets it before and after: once so this attempt skips the backoff, once so a manual
reconnect is not counted as an automatic one. The ladder has its own parameters (`withReconnectDefaults`:
250ms base, 30s cap) because a reconnect costs a process start; the first does not wait and the exponent
is capped at `min(n-1, 16)`.

**HTTP 410 Gone is terminal.** `ErrEndpointMoved` is neither retried nor reconnected, and carries the
frozen remediation text `movedHint` — the error text is a contract, asserted by tests.

**Ping probing and the circuit breaker are different things.** A probe the breaker could reject would
never see recovery, so `kindPing` is exempt. The rule: **a JSON-RPC error response counts as alive** (old
servers answer `ping` with method-not-found; the round trip completed, which is all a liveness probe may
conclude). Three consecutive transient failures flip to `ConnError`; the `hardConnError` set (ECONNREFUSED
/ EHOSTUNREACH / ENETUNREACH / ENETDOWN / `ErrEndpointMoved` / `transport.ErrClosed` /
`os.ErrProcessDone` / `io.ErrClosedPipe`) flips on the first. Background probing is opt-in
(`PingInterval == 0` means no prober) and one ping is bounded at 10s.

**`tools/list` coalesces; `tools/call` never does.** `listMerge` folds concurrent refreshes into one round
trip with waiters inheriting the leader's result — correct for a refresh, wrong for a non-idempotent call.
A waiter inheriting **the leader's own context error** while its own context is alive promotes itself to
leader and retries once, only once.

**`tools/list` is a walk, not a request.** MCP lets a server page its tool list, and `listAllTools`
follows `nextCursor` until it is *absent* — nil, never the empty string, which the specification calls
a valid position that MUST NOT be read as the end of results (`mcp.Cursor` is a pointer for exactly
that reason). Every failure direction stops the walk rather than shortening it: a page that fails
aborts, a cursor identical to the one just sent is refused as unable to advance, and `maxToolPages`
bounds how long a downstream can make a connect take. **None of them truncates.** A partial catalog
returned as a complete one is the failure the walk exists to prevent — before it, a paging server's
later tools were simply missing, with nothing anywhere to say so.

**Secret resolution is fail-closed, at dial time**, so a rotated key takes effect on the next reconnect
and resolved credentials never linger in config values. An unresolved placeholder is an **error**, never
passed through: the literal `${SECRET_GITHUB_TOKEN}` produces a 401 indistinguishable from "token
expired", and expanding to empty turns an authenticated endpoint into an anonymous one. Errors mention
only the KEY name.

**Vault lookups fall back from `(serverID, ScopeName, key)` to `(serverID, "_global", key)`.** That is
what makes derived instances usable — store `GITHUB_TOKEN` once and every derivation inherits it, while a
value under a specific scope overrides just that derivation. A vault **error** at either level aborts: a
broken keychain must never quietly downgrade a scoped credential to a shared one.

**A stdio child's PATH is widened to the login shell's, but only when it has to be**
(`widenPATHIfNeeded`). A process started by launchd or systemd inherits a four-entry PATH, which makes
package-manager shims (`npx`, `uvx`, `bunx`) unspawnable from the GUI and fine from the CLI. **The
precondition is the design, not an optimization**: capturing a login PATH costs an interactive shell that
sources an rc file, and the first stdio dial is the most timing-sensitive moment the gateway has — so the
command is looked up against the PATH the child would get and only an unresolvable one is repaired.
Widening rather than replacing keeps the result a strict superset; the capture is process-wide, cached,
bounded at 5s and fail-open; an explicit `PATH` in a server's `env` is neither probed nor widened, and the
docker runtime is skipped. Handing the child a good PATH is only half of it: `exec.Command` resolves
against the *parent's* PATH, so `transport.SpawnStdio` resolves against `StdioConfig.Env` instead
([foundation.md](foundation.md)), and either half alone leaves the bug where it was. `Deps.LoginPATH` is a
seam only because `secureenv.LoginPATH` is a process-wide `sync.Once` no test can ask twice.

**Derived instances: `Spec.ID` never changes.** Derivation specializes only connection parameters
(`${ROOT}` expansion in `Args` / `Env` / `Cwd`, plus explicit `Env` overrides), so `RouteOf` remains the
sole provenance, scope intersection still matches on `(serverID, rawTool)`, and the operator's config
keeps the name. Only `Spec.ScopeName` (= derive key) changes, which is what lets a derivation hold its own
vault entries. `URL` and `Headers` are **deliberately not derived** — a changed header needs no new
connection. `expandRoot` leaves the placeholder verbatim when the root is empty, because `--project ` or a
`""` cwd would silently run in the wrong directory while an unexpanded placeholder fails loudly at spawn.

**Four properties of `Pool`**: LAZY (dial on first `Acquire`), reference counting with **deferred close**
(`Release` starts the idle clock, `Sweep` closes; `DefaultDerivedIdleTTL` 30 min,
`DefaultDerivedSweepInterval` 5 min, so flipping between two roots does not restart a process per switch),
a **cap** (`DefaultMaxDerivedPerServer` 4; over it `Acquire` returns the baseline instance with
`Lease.Fallback` set and a warning), and **cascading** (`CloseKey` takes down every instance for one derive
key across all servers, including referenced ones — the session is already dead). Failure direction: **a
derivation that cannot connect is an error and never silently falls back to the baseline**, which would
execute with the wrong cwd/env/credentials and defeat the isolation that was asked for. Only the cap falls
back, and with no baseline at all it returns `ErrNoBaseInstance` instead.

**That carve-out is contested.** A security sweep read the cap fallback as the harm the rule forbids, and
two corrections hold: the cap is driven by **client-supplied roots**, so any client rotating through more
than `MaxPerServer` roots inside the idle window reaches it; and the baseline resolves secrets under the
**base `ScopeName`**, so a scoped vault lookup silently returns another scope's answer — a credential
crossing a boundary, not just a shared working directory. Kept because erroring at the cap turns a
degraded call into a hard failure on **every tool of that server** until the sweeper reclaims, and
`TestDerivedInstanceCapFallsBackToBase` pins the current behaviour. The middle option, reversing nothing:
keep the fallback but refuse it when the baseline would resolve secrets under a different vault scope than
the derivation asked for. This paragraph and that test move together.

**A crash must leave evidence, at startup and mid-life.** The handshake failure error embeds the last
`StderrRingLines` (20) **lines** of the child's stderr, each capped at 400 bytes — a **projection** of
`transport`'s 4KiB byte window rather than a second capture, dropping the first line when full because a
4KiB cut lands mid-line. The same window is read off the dying transport **before** it is closed and
carried onto the `respawned` / `respawn failed` line as `child_stderr`, or the log keeps the transport's
verdict (`broken pipe`) and loses the panic that produced it. It is attached only when a **failure**
triggered the respawn, since a manual `Reconnect` replaces a connection whose stderr is ordinary chatter.

**What this package logs: state CHANGES, never verdicts.** The breaker reports all three transitions and
`healthTracker` its own; neither reports an individual outcome that moved nothing. The breaker's verdict
is taken ahead of the owner queue, so during a cooldown every call is rejected before reaching any other
line: one line per rejected call is a storm, and none at all makes an outage indistinguishable from a
healthy server nobody called. A **retry** is the exception that proves the shape — an event, not a state,
at Debug. `Server.Close` logs because "downstream connected" needs a counterpart.

**Every line of one connection carries the same identity, bound once at `Connect`**: `logx.FieldServer`
plus, for a derivation, `logx.FieldInstance`. Since `Spec.ID` does not change under derivation, without
the second field four derivations write four connections' worth of lines under one `server` value and none
can be attributed. Bound, not stamped per call site — that is how one line ends up without it.

**Frame recording lives here, not in `internal/mcp/transport`**, because transport is standard-library
only and knows neither server identity nor a ledger. `callTransport` is the **only** place frames cross
the downstream boundary, so it is the only feed. The frames themselves go to `internal/calllog` — there is
no per-server file any more — and every one carries the `Origin` its caller named: the ledger call id when
a client asked for it, and a `cause` (`list`, `probe`, `refresh`) when nobody did. **The origin is an
argument, not a context value**: the gateway's call closure already holds the span, and a channel nobody
can see is how a field ends up unset at half the call sites.

**`seq` is the retry attempt.** One `routed` record can be followed by three `sent`/`recv` pairs when the
connection died twice on the way, and without the number those read as one exchange reported three times —
which is precisely what the retired per-server log could not express, having no call id either.

**Two `Deps` seams are functions rather than fields, for two different reasons.** `FramesFor func(Spec)`
because a `FrameLog` carries the server id it was created with, stamped on every frame — one shared log
would file every server's frames under whoever created it, so no plain field is kept as a fallback.
`Events func() *eventlog.Stream` because that stream is genuinely shared but is decided by **governance**,
which a gateway loads *after* it builds its pool; since `Deps` is captured once at `NewPool`, a plain field
would be read before the switch exists and every derived instance would be silent forever. The event
vocabulary belongs to [foundation.md](foundation.md).

**The switch is `ServerEntry.Trace`, applied as the log's enabled state**, and a log is created for
**every** server: `Server.trace` is captured once at `Connect`, so a nil handed out there could never be
filled in later, whereas a disabled log can be enabled in place — which is what lets
`agenthub server trace <id> on` reach a running client without reconnecting the server being debugged.
**The SINK is settable for the same reason**: servers connect while the ledger policy is still being
applied, and a log created in that window must start recording when the store arrives rather than stay
silent for the life of the process.

**Failure direction: fail-open** — no ledger, a full queue, a write that fails, all degrade to less
tracing and never to a failed call. The switch itself goes the other way, off unless the registry says
otherwise, because frames are captured before anything else touches the bytes — and a frame's BODY needs
the evidence key, so unredacted downstream traffic is never written in the clear.

**Three HTTP-side concerns live in this layer**, because the transport facade is pure standard library and
is not allowed to know about them:

- **SSRF blocking.** `netguard.DialControl` acts on the **resolved address** and opens a hole only for
  `ProvenanceLocal` plus a **literal** loopback — RFC1918/CGNAT/link-local stay blocked even for local
  servers, because cloud metadata services and intranet hosts live there. Hostnames are never resolved for
  the decision: a DNS answer may deny trust but must never grant it.
- **`${SECRET_X}` expansion** (above).
- **Bearer injection with one refresh and one replay after a 401/403.** The only place in the repository
  that repeats a non-idempotent call, on a narrow justification: the 401/403 is decided **before** the
  server dispatches, so the rejection proves the request had no side effects, and the request is rebuilt
  only when `GetBody` makes it replayable. An explicitly configured `Authorization` header always beats a
  vault credential.

**The injected credential never leaves the configured origin, and two independent gates say so.**
`newAuthClient`'s `CheckRedirect` refuses any hop `sameOrigin` rejects; `authRoundTripper.attach`
independently declines to set `Authorization` on a request aimed anywhere but the endpoint's own
scheme+host+port. Both fail closed and neither may be collapsed into the other: `authRoundTripper` sits
**below** net/http's redirect loop, so it runs again for a redirected hop and would re-attach the header
net/http just stripped, letting a downstream that answers `3xx` choose where its own credential is
delivered. `sameOrigin` is duplicated here rather than imported from `transport` because `internal/mcp` is
standard-library only (canonical.md §2 rule 2), and these are two gates, not one with a seam through it.

**How a dial was assembled is recorded before it is attempted, at Info.** `httpdial.go` had no log call
at all, so an HTTP downstream either worked or produced an error naming neither the wire protocol it
spoke nor where its `Authorization` came from — the two questions behind nearly every report about one.
"It returns 401" is answered by which of the vault credential and an operator-set header won; "it hangs
on connect" by which of streamable-HTTP and legacy SSE was chosen. This does **not** duplicate the event
log: `connected` / `connect_failed` say a connection changed state, never how it was set up.

The line carries the **host**, never `spec.URL`. A query string is a place tokens are put, and while
`logx` would scrub the shapes it recognises, a record that never carries the secret is not relying on
that. `endpointHost` reduces to `scheme://host` — enough to see a wrong port, or an `http://` that should
have been `https://` — drops userinfo by construction (it lives outside `URL.Host`), and returns empty
for anything it cannot parse, because a URL this package could not read is not one to quote back.

**Every rung of the refresh ladder says which one it stopped on**, at Debug. It used to end each of them
in a silent `return resp, nil`, so a 401 could not be told apart from "we never tried", "the refresh
broke" and "the fresh credential was refused too" — three fixes behind one symptom. The refusal branch is
one condition in code and **three** answers to a reader (the renewal broke / there was nothing to renew /
the renewal returned what the server had already rejected), so it is reported as three. The replay line
carries **both** statuses, because a replay that comes back 401 again sends people to the vault when the
problem is scope or audience.

`serverEvents.logger()` is what those lines go through. **A nil log is a supported state** — `Emit` guards
it and the zero value is what several tests construct — so a caller wanting the logger for something other
than an event guards it identically. Reaching for `e.log` directly is how a zero value becomes a panic on
whichever path happens to be exercised next.

**The token cache is per connection and must never outlive the vault's version of the truth.** The
credential is read once and held for the round tripper's life, because the alternative is a keychain round
trip per request on macOS — but the vault's writers are *other processes*, and hot reload cannot help,
since `specEqual` compares URL/args/env/headers and credentials are deliberately invisible to it (putting
them in the registry's comparison surface would mean the registry holds secrets). Four rules, each with
its own test:

- **A miss is never cached.** Only a hit sets `loaded`. A server enabled before its credential existed
  would otherwise hold the empty string forever, and on a server that answers anonymously no 401 would
  arrive to correct it.
- **A 401/403 re-reads the vault before renewing**, because the rejected credential is usually just this
  connection's stale copy and a read burns no refresh token. The `tok != stale` guard keeps this from
  swallowing the genuine renewal path.
- **A moved credential epoch drops the cache.** The first two rules are *reactive*; a credential rotated
  while the one in hand is still accepted produces no rejection, so the daemon's refresher could never
  reach a live connection. `WithEpoch` is how a source opts in; a source without one keeps the reactive
  contract exactly.
- **An elapsed credential deadline drops the cache.** The first three need something to happen elsewhere,
  which is unavailable to a standalone gateway whose token simply ages out — and rule two never fires on a
  server that answers an expired bearer with `200` and an error *result* ([oauth.md](oauth.md)). A source
  states its own through `NotAfter`, read fresh per request like the epoch, because a copy taken at load
  time would serve a credential past a deadline the source had already moved. Zero means "no deadline".

**The announcement plane** (`internal/secrets/announce.go`) supplies the epoch signal.
`<data>/secrets/credentials.rev` records server ids and a monotonic counter and **nothing else**, which is
what lets it sit unencrypted beside `secrets.enc` (asserted by a test); it is a file of its own rather
than a watch over the vault because a credential may live in the OS keyring, where replacing a value
changes no file at all. `Chain.Set` / `Chain.Delete` announce, being the choke point `auth login`,
`secret set` and both refreshers all land on. Both halves are fail-soft: an unwritable announcement or an
unparseable `credentials.rev` degrades to the behaviour of the release before it, since every consumer
still has its reactive path. What a gateway does with an announcement depends on the server's state
(`gateway/credwatch.go`): **connected** → bump the epoch so the next request re-reads the vault and
*nothing reconnects* (the daemon rewrites the vault every 60s; reconnecting per refresh would be a storm);
**not connected** → wake its re-dial rung, since the 401 retry hangs off a live round tripper and can do
nothing for a handshake that never completed. Epochs are keyed by server, not by scope, because a derived
instance inherits its base server's login.

### Current wiring status

`internal/gateway` wires `Log` / `Dial` / `ConnectTimeout` / `Secrets` / `AuthFor` / `FramesFor` / `Events`
/ `ClientID`, and `specsFromSnapshot` translates through `SpecFromEntry`, which accepts **every**
transport. HTTP transport, secret resolution, the OAuth bearer, frame tracing and event recording are all
live on the gateway path.

**`PingInterval` is unwired, deliberately**: at zero there is no background prober, the right choice for a
short-lived stdio gateway (`DefaultPingInterval` is the daemon's number). So `Health` moves only at
connect, at respawn, and on call outcomes — nothing polls, and a server that dies between calls is not
reported down until something calls it. `Breaker` / `Retry` / `Reconnect` run on this package's defaults.

---

## internal/mrtr

**Responsibility in one sentence**: answer one round of a downstream's `InputRequiredResult` — the
Multi Round-Trip Request input resolution that replaced in-band reverse RPC in MCP 2026-07-28 — and hand
back the `inputResponses` map for the retry.

One exported function, `Resolve(ctx, reqs, handler)`, and one seam: `Handler` answers a single request by
method. `internal/downstream` fills it with the **same** peer-handler adapter that serves legacy
server-initiated reverse RPCs, so both protocol generations answer `roots/list` — and refuse everything
unimplemented — identically. The retry loop is not here: re-issuing the original request with a new
JSON-RPC id, the echoed `requestState` and the collected responses lives in `internal/downstream`.

### Invariants and failure directions

**`requestState` never enters this package**, and that is the design rather than an omission. "The
coordinator cannot inspect or modify it" is a property of the package boundary — `Resolve`'s signature
cannot receive it — instead of a rule a reviewer has to keep applying.

**Three fail-closed decisions, each closed for a different reason.**

- **No input requests at all** → `ErrNoInputRequests`. Answering nothing and retrying the identical
  request could only loop, so the round ends instead of spinning.
- **`sampling/createMessage`** → `ErrSamplingUnsupported`. AgentHub does not proxy LLM calls
  ([mcp-2026-07-28.md](../mcp-2026-07-28.md) §6.2) and the client capabilities it declares never include
  sampling, so a server asking anyway is answered rather than obeyed. Callers surface it with
  `CodeMissingRequiredClientCapability` (-32021) semantics.
- **The first handler failure aborts the round**, and **no partial map is ever returned**. A retry
  carrying some answers and not others is indistinguishable, from the server's side, from a client that
  ignored a required input — so the failure has to be the whole round.

**Requests are answered sequentially, in sorted key order.** The determinism is not for the wire: a
handler may reach a human — a client eliciting the answer from its user — and stable ordering keeps that
experience the same across runs.

---

## internal/router

**Responsibility in one sentence**: aggregate the tools of several downstream servers (plus host-supplied
`Provider`s) into one namespaced, deterministic catalog, and provide the only legal reverse provenance
lookup.

Live aggregation and cache aggregation run through the **same** `build` core, so a cache-served
`tools/list` cannot drift from the live catalog. `*Router` is an immutable snapshot, atomically
pointer-swapped on change.

### Invariants and failure directions

**`RouteOf` is the only legal reverse mapping**: a pure map lookup with zero string parsing. `Lookup`
yields a nil `*downstream.Server` for cache-built entries — listable and routable, but **not callable**.
`LookupProvider` returns true only for host-supplied entries, so a caller cannot mistake a real server's
tool for one the host serves.

**Splitting on `__` is banned**, repository-wide. Even the gateway's "does this name have a route" check
goes through `RouteOf`; `discovery.IsBareName` is the only place in the repository that inspects `__`, and
its result is used **only for logging**.

**Exposed name generation is a deterministic three-part rule.** Base name is
`sanitize(serverID) + "__" + sanitize(rawTool)`, `sanitize` replacing every rune outside `[a-zA-Z0-9_-]`
with `_`. Collisions take `_2` / `_3` … ordered by ascending raw tool name with server id as secondary
key, and if a suffixed name is itself taken the scan continues upward; base names iterate in sorted order.
Same servers/tools/policy, same names and same `List` order — locked by golden tests.

**A tool crosses this hub whole.** `mcp.ToolDef` carries every member the specification gives a
`Tool`, and `inputSchema` / `outputSchema` / `annotations` / `icons` / `_meta` all travel as raw JSON.
Nothing here interprets them and the party they are addressed to is on the far side of this hop, so a
member this facade failed to name would be a member the downstream published and the client never saw.

**Aggregation applies no policy.** The catalog is the full surface every configured server offers;
narrowing happens once, above it, in `internal/scope`. There used to be a `Policy` here carrying two deny
sets, and removing it fixed a real defect as well as a duplicated mechanism: filtering at aggregation
renumbered the collision suffixes of a dropped tool's neighbours, so switching one tool off could silently
change another tool's exposed name.

**Providers aggregate under exactly the same rules** — same name generation, same collision suffixes, same
`RouteOf` provenance, same scope intersection, same `pipeline.Execute`; only the source of the bytes
differs. `BuildWith` appends providers **after** servers, so a provider id colliding with a server id
reports a duplicate and **the configured server wins**.

**`Catalog` is keyed by RAW tool names only.** It is the snapshot `internal/scope` intersects against;
exposed names never appear in it. `CatalogOf` skips nil servers, so a vanished server contributes no tools
and the scope layer treats "does not exist" as "not visible" — the closed direction. This `Catalog` and
`internal/catalog` (the curated server catalog) are different things; canonical.md A.4 has the ruling.

---

## internal/pipeline

**Responsibility in one sentence**: the repository's only execute path — two gates in frozen order, the
downstream call, and the shaping post hook that spans both the success and error branches.

### Invariants and failure directions

**The gate chain order is frozen: `scope → token_tier`**, pinned by a test. The first error
short-circuits and the call never reaches downstream. Both gates decide from configuration alone — what an
operator wrote down before the client connected — and neither reads the call's arguments. An earlier chain
had two more stages, an argument pre-validator and a human approval gate; both were removed and nothing
replaced them.

- `scopeGate`: `ScopeAllows(es, serverID, rawTool)` is **shared** by this gate and the gateway's
  `tools/list` projection, so "can be listed" and "can be called" cannot disagree. A nil `es`, an
  invisible server and an invisible tool all return false (fail-closed). But "there is no scope authority
  at all" (`Options.Scope == nil`, or returning nil — the cache-serving mode when the registry is
  unavailable) is decided **before** the call, and it allows: in that state there is no governance
  configuration to enforce.
- `tokenTierGate`: `TierCovers(req.CallerTier, ToolTier(req.Annotations))`, decided by **level** (write
  can call read, destructive can call anything). Two closed directions: missing or unparseable annotations
  count as destructive, and an unrecognized `CallerTier` covers nothing. An empty `CallerTier` is the only
  allow case and is not a hole — it means "this assembly has no tier authority", i.e. the stdio gateway
  serving a human's own session over a pipe that carries no credential. Only `internal/httpbridge` mints
  tiers.

**`CallRequest`'s `ServerID`/`RawTool` must come from `RouteOf`.** Its `Annotations` field is the one
where **absence is itself information**: no annotations = destructive, fail-closed.

**Every `Options` field may be zero**, and a zero-value `Options` assembles the M0 baseline (count + allow
+ pass through) — a documented "unauthorized assembly", not an error state. `BlockedError` / `ErrBlocked`
carry a gate rejection, and `Code` (`E_SCOPE_DENIED`, `E_TOKEN_TIER_DENIED`) is ABI the moment it ships.

**Shaping runs exactly once, over the outcome.** Shaping twice would consume the cursor twice and could
leave a truncation banner pointing at bytes nobody receives. There used to be more in this hook — an
injection scan and a leak scan ran ahead of the budget, with load-bearing relative ordering — and the
ordering rules went with them. The stage key is still `StageDefendAndShape` = `defend_and_shape`;
architecture.md §5 records why renaming it would break the gate-count parity assertions silently.

**Dependency constraint**: this package may not import `internal/ctlapi` (canonical.md §2 rule 3, enforced
by depguard) — the data plane does not depend on the control plane.

---

## internal/gateway

**Responsibility in one sentence**: assemble and run the per-client stdio gateway (the implementation
behind `agenthub connect --client <id>`) — it speaks the upstream MCP protocol, brings up downstreams, and
maintains the catalog and visibility, but **implements no governance decision**.

`LoadToolCache` and `LoadToolCacheEntries` exist so the offline CLI reads the same cache format the gateway
writes — **one writer, one decode**, the first a projection of the second that drops each entry's
`SavedAt`, which is what keeps two readers from disagreeing about the same files. `RootSource` is the
migration seam frozen by canonical.md A.5 #30.

### Invariants and failure directions

**Startup ordering: answer first, connect after.** `initialize` is answered before any downstream is
dialed, and downstreams connect concurrently in the background ([flows.md](../flows.md) §1). **A registry
load failure does not abort**: start with empty config, warn, answer from cache. While the live router is
not ready, `tools/list` is answered from cache (same exposed name rules) and `tools/call` answers a
**retryable** busy error (`mcp.CodeBusy`). The cache trade-off branches on registry health: healthy → serve only
the cached tools of currently enabled servers; broken → serve **all** cached tools, because in that state
there is no way to know who is enabled.

**Owed: that degraded start leaves no event, only a log line.** All three failures in `loadRegistry`
(the registry dir unresolved, `registry.Open` returning no store, a store whose documents were
quarantined) write a `Warn` and return `regOK=false`, and `gateway_started` is then emitted exactly as
it would be for a healthy start — same kind, same `Detail`. So `agenthub events` shows a gateway that
came up normally, and `--class disruption` shows nothing at all, while what actually happened is the
state two other entries here already describe as consequential: **all** cached tools served rather than
the enabled ones, and — per the ScopeLayers gap in "Open gaps" below — no scope authority merged, so
`scopeGate` takes its allow-because-there-is-none branch. The prose half is there in the log; the value
half a timeline, a `--json` consumer or an alert can switch on is not.

The fix is a vocabulary decision rather than a tidy: `registry_reload_failed` is the wrong kind (it
means "keeps serving the PREVIOUS generation", and here there has never been one), so this needs a new
gateway-scope kind, which is a published `--kind` selector the day it ships. That is four edits plus the
pair's row in `TestEveryKindIsClassifiedDeliberately` — see
[foundation.md](foundation.md#the-closed-vocabulary) — and a name nobody should mint in passing.

**Not writing to disk after shutdown is achieved by sealing the resource, not by joining goroutines.**
`connectAll` starts one goroutine per downstream and nothing joins them, so a connect that won the race
against cancellation would still reach `persistTools` — and after a shutdown *triggered by a configuration
change* it could leave behind a catalog collected under the configuration just replaced. `shutdown()`
therefore seals `toolCache` first, after which `write` returns `errCacheSealed` and never touches disk;
`mu` covers the **entire** `write`, so `seal()` waits out an in-flight one. A WaitGroup instead would
promote "one downstream that ignores cancellation" into "shutdown stalls for two minutes"
(`ConnectTimeout` is 120s) — a bounded small race traded for an unbounded stall.

**Scope is a query-time projection and never touches connections.** Narrowing scope (a profile edit, a
rebind) never disturbs a downstream connection; only a spec change in `servers.json` triggers a reconnect.
`currentScope()`'s failure direction: no registry store = no scope authority, return nil (the pipeline's
no-authority mode); a store that **exists** but fails to resolve returns an **empty** scope — an error
must never widen visibility. `catalogFromRouter` projects the router back into a raw-name catalog through
`RouteOf`, likewise never splitting exposed names.

**The resolved scope is reported wherever it is BASELINED, and only there** — startup, a content change,
a catalog swap (`logScopeShape`). Never per resolution: `currentScope()` also runs on the `tools/list`
and execute paths, so a line there would be one per call. The record is **counts, not names**: a hub
fronting a dozen servers lists hundreds of tools, and a line growing with the catalog is unreadable
exactly where it is wanted — `agenthub session` is interactive and can afford the names. The startup
counts describe the **cold** catalog, so a first-ever run legitimately reports zero servers and the real
shape arrives with the first catalog swap; that sequence is pinned by test rather than smoothed over.

**A scope's `Diags` reach the log, at Warn.** They are documented as never silent
([architecture.md §7](../architecture.md)), yet nothing in the gateway had ever read them — `agenthub
session` was the sole consumer. A dangling profile reference fails **closed** to an empty scope, so a
diagnostic describes a client that can suddenly see nothing: the loudest symptom the scope chain
produces, previously with the quietest explanation.

**At Debug the same points also report the convergence** — the shape each layer leaves behind, through
`scope.Converge` (see [config.md](config.md)). The Info shape says what a session ended up with; only
this says which layer took the rest away, and that was otherwise guesswork across three config files.
It is **gated on the level**, which is not a micro-optimisation: `Explain` re-folds the layer list once
per layer, so the work is real and must not happen when nobody will read it. Off, it costs one `Enabled`
call. A failure is swallowed to Debug — this is the explanation of a scope, not the scope, and a
diagnostic able to disturb a resolution would be the worse trade.

**The surface cache key is `discovery.Key{Generation, ScopeHash}`**, with `catGen` incremented on every
router swap and the scope hash covering every visibility-relevant field. A stale surface is therefore
structurally unservable: there is no explicit invalidation logic, and so no possibility of missing an
invalidation. Two concurrent builds for the same key are harmless (`discovery.New` is pure); a build over
an already-replaced catalog is discarded. `refreshScopeAndNotify` pushes only when the content hash moved,
and a content change also resets `SearchGuard`, whose streak describes a surface that no longer exists.

**Hot reload: two channels, one funnel.** The local registry watcher and the daemon control link both feed
`onRegistryChange`. A change notification **is a notification, not a snapshot** — the handler re-reads the
registry and adopts it under a `generation >= applied` test. Blast radius is routed by document kind:
`servers` diffs the enabled spec set so only new/removed/changed servers reconnect or close (no restart
storms); `governance` also syncs the skills switch, which changes the **catalog** and forces a rebuild;
`profiles` / `clients` / `governance` are scope inputs and only invalidate, recompute and push if the hash
changed, **never touching a downstream connection**. On a load failure the old config is retained and the
applied state is **not** advanced. `connectOne` re-confirms after connecting that the spec still exists
unchanged, so an expired definition is never wired into the catalog.

**`execTool` is the gateway's only execution path.** Host-supplied providers (skills) resolve **before**
the readiness check — they have no downstream to wait for, and calling them busy while other servers
connect would be a lie. Derived instance selection happens **after routing and before the gate chain**:
"which process executes" is a per-call connection-plane decision, while routing — and therefore
visibility, scope and the quota key — is always the baseline server, whose tool table also supplies the
routed tool's `inputSchema` / `annotations`.

**Unknown names are dropped fail-closed and never reinterpreted as meta-tools.** One exception is
carefully drawn: a name that **has a route** but is not on the surface is hidden by **scope**, so the call
**still enters the pipeline** and is rejected by the scope gate with its stable code — the enforcement
point is the gate. Only a name resolving to nothing at all is dropped, and if downstreams are still
connecting the answer is a **retryable busy** rather than "no such tool", because telling an agent a tool
does not exist teaches it to stop asking.

**Cancellation semantics.** `tools/call` gets its own goroutine and cancel so `notifications/cancelled`
can reach it, and a cancelled request **sends no reply** (MCP contract).

**`RootSource` is a singleflight cache with generation checking.** Concurrent misses coalesce into one
`roots/list` reverse RPC, and `invalidate` increments `gen` so an in-flight fetch discards a possibly
stale result. A client declaring no roots capability gets an empty root set, and that **is cached too**:
asking it would violate the capability contract. The protocol is annotated `DEPRECATED-UPSTREAM`, and
removing it changes only the `RootSource` implementation.

**`shapeResult` is the pipeline's `ResultShaper` seam, not a layer outside the pipeline** — which is why
every execution path is budgeted by the same rule. The cursor id is minted **before** shaping (`Shape`
embeds it in the truncation trailer); an unused id only leaves a hole in an already-guessable sequence.
When the remainder cannot be stored, the **complete result is delivered** rather than a page whose
continuation is already lost.

**Every logging and metrics face degrades on failure without affecting service**: no JSON log file → plain
text, no cache directory → no caching, no control socket path → no coordination.

**Every completed `tools/call` ends on exactly one operational log line, written only by `runCall`.** The
identity is the **routed** `(server, tool)` — `RouteOf` provenance, never the exposed name — plus the
upstream request id and the pipeline's duration. The id is load-bearing: every call runs on its own
goroutine, so without it six concurrent calls interleave into an unreadable sequence.

| Outcome | Level | Why |
|---|---|---|
| `tools/call served` | Info | The one thing the hub exists to do — see below for why it is not Debug |
| `tools/call cancelled` | Info | The one exit that sends no reply |
| `tools/call failed` | Warn | The class that was silent: downstream error, dead transport, open circuit, exhausted retries |
| `tools/call denied` | Warn | Carries the gate and the stable rejection code |

**The success line is at Info because otherwise a call that WORKED is recorded nowhere.** It sat at
Debug to stop an agent's hundreds of calls burying the failures — but the failures are at Warn, their
own level, which `logs --level warn` isolates, so that separation comes from the filter and hiding the
successes only cost the record. Neither of the other two streams covered it: the **call ledger is
disabled** until an operator enables it (`CallsPolicy.Enabled` zero-values to false) and the **event log
records server lifecycle, not calls**. So on a default installation "did the agent ever call this tool",
"which tool is slow" and "was this server used at all" had nothing to read. One line per call is
affordable — `callFields` carries no arguments and `internal/jsonl` rotates at 32 MiB.

**A denial is not a failure, and the split is the point**: nothing broke, the call was refused by
configuration written before the client connected. The scope gate was the one whose silence made a scope
denying everything and `Options.Scope == nil` (the no-authority mode that allows) produce the same empty
log. The two non-answers leave lines too: the retryable busy reply (Debug — "it did nothing for ten
seconds" would otherwise be unattributable), and a name routed to a cached catalog entry whose server
never connected. The client is told `unknown tool` in that second case by the anti-probing rule every miss
follows, so the **log is the only place** the difference between "no such tool" and "that server is down"
is recorded — the difference between an agent's bug and an operator's. A closed downstream is likewise
announced here, with the reason, **before** `Close` runs: `downstream.Close` cannot know whether the
operator deleted or edited the entry, and since `Close` blocks on the owner goroutine, this line without
its counterpart is a teardown that did not finish.

**Arguments never enter ordinary logs.** They are the part of a call carrying the user's data, and a log
that records them cannot be attached to a bug report. The separately configured access ledger (described
in architecture.md §5) stores them only in encrypted payload packs; what belongs here is its one
gateway-side invariant: **hot reload may replace the store, but an in-flight span retains its original
store and key**, so one lifecycle never straddles keys.

**`inproc.go` is why the HTTP face has no second execution path.** `Conn`/`Open` attach the same gateway
body to an in-memory pipe and write requests into the **same frame reader** the stdio face uses.
`Counters()` is the seam that proves it: gate counts on the in-process path must match stdio exactly.

**`statereport.go` is where downstream runtime state comes from.** The gateway is the only process holding
the connections, so it is the only thing that can answer "how is this server doing right now"; the daemon
only aggregates what it posts. The last connection failure stays structured long enough to classify a
typed HTTP 401/403 as `needs_auth` — classification never greps the rendered error, because a proxy's 502
body may include the words "http 401" and an OAuth login cannot repair that.

**How credentials enter this assembly.** `Config.CallerTier` is the operation tier of the **credential**
this gateway serves (minted from the agent token on the HTTP face, always empty for stdio); it flows
verbatim into `pipeline.CallRequest.CallerTier`, with no re-implementation of the decision here.
`Config.ScopeLayers` is the entry point for a credential's server allowlist and profile pin, wired to
`scope.Sources.Extra` — the same `Merge` as the five persisted layers, security fields intersecting,
**narrowing only**. Neither field is used by `agenthub connect`, and their zero values are exactly the
stdio behaviour.

### The re-dial ladder

A dial that fails records **why, and whether the typed failure rejected credentials** (`connErr`), so the
server reports as errored rather than as perpetually connecting. Until `redial.go` existed the connection
was never attempted again, so every recovery cost a client restart — whether the server came up slower
than the gateway, the network blinked, a stdio child crashed on first launch, or a credential arrived
after a 401 had been answered.

The ladder is **5s, 15s, 45s, 135s, then 5 minutes forever**, armed by the recorded failure and cleared by
a success, so `connErr` and the ladder can never disagree about whether a server is broken. Only the base
is configurable (`Config.RedialBase`); the tick and the ceiling derive from it, because two independent
knobs would let a caller set a base above its own cap. Three properties are load-bearing:

- **The cap is not decoration.** Without it a permanently dead server is dialed at the base delay forever,
  and for a stdio entry each rung is a process spawn.
- **The ladder is driven by a recorded failure, never by the tick.** A connected server is never
  re-dialed; a gateway respawning healthy stdio children on a timer would be worse than the bug it fixes.
- **Dials are claimed per server** (`beginDial` / `finishDial`) across startup, hot reload and re-dial, so
  a reload landing next to a due rung cannot produce two connections for one server. A reload that cannot
  claim a slot hands the server to the ladder rather than dropping it; otherwise a redefinition arriving
  while the previous dial is in flight ends up dialed by nobody, since that dial drops itself as stale.

Discovery mode rules out the cheaper design: in lazy mode a failed server's tools are absent from the
catalog, so no call can arrive to trigger a dial on demand. Recovery has to come off a timer, not traffic.

**The rungs are reported at Debug, because the dials alone do not explain the gaps between them.** Each
attempt is logged at Info with its attempt count and nothing said how long the wait before it was, so by
the rungs where the question gets asked — 45s, 135s, then five minutes forever — "it has given up" and
"it is waiting out a backoff it earned" read identically. `armLocked` therefore **returns** the rung and
its delay rather than writing them: it runs under `g.mu`, on which the whole re-dial plane serializes, and
a sink that blocked would hold up every dial in the process. `noteConnectResult` writes them once the lock
is released. A success clears the ladder instead of arming it and so writes nothing — a rung reported
beside a working server would describe a recovery that is not pending.

`wakeLocked` reports whether it woke anything, and the **false** case is why it reports at all: an
announcement for a server with no recorded failure wakes nothing, so storing a credential is followed by
no re-dial. Unexplained, that reads as a lost announcement and sends the reader after a broken watcher
instead of at a server that was never broken.

### Current wiring status

In `pipeline.Options` the gateway sets only `Scope` and `ResultShaper` — that is the whole surface. TOON
output format, intent variants and pin sets remain **unwired** (see the appendix).

Rate limiting is wired, but not through `pipeline.Options`: quotas are an admission wrapper around
`CallRequest.Call` (`ratelimit.go` + `runCall`). Access recording is wired the same way at a different
boundary (`audit.go` wraps dispatch and completion). Neither can alter the gate count or the call contents.

---

## internal/discovery

**Responsibility in one sentence**: decide which names a session is shown and what an incoming name
means — the three exposure modes (full / grouped / lazy), lazy mode's five meta-tools, the lexical ranker,
the search loop guard, and search audit records.

`Visible(rt, es)` projects the router catalog through the session's effective scope using
`pipeline.ScopeAllows`, **the same predicate as the pipeline's scope gate**; `New(Options)` freezes that
set into an immutable `Surface`. `SearchGuard` **deliberately does not belong to** `Surface`: guard state
is per-session, must outlive catalog rebuilds, and yet must be reset on a scope change, so its lifecycle
is the gateway's.

### Invariants and failure directions

**One scope, three enforcement points.** `tools/list`, `search_tools`' candidate filtering and
`call_tool`'s route validation all read the **same** `*scope.EffectiveScope`, and this package never
re-derives visibility. A tool not on the Surface cannot be listed, found or recommended, and
`describe_tool` goes through the same `byExposed` map, so it is **structurally** incapable of revealing a
tool that search hid — a property of the code, not a rule anyone has to remember.

**Determinism is a contract.** Exposure set, ordering, summary truncation and every user-visible string
are frozen by golden tests. Ties break on ascending exposed name and never rely on map iteration order;
scores are integers precisely so a tie is exactly decidable.

**Unknown names fail closed.** `Classify`'s order is fixed: meta names (where the mode allows them) →
grouped aggregate names → the visible tool set → Unknown. An unknown **bare name** (no `__`,
superficially meta-shaped) is treated like any other unknown, and under a cold catalog every name is
unknown. `exposesMeta` is narrower than `IsMetaName`: `call_tool` while variants are on, and the three
variants while variants are off, were never **listed** to this session, so classifying them as meta would
open a door the client cannot see; they fall to Unknown.

**The trade-offs of the three modes:**

- **full**: every visible tool listed as-is.
- **grouped**: one `<server>_tools` aggregate per visible server plus one `call_tool`, servers+1 entries.
  The tool **count** collapses (the expensive part of full is schemas, and grouped ships none), yet the
  agent still **need not search**: each aggregate's description **names** that server's tools (up to
  `groupNameListLimit` = 40, with an overflow note saying how many more and how to get them). Discovery
  stays **exact** — a name is either printed or invisible — and only the schema is deferred by one round
  trip. `call_tool` is placed **last**, so the entries the agent should read first lead.
- **lazy**: the five meta-tools in frozen order, plus pinned tools. A pinned tool whose exposed name
  collides with a meta name is dropped: the meta face must never be shadowed. Today every
  router-generated name contains `__` and cannot collide, but the rule is **enforced**, not assumed.

**The five meta-tool names and schemas are both ABI**: `status`, `search_tools`, `describe_tool`,
`call_tool`, `fetch_result`. Schemas are written as literals rather than marshaled from structs precisely
so those exact bytes are reviewable and golden-testable — agents are sensitive to wording. All meta-tool
arguments decode with `DisallowUnknownFields`: a misspelled argument must be a loud, recoverable error and
never a silently ignored field that lets the agent believe it got something it did not.

**`call_tool` and `fetch_result` have no handler in this package, by design.** Execution must go through
`internal/pipeline` and pagination through `internal/shaping`, so this package stops at `Resolve` /
`Parse`.

**Intent variants (ruling #18).** Lazy mode's single `call_tool` can be split into
`call_tool_read` / `call_tool_write` / `call_tool_destructive`, buying exactly one thing: clients whose
permission UI can only allow or deny a whole tool can allow `call_tool_read` while still requiring
confirmation for writes. Validation uses **equality**, not coverage (`TierCovers`) — if the destructive
variant also accepted read tools each variant would be a superset of those below it, and allowing the top
one would silently grant everything, the exact property the split exists to make visible. Each tool has
**one correct door** and the rejection text names it. `callWithFor` is the **only** place making that
choice and `ResolveCallVariant` enforces the same derivation at the entry point, so the pointer given to
the agent and the check it must pass cannot disagree. Tools with no annotations fall to the destructive
variant (fail-closed). The switch goes into `Key.Variants`, because it changes `tools/list` output while
touching neither generation nor scope hash — without it in the key a governance flip would keep serving
the old doors.

**`SearchGuard`'s state machine and its two deliberate asymmetries.** A new top name → streak = 1; the
same top → streak++; no results, any **non-search** action (`ObserveOther`), or a scope change (`Reset`) →
reset. Escalation to an imperative message needs streak ≥ `EscalateAfter` (3) **and** score ≥
`ConfidenceThreshold` (30). One: a low-confidence top **still advances the streak but never escalates** —
forcing an agent to call a tool the ranker itself doubts turns a weak guess into an instruction, and if a
later identical search scores above the line the accumulated streak escalates immediately. Two:
**escalation does not clear the streak** — only doing something else clears it, because the guard tracks a
loop and it is not over until the loop is over.

**The lexical ranker's weights and calibration.** `weightName` 10 / `weightServer` 4 / `weightDesc` 2,
multiplied by `exactFactor` 3 or `prefixFactor` 1, plus a `coverageBonus` of 5 rewarding coverage of more
**distinct** query terms. Each term contributes at most once per field and occurrence counts are ignored,
so stuffing repeated words into a description buys nothing; `minPrefixLen` 2 keeps single-letter terms
from matching everything. `ConfidenceThreshold` = 30 is calibrated so "confident" means "the query
literally names this tool": an exact tool-name term match scores `10*3 + 5 = 35`, a description-only exact
match 11, a name-prefix-only match 15. Zero-scoring candidates are discarded.

**Query validation and privacy.** `MaxQueryBytes` 512, `MaxQueryWords` 64, `MaxDescriptionTokens` 256 on
the index side (a malicious server must not make every search expensive with a megabyte-long
description). Check order is fixed (empty → bytes → words), so a query violating two limits always reports
the same code. `Query` **deliberately does not retain the original text**, and `Trace` records only the
query's **metrics** — a search query is free text the agent composed and may carry secrets, paths or an
injected payload, while tool names and scores are safe because they come from the catalog. Adding a field
to that struct is a privacy decision, and the golden test fails the moment someone adds a content field.

**`describe_tool`'s "one error, no oracle".** Of the conceivable per-id errors (not_found / invisible /
not offered by this server) only `not_found` is emitted: distinguishing them would turn `describe_tool`
into an oracle enumerating the part of the catalog deliberately not shown to this session. `fetch_result`
follows the same rule for cursors and `ResolveCall` for names. `MaxDescribeTools` = 5, and exceeding it is
**an error rather than a silent truncation**, which would let the agent believe it saw everything it asked
for. A call where none of the ids resolve still returns a **non-error** reply: the call was well-formed,
and a protocol error would deny the agent the remediation text.

**Budget projection of search results.** **No** hit carries a schema: each carries a one-line compact
signature (`toolsig`), and an agent that needs a schema asks `describe_tool`. Rank 1 additionally carries
the full description; the rest carry a `SummaryMaxBytes` = 140-byte summary — a byte bound rather than a
rune count, because what is defended is a token budget and only a byte bound also holds for CJK, with
truncation landing on a rune boundary. The `hit.lossy` flag is the pointer to "a describe round trip will
tell you something new".

---

## internal/discovery/toolsig

**Responsibility in one sentence**: render a downstream tool's JSON Schema into a **one-line** compact
signature, dropping the cost of one search result from a whole schema to one line of text.

`Cache` memoizes by input fingerprint and `Shared()` is the process-level instance that catalog indexing
`Warm`s, so a session's first search pays a map lookup instead of N schema walks. A second instance is
legal but silently wastes that warming.

### Invariants and failure directions

**The grammar is frozen** (locked by `testdata/signatures.golden`; the production rules are in
`toolsig/doc.go`), and two of its markers carry the meaning:

- **`?` marks optional parameters, not required ones.** The inverse would work too; the ruling marks
  optional because they are the minority in practice, so the marker is rarer and the line shorter.
- **`~` is an honesty marker**: the signature **cannot fully state** this parameter — a collapsed nested
  object, a truncated enum, an oversized default, a union type, a surviving `$ref`, or a name in
  `required` with no schema at all. It is the pointer to "describe_tool will tell you more".

**Parameter order is the only deterministic choice available**: required parameters in the **verbatim
order** of the `required` array, then optional ones sorted by ascending bytes. JSON member order does not
survive decoding into a Go map, and `required` is the only ordering signal the schema carries.

**Nesting expands exactly one level** — `obj{key,key}` at the top (keys sorted, capped by `MaxObjectKeys`
= 4), plain `obj` deeper, `arr<obj>` for an array of objects; both collapses set `~`. **`$ref` is not
resolved**: `internal/router` inlines refs first, and anything surviving renders as `any~` rather than
being chased, which would mean this package holding a schema store.

**Failure direction: better to say less than to say more.** Every unsupported construct becomes `TypeAny`
+ `lossy=true`, because a signature that says less than it knows is recoverable through `describe_tool`
and one that says more is not. An unparseable schema, or one that is not an object schema, renders
uniformly as `name(~) -> type` — one shape, no guessing.

**The length budget truncates required-first.** Over `MaxBytes` the list is cut and closed with
`…+N more`; since optional parameters sort last, dropping from the tail drops them first, and when
required parameters must go that is declared the same way. Postcondition: as long as the skeleton fits,
`len(Signature.Text) <= MaxBytes`. **The tool name is never truncated** — it is the key the agent calls
with, and a truncated key is worse than a long line.

**Cache eviction is clear-everything-when-full, not LRU.** The access pattern is "the same few hundred
fingerprints hit repeatedly until the catalog changes": LRU bookkeeping costs more than an occasional
clear-and-re-render, and a clear cannot leak stale entries the way a mis-ordered LRU can. Fingerprints
hash length-prefixed `(name, inputSchema, outputSchema, MaxBytes)`, so different tuples cannot collide.

---

## internal/shaping

**Responsibility in one sentence**: trim a tool result to a byte budget and hand the remainder back
through a `fetch_result` cursor — a cost-saving mechanism, not a security boundary.

`Store` has two implementations: `MemStore` (the stdio gateway — **the process is the session**, so cursor
lifetime is aligned with the client connection by construction) and `FileStore` (the daemon's HTTP face,
where cursors must survive a restart within the session TTL).

### Invariants and failure directions

**Three design rulings fix the shape of the feature:**

1. Truncation cuts at a rune offset inside a **text** content block. Structured blocks are **never split**
   and are deferred whole — which means a truncated page carries no `structuredContent`, and if the
   tool declared an `outputSchema` that page does not satisfy it. Nothing is lost (the payload is in
   the remainder), but it is a conformance cost with two possible fixes and a product decision in
   front of both: see docs/mcp-2026-07-28.md §7.14 before changing this. Page 1 preserves the
   original block structure; page 2 onward is a plain-text
   slice of the retained payload.
2. The recovery trailer is the **last** content block and is **exempt from the budget** — neither
   truncated nor wrapped, because a recovery hint the agent cannot read is not a recovery hint. A page may
   therefore exceed `Budget.Bytes` by exactly one trailer block.
3. Cursor ids are an ordinary, **guessable by design** sequence (`rc-%06d`, process-global, not
   per-owner). **Owner (session) validation is the only isolation**, and unknown, expired and other
   people's ids all return the **one** message `notFoundText` — distinguishable answers would turn a
   guessable id into a probing oracle.

**Owner comparison is constant-time** (`subtle.ConstantTimeCompare`), because this is an isolation
boundary and must not short-circuit at the first differing byte. `Fetch` checks the owner **again** after
`Store.Get`: the interface contract requires implementations to validate, and both sides of an isolation
boundary check.

**The budget is fail-open throughout.** An unparseable content array, a missing cursor id, an absent
owner, a remainder that cannot be stored — every one delivers the **untruncated** result rather than
destroying it. The closed direction belongs to `internal/pipeline`'s gates; losing a caller's data to save
tokens is a worse failure than spending more tokens.

**The never-larger guarantee.** The trailer is not free, so for a result barely over budget the trimmed
page can cost more than the original. `shape` compares once at the end and reverts when
`actualBytes >= baselineBytes` — every dimension must improve (fewer bytes **and** no data withheld).

**Pagination walking rules.** `paginate` fills the budget in order, stops at the **first segment that does
not fit**, and defers everything after it; preserving order is what makes rune offsets after linearization
meaningful. Structured segments are all-or-nothing; text segments split, but a slice smaller than
`minPartialBytes` (16) defers the whole block, because a two-character page plus a trailer costs more than
it delivers.

**The byte accounting is exact.** `escapedRuneLen` reproduces `encoding/json`'s string escaping overhead
(including the HTML escaping on by default), with invariant tests aligning it to the standard library, so
emitted block sizes are **predictable** rather than estimated. All slicing lands on rune boundaries.

**`Fetch`'s boundary behaviour.** A negative offset clamps to 0; an offset at or past the end serves an
**empty page** — a success, not a miss, because offering a recovery hint when there is nothing left would
be a lie. `page` has an "always deliver at least one rune" backstop against a livelock that never advances.

**Durable caching uses ordinary files, not an embedded database** (canonical.md A.6 #2). The path is
`<data>/cache/shaping/<sha256(owner)>/<cursor>.json`, written atomically (same-directory temp → chmod 0600
→ fsync → rename), swept by TTL. Reasoning: the house rule of zero new third-party dependencies; a
single-key point lookup with no queries, transactions or cross-key consistency; and a corrupt entry must
degrade to "lose one cursor", which per-file storage gives for free while a single-file database would
need recovery machinery. The owner-hashed directory keeps one session's cursors off another's path but is
**not** the isolation — `Entry.Owner` is verified on every read. `validID` is both a shape check and a
path safety check, since `FileStore` turns an id into a filename.

**Ordering invariant for format re-encoding: re-encode FIRST, then bound.** `ShapeResult` calls `Reformat`
and only then `shape`. The direction is load-bearing: the budget is spent on the **cheaper**
representation, so a result that fits once re-encoded is delivered whole instead of paginated, and the
retained remainder is the text the agent actually saw, so a `fetch_result` page continues in the same
notation. The recovery trailer still comes last either way, being appended by the truncation step.
**This paragraph once stated the opposite**, which matters because `FormatTOON` is still an unwired
switch: "restoring" the documented order would re-encode a page whose trailer already counted the
pre-encoding payload. Rewrite scope is fixed too: only `text` blocks, and only those whose payload is a
single JSON document; `structuredContent` is **never** re-encoded (it is the machine-readable channel,
clients may parse it, and TOON does not round-trip); and `toonenc.HeaderLine` is emitted at most once per
result.

---

## internal/shaping/toonenc

**Responsibility in one sentence**: project JSON into TOON (Token-Oriented Object Notation), a compact
**display** encoding, and hand it over only when it actually saved bytes.

### Invariants and failure directions

**The primary ruling (canonical.md §7 #4): TOON is a one-way projection.** The encoded document is for a
language model to **read**; it **does not round-trip and no decoder is provided**. Round-tripping would
need a type tag on every scalar (a bare `1` is indistinguishable from `"1"`), and those tags are exactly
the tokens this encoding exists to save. So the encoder quotes only where a reader might be misled, and
the caller states the contract in place with `HeaderLine`: the result is TOON, the arguments are **still**
JSON. Anything that must round-trip — `structuredContent`, tool arguments, cursors — stays JSON and never
enters this package.

**Two constructive guarantees:**

1. **Never larger.** `Consider` re-encodes and compares, and a document that did not beat the JSON form by
   `MinSavingsPct` (`DefaultMinSavingsPct` = 10) is returned verbatim with a `Decision` explaining why, so
   a caller can always use the return value directly. Winning 1% is not worth teaching a model a second
   notation mid-conversation. All comparisons are integer arithmetic — floating point would make the
   accept/reject boundary depend on rounding, and that boundary is what golden tests assert.
2. **Numeric fidelity.** Decoding uses `json.Decoder.UseNumber`, so integers beyond 2^53 travel as literal
   text and are emitted byte for byte. No value passes through a `float64`.

**The table form is the point of the whole encoding**, and its qualifying conditions, the quoting rules
and the frozen `TruncationMarker` are spelled out in `toonenc/doc.go`. Three of those choices are
contracts rather than details: the row delimiter is **fixed and never inferred**; **object keys sort by
ascending bytes**, because no Go decoder preserves JSON object order and determinism is a contract; and
truncation lands on line boundaries so a cut table stays parseable by eye. **Values beyond `MaxDepth`
(12) render as one line of compact JSON**: malicious input must not drive unbounded recursion.

**Failure directions are all open.** Not a single JSON document, an encoder error, blank input — all
return the input verbatim. Mangling a tool result to save tokens is far worse than spending more tokens.

**This package depends only on the standard library and reports only byte counts.** It has no notion of
tokens; the ledger that once converted bytes to tokens was removed, and nothing replaced it.

---

## internal/ratelimit

**Responsibility in one sentence**: cooperative call quotas sharing one counter file across processes —
**resource governance**, not a security control.

`Key{Client, Server, Tool}` uses the **post-routing** values (`RouteOf` provenance) and never the exposed
name: a rename must not change which quota a call spends.

### Invariants and failure directions

**Why it is not a third gate.** The frozen chain (scope → token tier) decides whether a call is **allowed
at all**, and both gates fail closed. Quotas decide whether an already-allowed call happens **now or a few
seconds from now**, and they fail open. So `StageName` (`rate_limit`) is deliberately **not** any
`pipeline.Gate*` value, and the position is achieved structurally: wrapping `CallRequest.Call` lands it
after **every** gate and immediately before the downstream call, so a call a gate rejected never consumes
a token.

**`ExceededError` unwraps into two errors at once** (Go 1.20 multiple unwrap): `*pipeline.BlockedError`, so
`errors.Is(err, pipeline.ErrBlocked)` still holds for any caller classifying gate rejections, and
`*mcp.Error`, so the gateway's existing `errors.As` path answers a JSON-RPC error with `data.retryAfterMs`
without a line of gateway change. `JSONRPCCode` is `mcp.CodeRateLimited`, and both it and the
gateway's `mcp.CodeBusy` are **positive**: MCP 2026-07-28 reserves all of -32768..-32000, so an
implementation's own codes go outside it. They were -32001 and -32000 until the rule was read, and
-32001 was the pre-2026 number for `HeaderMismatch` — a collision a ≤2025-11-25 client could
actually make, since the gateway still serves that generation.

**Multi-process correctness is the entire reason this package exists.** N gateway processes plus the
daemon share `<data>/state/ratelimits.json`. The reference implementation (toolport's `rate_limits.rs`)
reads the file, decides, and writes its own in-memory copy back — when two processes race, each writes a
state that never saw the other's increment and the quota silently doubles. Three things fix it:

1. **A dedicated lock file** `<data>/state/ratelimits.lock` holds an exclusive flock across the entire
   read-decide-write cycle. It is a **different** file because the data file is replaced by rename, and
   locking an inode a concurrent writer is about to swap out protects nothing.
2. State is **re-read from disk every time** inside the lock, so merging is read-modify-write, never
   last-writer-wins.
3. The data file is written atomically (same-directory temp, fsync, rename, fsync parent), so a reader
   outside the lock — or a crash mid-write — can never see half a file.

Counters are integer **millitokens** (`tokenScale` = 1000), never floating point: identical on-disk bytes
on every platform, a golden-testable file, and a merge that cannot drift on rounding.

**Rule evaluation is all-or-nothing.** `Allow` does two passes: the first evaluates every matching rule
against the state just read and returns immediately, **writing nothing**, if any one is out of tokens;
only the second deducts. If rule A has tokens and B does not, spending A's token bills a call that never
happened, and a long enough stream of rejections would starve A permanently.

**All matching rules are enforced (logical AND); there is no "most specific wins".** Quota sets merge in
the same direction as every other governance field here: monotonically tightening, so a narrow rule may
only restrict further and can never unlock what a broad rule forbids. Dimension matching supports only
exact and `Wildcard` — no prefixes, no globs, because a half-understood pattern language is how a quota
ends up governing nothing.

**Buckets are token buckets, not fixed windows.** Capacity is `Limit`, refilled at `Limit` per `Window`, so
a burst of `Limit` is allowed followed by smooth limiting; a fixed window would let `2*Limit` through at a
boundary. `retryAfter` rounds up to milliseconds and is **never 0** — retrying in 0ms is a hot loop.

**`Duration` accepts strings only.** A bare `60` is ambiguous between seconds/milliseconds/nanoseconds, and
that ambiguity gets discovered in production as a quota off by 1000x.

**The failure direction splits in two by timing.**

*Fail-closed at assembly* — when rules **are configured**, `New` rejects three cases: an invalid rule set;
a build without a cross-process file lock (`flock_stub.go`, no longer darwin/linux/windows, all of which
set `crossProcessLockSupported = true` — without it counts would silently multiply by the number of gateway
processes); and a counter file that cannot be locked/read/replaced right now (`probe` tests once rather
than leaving it for each call to discover). All three are the same rule: **claim a quota and you must
honour it or error.** With no rules none trigger, and an empty rule set never touches the filesystem.

*Fail-open at call time, but loud* — a counter file that becomes corrupt/unreadable/unwritable at runtime
lets the call through, sets `Decision.Degraded`, logs, emits an `Event`, and (when corrupt) quarantines the
bad file once. A rate limiter is not a security boundary, and a counter that breaks at 3am must not become
an outage for every agent on the machine. An unknown file version is treated exactly like a corrupt one:
quarantine, restart from empty, never half-interpret.

**"Loud" is not rhetoric; it is the entire precondition for fail-open being acceptable.** What an attacker
wants from a rate limiter is a **silent pass**. So every uncounted pass **both logs and emits an `Event`**,
and the assembler must wire both `Logger` and `OnEvent` — "the quota didn't fire" and "the quota isn't
running" must never look alike. `Event` fires only on DENIED or DEGRADED (one per call would drown its own
signal) and carries identifiers only, never arguments or payloads.

**The two degraded paths report at different levels, and the difference is the rule, not a preference.**
A counter file that has become *unusable* — calls admitted uncounted, nothing enforcing anything — is
**Error**, the level `internal/eventlog`'s `Level` reserves for a protective capability failing and names
this exact condition as an example of; its sibling example, `internal/gateway`'s "ledger unavailable;
calls run unrecorded", is Error for the same reason and also serves the call. Recovered *corruption* stays
**Warn**: the file was quarantined and counters restarted, so this call went uncounted but enforcement is
running again. Only the first is worth alerting on, and it is reachable only by breaking a store that
built successfully — an unusable counter file with rules configured refuses to build at all.

**File size is self-limiting**: buckets idle beyond `idleTTL` (1 hour) are dropped (untouched that long, a
bucket has already refilled), and over `maxBuckets` (4096) the **least recently updated** go first —
dropping a stale bucket is safe, dropping a hot one would pardon an active abuser.

**`ConfigFromGovernance` is the single translation from governance.json to a rule set, and one bad rule
vetoes the whole document** — a partially applied quota set is one nobody can reason about.

### Current wiring status

**Wired** into the stdio gateway in three places: `gateway/ratelimit.go` builds the limiter from
`governance.json`'s `rateLimits` (reusing the `<data>/state` `Store` across rebuilds) and hot-reloads on
governance changes; `runCall` is the single `CallRequest` construction point where `Guard` wraps the call
closure, so a direct `tools/call` and lazy mode's `call_tool` share one enforcement point; and `Event` is
wired to the gateway logger, with rejections at Warn and **degraded (uncounted) passes at Error**.

A governance edit that fails at runtime retains the last usable rule set: refusing service would turn an
unrelated typo into an outage for running agents, while degrading to "no quota" is precisely the silent
widening this package refuses.

Rule sets live at the **global layer only** and do not enter the three-layer scope chain — the reasoning is
on `registry.GovernanceDoc.RateLimits` (rule patterns already carry client/server/tool dimensions, buckets
are keyed by rule pattern, and the same pattern at several layers would split one quota into one per
layer).

---

## Appendix: faces implemented in this layer but not yet wired

Code-complete with tests, but the assembly layer has not connected them. Listed because "thinking
something is in effect when it isn't" is far more dangerous than "knowing it isn't done".

1. **`fetch_result`'s `limit` parameter is accepted but has no effect.** The field is in the frozen schema
   and `gateway/handleFetchResult` explicitly does not honour it — page size comes from the shaping budget
   of page 1, stored alongside the entry. The field is retained so the wire shape does not change when it
   lands.
2. **A batch of switches.** `shaping.FileStore` and `shaping.Reformat`/`ShapeResult` (TOON output) have no
   caller — the gateway calls `shaping.Shape` directly. `discovery.Options.IntentVariants` is never set
   (the registry already has the `intentVariants` field and `IntentVariantsEnabled()`; the gateway does not
   read it), and `Options.Pins` is passed from `g.pins`, which nothing ever assigns.

This is now the only such list; `architecture.md` §12's summary table of the same subject was deleted
rather than emptied. Several removed entries were governance faces — a router policy with
Allow/DenyDestructive seams, a fail-closed HITL default, leak and self-heal hooks on `pipeline.Options` —
and every one was deleted rather than wired, because an unwired governance seam is the most dangerous
thing this appendix can hold: it reads to a hurried operator as protection already in place. What is left
is presentational, which is why it may wait.

## Open gaps, raised by the 2026-07-31 sweep

- **A credential's own scope narrowing is wired only when the registry store opened.** In `newGateway` the
  whole `scope.NewCachedResolver` construction, including the `Extra` closure that reads
  `Config.ScopeLayers`, sits inside `if g.store != nil`. When `loadRegistry` fails at that instant an agent
  token's server allowlist and profile pin are never merged at all: `scopeGate` takes its allow branch, the
  catalog serves the unfiltered disk tool cache, and `discovery.Visible` passes everything — so a token
  scoped to one server receives the names, descriptions and input schemas of every server ever cached under
  `<data>/cache/tools`. With `g.store` nil no registry watcher starts, so the connection stays that way for
  life. Execution is still blocked (no specs → busy), making this catalog disclosure rather than call
  execution, but it contradicts the failure direction `Config.ScopeLayers` documents for itself ("layers
  can only tighten, so a broken source costs visibility, never grants it"). Fix: when `ScopeLayers` is
  non-nil apply them regardless of `g.store` (build the resolver against an empty registry snapshot so the
  Extra layers still intersect), or refuse to assemble a gateway for a constrained credential with no
  registry authority.
- **`http.ProxyFromEnvironment` routes around the dial-time screen.** Two transports set it, and they are
  in two packages: `downstream/httpdial.go` (the auth client) and `mcp/transport/httpcommon.go`
  (`newHTTPClient` — the one every HTTP and SSE downstream actually speaks over). With
  `HTTP_PROXY`/`HTTPS_PROXY` configured the guarded `DialContext` screens the PROXY's address and the
  proxy then resolves and connects to the real destination: a hostname `screenEndpoint` saw resolve
  publicly can be reached privately through the proxy's own DNS, and netguard never sees the address that
  matters. This needs a decision rather than a patch — disabling environment proxies breaks every operator
  behind a corporate proxy, while keeping them makes the SSRF screen advisory whenever one is set.
  **The second location constrains the shape of that decision**: `internal/mcp` is standard library only
  (AGENTS.md constraint 2), so it can consult neither netguard nor a policy of its own, and its half has
  to arrive as something the caller sets — the arrangement `DialContextFunc` already uses for the screen
  itself. A fix applied to `httpdial.go` alone leaves the proxy in place on the transport that carries
  the traffic, which is the half that matters.
