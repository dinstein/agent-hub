# Scope resolution, sessions, the event bus

> **Answers** how the layers are folded into one `EffectiveScope`, what a session is, and how the daemon notifies itself.
> **Not here** what the layers mean → [../model.md](../model.md); where the layers are stored → [registry.md](registry.md).
> **Kept true by** `internal/scope`'s golden hash test, `TestDiscoveryForMatchesTheResolvedSession`, and `internal/event`'s hand-driven timer tests.

`internal/scope` folds the persisted layers into one content-addressed value. `internal/session` owns
session identity and liveness and nothing else — a session has no scope of its own, so there is nothing
about it to mutate. `internal/event` is the daemon's in-process notification bus, and is not
`internal/eventlog`, which is the persisted record ([records.md](records.md)).

## internal/scope

**`Merge` is pure** — same input, same output, no mutation and no aliasing. The stdio gateway and the
daemon call this one implementation, which is where "both modes behave identically" lands in code.
`MergeWithDiagnostics` folds diagnostics in **before hashing**, so they participate in content
addressing.

**`Converge` is that purity being spent.** A finished `EffectiveScope` cannot say which layer narrowed
it, and since the chain is an intersection, a client seeing nothing has exactly one layer to blame.
`Converge` re-folds the prefixes and reports the shape after each — safe to run off the resolution path
precisely because `Merge` has no side effects. An empty chain yields **no** steps, not one describing
the bare catalogue: a fold with nothing folded in is not a narrowing, and a step for it would sit in
front of every trace claiming a layer acted.

**The layer composition has one source.** `CachedResolver.layersFor` builds the list — persisted
layers, then `Sources.Extra` — and both `Resolve` and `Explain` go through it. A second copy of that
order is how a diagnostic starts describing a resolution that no longer runs. `Explain` fails closed
like `Resolve`: no registry snapshot is an error, never an empty explanation that would read as
"nothing narrowed anything".

**The two classes of merge semantics must not be confused.** Security fields tighten monotonically:
server visibility intersects across layers, seeded from the catalogue's server set, and a tool's
`Allow` intersects. Experience fields take the nearest value — `Discovery` from the most specific
layer, `ResultBudget` per key — with `Budget.Forced` capped at the minimum, so a forced budget can only
push the nearest value down.

**The numeric ordering of `LayerKind` is the specificity ordering.** `Merge` does not require its layers
sorted; specificity comes entirely from comparing `LayerKind`, so swapping the enum values silently
changes who wins.

**`nil` and `[]` are different, and the difference is the whole rule.** An absent selector means "no
intervention", a nil `Allow` means the server's full tool set, an empty `Allow` means nothing at all.
`cloneStrings` preserves that distinction in memory as `omitzero` preserves it on disk.
`scope.ToolSelector` is a type alias for `registry.ToolSelector`, so the semantics have one source, and
its keys are always **raw tool names, never exposed names**.

**Dangling profile references fail closed to the empty set, and never silently.** A reference to a
profile that does not exist appends a profile layer with `Servers: []` and emits a `Diagnostic`.
`FromRegistry` never falls back to the active profile — that would turn deleting a profile into a
silent widening. `PinnedProfileLayer` mirrors this for a credential-supplied pin: an unresolvable name
yields a block-all layer plus `ok=false`.

**`Sources.Extra` can only tighten.** It appends layers after the persisted ones so a credential with
no registry entry can join the same intersection. They are ordinary layers that `Merge` treats no
differently. Its contract: the returned layers must be a **pure function of session id + registry
generation**, because they do not enter the cache key — a source varying independently of those two
serves a stale scope until the next invalidation.

**A frontend asking "which discovery mode does this profile get" goes through `DiscoveryFor`.**
`profile ls` asks the same question a session does, so both share `pickDiscovery` and the same layer
construction. `DiscoveryFor` deliberately does not know `discovery.DefaultMode`: "no layer set one"
comes back as `ok=false` and the caller applies the built-in, so the default stays in the package that
owns it. `ServerToolsLayer` is exported for the same reason — `server tool ls` merges exactly that
layer rather than a second filter written beside it.

**`EffectiveScope.Hash` covers every field except `Generation` and `Hash` itself.** `Generation` records
which registry state the value came from; it is not content identity. The hash uses a length-prefixed
canonical encoding with map keys visited in order, so it is stable across processes and Go versions,
and a golden test pins it. `Changed` compares only the hash: otherwise one registry rebuild amplifies
into a notification storm.

**Better to over-invalidate than to under-invalidate.** `EvRootChanged` clears one session;
`EvRegistryChanged`, `EvCatalogChanged` and **any unknown event kind** clear everything. The catalogue
is not in the cache key, so `EvCatalogChanged` is the only channel through which a downstream tool-set
change becomes visible. Over-invalidating costs a recomputation; under-invalidating costs emitting the
wrong visibility.

**Refuse to resolve without a registry snapshot.** `Resolve` errors when `src.Registry()` returns nil
rather than conjuring an "empty but legal" scope. A nil `Catalog` function, or one returning an empty
catalogue, resolves to zero visible servers — also the closed direction.

**`SessionKey.Root` survives as a field, but no persisted layer reads it.** The stdio gateway fills it
from the first MCP root the client reports, because `internal/downstream` derives per-root server
instances from it.

**`NormalizePath` never canonicalizes and never touches the disk.** Four pure-string operations:
backslashes to `/`, collapse repeated slashes (a leading `//` for UNC survives), strip the trailing
slash (a bare `/` survives), lowercase Windows-shaped paths wholesale. A client-reported path may not
exist on this machine, so symlink resolution or existence probing would both fail and introduce TOCTOU.
It must stay idempotent — it is applied repeatedly to output it has already normalized.

**followActive reads the snapshot, not a state file.** `activeProfileName` returns
`snap.Governance.V.ActiveProfile`, which is what makes `profile use` take effect and keeps
`FromRegistry` pure. Unset returns `""`; an unresolvable name goes through the same dangling-reference
path as a named binding.

### Current assembly status

**A pinned profile contributes no `Discovery`.** There are two routes to "this session follows profile P" — a `clients.json` binding, which reaches
`profileLayer`, and an agent token's `Profile` pin, which reaches `PinnedProfileLayer`. Only the first
copies the profile's `Discovery` onto the layer, so an HTTP credential pinned to a profile whose mode is
`lazy` is served in the global mode instead, while a client bound to that same profile is not. Every
security field agrees across the two; this one does not.

Left as it is because the answer is a product decision — a token's presentation mode may legitimately
belong to the token rather than to the profile it borrows visibility from — and either way settling it
changes what a live agent is served. The divergence is noted at `PinnedProfileLayer` too, so the two
constructors cannot be read side by side without meeting it.

## internal/session

The daemon-side session registry: it mints identities and tracks liveness. What a session may see is
resolved from the registry every time it is asked.

**Two identity shapes, each for a different reader.** The human-facing short id `"client:seq"`, with seq
monotonic per client and never reused within the daemon's lifetime; and the protocol-facing 128-bit
random token (`Mcp-Session-Id`). stdio sessions have no token — `TokenHex()` returns `""` and
`MatchToken` always returns false.

**Token comparison must be constant-time, and anomalous input is denied.** `MatchToken` returns false
outright for three cases — not an HTTP session, hex decode failure, wrong length — and only then reaches
`subtle.ConstantTimeCompare`. `FindByToken` compares in constant time per candidate.

**No entropy, no existence.** `OpenHTTP` errors when `io.ReadFull` fails and never mints a session with
an under-filled token.

**A re-registering gateway always gets a new identity.** seq is never reused, so a reference to the old
session must break rather than quietly rebind to a different connection.

**Only HTTP sessions are reaped by TTL** — 24h default, 5-minute sweep. A stdio session's lifetime is
the gateway process's, and the reaper skips them explicitly.

**Root is a mutable attribute, not part of identity.** `SetRoots` updates it on `roots/list_changed`.

**Derivation keys and scope live on two different planes.** Nothing in `derive.go` touches a scope type
and `DeriveKey` enters no scope hash: narrowing a session must not restart a process, and switching
instances must not change a visible tool name. `DeriveRoot` deliberately returns an empty key for a
session with no root — use the base instance — rather than degrading to a key built from the session
id, which would hand a rootless session private state the operator meant to isolate per project and
spawn one process per rootless session. A multi-root session takes the **first** reported root rather
than a digest of the set, because this key is the vault scope name an operator administers credentials
under and has to stay readable.

**Cascading close only takes down instances keyed by session.** A root-keyed instance is shared by
construction with every session on that root, so tearing one down here would cut a neighbour's
connection; those are left to the pool's idle TTL. Worst case an instance lives 30 minutes too long,
not a call arriving one instance too late.

### Current assembly status: the HTTP half is not wired

**Everything above about HTTP sessions describes code nothing calls.** `Register` mints the stdio
gateway sessions the control plane lists and closes, and that half is live. `OpenHTTP` has no caller
outside tests, so no session with `OriginHTTP` is ever created — and with it, `SessionHello`, the
128-bit token, `TokenHex`, `MatchToken` and `FindByToken` are all reachable only from tests. The
reaper `daemon.go` starts is real and runs every five minutes; it skips stdio sessions by origin, so
in production it scans a table that can never hold anything it would reap.

**The live HTTP session table is `internal/httpbridge`'s**, which mints its own ids and keeps its own
bounds: `DefaultSessionTTL` is **30 minutes** there, not the 24 hours named above, and the capacity cap
and ownership checks are its own too ([controlplane.md](controlplane.md)). Two implementations of one
noun, one of them wired. Read the invariants above as a description of this package rather than of
what an `Mcp-Session-Id` does today.

Left alone because the choice between them is a design decision with an argument to make — the
constant-time comparison and the no-entropy-no-existence rule here are the stricter pair, and moving
the HTTP face onto them is a behaviour change, not a tidy.

## internal/event

The daemon's in-process bus, plus two mergers over one implementation: `NewCoalescer` anchors its window
at a key's **first** `Add` (throttle, bounded latency, 50ms default); `NewSettler` **resets** the window
on every `Add` (debounce, collapsing a lifecycle into one terminal event, 750ms default).
`internal/ctlapi/sse.go` consumes both.

**`Publish` never blocks — that is why this package exists.** A full subscriber buffer means the event
is dropped and counted, never that the publisher stalls. Consumers must treat the bus as a change
**notification** channel, not a change **log**: when `Dropped()` is non-zero, or after a reconnect, the
consumer re-reads authoritative state. Losing a notification is recoverable; blocking the publisher is
not.

**The ordering of unsubscribe and channel close is an invariant.** `Close` removes the subscription
under the write lock and only then closes the channel; `Publish` sends only under the read lock, so no
send can race the close. `Close` is idempotent.

**The payload is the same value for every subscriber and must be treated as immutable.**

**A merger's payload is built lazily and exactly once.** Only the last builder passed to `Add` is
invoked, at fire time, so a burst of K occurrences pays for an expensive payload once. Builders run on
the timer goroutine holding none of the merger's locks, so they must capture state by reference or be
cheap closures.

**The generation sequence lives on the merger, so no two armed timers share a number.** A timer that has
begun firing cannot be stopped, so every re-armed timer captures a `gen` and `fire` ignores stale
callbacks. A per-entry counter answered the reset race and not the same race across `Flush`: a fresh
entry started at zero, a coalescer entry is never reset so its stranded callback held zero too, and the
two collided — a key's timer begins firing, `Flush` detaches and emits that entry, the key is `Add`ed
again, and the stranded callback matches the NEW entry and fires it a window early. Both cases are
pinned by tests that run the timer by hand: the interleaving is the subject, and a sleep would test the
scheduler instead.

**`Close` discards pending events; `Flush` fires them.** Dropping one merged notification at shutdown is
acceptable, since the bus contract already requires consumers to re-read after a loss. After `Close`,
`Add` is a no-op.

**Standard library only.** This package sits beneath every business package.
