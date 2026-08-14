# The registry

> **Answers** how configuration lives on disk, how five processes write it without disagreeing, and how a change reaches a running gateway.
> **Not here** what the configuration means → [../model.md](../model.md); the write path as a sequence → [flows.md#config-writes](../flows.md#config-writes).
> **Kept true by** `internal/registry`'s multi-process tests and `test/e2e/apiwrite_test.go`.

`internal/registry` is the on-disk source of truth shared by the CLI, every gateway process and the
daemon: multi-document, unknown-field-preserving, cross-process atomic writes, a monotonic generation,
change awareness and self-write suppression.

Five documents named by their `DocKind` — `meta`, `servers`, `profiles`, `clients`, `governance` — plus
a sibling `.lock` guarding all of them, `backups/` holding five rolling generations per document, and
`.runstate.json`, the crash marker whose dot prefix keeps it out of the `<kind>.json` namespace.

## Key types

**`Doc[T]`** is the persistence envelope: a typed view plus verbatim passthrough of unknown fields,
with known fields winning on collision, so fields written by a newer agenthub survive an older
version's load-modify-save. The preservation is per level — `ServersDoc.Servers` holds
`Doc[ServerEntry]`, so unknown fields inside one server entry survive too.

**Passthrough is exactly what makes a retired field dangerous**, which is why `HasUnknownField(name)`
exists. A field the type system dropped keeps round-tripping verbatim, so a rule an operator wrote
while it worked still looks applied long after it stopped applying — and when the retired rule was a
narrowing one, "stopped applying" means widening. `agenthub doctor`'s `scope:projects` check uses it.
Reading the key is deliberately all it exposes: a caller may ask whether a name is present, never reach
in and act on its contents.

**`Store`** is a handle on one directory. `Open` still returns a usable `*Store` when a document had to
be quarantined, alongside a non-nil error joining an `*UnreadableError` — whether that is fatal is the
caller's call. `Update` is the full `lock → load → modify → commit → bump` transaction. `Tx`, the
mutable view its callback sees, is valid only for that callback and does not expose `meta.json`: the
generation is the store's business. `Snapshot` is the immutable view, deep-copied so it is independent
of maps the callback may still hold.

Two domain shapes worth knowing:

- **`ClientEntry` holds `{Profile, ProfileRef}` and nothing else.** A client selects a profile and never
  narrows on top of one. `Binding()` hardcodes the priority "explicit `ProfileRef` > the `profile`
  shorthand > the layer default". `Profile` carries the `Discovery` field, because discovery describes
  the tool set it is attached to.
- **`GovernanceDoc.RateLimits` exists only at the global layer** and never enters the three-layer scope
  chain: the rule patterns already carry the (client, server, tool) dimensions, and counting buckets
  are keyed by rule pattern, so the same pattern at several layers would either split one quota into
  one bucket per layer — a multiplied limit, the opposite of "only tighten" — or need a per-pattern
  min-merge that exists nowhere else. The registry stores it verbatim; parsing, validation and
  enforcement live in `internal/ratelimit`, which errors on the whole rule set rather than silently
  dropping a rule it cannot understand.

## Invariants

**One directory lock, not one per document**, because `meta.json`'s generation must be written
atomically with the batch of documents it covers. It is a flock on `<dir>/.lock`: non-blocking attempts
plus 5ms polling until timeout, then a `*LockTimeoutError` satisfying `errors.Is(err, ErrLockTimeout)`,
which the CLI maps to exit code 7.

**`Update` never trusts the in-memory snapshot.** It reloads from disk under the lock every time, so a
stale `Snapshot` cannot overwrite what another process just wrote.

**The generation is incremented only when something was written, and only under the lock.** The no-op
guard compares parsed JSON values — `canonicalize` keeps numbers verbatim through `json.Number`, sorts
object keys and strips whitespace — so key-order jitter causes no phantom bumps. Input that
`canonicalize` fails on is judged "not equal", forcing a rewrite: the safe direction for a persistence
layer.

**An unparseable file is never destroyed by reading it.** A parse failure is retried four times at 75ms
to ride over a non-atomic external writer; only then is the file renamed to
`<name>.json.unreadable-<timestamp>` — quarantine, never destroy — a default written, and an
`*UnreadableError` reported. One quarantined document does not block updates to the others.

**A missing file gets a default written, and that is not a change.** First contact persists it so the
file exists from then on; it does not trigger a bump.

**Rolling backups rotate only on a real write**, so the five slots always hold five distinct
generations.

**`atomicWrite` never leaves half a target file behind**: temp file in the same directory → `chmod 0600`
→ write → `fsync` → `rename` over the target → `fsync` the parent directory. The parent fsync is
tolerated to fail on filesystems that do not support it, where rename is still atomic.

**Registry documents never store credentials.** `${SECRET_X}` placeholders in `ServerEntry.Env` and
`Headers` are persisted verbatim, and resolution against the vault happens at connect time. In the same
spirit `OAuthHint` deliberately lacks a `needsAuth` field: whether a server currently needs
authorization is runtime state, and persisting it would create a second source of truth — a stale
`"needsAuth": false` keeps a Ready badge on a server that 401s on every call.

**`omitzero` on tri-state fields is load-bearing.** For `ToolSelector.Allow`, `ServerEntry.Tools` and
`Profile.Servers`, nil and `[]` mean different things, and `omitempty` would erase an empty list from
disk — turning "block everything" into "allow everything".

**An unknown runtime name is rejected, not treated as host.** A typo like `"dcoker"` errors out in
`ValidateRuntime` rather than quietly discarding the isolation the operator asked for. The docker
runtime applies only to the stdio transport and must carry an image.

## Generation, self-write suppression, and the watcher

Three questions, three mechanisms: the generation answers "did anything change", self-write suppression
answers "did I write this myself", and the Applier answers "should this state I just read be adopted".

**The Applier's criterion is "the generation read ≥ the generation applied", never "equal to the event's
Rev"** ([canonical.md §5c](../canonical.md#5c-the-config-hot-reload-path-two-things-not-to-get-wrong) #2).
A push is only a notification and carries no snapshot, so the consumer re-reads the files itself; under
several writes in quick succession the generation read exceeds the Rev of the event in hand, and an
equality test rejects it and waits forever for an event that will never come. `MarkApplied` only
increases, so a late out-of-order apply cannot push the criterion backwards; `Apply(gen, fn)` holds one
lock across check and update; and a failed apply is not recorded, so the next trigger retries.

**Self-write suppression fails open, toward reloading.** `selfWriteSet` is a bounded TTL set — 64 slots,
10s expiry, registered before the write, withdrawn if the write fails, cleared wholesale when an
external change is observed. A TTL expiring or a fingerprint not matching costs at worst one redundant
empty reload, and it cannot mask an external change: content whose fingerprint is not in the set is
always treated as external. Both the withdrawal and the clearing are necessary — a fingerprint for
content that never reached disk must not suppress a future external write that happens to be identical,
and once someone else has touched the registry the pending fingerprints no longer describe the on-disk
lineage.

**Watching runs on two channels, both always on.** fsnotify with a 200ms debounce is the primary signal
and 2s polling is the safety net, because fsnotify is unreliable on SMB and network mounts and may not
initialize at all. Any initialization failure degrades to pure polling rather than failing `Watch`: a
slightly laggy watcher beats no watcher.

`scan()` holds no cross-process lock — our own writes are atomic renames, and the torn state a
non-atomic external writer produces fails canonicalization, so the next trigger retries. Its rules:

- read `meta.json` first and **abandon the whole round if it cannot be read**, advancing nothing;
- compare each content document against **this watcher's last applied baseline**, which is why events
  carry a precise `DocKind` rather than a vague "something changed";
- a failed read or canonicalization always continues: **a failed load never advances the baseline**, so
  a half-written file is never mistaken for new state;
- a self-write fingerprint hit advances the baseline silently and emits no event;
- an external change clears the self-write set first, then advances and emits;
- **event delivery never blocks the scan loop**: a full channel parks the event by kind, keeping the
  latest Rev, and redelivers on the next trigger. Merging by kind is safe because consumers were always
  going to re-read and do not trust the Rev.

A watcher seeds its baseline from the store's current snapshot at creation, so state this process has
already applied is not reported again. `meta.json` only supplies `Change.Rev`; it is never itself a
`Kind`.

## The crash marker

`ArmRunMarker(dir)` atomically reads out the previous run's outcome and arms a new marker; `Resolve()`
marks it clean as the last step of a graceful shutdown. A process that is SIGKILLed, panics or loses
power never resolves, and **that failure to resolve is the signal**. `daemon.Run` arms after
`ctlapi.Listen` succeeds, so a second daemon that lost the socket race cannot clobber the winner's
marker. `agenthub doctor` reports the result through the read-only `PreviousShutdown`, which never arms.

Failing to write the marker costs only the next startup its diagnostic capability, so it degrades to a
warning.

**Resolve rewrites rather than deletes.** "No marker" must stay distinguishable from "a resolved
marker", or a first run would be reported as a clean shutdown with no evidence. **Every ambiguity falls
toward `ShutdownUnknown`**: a marker that cannot be read or parsed, or that carries an unrecognized
version, is unknown, because a diagnostic must not issue a clean bill of health out of thin air. The
pid and timestamp are diagnostic only, and the verdict does not depend on whether that pid still
exists.
