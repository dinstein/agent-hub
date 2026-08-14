# The skill library

> **Answers** how a skill is stored, how a copy is written into a client's directory, and what agenthub refuses to do to bytes it did not write.
> **Not here** the commands → [../guide.md](../guide.md); what a client may call → [../model.md](../model.md).
> **Kept true by** `internal/skills`' golden renderers and its cross-process lock tests.

`internal/skills` holds an agenthub-owned, content-addressed library, the materialized copies inside AI
client directories, the receipts for those copies, and a protocol face serving the library upstream as
read-only MCP tools.

## Two layers, and one honest limit

An MCP server is a runtime intermediary and agenthub sits on the call path, so visibility can change per
session. Skills are the opposite: clients read them straight off the filesystem and agenthub is **not**
on the read path. That forces two layers.

| Layer | Is | Where |
|---|---|---|
| **the store** | agenthub's canonical copy, content-addressed | `<skills>/store/<id>/<contentHash>/`, indexed by `skills.json` |
| **an install** | a *receipt* that this skill was materialized once, for this client, under this scope | `installs.json` |

Receipts go stale, so every one must be verifiable, repairable and never blindly trusted.

Which yields this package's single most important sentence, written into the `Granularity` field of
every return value: **file materialization can only achieve client granularity, never session
granularity.** Once bytes are on disk, every session of that client sees them, and agenthub cannot
retract a file for one session while keeping it for another. Per-session skill visibility can only go
through the skills-over-MCP face, where agenthub actually is on the read path. `GranularityClient` is
echoed back in every result precisely so the CLI and GUI are forced to state the limitation.

**`<id>` is a path segment, so its shape check is a path safety check.** `validID` accepts 1–64
lowercase ASCII letters, digits and single inner dashes — exactly what `slugify` mints — and refuses
rather than sanitizes, because a sanitizer must be right about every escaping form while a shape check
need only be right about one. Three exclusions are load-bearing: separators and `.`/`..` fall outside
the character set; uppercase, because two ids differing only in case share one directory on a
case-insensitive filesystem while the index counts them as two skills; and the empty string, because it
collapses the join onto the store directory itself, which a removal would then delete whole. The two
deleting paths re-check rather than trust the index they read the id from.

## Two write strategies

**`StrategyOwnedDir`: agenthub owns the entire directory and can rebuild it from scratch.** Ownership is
proven by the marker file `.agenthub-managed.json` and **only** by it — path conventions, naming
patterns and receipts are all things a user might reproduce by coincidence, whereas an explicit marker
cannot be produced by accident. A directory without our marker is somebody else's, always reports
`StateConflict`, and is never absorbed. `applyOwnedDir` rebuilds rather than merges, so stray files from
an older version cannot survive; the marker is checked **before** deletion and written only at the end,
so a crash mid-write leaves a directory that verifies as `Drifted` (repairable) rather than one that
looks complete.

**`StrategySentinelBlock`: agenthub owns the span between BEGIN/END inside someone else's file.** The
marker strings are **frozen**: changing them orphans every block agenthub has ever written, and an
orphaned block is indistinguishable from user content and would be left there forever. Bytes outside the
sentinels are preserved verbatim, with one exception documented on `upsertBlock`: appending to a file
that does not end in a newline adds one, so the start marker gets a line to itself.

`findBlock` is the safety valve for the whole strategy: anything other than "exactly zero" or "exactly
one well-formed pair" — unpaired, inverted, duplicated — returns a `*SentinelError` and the caller must
refuse to write. Broken markers mean we can no longer tell which bytes are ours, and overwriting on a
guess is how a "managed block" tool eats someone else's file. Import applies the source-side half of the
same guard: a package whose name, description or SKILL.md body contains an agenthub sentinel is rejected
at the door, because an embedded END marker would truncate its own block and everything after it would
silently become user content agenthub will never manage again.

## ApplyState

Five states — `applied`, `stale`, `drifted`, `missing`, `conflict` — answering exactly one question, "are
the bytes still where we think they are", and deliberately not "is this content trustworthy".
`verifyOne`'s order is most actionable first:

1. Is there a shadowing file in the container (`TargetDef.BlockedIf`)? → `conflict`
2. Are the bytes there? → `missing`
3. Are they ours — owned-dir marker, or a well-formed sentinel block? → `conflict`
4. Does the content match the receipt? → `drifted`
5. Does the library entry still exist and is it unchanged? → entry gone: `conflict`; library moved on:
   `stale`; otherwise `applied`

"Library entry gone" is `conflict` rather than `missing` deliberately: something deleted a skill without
deleting its install, and automated writing has to stop and wait for a human.

## Invariants

**Every read-modify-write goes through `withState`; there is no second path.** N gateways plus the
daemon plus the CLI all mutate this state. A cross-process flock on a single `.lock` guards the whole
skills directory, and the three state files are loaded and saved as one unit under that one lock —
every interesting operation touches at least two of them, and one lock makes cross-file consistency
structural rather than an ordering convention nobody can verify. Read-only callers take the same
exclusive lock: operations are short, and correctness beats concurrency here.

**A corrupt state file always fails closed, and is never renamed out of the way.** A file that exists but
will not parse is a `*CorruptError`, the operation aborts, and the file stays in place — renaming it to
`.corrupt` would make the next read look like a legitimate brand-new store, which is exactly the silent
re-baselining an attacker wants. A *missing* file is what constitutes a brand-new store. Four read
retries absorb rename transients; a parse failure surviving them is real corruption.

**A missing `enabled` field reads as disabled.** The persisted spelling is `Enabled`, not `Disabled`,
precisely so a hand-written or truncated record omitting it reads as disabled — the closed direction for
"should agenthub push these bytes into a client directory".

**Fingerprints and pins: a mismatch refuses to propagate.** `Fingerprint` is `"v1:<sha256>"` covering
content **plus** metadata (name, description, kind), strictly broader than `ContentHash`. Description is
included because it is what the client's model actually reads when deciding whether to invoke a skill —
identical files with a swapped description is a meaningful change, and a classic prompt injection
vector. `Version` and timestamps are excluded: a version bump with unchanged content is not a content
change, and folding in timestamps would make re-importing identical bytes produce an unstable
fingerprint. The `HashSchemaVersion` prefix earns its keep — once the formula changes, old pins must be
identifiable as "different algorithm" rather than "content changed", or a formula upgrade presents as a
fleet-wide alert and users learn to ignore alerts.

**`requireTrusted` re-reads the library copy from disk**, ahead of `InstallTo` and `syncOne`, through the
same full recomputation `Verify` uses: hash the files on disk, rebuild the fingerprint from what is
actually there, compare both against the recorded values. **An index a tamperer has edited cannot vouch
for itself.** Three fail-closed directions and one honest limit: mismatched entries refuse
(`TamperError`, carrying the fingerprint recomputed from disk); a copy that cannot be read or hashed
refuses too (`ErrUnverifiable`) rather than reading as "nothing to compare", which is the state an
attacker can arrange most cheaply; unpinned entries are allowed, since they predate the pin mechanism,
but have still had their content hash checked, so "unpinned" means "no baseline", never "unverified".
The limit: a tamperer who rewrites the library copy, the index and the pin file consistently is not
detectable here — all three are files on the same disk.

**Pins are never deleted, not even by `Remove`.** A skill deleted and added back is compared against the
original baseline rather than blindly re-pinned: re-pinning on re-add is how a tampered copy launders
itself back into trust.

**Drift refuses to be overwritten unless a human explicitly decides.** A materialized copy modified by
something other than agenthub returns `ErrDrifted` and the caller must pass `AllowDrift`. Drift is the
user telling us something, even if what they mean is "I edited the wrong file"; silently rolling it back
is how a sync tool teaches users not to trust its own receipts.

**Import is this package's largest attack surface, and every rejection is non-negotiable.** `scanTree`
rejects symlinks of any kind (following one copies content from outside the package; preserving one
points the installed copy at an attacker-chosen path in the user's home), non-regular files, a marker
file in the source tree (that is the ownership credential for an install directory), and trees over the
size and count limits. Path escapes are structurally impossible — every path is derived by `filepath.Rel`
from the walk root — but the check stays, because the whole install layer trusts `FileEntry.Path` to be
package-relative. `copyTree` re-hashes each file as it copies and compares against the scan result, so a
source that changed in between aborts the import rather than producing a library copy whose
`ContentHash` is lying.

**`Options.ContentScanner` is the seam for injecting an injection scanner**, and a hit rejects the
import outright rather than importing and flagging — an imported skill is one `sync` away from being
materialized into a client directory. **Nothing sets it today**: the scanner it was shaped for went with
the removed governance surface, so that sentence describes what the seam would do, not a check an import
currently passes.

**An unreadable entry inside `hashDir` is an error, never a skipped file.** Silently skipping would let a
permissions trick hide drift. A symlink or device file appearing where a skill file belongs is drift by
definition, and gets a hash that can never match.

**`Sync` converges; a conflict on one skill never aborts the batch.** A shadowed file or a hand-edited
copy is recorded as one failed item while the other skills still converge, and only a store-level
failure returns an error. Pruning is the default, but happens **only within the containers this request
converged** — a sync of project A must never de-materialize project B, and the receipt's `Container`
field is what distinguishes them. `Disable` does not de-materialize anything by itself: the bytes stay
until a `Sync` or an explicit `Remove` converges the target, and until then the receipt honestly reports
their presence.

**The library's `Enabled` and scope's `SkillSelector` only narrow, never widen.** A disabled skill is not
materialized no matter what the selector says, and the selector's tri-state semantics are exactly those
of the scope chain's tool selector.

**`Force` means "stop tracking", never "delete things we cannot prove are ours".** Files on a conflicting
target stay in place and only the receipt is discarded.

**`CharCap` measures the entire rendered file, not just our block** — the lesson of Windsurf's
6000-character limit: what the client truncates is the file, so budgeting per block measures the wrong
thing. Exceeding it is ruled `conflict`, because silently writing a file the client will truncate
produces a skill that exists but is broken. **`BlockedIf` shadow detection** is the lesson of
`AGENTS.override.md`: a file the client prefers can make our write invisible, and an invisible write
paired with a healthy receipt is a lie.

**The SKILL.md frontmatter parser is a deliberately restricted YAML subset.** It recognizes only
single-line `key: value`: this package cannot take on a dependency, and a half-finished YAML parser that
silently misreads nested structure is worse than one that admits it does not understand a line. Lines it
does not understand are **never discarded** — they are kept verbatim in `Meta.Extra` and written back in
their original positions. No frontmatter is valid; an **unclosed** frontmatter is an error, because
guessing where the metadata ends is how a whole document lands inside the description field. For
duplicate keys the **first** occurrence wins: taking the later one would let an appended line silently
override a value that has already been reviewed.

**Three versions are retained** — the current content-addressed version plus the two most recent older
ones. Old versions are what rollback and drift diffs read, and pruning to one saves space by deleting
evidence.

**This package never shells out to git and never touches the network.** git sources are imported from a
local checkout the caller already has, and `--pin <rev>` is recorded so the revision that produced the
library copy is reproducible. Fetch, clone and ref resolution are a capability boundary
[canonical.md §4](../canonical.md#4-known-capability-boundaries) records deliberately, and until it
moves, an `Update` on a git skill without a new checkout path returns `ErrGitFetchUnsupported` rather
than reporting "already up to date" without having looked.

## The skills-over-MCP face

**Tools rather than resources.** Resources are semantically the better fit, but the gateway's upstream
face offers only tools, and inventing a protocol face inside a subsystem package is the wrong place for
it.

**The host stays on the gate path.** This type never answers on its own in any privileged way; the
gateway routes the call through the same `pipeline.Execute` as a downstream call, so the scope and tier
gates apply identically.

**Enabled state is verified live at call time.** `Tools()` serves a snapshot — it is invoked on every
catalogue build and cannot do I/O — but `Call` re-reads the library, so a skill disabled or deleted
since the last `Refresh` is rejected rather than served out of a stale snapshot.

`NewProvider`'s snapshot starts empty, so a broken or unreadable store broadcasts zero skills rather
than a stale set; a failed `Refresh` retains the previous snapshot, since serving the last known-good
set beats serving nothing because a lock was busy. Disabled skills are invisible rather than "listed and
then refused" — the same anti-probing rule scope narrowing follows. When two ids sanitize to the same
tool name the first in sort order is kept and the rest skipped: a silently shadowed skill is worse than
a nonexistent one. `Annotations()` is payload, not decoration: the tier ladder treats **missing**
annotations as destructive, so a read-only tool without annotations would be refused to a read-only
credential.

## Capability boundaries

`ApplyState` landed at five values rather than one per failure: any target we are not allowed to write
is `StateConflict`, and a removed install has no receipt at all. The targets table landed at three rows
— claude-code as the owned-dir reference, cursor as the sentinel-block reference, and generic to prove
the table extends without code changes.

**Owed: two sentinels reach the CLI with no arm in `classifySkillsError`, so they exit 1 as
`E_GENERAL`.** That function is a `switch` over `errors.Is` whose `default` returns the error
unclassified — a shape that says nothing when a new sentinel is added beside the ones already handled.

| Sentinel | Today | Should be | Why |
|---|---|---|---|
| `ErrInvalidID` | 1 / `E_GENERAL` | 2 / `E_USAGE` | a rejected `--id` is an argument the user typed |
| `ErrUnverifiable` | 1 / `E_GENERAL` | 6 / `E_GOVERNANCE_DENIED` | it is the second arm of the same fail-closed decision as `ErrTampered`, which maps to 6 |

Not fixed here because an exit code is a machine contract and the table is frozen: moving a case is the
table owner's call.
