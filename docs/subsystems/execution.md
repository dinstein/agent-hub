# Execution: the gates, the gateway, the quotas

> **Answers** what happens between a routed name and a downstream call, and how the stdio gateway is assembled around it.
> **Not here** the connection itself → [downstream.md](downstream.md); who may see what → [../model.md](../model.md).
> **Kept true by** `TestFrozenGateChainOrder`, the stdio/in-process gate-count parity assertions, and `internal/ratelimit`'s multi-process test.

`internal/pipeline` is the repository's only execute path. `internal/gateway` does only assembly.
`internal/tier` is the vocabulary both use for read / write / destructive. `internal/ratelimit` wraps the
call closure and is deliberately not a gate.

**Failure directions are layered.** The gate chain is always fail-closed; the cost-saving mechanisms —
shaping, toonenc, ratelimit — are always fail-open and must be loud. Stuffing a fail-open stage into a
fail-closed chain is how a rate limiter becomes a bypass.

## internal/tier

The vocabulary of the three operation tiers `read | write | destructive`, standard library only. It is a
leaf on purpose: seven packages need these three words and none should import another to say "read".

**`Covers(caller, tool)` decides by rank, not equality**: a write credential can call read tools, and
destructive can call anything.

**The empty string means "no tier privilege", not "the lowest tier".** stdio callers are the human's own
session and carry no agent token, so the tier gate has nothing to enforce against them. That is a
different thing from an *unrecognized* tier, which ranks 0 and covers nothing — fail-closed, because a
typo in a stored token should be refused, not escalated.

**The first and last rows of `ToolTier` are different cases; do not merge them.**

| annotations | Tier | Why |
|---|---|---|
| absent entirely, null, or unparseable | `destructive` | the server said nothing at all; an unannotated tool must never be reachable by a read-only credential |
| `readOnlyHint == true` | `read` | |
| `destructiveHint == true` | `destructive` | |
| `destructiveHint == false` | `write` | |
| an annotations object exists, but neither hint is set | `write` | the server did describe itself, it just stayed silent on this item |

**An annotated but silent tool is `write`, not `destructive`.** The MCP spec's default for a missing
`destructiveHint` is destructive, and this ladder departs from it deliberately when an annotations object
exists: `ToolTier` feeds coarse credential separation, and treating every annotated-but-silent tool as
destructive would collapse the ladder into one rung. A missing or unparseable annotations value is still
destructive.

**Intent variants use equality, not coverage.** `call_tool_read` accepts only read tools, because a
variant expresses "what I intend to do" while a credential expresses "how far I am permitted to go".

## internal/pipeline

**The gate chain order is frozen: `scope → token_tier`**, pinned by a test. The first error
short-circuits and the call never reaches downstream. Both gates decide from configuration alone — what
an operator wrote down before the client connected — and neither reads the call's arguments.

- **`scopeGate`.** `ScopeAllows(es, serverID, rawTool)` is **shared** by this gate and the gateway's
  `tools/list` projection, so "can be listed" and "can be called" cannot disagree. A nil scope, an
  invisible server and an invisible tool all return false. But "there is no scope authority at all" —
  `Options.Scope` nil, or returning nil, which is the cache-serving mode when the registry is
  unavailable — is decided **before** the call, and it allows: in that state there is no governance
  configuration to enforce.
- **`tokenTierGate`.** `TierCovers(req.CallerTier, ToolTier(req.Annotations))`, decided by level. Two
  closed directions: missing or unparseable annotations count as destructive, and an unrecognized
  `CallerTier` covers nothing. An empty `CallerTier` is the only allow case and is not a hole — it means
  this assembly has no tier authority, which is the stdio gateway serving a human's own session over a
  pipe that carries no credential. Only `internal/httpbridge` mints tiers.

**`CallRequest`'s `ServerID` and `RawTool` must come from `RouteOf`.** Its `Annotations` field is the one
where **absence is itself information**: no annotations means destructive.

**Every `Options` field may be zero**, and a zero-value `Options` assembles the baseline — count, allow,
pass through — a documented unauthorized assembly, not an error state. `BlockedError` carries a gate
rejection, and `Code` (`E_SCOPE_DENIED`, `E_TOKEN_TIER_DENIED`) is ABI the moment it ships.

**Shaping runs exactly once, over the outcome.** Shaping twice would consume the cursor twice and could
leave a truncation banner pointing at bytes nobody receives. The stage key is still `defend_and_shape`
after the defences in it were removed, because the gate-count parity assertions compare these stage keys
and renaming one would leave the tests passing while comparing nothing.

**Dependency constraint**: this package may not import `internal/ctlapi`
([canonical.md §2](../canonical.md#hard-dependency-direction-constraints-enforced-at-compile-time-by-depguard)
rule 3) — the data plane does not depend on the control plane.

## internal/gateway

The per-client stdio gateway behind `agenthub connect --client <id>`: it speaks the upstream MCP
protocol, brings up downstreams, and maintains the catalogue and visibility, but **implements no
governance decision**.

### Startup

**Answer first, connect after.** `initialize` is answered before any downstream is dialed, and
downstreams connect concurrently in the background. **A registry load failure does not abort**: start
with empty config, warn, answer from cache. While the live router is not ready, `tools/list` is answered
from cache under the same exposed-name rules and `tools/call` answers a retryable busy error.

The cache trade-off branches on registry health: healthy serves only the cached tools of currently
enabled servers; broken serves **all** cached tools, because in that state there is no way to know who is
enabled.

**Owed: that degraded start leaves no event, only a log line.** All three failures in `loadRegistry`
write a `Warn` and return `regOK=false`, and `KindGatewayStarted` is then emitted exactly as it would be
for a healthy start. So `agenthub events` shows a gateway that came up normally and `--class disruption`
shows nothing, while what actually happened is consequential: all cached tools served rather than the
enabled ones, and — per the `ScopeLayers` gap below — no scope authority merged, so `scopeGate` takes its
allow-because-there-is-none branch. The fix is a vocabulary decision rather than a tidy:
`registry_reload_failed` is the wrong kind, since it means "keeps serving the previous generation" and
here there has never been one, so this needs a new gateway-scope kind — a published `--kind` selector the
day it ships ([records.md](records.md#the-closed-vocabulary)).

**Owed: a credential's own scope narrowing is wired only when the registry store opened.** In
`newGateway` the whole `scope.NewCachedResolver` construction, including the `Extra` closure that reads
`Config.ScopeLayers`, sits inside `if g.store != nil`. When `loadRegistry` fails at that instant, an
agent token's server allowlist and profile pin are never merged at all: `scopeGate` takes its allow
branch, the catalogue serves the unfiltered disk tool cache, and `discovery.Visible` passes everything —
so a token scoped to one server receives the names, descriptions and input schemas of every server ever
cached. With `g.store` nil no registry watcher starts, so the connection stays that way for life.
Execution is still blocked (no specs means busy), making this catalogue disclosure rather than call
execution, but it contradicts the failure direction `Config.ScopeLayers` documents for itself. Fix: when
`ScopeLayers` is non-nil apply them regardless of `g.store`, or refuse to assemble a gateway for a
constrained credential with no registry authority.

**Not writing to disk after shutdown is achieved by sealing the resource, not by joining goroutines.**
`connectAll` starts one goroutine per downstream and nothing joins them, so a connect that won the race
against cancellation would still reach `persistTools` — and after a shutdown triggered by a configuration
change it could leave behind a catalogue collected under the configuration just replaced. `shutdown()`
therefore seals `toolCache` first, after which `write` never touches disk, and `mu` covers the entire
`write` so `seal()` waits out an in-flight one. A WaitGroup instead would promote "one downstream that
ignores cancellation" into "shutdown stalls for two minutes".

### Scope and the catalogue

**Scope is a query-time projection and never touches connections.** Narrowing scope never disturbs a
downstream connection; only a spec change in `servers.json` triggers a reconnect. `currentScope()`'s
failure direction: no registry store means no scope authority, return nil, which is the pipeline's
no-authority mode; a store that **exists** but fails to resolve returns an **empty** scope, because an
error must never widen visibility.

**The resolved scope is reported wherever it is baselined, and only there** — startup, a content change,
a catalogue swap. Never per resolution, since `currentScope()` also runs on the list and execute paths.
The record is **counts, not names**: a hub fronting a dozen servers lists hundreds of tools, and a line
growing with the catalogue is unreadable exactly where it is wanted. The startup counts describe the
**cold** catalogue, so a first-ever run legitimately reports zero servers and the real shape arrives with
the first catalogue swap.

**A scope's `Diags` reach the log, at Warn.** A dangling profile reference fails closed to an empty
scope, so a diagnostic describes a client that can suddenly see nothing: the loudest symptom the scope
chain produces. At Debug the same points also report the convergence — the shape each layer leaves
behind — which is the only thing that says which layer took the rest away. It is gated on the level
because `Explain` re-folds the layer list once per layer, so the work is real.

**The surface cache key is `discovery.Key{Generation, ScopeHash}`**, with the generation incremented on
every router swap and the scope hash covering every visibility-relevant field. A stale surface is
therefore structurally unservable: there is no explicit invalidation logic, and so no possibility of
missing an invalidation. Two concurrent builds for the same key are harmless; a build over an
already-replaced catalogue is discarded. `refreshScopeAndNotify` pushes only when the content hash moved,
and a content change also resets `SearchGuard`, whose streak describes a surface that no longer exists.

**Hot reload: two channels, one funnel.** The local registry watcher and the daemon control link both
feed `onRegistryChange`. Blast radius is routed by document kind: `servers` diffs the enabled spec set so
only new, removed or changed servers reconnect; `governance` also syncs the skills switch, which changes
the catalogue and forces a rebuild; `profiles`, `clients` and `governance` are scope inputs and only
invalidate, recompute and push if the hash changed, never touching a connection. On a load failure the
old config is retained and the applied state is not advanced. `connectOne` re-confirms after connecting
that the spec still exists unchanged, so an expired definition is never wired into the catalogue.

### Executing

**`execTool` is the gateway's only execution path.** Host-supplied providers resolve **before** the
readiness check — they have no downstream to wait for, and calling them busy while other servers connect
would be a lie. Derived instance selection happens **after routing and before the gate chain**: which
process executes is a per-call connection-plane decision, while routing — and therefore visibility, scope
and the quota key — is always the baseline server. Both branches read the routed tool's annotations
through `router.Def(exposed)` rather than a scan of a server's live tool table, so a `list_changed`
refresh landing between routing and this read cannot hand the gate an annotation set inconsistent with
the snapshot the call was routed against.

**Unknown names are dropped fail-closed and never reinterpreted as meta-tools.** One exception is
carefully drawn: a name that **has a route** but is not on the surface is hidden by scope, so the call
still enters the pipeline and is rejected by the scope gate with its stable code — the enforcement point
is the gate. Only a name resolving to nothing at all is dropped, and if downstreams are still connecting
the answer is a retryable busy rather than "no such tool", because telling an agent a tool does not exist
teaches it to stop asking.

**Cancellation semantics.** `tools/call` gets its own goroutine and cancel so `notifications/cancelled`
can reach it, and a cancelled request sends no reply.

**`RootSource` is a singleflight cache with generation checking.** Concurrent misses coalesce into one
`roots/list` reverse RPC, and `invalidate` increments the generation so an in-flight fetch discards a
possibly stale result. A client declaring no roots capability gets an empty root set, and that is cached
too: asking it would violate the capability contract.

**`shapeResult` is the pipeline's `ResultShaper` seam, not a layer outside the pipeline**, which is why
every execution path is budgeted by the same rule. The cursor id is minted before shaping; an unused id
only leaves a hole in an already-guessable sequence. When the remainder cannot be stored, the complete
result is delivered rather than a page whose continuation is already lost.

### What a call leaves behind

**Every completed `tools/call` ends on exactly one operational log line, written only by `runCall`.** The
identity is the **routed** `(server, tool)` plus the upstream request id and the pipeline's duration. The
id is load-bearing: every call runs on its own goroutine, so without it six concurrent calls interleave
into an unreadable sequence.

| Outcome | Level | Why |
|---|---|---|
| `tools/call served` | Info | the one thing the hub exists to do |
| `tools/call cancelled` | Info | the one exit that sends no reply |
| `tools/call failed` | Warn | downstream error, dead transport, open circuit, exhausted retries |
| `tools/call denied` | Warn | carries the gate and the stable rejection code |

**The success line is at Info because otherwise a call that WORKED is recorded nowhere.** The failures
are at Warn, their own level, which `logs --level warn` isolates, so that separation comes from the
filter and hiding the successes only cost the record. Neither of the other two streams covers it: the
call ledger's evidence tier is disabled until an operator enables it, and the event log records server
lifecycle, not calls. One line per call is affordable — the fields carry no arguments, and the stream
rotates at 32 MiB.

**A denial is not a failure, and the split is the point**: nothing broke, the call was refused by
configuration written before the client connected. The two non-answers leave lines too: the retryable
busy reply at Debug, and a name routed to a cached catalogue entry whose server never connected. The
client is told `unknown tool` in that second case by the anti-probing rule, so **the log is the only
place** the difference between "no such tool" and "that server is down" is recorded — the difference
between an agent's bug and an operator's.

**Arguments never enter ordinary logs.** They are the part of a call carrying the user's data, and a log
that records them cannot be attached to a bug report. One gateway-side invariant belongs here: **hot
reload may replace the ledger store, but an in-flight span retains its original store and key**, so one
lifecycle never straddles keys.

### The two faces of one gateway body

**`inproc.go` is why the HTTP face has no second execution path.** `Conn`/`Open` attach the same gateway
body to an in-memory pipe and write requests into the same frame reader the stdio face uses.
`Counters()` is the seam that proves it: gate counts on the in-process path must match stdio exactly.

**`subscribe.go` is the gateway→client direction on that face, and it is a fan-out because one `Conn` has
many clients.** A Conn is per-credential and shared by every HTTP session that credential opened, so each
consumer takes a `Subscription` and `internal/httpbridge` turns one into an SSE response. Delivery is
**coalescing, latest-wins per method**, not buffered: every notification this face carries is an edge
with no payload the client needs, so collapsing two costs nothing — while a fixed buffer that fills has
to drop the NEWEST edge, which is precisely how a client ends up believing a stale tool set. `offer`
never blocks: the read loop it runs on is the only reader of the gateway's output pipe, and one slow
consumer must not stall a connection every other session shares. **With nobody subscribed a notification
is still dropped**, deliberately — a client that opened no stream is not owed one.

**Nothing on that path is a gate.** A notification reaching `fanout` was already produced for that
credential's scope, on the far side of the pipeline, so a subscription filter is the client's own
narrowing.

**`subscriptions.go` is the stdio half, where a subscription decides WHETHER rather than HOW.** stdio has
no body to hold open, so the filter's only effect is to suppress. The rule is *having subscribed*, not
the protocol generation: a session that subscribed is narrowed to its filter exactly, and a session that
never did keeps receiving everything. That second half is a **deliberate deviation** from 2026-07-28,
which says a server sends nothing a client did not subscribe to. Withholding was implemented and
reverted: it leaves a client that never calls the method holding a tool set that can go stale forever
with nothing saying so.

**`statereport.go` is where downstream runtime state comes from.** The gateway is the only process
holding the connections, so it is the only thing that can answer "how is this server doing right now";
the daemon only aggregates what it posts. The last connection failure stays structured long enough to
classify a typed HTTP 401/403 as `needs_auth` — classification never greps the rendered error, because a
proxy's 502 body may include the words "http 401" and an OAuth login cannot repair that.

**How credentials enter this assembly.** `Config.CallerTier` is the operation tier of the credential this
gateway serves — minted from the agent token on the HTTP face, always empty for stdio — and it flows
verbatim into `pipeline.CallRequest.CallerTier`. `Config.ScopeLayers` is the entry point for a
credential's server allowlist and profile pin, wired to `scope.Sources.Extra`: the same `Merge` as the
persisted layers, security fields intersecting, narrowing only. Neither field is used by
`agenthub connect`, and their zero values are exactly the stdio behaviour.

### The re-dial ladder

A dial that fails records why, and whether the typed failure rejected credentials, so the server reports
as errored rather than as perpetually connecting. Until this existed the connection was never attempted
again, so every recovery cost a client restart.

The ladder is **5s, 15s, 45s, 135s, then 5 minutes forever**, armed by the recorded failure and cleared
by a success, so the recorded error and the ladder can never disagree about whether a server is broken.
Only the base is configurable; the tick and the ceiling derive from it, because two independent knobs
would let a caller set a base above its own cap. Three properties are load-bearing:

- **The cap is not decoration.** Without it a permanently dead server is dialed at the base delay
  forever, and for a stdio entry each rung is a process spawn.
- **The ladder is driven by a recorded failure, never by the tick.** A connected server is never
  re-dialed; a gateway respawning healthy stdio children on a timer would be worse than the bug it fixes.
- **Dials are claimed per server** across startup, hot reload and re-dial, so a reload landing next to a
  due rung cannot produce two connections for one server. A reload that cannot claim a slot hands the
  server to the ladder rather than dropping it.

Discovery mode rules out the cheaper design: in lazy mode a failed server's tools are absent from the
catalogue, so no call can arrive to trigger a dial on demand. Recovery has to come off a timer, not
traffic.

**The rungs are reported at Debug**, because the dials alone do not explain the gaps between them. By the
rungs where the question gets asked — 45s, 135s, then five minutes forever — "it has given up" and "it is
waiting out a backoff it earned" read identically. `armLocked` returns the rung and its delay rather than
writing them, because it runs under the lock the whole re-dial plane serializes on. `wakeLocked` reports
whether it woke anything, and the **false** case is why it reports at all: an announcement for a server
with no recorded failure wakes nothing, and unexplained that reads as a lost announcement.

### Current wiring status

In `pipeline.Options` the gateway sets only `Scope` and `ResultShaper` — that is the whole surface. Rate
limiting is wired, but not through `pipeline.Options`: quotas are an admission wrapper around
`CallRequest.Call`. Call recording is wired the same way at a different boundary. Neither can alter the
gate count or the call contents. `discovery.Options.IntentVariants` is never set, and `Options.Pins` is
passed from a field nothing assigns.

## internal/ratelimit

Cooperative call quotas sharing one counter file across processes — resource governance, not a security
control. `Key{Client, Server, Tool}` uses the **post-routing** values and never the exposed name: a
rename must not change which quota a call spends.

**Why it is not a third gate.** The frozen chain decides whether a call is allowed **at all**, and both
gates fail closed. Quotas decide whether an already-allowed call happens **now or a few seconds from
now**, and they fail open. So `StageName` is deliberately not any `pipeline.Gate*` value, and the
position is achieved structurally: wrapping `CallRequest.Call` lands it after every gate and immediately
before the downstream call, so a call a gate rejected never consumes a token.

**`ExceededError` unwraps into two errors at once**: `*pipeline.BlockedError`, so
`errors.Is(err, pipeline.ErrBlocked)` still holds for any caller classifying gate rejections, and
`*mcp.Error`, so the gateway's existing path answers a JSON-RPC error with `data.retryAfterMs` without a
line of gateway change. `JSONRPCCode` and the gateway's `CodeBusy` are both **positive**: MCP 2026-07-28
reserves all of −32768..−32000, so an implementation's own codes go outside it.

**Multi-process correctness is the entire reason this package exists.** N gateway processes plus the
daemon share `<data>/state/ratelimits.json`. Reading the file, deciding, and writing an in-memory copy
back means that when two processes race, each writes a state that never saw the other's increment and the
quota silently doubles. Three things fix it:

1. **A dedicated lock file** holds an exclusive flock across the entire read-decide-write cycle. It is a
   different file because the data file is replaced by rename, and locking an inode a concurrent writer
   is about to swap out protects nothing.
2. State is **re-read from disk every time** inside the lock, so merging is read-modify-write, never
   last-writer-wins.
3. The data file is written atomically, so a reader outside the lock — or a crash mid-write — can never
   see half a file.

Counters are integer millitokens, never floating point: identical on-disk bytes on every platform, a
golden-testable file, and a merge that cannot drift on rounding.

**Rule evaluation is all-or-nothing.** `Allow` does two passes: the first evaluates every matching rule
and returns immediately, writing nothing, if any one is out of tokens; only the second deducts. If rule A
has tokens and B does not, spending A's token bills a call that never happened, and a long enough stream
of rejections would starve A permanently.

**All matching rules are enforced (logical AND); there is no "most specific wins".** Quota sets merge in
the same direction as every other governance field: monotonically tightening. Dimension matching supports
only exact and wildcard — no prefixes, no globs, because a half-understood pattern language is how a
quota ends up governing nothing.

**Buckets are token buckets, not fixed windows.** Capacity is `Limit`, refilled at `Limit` per `Window`,
so a burst is allowed followed by smooth limiting; a fixed window would let twice the limit through at a
boundary. `retryAfter` rounds up to milliseconds and is never 0 — retrying in 0ms is a hot loop.

**`Duration` accepts strings only.** A bare `60` is ambiguous between seconds, milliseconds and
nanoseconds, and that ambiguity gets discovered in production as a quota off by 1000x.

**The failure direction splits in two by timing.**

*Fail-closed at assembly.* When rules are configured, `New` rejects three cases: an invalid rule set; a
build without a cross-process file lock, without which counts would silently multiply by the number of
gateway processes; and a counter file that cannot be locked, read or replaced right now. All three are
the same rule: **claim a quota and you must honour it or error.** With no rules none trigger, and an
empty rule set never touches the filesystem.

*Fail-open at call time, but loud.* A counter file that becomes corrupt or unwritable at runtime lets the
call through, sets `Degraded`, logs, emits an `Event`, and quarantines the bad file once. A rate limiter
is not a security boundary, and a counter that breaks at 3am must not become an outage for every agent on
the machine. An unknown file version is treated exactly like a corrupt one: quarantine, restart from
empty, never half-interpret.

**"Loud" is not rhetoric; it is the entire precondition for fail-open being acceptable.** What an
attacker wants from a rate limiter is a silent pass. So every uncounted pass **both logs and emits an
`Event`**, and the assembler must wire both — "the quota didn't fire" and "the quota isn't running" must
never look alike. `Event` fires only on denied or degraded, and carries identifiers only.

**The two degraded paths report at different levels, and the difference is the rule.** A counter file
that has become *unusable* — calls admitted uncounted, nothing enforcing anything — is **Error**, the
level reserved for a protective capability failing. Recovered *corruption* stays **Warn**: the file was
quarantined and counters restarted, so this call went uncounted but enforcement is running again.

**File size is self-limiting**: buckets idle beyond an hour are dropped, since a bucket untouched that
long has already refilled, and over 4096 buckets the least recently updated go first — dropping a stale
bucket is safe, dropping a hot one would pardon an active abuser.

**`ConfigFromGovernance` is the single translation from governance.json to a rule set, and one bad rule
vetoes the whole document** — a partially applied quota set is one nobody can reason about. A governance
edit that fails at runtime retains the last usable rule set: refusing service would turn an unrelated
typo into an outage for running agents, while degrading to "no quota" is precisely the silent widening
this package refuses.
