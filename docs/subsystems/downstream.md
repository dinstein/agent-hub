# Downstream connections

> **Answers** how one downstream server's connection lives and dies: the queue, the breaker, retries, credentials, derived instances.
> **Not here** the wire protocol → [protocol.md](protocol.md); the name a tool is exposed under → [exposure.md](exposure.md).
> **Kept true by** `internal/downstream`'s breaker and backoff tests, and `TestDerivedInstanceCapFallsBackToBase`.

`internal/downstream` owns the entire lifecycle of one connection — spawn or dial, handshake, serialized
call queue, circuit breaker, retries, tool table cache, health probing, and the pool that runs one server
as several derived instances. It knows nothing about the name a tool is exposed under.
`internal/mrtr` answers one round of a 2026-07-28 server's `InputRequiredResult`.

`SpecFromEntry` is the only translation from a `registry.ServerEntry` to a runtime `Spec`, so a new
transport cannot land in one caller and be silently dropped in another. `Connect` — dial, handshake,
first `tools/list` — is bounded by `DefaultConnectTimeout` = 120s, generous because a cold-cache
npx/uvx start can take minutes.

## Concurrency

**The owner goroutine plus the `calls` channel is the entire model.** One owner goroutine per `Server`
consumes a capacity-1 `calls chan callReq` — serialization by communication, not by mutex. The caller
blocks on a buffered(1) `reply` channel, so the owner never blocks writing a reply and a sleeping retry
costs the owner's time, not the caller's. Three `callKind`s share the queue: `kindCall`
(breaker-governed), `kindRefresh`, and `kindPing` (breaker-exempt).

**`tools/list` coalesces; `tools/call` never does.** `listMerge` folds concurrent refreshes into one
round trip with waiters inheriting the leader's result — correct for a refresh, wrong for a
non-idempotent call. A waiter inheriting the leader's own context error while its own context is alive
promotes itself to leader and retries once, only once.

**`tools/list` is a walk, not a request.** MCP lets a server page its tool list, and `listAllTools`
follows `nextCursor` until it is **absent** — nil, never the empty string, which the specification calls
a valid position that MUST NOT be read as the end of results. Every failure direction stops the walk
rather than shortening it: a page that fails aborts, a cursor identical to the one just sent is refused
as unable to advance, and `maxToolPages` bounds how long a downstream can make a connect take. **None of
them truncates.** A partial catalogue returned as a complete one is the failure the walk exists to
prevent.

## Breaker, retries, respawn

**The circuit breaker decides before enqueueing**, so during cooldown the caller fails immediately with
`ErrCircuitOpen` and never occupies a queue slot. Three consecutive health failures open it; after a 20s
cooldown exactly one half-open probe is admitted. A straggler failure arriving while already open does
**not** refresh `openedAt`, which would extend the outage indefinitely.

**Only `transport.ClassUnavailable` counts as a health failure.** An ordinary error response proves the
connection works and **resets** the streak; context cancellation is neutral.

**Retries cover exactly two classes**: `ClassRetry` — errors proving the request never reached the
server — and JSON-RPC 429, which some stdio servers use to wrap an HTTP 429. I/O errors *after* the send,
and ordinary error responses, are never retried: `tools/call` is not idempotent. A `RetryAfter` hint is
only jittered upward, never shortened.

**A failed half-open probe rebuilds the connection once** and retries the probe on the new one. The
residual window is accepted: a process that dies mid-call may execute twice.

**A respawn names which of its three causes fired.** `half-open-probe` points at the downstream,
`dead-connection` (a call the transport rejected pre-send) at the network in between, and `manual` at
nobody. The fix goes elsewhere for each.

**The reconnect counter survives a successful respawn; a connection that ANSWERED resets it.** Dialing
and handshaking is exactly what a crash loop does successfully every time, so success alone must not
reset the backoff exponent. What resets it is `Server.served` — the replaced connection completed at
least one round trip, a JSON-RPC error response included. A crash loop therefore climbs unchanged, while
a long-lived HTTP/SSE stream reaped for idleness starts over instead of reaching the 30s cap and charging
every later call ~22s of sleep. `Reconnect()` resets it before and after: once so this attempt skips the
backoff, once so a manual reconnect is not counted as an automatic one.

**Owed: a `tools/list` refresh that answers does not set `served`.** `kindCall` and `kindPing` record
their round trip; `kindRefresh` returns before either. A connection whose only traffic between two deaths
was refreshes is therefore charged to the ladder as a crash loop, which is precisely the reading `served`
was added to prevent. It is narrow — any assembly with `PingInterval > 0` corrects itself within one
probe period — so the exposure is a gateway with background probing off whose downstream is chatty enough
to keep refreshing it. The fix is one `s.served.Store(true)` on a successful refresh, a behaviour change
to the backoff rather than a tidy.

**HTTP 410 Gone is terminal.** `ErrEndpointMoved` is neither retried nor reconnected, and carries frozen
remediation text asserted by tests.

**Ping probing and the circuit breaker are different things.** A probe the breaker could reject would
never see recovery, so `kindPing` is exempt. **A JSON-RPC error response counts as alive** — old servers
answer `ping` with method-not-found, and the round trip completed, which is all a liveness probe may
conclude. Three consecutive transient failures flip to `ConnError`; the `hardConnError` set (ECONNREFUSED,
EHOSTUNREACH, ENETUNREACH, ENETDOWN, `ErrEndpointMoved`, `transport.ErrClosed`, `os.ErrProcessDone`,
`io.ErrClosedPipe`) flips on the first. Background probing is opt-in and one ping is bounded at 10s.

## MRTR: one round of input resolution

A 2026-07-28 server may answer `tools/call` with `input_required`. `Server.Call` collects the requested
inputs and re-issues the original params, up to `maxInputRounds` (4), so callers only ever see a complete
result and the gate chain — which ran once on the original call — is never re-entered by a retry. Each
round re-enters the owner queue separately, so a slow collection does not block other calls to this
server.

**`requestState` never enters `internal/mrtr`**, and that is the design rather than an omission.
`Resolve`'s signature cannot receive it, so "the coordinator cannot inspect or modify it" is a property
of the package boundary instead of a rule a reviewer has to keep applying. `internal/downstream` fills
the `Handler` seam with the **same** peer-handler adapter that serves legacy reverse RPCs, so both
protocol generations answer `roots/list` — and refuse everything unimplemented — identically.

Three fail-closed decisions, each closed for a different reason:

| Case | Result | Why |
|---|---|---|
| no input requests at all | `ErrNoInputRequests` | answering nothing and retrying the identical request could only loop |
| `sampling/createMessage` | `ErrSamplingUnsupported` | AgentHub does not proxy LLM calls and declares no such capability, so a server asking anyway is answered rather than obeyed |
| the first handler failure | aborts the round, **no partial map** | a retry carrying some answers and not others is indistinguishable, from the server's side, from a client that ignored a required input |

Requests are answered sequentially in sorted key order. The determinism is not for the wire: a handler
may reach a human, and stable ordering keeps that experience the same across runs.

## Credentials

**Secret resolution is fail-closed, at dial time**, so a rotated key takes effect on the next reconnect
and resolved credentials never linger in config values. An unresolved placeholder is an **error**, never
passed through: the literal `${SECRET_GITHUB_TOKEN}` produces a 401 indistinguishable from "token
expired", and expanding to empty turns an authenticated endpoint into an anonymous one. Errors mention
only the key name.

**Vault lookups fall back from `(serverID, ScopeName, key)` to `(serverID, "_global", key)`.** That is
what makes derived instances usable — store `GITHUB_TOKEN` once and every derivation inherits it, while a
value under a specific scope overrides just that derivation. A vault **error** at either level aborts: a
broken keychain must never quietly downgrade a scoped credential to a shared one.

**Bearer injection retries once after a 401/403**, and this is the only place in the repository that
repeats a non-idempotent call. The justification is narrow: the 401/403 is decided **before** the server
dispatches, so the rejection proves the request had no side effects, and the request is rebuilt only when
`GetBody` makes it replayable. An explicitly configured `Authorization` header always beats a vault
credential.

**The injected credential never leaves the configured origin, and two independent gates say so.**
`newAuthClient`'s `CheckRedirect` refuses any hop `sameOrigin` rejects; `authRoundTripper.attach`
independently declines to set `Authorization` on a request aimed anywhere but the endpoint's own
scheme+host+port. Both fail closed and neither may be collapsed into the other: `authRoundTripper` sits
**below** net/http's redirect loop, so it runs again for a redirected hop and would re-attach the header
net/http just stripped, letting a downstream that answers `3xx` choose where its own credential is
delivered.

**The token cache is per connection and must never outlive the vault's version of the truth.** The
credential is read once and held for the round tripper's life, because the alternative is a keychain
round trip per request on macOS — but the vault's writers are *other processes*, and hot reload cannot
help, since `specEqual` compares URL, args, env and headers while credentials are deliberately invisible
to it. Four rules, each with its own test:

- **A miss is never cached.** Only a hit sets `loaded`. A server enabled before its credential existed
  would otherwise hold the empty string forever, and on a server that answers anonymously no 401 would
  arrive to correct it.
- **A 401/403 re-reads the vault before renewing**, because the rejected credential is usually just this
  connection's stale copy and a read burns no refresh token.
- **A moved credential epoch drops the cache.** The first two rules are reactive; a credential rotated
  while the one in hand is still accepted produces no rejection, so the daemon's refresher could never
  reach a live connection. `WithEpoch` is how a source opts in.
- **An elapsed credential deadline drops the cache.** The first three need something to happen elsewhere,
  which is unavailable to a standalone gateway whose token simply ages out — and rule two never fires on
  a server that answers an expired bearer with `200` and an error *result*. `NotAfter` is read fresh per
  request, because a copy taken at load time would serve a credential past a deadline the source had
  already moved.

**The announcement plane supplies the epoch signal.** `<data>/secrets/credentials.rev` records server ids
and a monotonic counter and **nothing else**, which is what lets it sit unencrypted beside `secrets.enc`;
it is a file of its own rather than a watch over the vault because a credential may live in the OS
keyring, where replacing a value changes no file at all. `Chain.Set` and `Chain.Delete` announce, being
the choke point `auth login`, `secret set` and both refreshers all land on. Both halves are fail-soft.

What a gateway does with an announcement depends on the server's state: **connected** → bump the epoch so
the next request re-reads the vault, and *nothing reconnects* (the daemon rewrites the vault every 60s;
reconnecting per refresh would be a storm); **not connected** → wake its re-dial rung, since the 401
retry hangs off a live round tripper and can do nothing for a handshake that never completed. Epochs are
keyed by server, not by scope, because a derived instance inherits its base server's login.

**Two layers read the same epoch, for two different things.** `WithEpoch` drops the cached **bearer**;
the proactive source drops its **schedule**. The second matters because a renewal that gives up earns a
hold measured in hours — a day, for a grant the provider refused — and that hold describes the credential
it was taken about. `auth login` replaces exactly that credential, so without the epoch the repair would
sit unused until a schedule expired on a credential that no longer exists.

**A stdio child's PATH is widened to the login shell's, but only when it has to be.** A process started
by launchd or systemd inherits a four-entry PATH, which makes package-manager shims unspawnable from the
GUI and fine from the CLI. The precondition is the design, not an optimization: capturing a login PATH
costs an interactive shell that sources an rc file, and the first stdio dial is the most timing-sensitive
moment the gateway has — so the command is looked up against the PATH the child would get and only an
unresolvable one is repaired. An explicit `PATH` in a server's `env` is neither probed nor widened, and
the docker runtime is skipped. Handing the child a good PATH is only half of it: `exec.Command` resolves
against the *parent's* PATH, so `transport.SpawnStdio` resolves against `StdioConfig.Env` instead
([protocol.md](protocol.md)), and either half alone leaves the bug where it was.

**SSRF blocking lives at this layer**, because the transport facade is standard library only.
`netguard.DialControl` acts on the **resolved address** and opens a hole only for `ProvenanceLocal` plus
a **literal** loopback: RFC1918, CGNAT and link-local stay blocked even for local servers, because cloud
metadata services and intranet hosts live there. Hostnames are never resolved for the decision — a DNS
answer may deny trust but must never grant it.

**Owed: `http.ProxyFromEnvironment` routes around that screen.** Two transports set it, in two packages:
`downstream/httpdial.go` (the auth client) and `mcp/transport/httpcommon.go` (the one every HTTP and SSE
downstream actually speaks over). With a proxy configured, the guarded `DialContext` screens the proxy's
address and the proxy then resolves and connects to the real destination, so netguard never sees the
address that matters. This needs a decision rather than a patch — disabling environment proxies breaks
every operator behind a corporate proxy, while keeping them makes the SSRF screen advisory whenever one
is set. The second location constrains the shape of that decision: `internal/mcp` is standard library
only, so its half has to arrive as something the caller sets. A fix applied to `httpdial.go` alone leaves
the proxy in place on the transport that carries the traffic.

## Derived instances

**`Spec.ID` never changes.** Derivation specializes only connection parameters — `${ROOT}` expansion in
`Args`, `Env` and `Cwd`, plus explicit `Env` overrides — so `RouteOf` remains the sole provenance, scope
intersection still matches on `(serverID, rawTool)`, and the operator's config keeps the name. Only
`Spec.ScopeName` changes, which is what lets a derivation hold its own vault entries. `URL` and `Headers`
are deliberately not derived: a changed header needs no new connection. `expandRoot` leaves the
placeholder verbatim when the root is empty, because `--project ` or a `""` cwd would silently run in the
wrong directory while an unexpanded placeholder fails loudly at spawn.

Four properties of `Pool`: **lazy** (dial on first `Acquire`); **reference counting with deferred close**
(`Release` starts a 30-minute idle clock, `Sweep` closes, so flipping between two roots does not restart
a process per switch); a **cap** of 4 per server, over which `Acquire` returns the baseline instance with
`Lease.Fallback` set and a warning; and **cascading** — `CloseKey` takes down every instance for one
derive key across all servers, because the session is already dead.

**A derivation that cannot connect is an error and never silently falls back to the baseline**, which
would execute with the wrong cwd, env and credentials and defeat the isolation that was asked for. Only
the cap falls back, and with no baseline at all it returns `ErrNoBaseInstance`.

**That carve-out is contested.** A security sweep read the cap fallback as the harm the rule forbids, and
two corrections hold: the cap is driven by **client-supplied roots**, so any client rotating through more
than four roots inside the idle window reaches it; and the baseline resolves secrets under the **base
`ScopeName`**, so a scoped vault lookup silently returns another scope's answer — a credential crossing a
boundary, not just a shared working directory. Kept because erroring at the cap turns a degraded call into
a hard failure on **every tool of that server** until the sweeper reclaims. The middle option, reversing
nothing: keep the fallback but refuse it when the baseline would resolve secrets under a different vault
scope than the derivation asked for. This paragraph and `TestDerivedInstanceCapFallsBackToBase` move
together.

## What a connection writes down

**A connection says how it was made and what it agreed to**, at Info, in three places:

| Where | Records | Because |
|---|---|---|
| `dialStdio` | runtime, command, args, cwd | minutes of honest "connecting" said nothing about what had been launched |
| `dialHTTP` | transport, auth source, endpoint host | 401 and hang are the two reports, and this is which of two credentials and which of two protocols |
| `dialAndInit` | protocol version, peer name and version, capability keys | the terms the connection runs under, on every reconnect |

**`runtime` is the load-bearing one**: isolation a config claims must be delivered or refused, never
silently degraded to a host spawn, and this is the only place recording which of the two a connection
actually got.

**The child environment is never logged, at any level.** It is the one input holding expanded secrets,
and never writing them beats redacting them. Command and args go in as one string so `ScrubString` runs
over them: `slog` passes a `[]string` through as an opaque value that no pattern ever sees.

**The handshake line spells the peer's name `server_name`, not `server`.** The bound key is the registry
id, and a reader taking the last of two identical keys would join on the peer's self-report.

**The line carries the host, never `spec.URL`.** A query string is a place tokens are put, and a record
that never carries the secret is not relying on the scrubber. `endpointHost` reduces to `scheme://host`,
drops userinfo by construction, and returns empty for anything it cannot parse.

**This package logs state CHANGES, never verdicts.** The breaker reports all three transitions and
`healthTracker` its own; neither reports an individual outcome that moved nothing. The breaker's verdict
is taken ahead of the owner queue, so during a cooldown every call is rejected before reaching any other
line: one line per rejected call is a storm, and none at all makes an outage indistinguishable from a
healthy server nobody called.

**Every line of one connection carries the same identity, bound once at `Connect`** — the server id plus,
for a derivation, the instance key. Since `Spec.ID` does not change under derivation, without the second
field four derivations write four connections' worth of lines under one `server` value.

**Every rung of the refresh ladder says which one it stopped on**, at Debug. It used to end each rung in
a silent `return resp, nil`, so a 401 could not be told apart from "we never tried", "the refresh broke"
and "the fresh credential was refused too" — three fixes behind one symptom. The refusal branch is one
condition in code and three answers to a reader, so it is reported as three. The replay line carries both
statuses, because a replay that comes back 401 again sends people to the vault when the problem is scope
or audience.

**A crash must leave evidence.** The handshake failure error embeds the last 20 lines of the child's
stderr, each capped at 400 bytes — a projection of `transport`'s 4 KiB byte window rather than a second
capture. The same window is read off the dying transport **before** it is closed and carried onto the
`respawned` line, or the log keeps the transport's verdict (`broken pipe`) and loses the panic that
produced it. It is attached only when a failure triggered the respawn.

**Owed: a full window's leading fragment is reported as a whole line.** `tailBuffer` cuts on a byte
boundary and `tailLines` is handed the window's contents without its capacity, so it cannot tell a full
window from a short one. Half a line in a crash report is worse than no line; closing it means plumbing
the cap through `stderrTail`, which changes the error text.
`TestTailLinesReportsALeadingFragmentAsAWholeLine` pins today's behaviour and is the test that must
change when it is fixed.

**Frame recording lives here, not in `internal/mcp/transport`**, because transport is standard library
only and knows neither server identity nor a ledger. `callTransport` is the only place frames cross the
downstream boundary, so it is the only feed. Frames go to `internal/calllog` and each carries the
`Origin` its caller named: the ledger call id when a client asked for it, and a cause (`list`, `probe`,
`refresh`) when nobody did. **The origin is an argument, not a context value**: a channel nobody can see
is how a field ends up unset at half the call sites. `seq` is the retry attempt, so one `routed` record
followed by three `sent`/`recv` pairs reads as three attempts of one call.

**The trace switch is `ServerEntry.Trace`, applied as the log's enabled state**, and a log is created for
every server: `Server.trace` is captured once at `Connect`, so a nil handed out there could never be
filled in later, whereas a disabled log can be enabled in place — which is what lets
`agenthub server trace <id> on` reach a running client without reconnecting the server being debugged.
The sink is settable for the same reason. Failure direction is **fail-open**: no ledger, a full queue or
a failed write all degrade to less tracing and never to a failed call. The switch itself goes the other
way, off unless the registry says so, because a frame's body needs the evidence key.

## Current assembly status

`internal/gateway` wires `Log`, `Dial`, `ConnectTimeout`, `Secrets`, `AuthFor`, `FramesFor`, `Events` and
`ClientID`, and `specsFromSnapshot` translates through `SpecFromEntry`, which accepts every transport.
HTTP transport, secret resolution, the OAuth bearer, frame tracing and event recording are all live.

**`PingInterval` is unwired, deliberately**: at zero there is no background prober, the right choice for a
short-lived stdio gateway. So `Health` moves only at connect, at respawn and on call outcomes — a server
that dies between calls is not reported down until something calls it.
