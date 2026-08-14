# Configuration and Scope Layer

Seven packages. `internal/scope` folds the persisted configuration layers into one content-addressed
`EffectiveScope`; `internal/session` owns session identity and liveness and nothing else — a session
has no scope of its own, so there is nothing about it to mutate; `internal/event` is the daemon's
in-process notification bus. The other four guard a class of external state: `internal/secrets` (the
credential vault) with `internal/secrets/secureenv` (the environment handed to a spawned child),
`internal/clients` (other applications' config files), and `internal/skills` (the skill library and
its materialized copies).

Those four do not depend on one another. What they share is one discipline: **if you can't read it,
error; if you can't change it, refuse; if you don't understand it, don't write it.** Violating one of
the invariants below turns fail-closed into fail-open.

The merge model itself — which fields intersect, why `clients.json` is not a layer, why the session
root left the cache key — belongs to
[model.md](../model.md).

---

## internal/scope

Folds the configuration layers into a deterministic, content-addressed `EffectiveScope`: which servers
and tools this session sees, and how big a result may be.

### Invariants and failure directions

**`Merge` is pure** — same input, same output, no mutation and no aliasing of the input. The stdio
gateway and the daemon call this one implementation, which is where "both modes behave identically"
lands in code. `MergeWithDiagnostics` folds diagnostics in **before hashing**, so they participate in
content addressing.

**`Converge` is that purity being spent.** A finished `EffectiveScope` cannot say WHICH layer narrowed
it, and since the chain is an intersection that no layer may widen, a client seeing nothing has exactly
one layer to blame. `Converge` re-folds the prefixes and reports the shape after each — safe to run
entirely off the resolution path precisely because `Merge` has no side effects, and reported as counts
rather than names for the same reason the gateway reports a finished scope that way. An empty chain
yields **no** steps, not one describing the bare catalog: a fold with nothing folded in is not a
narrowing, and a step for it would sit in front of every trace claiming a layer acted.

**The layer composition has one source.** `CachedResolver.layersFor` builds the list — persisted layers,
then `Sources.Extra` — and both `Resolve` and `Explain` go through it. A second copy of that order is how
a diagnostic starts describing a resolution that no longer runs, which is the same reason `pickDiscovery`
is a function rather than eight inline lines. `Explain` fails closed like `Resolve`: no registry snapshot
is an error, never an empty explanation that would read as "nothing narrowed anything".

**The two classes of merge semantics must not be confused.** Security fields tighten monotonically:
server visibility intersects across layers (seeded from the catalog's server set), and a tool's `Allow`
intersects. There is no deny list anywhere and no boolean switch left to fold — a deny would answer a
newly-added downstream tool in the opposite direction from an allow, and one configuration must not
give two answers. Experience fields take the nearest value: `Discovery` from the most specific layer
(later layer wins within a `LayerKind`), `ResultBudget` per key. The one exception is `Budget.Forced`,
capped at the **minimum**: a forced budget can only push the nearest value down.

**Current assembly status: a pinned profile contributes no `Discovery`.** There are two routes to "this
session follows profile P" — a `clients.json` binding, which reaches `profileLayer`, and an agent token's
`Profile` pin, which reaches `PinnedProfileLayer` (`internal/daemon/httpdata.go`, `internal/cli/tool.go`).
Only the first copies the profile's `Discovery` onto the layer, so an HTTP credential pinned to a profile
whose mode is `lazy` is served in the global mode instead, while a client bound to that same profile is
not. Every security field agrees across the two; this one field does not, and the rule above states no
exception for it. Left as it is because the answer is a product decision — a token's presentation mode may
legitimately belong to the token rather than to the profile it borrows visibility from — and either way
settling it changes what a live agent is served. The divergence is also noted at `PinnedProfileLayer`, so
the two constructors cannot be read side by side without meeting it.

**The numeric ordering of `LayerKind` is the specificity ordering and must not be rearranged.** `Merge`
does not require its layers sorted; specificity comes entirely from comparing `LayerKind`, so swapping
the enum values silently changes who wins.

**`nil` and `[]` are different, and the difference is the whole rule.** An absent selector means "no
intervention"; a nil `Allow` means the server's full tool set; an **empty** `Allow` means nothing at
all. Hence `omitzero`, not `omitempty` — dropping an empty list on the way to disk turns block-all into
allow-all, silently and in the fail-open direction. `cloneStrings` preserves the same distinction in
memory. `scope.ToolSelector` is a type alias for `registry.ToolSelector`, so those semantics have one
source of truth, and its keys are always **raw tool names, never exposed names**.

**Dangling profile references fail closed to the empty set, and never silently.** A reference to a
profile that does not exist (or a named binding with no name) appends a profile layer with
`Servers: []` and emits a `Diagnostic`. `FromRegistry` never falls back to the active profile — that
would turn deleting a profile into a silent widening. Diagnostics are part of `EffectiveScope`;
`session show` and `doctor` print them. `PinnedProfileLayer` mirrors this for a credential-supplied
pin: an unresolvable name yields a block-all layer plus `ok=false`.

**`Sources.Extra` can only tighten.** It appends layers after the persisted ones so a credential with
no registry entry can join the same intersection (the daemon's HTTP data plane folds an agent token's
server allowlist and profile pin in here). They are ordinary layers that `Merge` treats no differently,
so Extra has no shape that widens visibility. Its contract: the returned layers must be a **pure
function of session id + registry generation**, because they do not enter the cache key — a source
varying independently of those two serves a stale scope until the next invalidation.

**A front end asking "which discovery mode does this profile get" goes through `DiscoveryFor`, never
through its own copy of the rule.** `profile ls` asks the same question a session does, so both callers
share `pickDiscovery` and the same layer construction;
`TestDiscoveryForMatchesTheResolvedSession` asserts they agree for every combination. `DiscoveryFor`
deliberately does **not** know `discovery.DefaultMode`: "no layer set one" comes back as `ok=false` and
the caller applies the built-in, so the default stays in the package that owns it. `ServerToolsLayer` is
exported for the same reason — `server tool ls` merges exactly that layer rather than a second filter
written beside it.

Note the asymmetry: `PinnedProfileLayer` carries a profile's servers and tools but **not** its
discovery, so a token-pinned profile is presented in whatever mode the rest of the chain resolves to.

**`EffectiveScope.Hash` covers every field except `Generation` and `Hash` itself.** `Generation` records
which registry state the value came from; it is not content identity and is stamped after the merge. The
hash uses a length-prefixed canonical encoding with map keys visited in order, so it is stable across
processes and Go versions, and a golden test pins it. `Changed` compares only the `Hash`: only a content
change is worth pushing `tools/list_changed`, otherwise one registry rebuild amplifies into a
notification storm.

**Better to over-invalidate than to under-invalidate.** `EvRootChanged` clears one session;
`EvRegistryChanged` and `EvCatalogChanged` clear everything, and **an unknown event kind also clears
everything**. The catalog is not in the cache key, so `EvCatalogChanged` is the only channel through
which a downstream tool-set change becomes visible; lose it and stale scopes are served forever.
Over-invalidating costs a recomputation; under-invalidating costs emitting the wrong visibility.

**Refuse to resolve without a registry snapshot.** `Resolve` errors when `src.Registry()` returns nil
rather than conjuring an "empty but legal" scope. A nil `Catalog` function, or one returning an empty
catalog, resolves to zero visible servers — also the closed direction.

**`SessionKey.Root` survives as a field, but no persisted layer reads it.** The stdio gateway still fills
it from the first MCP root the client reports (`cachedPrimaryRoot`, `gateway/derive.go`) because
`internal/downstream` derives per-root server instances from it. `EvRootChanged` is kept for the same
reason it clears only one entry: dropping one is cheap, and letting each caller decide which notices
matter is how a stale scope gets served the next time something does depend on the root.

**`NormalizePath` never canonicalizes and never touches the disk.** Four pure-string operations:
backslashes to `/`, collapse repeated slashes (a leading `//` for UNC survives), strip the trailing
slash (a bare `/` survives), lowercase Windows-shaped paths wholesale. A client-reported path may not
exist on this machine, so symlink resolution or existence probing would both fail and introduce TOCTOU.
It must stay idempotent — it is applied repeatedly to output it has already normalized. `PathIsWithin`,
the longest-prefix matcher the retired project layer used, was **deleted with that layer**: an exported,
tested helper nothing calls reads as a supported entry point, and the next caller would inherit a
failure direction chosen for a job that no longer exists.

**followActive reads the snapshot, not a state file.** `activeProfileName` returns
`snap.Governance.V.ActiveProfile`; it was once hardcoded to `""` while `agenthub profile use` wrote the
name into a separate state file, so the mark could be set and listed but no session ever applied it.
Reading it off the snapshot both makes followActive follow and keeps `FromRegistry` pure. Unset returns
`""` (no narrowing, matching `profile use -`); an unresolvable name goes through the same
dangling-reference path as a named binding, which is what keeps it fail-closed.

---

## internal/session

The daemon-side session registry: it mints identities and tracks liveness. What a session may see is
resolved from the registry every time it is asked, so there is nothing here to mutate and no way to
change a live session's surface.

### Invariants and failure directions

**Two identity shapes, each for a different reader.** The human-facing short ID `"client:seq"` (e.g.
`claude-code:17`), seq monotonic per client and **never reused** within the daemon's lifetime; and the
protocol-facing 128-bit random token (`Mcp-Session-Id`). stdio sessions have no token (all zeros);
`TokenHex()` returns `""` for them and `MatchToken` always returns false.

**Token comparison must be constant-time, and anomalous input is denied.** `MatchToken` returns false
outright for three cases — not an HTTP session, hex decode failure, wrong length — and only then reaches
`subtle.ConstantTimeCompare`. `FindByToken` compares in constant time per candidate and returns
`(nil, false)` for unknown or malformed tokens.

**No entropy, no existence.** `OpenHTTP` errors when `io.ReadFull` fails and never mints a session with
an under-filled token.

**A re-registering gateway always gets a new identity.** seq is never reused, so a reference to the old
session must break rather than quietly rebind to a different connection.

**Only HTTP sessions are reaped by TTL** (24h default, 5-minute sweep). A stdio session's lifetime is the
gateway process's; the reaper skips them explicitly.

**Root is a mutable attribute, not part of identity.** `SetRoots` updates it on `roots/list_changed`; it
was never in the session ID.

**Derivation keys and scope live on two different planes.** Nothing in `derive.go` touches a scope type
and `DeriveKey` enters no scope hash: narrowing a session must not restart a process, and switching
instances must not change a visible tool name. `DeriveRoot` **deliberately returns an empty key** for a
session with no root (use the base instance) rather than degrading to a key built from the session ID —
that would hand a rootless session private state the operator meant to isolate per project, and spawn one
process per rootless session. A multi-root session takes the **first** reported root rather than a digest
of the set, because this key is the vault scope name an operator administers credentials under and has to
stay readable.

**Cascading close only takes down instances keyed by session.** A root-keyed instance is shared by
construction with every session on that root, so tearing one down here would cut a neighbour's
connection; those are left to the pool's idle TTL. Worst case an instance lives 30 minutes too long, not
a call arriving one instance too late.

---

## internal/event

The daemon's in-process notification bus, plus two mergers over one implementation: `NewCoalescer`
anchors its window at a key's **first** `Add` (throttle, bounded latency, 50ms default); `NewSettler`
**resets** the window on every `Add` (debounce, collapsing a lifecycle into one terminal event, 750ms
default). `internal/ctlapi/sse.go` consumes both — server list changes through the coalescer, scan-type
topics through the settler.

Not to be confused with `internal/eventlog`, a separate package holding the persisted event **record**
— see [foundation.md](foundation.md).

### Invariants and failure directions

**`Publish` never blocks — that is the reason this package exists.** A full subscriber buffer means the
event is dropped and counted, never that the publisher stalls. Consumers must treat the bus as a **change
notification channel**, not a **change log**: when `Dropped()` is non-zero (or after a reconnect) the
consumer re-reads authoritative state. Losing a notification is recoverable; blocking the publisher is
not.

**The ordering of unsubscribe and channel close is an invariant.** `Close` removes the subscription under
the write lock and **only then** closes the channel; `Publish` sends only under the read lock, so no send
can race the close. `Close` is idempotent.

**The payload is the same value for every subscriber and must be treated as immutable.**

**A merger's payload is built lazily and exactly once.** Only the **last** builder passed to `Add` is
invoked, at fire time, so a burst of K occurrences pays for an expensive payload once. Builders run on the
timer goroutine (or `Flush`'s caller) holding **none** of the merger's locks, so they must capture state by
reference or be cheap closures.

**The reset race in settling mode is solved with a `gen` counter.** A timer that has begun firing cannot be
stopped, so every re-armed timer captures the `gen` at that moment and `fire` ignores stale callbacks.

**The generation sequence lives on the MERGER, so no two armed timers share a number.** A per-entry
counter answered the reset race and not the same race across `Flush`: a fresh entry started at zero, a
coalescer entry is never reset so its stranded callback held zero too, and the two collided — a key's timer
begins firing, `Flush` detaches and emits that entry, the key is `Add`ed again, and the stranded callback
matches the NEW entry and fires it a window early. A monotonic sequence on the `Merger` makes a stranded
callback match nothing but the timer it was armed for, and subsumes the reset case rather than replacing
it. Both are pinned by tests that run the timer by hand: the interleaving is the subject, and a sleep would
test the scheduler instead.

**`Close` discards pending events; `Flush` fires them.** Dropping one merged notification at shutdown is
acceptable — the bus contract already requires consumers to re-read after a loss. After `Close`, `Add` is a
no-op.

**Standard library only.** This package sits beneath every business package and must stay
dependency-free.

---

## internal/secrets

The credential vault: a four-level chain over environment variables, an XChaCha20-Poly1305 encrypted file
and the OS keyring, every entry addressed by `(ServerID, Scope) + Key` with `Scope` defaulting to
`_global`.

### Invariants and failure directions

**Four levels, first hit wins. An empty or whitespace-only value counts as "unset" at the two
environment levels** — `envValue` trims before accepting, so an exported-but-empty variable falls
through rather than shadowing the vault.

| Level | Source | Activation |
|---|---|---|
| 1 | environment variable `AGENTHUB_SECRET_<KEY>` | always |
| 2 | bare environment variable `<KEY>` | explicit opt-in `AGENTHUB_ALLOW_BARE_SECRET_ENV=1` |
| 3 | `secrets.enc` (XChaCha20-Poly1305) | `AGENTHUB_SECRET_KEY` set, or the dev-fallback pair of files already exists |
| 4 | OS keyring (zalando/go-keyring) | availability probe passes |

**Owed: levels 3 and 4 do not apply that trim, and the difference is not only cosmetic.** `Get` returns
whatever the enc file or the keyring holds, and `Set` accepts any value — `Ref.Validate` covers the
server id and the key, never the value — so a stored empty string is reported as **present** and shadows
the keyring level beneath it. The consequence that matters is caught one layer up rather than here:
`downstream.expandSecrets` treats `!ok || val == ""` as unresolved, which is what stops a header
expanding to nothing and turning an authenticated endpoint anonymous. That guard is **narrower than the
rule above** — it tests `== ""`, not a trim — so a whitespace-only vault entry is refused at levels 1–2
and delivered at levels 3–4. Owed rather than fixed here because tightening `Get` changes what `secret
get`, `server inspect` and the control plane answer for an already-stored value: a behaviour change with
a blast radius, not a tidy.

**Level 2 being off by default is fail-closed:** no arbitrary environment variable is a credential unless
the user asks. Even when on, `envValue` **never** resolves a variable starting with `AGENTHUB_` through the
bare path — the opt-in must not become a way to read out our own control variables. Related reserved-name
collision: an entry named `key` would map to `AGENTHUB_SECRET_KEY`, the key material for the encrypted
file, so `envValue` skips that name explicitly. Key material must never be readable through the credential
chain.

**"Couldn't read it" and "read something broken" must be distinguished.** A file that will not decrypt, or
a keyring reporting anything other than not-found, is raised as an **error** — never a miss carried further
down the chain. A mistyped `AGENTHUB_SECRET_KEY` or a broken keychain must be visible, not degraded into
"that credential isn't set". The **only** exception is a keyring whose availability probe fails: that
machine has no such level, so it is skipped without error and writes land in the encrypted file instead
(A.6 #5).

**Three keyring hardening measures, none optional.** The availability probe **reads only, never writes** — a
`Set` probe triggers the destructive macOS confirmation dialog; it reads a well-known nonexistent account, and
both success and `ErrKeyringNotFound` prove the backend alive, while a timeout or any other error marks it
unavailable. The probe's conclusion is **cached for the process lifetime**, so an unavailable keyring flips
the chain to the encrypted file without re-prompting per call. And **every operation has a hard timeout** (3s
default), after which the worker goroutine is **deliberately abandoned** — a stuck keychain prompt cannot be
cancelled, so abandoning it is the only way to unblock the caller; the result travels over a buffered channel
so an abandoned worker can never collide with the caller's return value.

**The OS keyring cannot be enumerated, so there is a self-managed key registry.** `keyring-keys.json`
mirrors **key names only**, never values, and is **modified only in sync with a successful keyring
mutation** — so it neither claims keys the keyring has lost nor misses keys it still holds.

**The dev-mode fallback is an explicit ruling (A.6 #5), not laziness.** Every `go build` produces a new
unsigned binary and the macOS keychain ACL re-prompts each time, so a failed keyring probe (or
`AGENTHUB_DEV_SECRETS=1`) sends writes to `secrets.enc` under a key auto-generated and persisted **beside
it** in `secrets.enc.key` (0600). Say the quiet part out loud: a key stored next to the ciphertext is
obfuscation, not encryption at rest, and an attacker who reads both files has the plaintext. Acceptable for
the dev fallback only. `encForRead` requires **both** files to exist before using that key, so data written
by the dev backend stays readable once the keyring probe passes again.

**Persistence discipline for the encrypted file.** The whole map is sealed under a single random nonce; the
AAD is `"agenthub/secrets/v1"`, binding the ciphertext to the format version so a v2 envelope can never be
replayed as a v1; writes go through the atomic ladder (temp file in the same directory, 0600, write, fsync,
rename, fsync parent). A missing file is an empty map, not an error.

**`storageKeyPrefix = "agenthub/v1"` is frozen and golden-tested.** Changing it orphans every stored
credential. Within a component only `%` and `/` are percent-escaped — the only two bytes that break the
delimiter structure — so `secret ls` stays readable. `ParseStorageKey` errors on an unknown prefix or
malformed escaping rather than dropping a key it cannot decode from an enumeration.

**`HasUnreadableEnc` exists so an exhaustive caller can fail loud.** With an enc file on disk this process
has no key for, `List` silently returns only the keyring half, and an empty answer is indistinguishable
from an empty vault — a credential purge built on it reports success while a refresh token survives, and
re-adding the same server id revives it. Any doubt, including a stat error, answers **true**: a spurious
warning costs nothing next to a silently retained credential.

**Migration is explicit, ordered, and backend-level.** `Migrate` goes read old → write new → read back and
verify → delete old, and a failure at any step leaves two copies: a duplicated credential is recoverable, a
lost one is not. It **must** be passed backend-level stores (`Chain.Backend`), never a `*Chain`:
`*Chain.Get` consults environment variables first, so one environment variable could satisfy the read-back
while the new backend stored nothing — and passing verification is exactly what deletes the old entry
(`TestBackendIgnoresEnvironmentLevels` pins this). `Chain.Backend` resolves availability **eagerly**,
returning `ErrBackendUnavailable` on the spot: discovering halfway through that the destination cannot be
written is how a half-migrated vault happens. It stays a command rather than automatic behaviour because
backend availability changes underneath the operator, and moving credentials nobody asked to move is worse
than leaving them — the chain resolves from the old backend right up until it becomes unavailable one day and
the credentials appear to vanish. The environment levels have **no** `Store` and are not in `BackendKinds()`:
they are per-process input, not storage, so nothing can migrate into or out of them.

**Concurrency: two locks, in one order.** `Chain` serializes its own operations with an in-process mutex,
and every **write** additionally takes a cross-process lock — a dedicated `vault.lock` in the secrets
directory, held across the whole read-modify-write cycle (ruling A.3 #1). The in-process mutex is taken
**outside** the file lock, so goroutines of one process queue in memory and only one ever competes for the
file; the reverse order would have each open its own descriptor and contend through the filesystem for no
gain, since `flock` is per-open-file-description and `LockFileEx` per-handle.

**All six write paths take it**, not just the two the API leads with: `Chain.Set`, `Chain.Delete`, and the
four backend-level methods behind `Chain.Backend`. The backend stores need it most — their caller is
`Migrate`, which writes the destination, reads it back to verify, and only then deletes the source, so a
racing writer that clobbers the destination in between turns a verified handover into the deletion of the
last remaining copy.

**The lock covers key selection, not only the map update.** `encForWrite`'s dev branch calls
`loadOrCreateDevKey`, a read-then-create of `secrets.enc.key`; two processes reaching it unguarded each
generate a key and the second overwrites the first, leaving an enc file **neither can open** — the whole
vault rather than one entry, and a different failure from a lost update.

**A dedicated lock file, never the data files.** `secrets.enc`, `keyring-keys.json` and `secrets.enc.key`
are all replaced by `rename`, so a lock on one of those inodes guards nothing: the winner renames a new file
over the path and the two processes end up holding locks on different inodes. `internal/ratelimit` reached
the same conclusion for the counter file.

**Reads and the announcement stay outside it, deliberately.** Writers publish by rename, so an unlocked
reader sees one whole version or the next, never a splice — and `Get` sits on the hot path of every
`${SECRET_X}` expansion, which is exactly where `Announce` refused a cross-process lock for the
credentials-rev hint. A write is not a hint, so it takes the lock; the hint still does not.

Failure direction: **fail closed**. Every acquisition failure — including a build with no `flock`
implementation — returns without the lock and the write reports that it could not run, rather than
proceeding unserialized. A write that says it did not happen is recoverable; a credential that silently
vanished is not. `internal/secrets/multiproc_test.go` is the N-process acceptance test A.3 #1 requires, and
both of its cases were run against a build with the lock removed: the first loses entries, the second
reports `cannot decrypt secrets.enc`. The sibling `<server>.refresh.lock` in `oauthflow` is a different lock
for a different job — it serializes **refreshes of one server**, upstream of the write that lands here.

**Tests never touch the real keychain.** The keyring sits behind the `Backend` interface with fakes injected
everywhere; the real-backend smoke test runs only under `AGENTHUB_TEST_REAL_KEYRING=1`.

---

## internal/secrets/secureenv

Pure functions building a hardened environment for a downstream process about to be spawned: allowlist
admission, login-shell PATH capture, proxy-variable userinfo redaction.

### Current assembly status — only the PATH half is wired

`LoginPATH` and `MergePATH` are called from `internal/downstream/spec.go`'s `widenPATHIfNeeded`, so a stdio
child whose command cannot be found under the PATH it would be given is retried against the login shell's —
the fix for a GUI-launched daemon unable to spawn `npx`. A PATH that already resolves the command never
triggers a capture.

**`Filter`, `Config`, `RedactProxyValue` and `CaptureLoginPATH` have no caller outside this package's
tests**, so everything the next section describes is a capability that exists rather than a rule in force.
What a spawned downstream actually receives today is the parent environment minus the `AGENTHUB_` prefix,
stripped by `internal/downstream`'s own `buildEnv` (`spec.go`, `envPrefix`) — a deny list, which is the
opposite shape from the allowlist below. Recorded rather than fixed because admitting only the allowlist
changes the environment of every spawned server, which can break a downstream that reads a variable nobody
enumerated: a behaviour change with a blast radius, not a tidy.

### Invariants and failure directions

**Deny by default** — *of `Filter`, which nothing calls yet; see above.* Anything not explicitly allowed is
dropped; there is no "everything but a blocklist" mode. **The `AGENTHUB_` prefix is a hard deny that
`Config` cannot override** — our own control variables must never leak downstream. This one IS in force,
because `internal/downstream` strips the prefix itself; the two were designed to stack idempotently, and
today only the second is running.

**Proxy variables are not forwarded by default** — proxy endpoints frequently embed credentials. With
`ForwardProxy` on, values go through `RedactProxyValue`: `NO_PROXY` is a plain host list and passes
verbatim, a value with no `@` passes verbatim, and a value containing `@` that **cannot** be positively
identified and stripped as URL userinfo (a scheme-less `user:pass@host` parses into the opaque part) is
**dropped outright**. We never forward a value we cannot prove is credential-free.

**`LoginPATH` is the one place in this layer that is deliberately fail-open.** Processes launched by
launchd/systemd inherit a truncated PATH, and the login shell's PATH is the one an interactive user
actually has (this bit mcpproxy three times). Capture takes the **last** non-empty line of output (a profile
may print a greeting first), with a 5-second hard timeout across both modes and `cmd.WaitDelay = 1s` to
force the pipes closed — otherwise the login shell's children inherit the stdout pipe and `Output` blocks
until every descendant exits, even after the context kills the shell. Any failure falls back to the current
process's `PATH`: a broken login shell must not block a spawn, and the worst case is keeping the truncated
PATH we already had, never less. Cached with `sync.Once`, once per process.

**`-l` alone is not enough, which is why `captureModes` is a list**: `-i -l -c 'echo $PATH'` first, plain
`-l -c` as fallback. A login shell sources only the login profile, while the line that puts Homebrew, nvm or
pyenv on PATH conventionally lives in the **interactive** rc file — so the directory holding `npx` is exactly
the one `-l` does not find. Measuring this from a terminal proves nothing: `zsh -l -c 'echo $PATH'` prints a
complete PATH only because it inherited the interactive shell's. The launchd case has no such parent and is
the only case this code runs in. `-i` stays fallible rather than assumed (a shell refusing to be interactive
without a tty falls through to `-l`), and a cancelled context breaks the ladder instead of spending the
caller's remaining budget on a second timeout.

**`MergePATH` appends and never reorders.** `base` is preserved byte for byte and the directories of `extra`
it does not already list are appended in `extra`'s order, so the result is a strict superset in which every
command that already resolved under `base` resolves to the same file. That is what lets a caller apply it
unconditionally rather than behind a guess at whether the current PATH "looks truncated". Empty entries in
`extra` are dropped — POSIX reads one as the current directory, which a login profile should not be able to
add to a spawn — while an empty entry already in `base` is left alone, since removing it would change what
`base` resolves.

---

## internal/clients

Adapt AI client config file formats: detect where they are installed, write the agenthub gateway entry in,
safely take it back out, report what is actually there.

### A shape-driven adapter table

Behaviour is driven by the **shape** of the config rather than by a hand-written branch per product.

| Shape | Meaning | Rows | Clients |
|---|---|---|---|
| `ShapeServerMap` | A JSON file with `{"mcpServers": {...}}` at the top level | 7 | claude-code, claude-desktop, cursor, windsurf, cline, roo-code, gemini-cli |
| `ShapeNested` | The same name→entry map, buried under a key path inside a larger document | 2 | vscode (`mcp.servers`), zed (`context_servers`) |
| `ShapeTOML` | A TOML document, **detect only, never rewrite** | 1 | codex |
| `ShapeYAML` | A YAML document, **detect only, never rewrite** | 1 | continue |
| `ShapeRemote` | No config file on this machine at all | 1 | open-webui |

Twelve rows. Adding a client is one more row in `table.go`, not one more code path; `Shape.Writable()` is
true only for the two JSON shapes. A row's `locs` is ordered **project first**, but that is **read
priority** (on a duplicate name the project-level definition wins), **not** a write preference — see
`DefaultPlacement` below. `locSpec.home` is a GOOS-to-path map, and a missing GOOS makes that location
unavailable on that platform rather than guessed; no build tags are involved.

**Every row's `home` map answers for Windows too**, through one of three builders: `sameOnAll` for the
CLI-shaped clients whose dotfile sits in the profile identically on all three platforms, `perOS` where the
BASE differs and not just the segments, and `vscodeUserDir` for the extension-hosted ones. Keeping the two
conventions apart is the point — copying one onto the other is how a write lands somewhere the client never
reads. Zed is where it bites (`.config/zed` on macOS and Linux, `%APPDATA%\Zed` there), and `%APPDATA%` is
read from the environment rather than rebuilt under the home directory. Claude Desktop carries the one
platform-specific branch of its own (`clientSpec.redirect`), because an MSIX install reads a VIRTUALIZED
`%APPDATA%` and the documented path is one the packaged application never opens — writing there would
parse, verify, and never be read. `TestWindowsUserPathsAreTheOnesClientsRead` and
`TestMSIXClaudeDesktopIsPreferredOnlyWhenItExists` pin both halves from a non-Windows host.

**None of it has run on Windows hardware**, which is the status [windows.md](../windows.md) owns — it names
`client connect` writing a file the client actually reads as an open verification item.

### Invariants and failure directions

**Default writes go to the user level (`DefaultPlacement = User`).** The entry carries **the absolute path of
this machine's agenthub binary**, and project-level files are meant to be committed, so defaulting to project
would commit a path that holds only on your own machine; agenthub is also one hub shared by every client on
this machine, not something re-wired per repository. **Which servers a client can see is decided by
`internal/scope`, never by which file the entry was written into.** A row with no user location on this
platform (or an unresolvable `$HOME`) falls back to the first location, which keeps every Windows row
writable.

**An explicitly specified placement is either honored exactly or refused.** `PathFor` returns `""` for a
client lacking the location and callers error on that; they **never** redirect the write elsewhere, because
writing the gateway entry into a file nobody named is worse than an up-front refusal. `--path` plus
`--placement` is a usage error, not a place to invent a precedence.

**`DisconnectDefault` is the backstop for the default write target having moved.** A disconnect with no target
checks the default first and **only** then, if no agenthub-owned entry is there, this client's other location
— entries written before the default moved to user level still sit in `.mcp.json`, and "the entry is obviously
still there but we report not connected" is the least acceptable answer. It is not a search. A fallback
location that fails for another reason (unparseable, oversized, denied) **returns that error** rather than
being skipped: a file agenthub refuses to touch must not be reported as "nothing in there". A call that named
a path or placement does not take this route — an explicit target is an instruction, not a starting point.

**macOS TCC: `Detect` only stats, never reads.** Reading another application's data directory triggers the
privacy dialog, and a bulk scan popping a dozen of them is worse than not scanning. Content reading happens
only in `Inspect` and `Import`, single-client user actions where the dialog is explicable.

**"There is no such file" and "you're not allowed to look at this file" are never conflated.** A denied access
becomes a `*PermissionError` with remediation text and `HTTPStatus()` 403, not 404 — the two cases call for
opposite user actions. `classifyAccess` classifies as denied only on `errors.Is(err, fs.ErrPermission)`, so no
ambiguous I/O error gets dressed up as a TCC prompt.

**A parse failure must error and must never destroy.** A file that exists but will not parse aborts the whole
operation with a `*ParseError`, leaving the file untouched. JSONC counts as unparseable on the non-splice
path, and the error carries the specific JSONC diagnostic — the most common reason a real `settings.json`
fails, where "invalid JSON" would read like a bug. Every `*ParseError` carries a hand-pasteable snippet.

**Anything over `MaxConfigSize` (64 MiB) is refused before any read at all.** The stat size is checked first,
and `readLimited` catches it again with an `io.LimitReader` in case the file grows in between.

**Unknown fields and foreign entries are preserved byte for byte** — every level from the document root down
to the server map is held as `map[string]json.RawMessage`.

**Backups are centralized, not in-place.** The original goes to `<data>/backups/clients/<client>-<ts>Z.json`
(0600, rotated per `DefaultKeepBackups = 10`) rather than a sidecar: a project-level `.mcp.json` lives in a
git working tree, and a `.mcp.json.agenthub-backup` beside it would dirty `git status` on every connect and
risk committing someone else's credentials. 0600 because a client config's env block frequently holds API
tokens. Backups use `O_EXCL` plus a suffix loop for same-microsecond collisions; **rotation is best-effort**,
since failing to delete an old copy must never fail a connect whose new backup already landed. **If the backup
cannot be written the whole operation fails and the target is untouched** — modifying a user's config with no
recoverable copy is worse than not connecting.

**`Disconnect` identifies by ownership, never by name.** `ownedBy` requires the entry's args to contain both
the `connect` subcommand and a `--client` value equal to this client's ID. An entry that merely happens to be
named `agenthub` is **not** ours; an entry the user renamed that still points at our gateway **is**. The
"identify by shape, not by name" rule, inherited from toolport's repoint.

**Writes are atomic and preserve the original permissions.** New files are created 0644 (a project-level
`.mcp.json` is meant to be committed, unlike registry documents at 0600); existing files keep their own mode.
A result byte-identical to the current content returns `Changed: false` and does not write, so repeated
connects are idempotent. Directory fsync is best-effort — a project directory may live on a filesystem that
refuses it, and the rename is atomic regardless.

**Only rewrite documents that round-trip losslessly — the rule is about RE-ENCODING.** TOML/YAML re-encoders
drop comments, key order and anchors: a config-destruction machine wearing a helpful hat. Those clients get
detection plus one precise manual snippet, and `Connect` **fails loudly** with the snippet rather than
half-working. `probeFormat.Disconnect` refuses for the mirror reason — agenthub never wrote anything there.

**Delegation: agenthub does not re-encode the document, it asks the tool that owns the format.** A row may
carry a `delegate` (codex does), and `Connect`/`Disconnect` then run `codex mcp add|remove` instead of
printing advice — a connect that changes nothing is not a connect. Three properties make it delegation rather
than a shrug: the file is **backed up first**, exactly as agenthub's own writes are; the result is **verified
by re-reading it**, so a CLI that exits 0 having written nothing is a failure; and a delegate that is absent,
fails, or leaves the wrong state **falls back to the instructions** rather than reporting a success nobody
checked. Execution is refusable per invocation (`--manual`) or per machine (`AGENTHUB_NO_CLIENT_CLI=1`), and
the environment can only ever **forbid** it — a variable that could switch execution back on for a caller that
passed `NoDelegate` would let a program run other programs behind its caller's back.

**JSONC is spliced, not re-encoded.** Zed's `settings.json` ships with a comment header and VS Code's is JSONC
by convention, so the default install of both was a client agenthub refused to touch. `jsonc.go` reads them
with a comment-blanking pass — comments and trailing commas become spaces of the SAME LENGTH, so offsets in
the blanked copy are offsets in the original — and writes by replacing the bytes of agenthub's own entry and
nothing else, which is why the user's comments, key order and indentation survive.

**The safety does not come from the locator being right; it comes from proving the result.** `verifySplice`
runs before anything reaches the disk: the edit must parse, must differ from the original in exactly the
entries agenthub meant to change (deep-compared with those entries removed from both sides), and must carry
byte-identical comments. Any doubt leaves the file untouched. A shape the locator cannot walk — a section key
that is not an object, a root that is not an object — is refused rather than guessed at. Both hand-written
passes are fuzzed (`FuzzBlankJSONC`, `FuzzSpliceEntryKeepsEverythingElse`).

**A duplicated key resolves to the LAST occurrence, because that is the one the file's owner reads.**
`encoding/json` keeps the last and so do the client applications, but the locator once walked to the first, so
an edit landed in a section nobody reads — invisibly, because the document came back byte-identical and
`verifySplice` could not object when both of its decodes went through `encoding/json` and agreed with each
other. This is the one case where proving the result cannot save a wrong locator, which is why the rule lives
here rather than in the verifier.

**That rule is about writing; reading is a separate power.** A row may carry `readTable`, and codex does:
`scanTOMLServers` answers "is our entry in here?" for `~/.codex/config.toml` while `Connect` still refuses it.
Refusing to read bought nothing — it made `client ls` say "?" for a plainly connected client, and made doctor
assert "no agenthub gateway entry" about a file it had never opened. The scanner models exactly
`[mcp_servers.NAME]` tables and the four keys agenthub uses, and **refuses the whole document** on anything
else (array-of-tables, an inline `mcp_servers = {...}`, an unterminated string, an escape outside TOML's set).
`ok=false` is the contract: callers report "unknown", and nothing this scanner cannot read is ever converted
into "the entry is not there". Scanned entries are re-rendered as the JSON shape the package already speaks
and go back through `summarise`/`ownedBy`, so ownership has one implementation for every format. It is fuzzed
(`FuzzScanTOMLServers`), which caught it accepting `"\0"` and inventing a `0` that was not in the file.

**`locationFor`'s match order guarantees the section is deterministic**: exact path equality, then equal
basename (what makes `--path /tmp/x/settings.json` behave like a real settings.json instead of silently
picking a different section), then this client's primary location. A path that does not match **never** guesses
at another client's shape.

**`ConnectState` fails loud, and that is the whole point of it.** "Is agenthub wired into this client?" has
five answers, not two: `connected` (some location holds an entry agenthub itself wrote — decided by ownership,
never by name), `not_connected` (every location was opened and understood, none had one), `denied`,
`unreadable` (there, but agenthub refuses to interpret it), and `unknown` (nobody looked: a probe-only shape,
or a caller that asked not to read). A positive finding wins outright; after that the loudest doubt wins, so a
location agenthub could not see never degrades into "not connected". It returns the placements holding the
entry alongside the state, because "connected in the user file while the project file still holds one" is
exactly what a disconnect has to know about.

---

## internal/skills

An agenthub-owned, content-addressed skill library, its materialized copies inside AI client directories
(and the receipts for those copies), and a protocol face serving the library upstream as read-only MCP
tools.

### The two-layer model and "honest granularity"

An MCP server is a runtime intermediary and agenthub sits on the call path, so visibility can change per
session. Skills are the opposite: clients read them straight off the filesystem and agenthub is **not** on
the read path. That forces two layers — **the store** (agenthub's canonical copy, content-addressed at
`<skills>/store/<id>/<contentHash>/` and indexed by `skills.json`, the only source of truth) and **an
install** (a *receipt* in `installs.json` recording that this skill was materialized once, for this client,
under this scope; receipts go stale, so every one must be verifiable, repairable and **never blindly
trusted**).

Which yields this package's single most important sentence, written into the `Granularity` field of every
return value: **file materialization can only achieve client granularity, never session granularity.** Once
bytes are on disk, every session of that client sees them, and agenthub cannot retract a file for one session
while keeping it for another. Per-session skill visibility can only go through the skills-over-MCP path
(`mcp.go`), where agenthub actually is on the read path. `GranularityClient` is echoed back in every result
precisely so the CLI and GUI are forced to state the limitation rather than imply a precision that does not
exist.

**`<id>` is a path segment, so its shape check is a path safety check.** `validID` accepts 1–64 lowercase
ASCII letters, digits and single inner dashes — exactly what `slugify` mints — and refuses rather than
sanitizes, because a sanitizer must be right about every escaping form while a shape check need only be right
about one. (An explicit `--id` was once stored verbatim, so `../../../some-dir` escaped the store on both the
copy and the delete.) Three exclusions are load-bearing: separators and `.`/`..` fall outside the character
set; uppercase, because two IDs differing only in case share one directory on a case-insensitive filesystem
while the index counts them as two skills; and the empty string, because it collapses the join onto the store
directory itself, which a removal would then delete whole. The two **deleting** paths (`Remove`,
`pruneVersions`) re-check rather than trust the index they read the ID from — dropping a library entry while
leaving its files is recoverable, deleting the wrong tree is not.

### The two write strategies

**`StrategyOwnedDir`: agenthub owns the entire directory and can rebuild it from scratch.** Ownership is
proven by the marker file `.agenthub-managed.json` (`MarkerFileName`) **and only by it** — path conventions,
naming patterns and receipts are all things a user might reproduce by coincidence, whereas an explicit marker
cannot be produced by accident. A directory without our marker is somebody else's, always reports
`StateConflict`, and is **never absorbed**. `applyOwnedDir` **rebuilds** rather than merges, so stray files
from an older version or a half-finished write cannot survive; the ordering is deliberate — the marker is
checked **before** deletion and written only at the end, so a crash mid-write leaves a directory that verifies
as `Drifted` (repairable) rather than one that looks complete.

**`StrategySentinelBlock`: agenthub owns the span between BEGIN/END inside someone else's file.** The marker
strings (`<!-- agenthub:skill:<id>:start -->` / `:end -->`) are **frozen**: changing them orphans every block
agenthub has ever written, and an orphaned block is indistinguishable from user content and would be left
there forever. Bytes outside the sentinels are preserved verbatim, with the **one** exception documented on
`upsertBlock`: appending to a file that does not end in a newline adds one, so the start marker gets a line to
itself.

`findBlock` is the safety valve for the whole strategy: anything other than "exactly zero" or "exactly one
well-formed pair" (unpaired, inverted, duplicated) returns a `*SentinelError` and the caller **must** refuse
to write. Broken markers mean we can no longer tell which bytes are ours, and the only safe action is to stop
and say so — overwriting on a guess is how a "managed block" tool eats someone else's file. `SentinelError`
satisfies `errors.Is(err, ErrConflict)` because the failure direction is identical. Import applies the
source-side half of the same guard: a package whose name, description or SKILL.md body contains an agenthub
sentinel string is **rejected at the door**, because an embedded END marker would truncate its own block and
everything after it would silently become "user content" agenthub will never manage or remove again.

### ApplyState and decision precedence

Five states — `applied`, `stale`, `drifted`, `missing`, `conflict` — answering exactly one question, "are the
bytes still where we think they are", and deliberately not "is this content trustworthy". `verifyOne`'s order
is most actionable first, each level a different question:

1. Is there a shadowing file in the container (`TargetDef.BlockedIf`)? → `conflict`
2. Are the bytes there? → `missing`
3. Are they ours (owned-dir: marker file; sentinel: block present and well-formed)? → `conflict`
4. Does the content match the receipt? → `drifted`
5. Does the library entry still exist, and is it unchanged? → entry gone: `conflict`; library moved on:
   `stale`; otherwise `applied`

Ruling "library entry gone" as `conflict` rather than `missing` is deliberate: something deleted a skill
without deleting its install, and automated writing has to stop and wait for a human.

### Invariants and failure directions

**Every read-modify-write goes through `withState`; there is no second path.** N gateways plus the daemon plus
the CLI all mutate this state, so multi-writer discipline is a necessity rather than an optimization. A
cross-process flock on a single `.lock` guards the whole skills directory, and the three state files are
loaded and saved as one unit under that one lock — every interesting operation touches at least two of them
(add writes the index and a pin; remove writes the index and receipts), and one lock makes cross-file
consistency structural rather than an ordering convention nobody can verify. Read-only callers take the same
exclusive lock: operations are short, and correctness beats concurrency here.

**A corrupt state file always fails closed, and is never renamed out of the way.** A file that exists but will
not parse is a `*CorruptError`, the operation aborts, and the file **stays in place** — renaming it to
`.corrupt` would make the next read look like a legitimate brand-new store, which is exactly the silent
re-baselining an attacker wants. A *missing* file is what constitutes a brand-new store. Unreadable,
unparseable, trailing data, empty file (the atomic writer never produces one) and unsupported version all
count as corrupt. Four read retries absorb rename transients on the lock-free read path; a parse failure
surviving them is real corruption.

**A missing `enabled` field reads as disabled.** The persisted spelling is `Enabled`, not `Disabled`,
precisely so a hand-written or truncated record omitting it reads as **disabled** — for "should agenthub push
these bytes into a client directory", that is the closed direction. `Add` always writes the field explicitly.

**Fingerprints and pins: a mismatch refuses to propagate.** `Fingerprint` is `"v1:<sha256>"` covering content
**plus** metadata (name, description, kind), strictly broader than `ContentHash`. Description is included
because it is what the client's model actually reads when deciding whether to invoke a skill — identical files
with a swapped description **is** a meaningful change, and a classic prompt injection vector. `Version` and
timestamps are deliberately excluded: a version bump with unchanged content is not a content change, and
folding in timestamps would make re-importing identical bytes produce an unstable fingerprint. The
`HashSchemaVersion` prefix earns its keep — once the formula changes, old pins must be identifiable as
"different algorithm" rather than "content changed", or a formula upgrade presents as a fleet-wide alert and
users learn to ignore alerts.

**`requireTrusted` re-reads the library copy from disk**, ahead of `InstallTo` and `syncOne`, through the same
`verifyLibrary` full recomputation `Verify` uses: hash the files on disk, rebuild the fingerprint from what is
actually there, compare both against the recorded values. It once compared two values out of the same two
files (`pins.Pins[id].Fingerprint` against `sk.Fingerprint`), so a `SKILL.md` edited after pinning passed a
check documented as fail-closed and got spliced into a client's rule file, where the model reads it as its own
instructions. **An index a tamperer has edited cannot vouch for itself.**

Three fail-closed directions and one honest limit. **Mismatched entries refuse** (`TamperError`, carrying the
fingerprint recomputed from disk rather than the index's). **A copy that cannot be read or hashed refuses**
too (`ErrUnverifiable`) rather than reading as "nothing to compare" — that is the state an attacker can
arrange most cheaply of all. **Unpinned entries are allowed** (they predate the pin mechanism) but have still
had their content hash checked, so "unpinned" means "no baseline", never "unverified". The limit: a tamperer
who rewrites the library copy, the index AND the pin file consistently is not detectable here — all three are
files on the same disk, and no recomputation outranks a baseline the attacker also wrote — as is the window
between this check and the read at materialization.

**Pins are never deleted, not even by `Remove`.** A skill deleted and added back is compared against the
**original baseline** rather than blindly re-pinned: re-pinning on re-add is how a tampered copy launders
itself back into trust.

**Drift refuses to be overwritten unless a human explicitly decides.** A materialized copy modified by
something other than agenthub returns `ErrDrifted`, and the caller must pass `InstallRequest.AllowDrift`.
Drift is the user telling us something, even if what they mean is "I edited the wrong file"; silently rolling
it back is how a sync tool teaches users not to trust its own receipts.

**Import is this package's largest attack surface, and every rejection is non-negotiable.** `scanTree` rejects
symlinks of any kind (following one copies content from outside the package; preserving one points the
installed copy at an attacker-chosen path in the user's home), non-regular files, a `MarkerFileName` in the
source tree (that is the ownership credential for an install directory, so a package carrying one could forge
ownership), and trees over the size and count limits (a mistyped path should fail fast, not copy several GB).
Path escapes are structurally impossible — every path is derived by `filepath.Rel` from the walk root — but
the check stays, because the whole install layer trusts `FileEntry.Path` to be package-relative. `copyTree`
**re-hashes** each file as it copies and compares against the scan result, so a source that changed in between
aborts the import rather than producing a library copy whose `ContentHash` is lying (TOCTOU).

**`Options.ContentScanner` is the seam for injecting an injection scanner**, and a hit **rejects the import
outright** rather than importing and flagging — an imported skill is one `sync` away from being materialized
into a client directory. The hook lives in `Options` so this package depends on no guard package. **Nothing
sets it today**: the scanner it was shaped for went with the removed governance surface, so the sentence above
describes what the seam would do, not a check an import currently passes.

**An unreadable entry inside `hashDir` is an error, never a skipped file.** Silently skipping would let a
permissions trick hide drift. A symlink or device file appearing where a skill file belongs is drift by
definition, and gets a hash that can never match.

**`Sync`'s convergence semantics.** A conflict on one skill **never** aborts the batch: a shadowed file or a
hand-edited copy is recorded as one failed item while the other skills still converge, and only a store-level
failure returns an error. Pruning is the default, because "sync" means converge — but it happens **only within
the containers this request converged**: a sync of project A must never de-materialize project B, and the
receipt's `Container` field is what distinguishes them. `Disable` **does not** de-materialize anything by
itself: the bytes stay until a `Sync` or an explicit `Remove` converges the target, and until then the receipt
honestly reports their presence.

**The library's `Enabled` and scope's `SkillSelector` only narrow, never widen.** A disabled skill is not
materialized by `Sync` no matter what the selector says, and `SkillSelector`'s tri-state semantics are exactly
those of the scope chain's tool selector.

**The boundary of `Remove` and `Force`.** `Force` means "stop tracking", **never** "delete things we cannot
prove are ours": files on a conflicting target stay in place and only the receipt is discarded.

**`CharCap` measures the entire rendered file, not just our block** — the lesson of Windsurf's 6000-character
limit: what the client truncates is the file, so budgeting per block measures the wrong thing. Exceeding it is
ruled `conflict`, because silently writing a file the client will truncate produces a skill that "exists but is
broken", which is worse than one that "doesn't exist and is reported as such". **`BlockedIf` shadow detection**
is the lesson of `AGENTS.override.md`: a file the client prefers can make our write invisible, and an invisible
write paired with a healthy receipt is a lie.

**Both `renderSkillBody` and `renderSkillDocument` call out unmaterialized attachments.** A single shared file
cannot hold attachments and an MCP reply cannot hand over a directory; saying so in the rendered text is the
honest alternative to pretending the install is complete. Both are deterministic renderers pinned by golden
tests.

**The SKILL.md frontmatter parser is a deliberately restricted YAML subset, not a YAML implementation.** It
recognizes only single-line `key: value` (quotes allowed): this package cannot take on a dependency, and a
half-finished YAML parser that silently misreads nested structure is worse than one that admits it does not
understand a line. Lines it does not understand are **never discarded** — they are kept verbatim in
`Meta.Extra` and written back in their original positions, so richer frontmatter round-trips losslessly even
though agenthub reads four keys. No frontmatter is valid (the whole file becomes the Body); an **unclosed**
frontmatter is an error, because guessing where the metadata ends is how a whole document lands inside the
description field. For duplicate keys the **first** occurrence wins: taking the later one would let an
appended line silently override a value that has already been reviewed.

**Three versions are retained** (`keptVersions`): the current content-addressed version plus the two most
recent older ones. Old versions are what rollback and drift diffs read, and pruning to one saves space by
deleting evidence.

**Three properties of the skills-over-MCP face.** It is currently shaped as **tools rather than resources**:
resources are semantically the better fit, but the gateway's upstream face offers only tools, and inventing a
protocol face inside a subsystem package is the wrong place for it. **The host stays on the gate path**: this
type never answers on its own in any privileged way, and the gateway routes the call through the same
`pipeline.Execute` as a downstream call, so the scope and tier gates apply identically. And **enabled state is
verified live at call time**: `Tools()` serves a snapshot (it is invoked on every catalog build and cannot do
I/O), but `Call` re-reads the library, so a skill disabled or deleted since the last `Refresh` is rejected
rather than served out of a stale snapshot.

`NewProvider`'s snapshot **starts empty**: nothing is exposed until a `Refresh` succeeds, so a broken or
unreadable store broadcasts zero skills rather than a stale set. A failed `Refresh` retains the previous
snapshot (serving the last known-good set beats serving nothing because a lock was busy). Disabled skills are
**invisible** rather than "listed and then refused" — the same anti-probing rule scope narrowing follows. When
two IDs sanitize to the same tool name the first in sort order is kept and the rest skipped: a silently
shadowed skill is worse than a nonexistent one. `Annotations()` is **payload, not decoration**: the tier ladder
treats **missing** annotations as destructive (fail-closed), so a read-only tool without annotations would be
refused to a read-only credential.

**This package never shells out to git and never touches the network.** git sources are imported from a local
checkout the caller already has, and `--pin <rev>` is **recorded** (`Source.GitRef` / `Source.PinnedCommit`) so
the revision that produced the library copy is reproducible. Fetch, clone and ref resolution are **not planned
work that has slipped** — they are a capability boundary canonical.md §4 records deliberately, and until it
moves, an `Update` on a git skill without a new checkout path returns `ErrGitFetchUnsupported` rather than
reporting "already up to date" without having looked.

### Current capability boundaries

`ApplyState` landed at **five** values rather than one per failure: any target we are not allowed to write is
`StateConflict` (being occupied by someone else is only one of its causes), and a removed install has no
receipt at all, so it needs no state value. The targets table landed at **three rows**: claude-code as the
owned-dir reference implementation, cursor as the sentinel-block reference implementation, and generic to
prove the table extends without code changes.

The cross-process lock is implemented for darwin/linux (`syscall.Flock`) and Windows (`LockFileEx` via
`internal/platform`); any other platform gets a compile-time placeholder. The Windows half has never run on a
real Windows machine — see [../windows.md](../windows.md).

**Owed: two sentinels reach the CLI with no arm in `classifySkillsError`, so they exit 1 as `E_GENERAL`.**
That function (`internal/cli/skill.go`) is a `switch` over `errors.Is` whose `default` returns the error
unclassified — a shape that says nothing when a new sentinel is added beside the ones already handled.

| Sentinel | Today | Should be | Why |
|---|---|---|---|
| `ErrInvalidID` | 1 / `E_GENERAL` | 2 / `E_USAGE` | A rejected `--id` is an argument the user typed, and the frozen exit table gives row 2 as "arguments, unknown flag, unknown subcommand". |
| `ErrUnverifiable` | 1 / `E_GENERAL` | 6 / `E_GOVERNANCE_DENIED` | It is the second arm of the same fail-closed decision as `ErrTampered`, which maps to 6. A copy that mismatches its pin and one that cannot be hashed at all are the same refusal. |

Not fixed here because an exit code is a machine contract and the table is frozen: moving a case from 1 to 2 or
6 is the table owner's call. `ErrGitFetchUnsupported` is also unmapped and arguably right there — a feature
that does not exist is a general failure, and its message already names the remedy. `ErrSkillUnavailable` is
correctly absent: it is raised on the MCP face and handled there.

---

## Raised by the 2026-07-31 sweep, not fixed on that branch

Recorded beside the code they are about, not in a backlog file. All three were re-verified against the source
during a later tidy pass and still reproduce.

- **`clients/jsonc.go`'s `dropChanged` unwinds only ONE created section level, which breaks the DEFAULT
  `agenthub client connect vscode`.** VS Code's user placement is the two-level section `["mcp","servers"]`.
  Against a `settings.json` with comments (so the splice path is taken) and no `mcp` key, `spliceEntry`
  correctly inserts `"mcp": {"servers": {"agenthub": …}}`, but `dropChanged` deletes only `parent["servers"]`,
  so the created `"mcp": {}` survives in the after-document while `before` has no `mcp` at all: the deep
  comparison fails and the connect is refused, accusing the edit of changing something it did not. The fix is
  to walk the section path back up, dropping every ancestor the deletion left empty. The single-level cases
  (`mcpServers`, `context_servers`) pass, which is why the tests and `FuzzSpliceEntryKeepsEverythingElse` —
  all one-element sections — miss it.
- **`clients/jsonfile.go:338` — an unchecked read-to-rename window.** The rendered result is renamed over the
  target without confirming the target is still the file that was read. VS Code, Zed and Cursor all rewrite
  their settings on their own schedule, so a concurrent edit between the read into `c.orig` and the rename is
  lost — and lost from the backup too, which preserves the stale `c.orig` rather than what was actually on
  disk. Re-reading and comparing (content hash, or dev/ino+mtime+size) immediately before the rename would let
  it refuse and back up what it observed.
- **`secrets/store.go:190` — a keyring credential is committed before its enumeration record.** `setLocked`
  writes the value to the OS keyring and only then calls `registryAdd`. If `keyring-keys.json` cannot be
  created, synced or renamed, the caller gets an error while the credential survives in the keyring, unlisted:
  `List` reads that registry, so exhaustive server removal and later migration both miss it, and reusing the
  same server id can resurrect it. Either pre-register the key and keep that conservative record on an
  ambiguous write, or roll back a confirmed keyring write when `registryAdd` fails.
