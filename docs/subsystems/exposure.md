# Naming and exposure

> **Answers** where an exposed name comes from, which names a session is shown, and what an incoming name means.
> **Not here** whether a name may be called → [execution.md](execution.md); what the modes mean to a user → [../model.md](../model.md).
> **Kept true by** golden tests on name generation, the discovery surfaces and every user-visible string.

`internal/router` aggregates several servers into one namespaced catalogue and owns the only legal
reverse lookup. `internal/discovery` decides which names a session is shown across the three modes.
`internal/discovery/toolsig` renders a schema into one line. None of them executes anything.

Two disciplines run through all three:

1. **An exposed name is an opaque handle.** Names are `sanitize(serverID) + "__" + sanitize(rawTool)`,
   but a server id or a tool name may itself contain `__`, so splitting is ambiguous and banned
   repository-wide. Every reverse mapping goes through `router.RouteOf`.
2. **A tool crosses this hub whole.** `mcp.ToolDef` carries every member the specification gives a
   `Tool`, and schemas, annotations, icons and `_meta` all travel as raw JSON. A member this facade
   failed to name would be a member the downstream published and the client never saw.

## internal/router

Live aggregation and cache aggregation run through the **same** `build` core, so a cache-served
`tools/list` cannot drift from the live catalogue. `*Router` is an immutable snapshot, atomically
pointer-swapped on change.

**`RouteOf` is the only legal reverse mapping**: a pure map lookup with zero string parsing. `Lookup`
yields a nil `*downstream.Server` for cache-built entries — listable and routable, but **not callable**.
`LookupProvider` returns true only for host-supplied entries, so a caller cannot mistake a real server's
tool for one the host serves.

**Splitting on `__` is banned repository-wide.** Even the gateway's "does this name have a route" check
goes through `RouteOf`. `discovery.IsBareName` is the only place in the repository that inspects `__`,
and its result is used only for logging.

**Exposed name generation is a deterministic three-part rule.** `sanitize` replaces every rune outside
`[a-zA-Z0-9_-]` with `_`. Collisions take `_2`, `_3` … ordered by ascending raw tool name with server id
as secondary key, and if a suffixed name is itself taken the scan continues upward; base names iterate in
sorted order. Same servers, tools and policy produce the same names and the same `List` order, locked by
golden tests.

**Aggregation applies no policy.** The catalogue is the full surface every configured server offers;
narrowing happens once, above it, in `internal/scope`. Filtering at aggregation renumbered the collision
suffixes of a dropped tool's neighbours, so switching one tool off could silently change another tool's
exposed name.

**Providers aggregate under exactly the same rules** — same name generation, same collision suffixes,
same provenance, same scope intersection, same `pipeline.Execute`; only the source of the bytes differs.
`BuildWith` appends providers **after** servers, so a provider id colliding with a server id reports a
duplicate and the configured server wins.

**`Catalog` is keyed by RAW tool names only.** It is the snapshot `internal/scope` intersects against;
exposed names never appear in it. `CatalogOf` skips nil servers, so a vanished server contributes no
tools and the scope layer treats "does not exist" as "not visible". This `Catalog` and `internal/catalog`
— the curated server catalogue — are different things.

## internal/discovery

`Visible(rt, es)` projects the router catalogue through the session's effective scope using
`pipeline.ScopeAllows`, **the same predicate as the pipeline's scope gate**; `New(Options)` freezes that
set into an immutable `Surface`. `SearchGuard` deliberately does not belong to `Surface`: guard state is
per-session, must outlive catalogue rebuilds, and yet must be reset on a scope change, so its lifecycle
is the gateway's.

**One scope, three enforcement points.** `tools/list`, `search_tools`' candidate filtering and
`call_tool`'s route validation all read the same `*scope.EffectiveScope`, and this package never
re-derives visibility. `describe_tool` goes through the same `byExposed` map, so it is **structurally**
incapable of revealing a tool that search hid — a property of the code, not a rule anyone has to
remember.

**Unknown names fail closed.** `Classify`'s order is fixed: meta names where the mode allows them,
grouped aggregate names, the visible tool set, then Unknown. An unknown bare name is treated like any
other unknown, and under a cold catalogue every name is unknown. `exposesMeta` is narrower than
`IsMetaName`: `call_tool` while variants are on, and the three variants while variants are off, were
never **listed** to this session, so classifying them as meta would open a door the client cannot see.

### The three modes

- **full**: every visible tool, listed as-is.
- **grouped**: one `<server>_tools` aggregate per visible server plus one `call_tool`. The tool *count*
  collapses — the expensive part of full is schemas, and grouped ships none — yet the agent still need
  not search: each aggregate's description **names** that server's tools, up to 40, with an overflow note
  saying how many more and how to get them. Discovery stays exact, and only the schema is deferred by one
  round trip. `call_tool` is placed last, so the entries the agent should read first lead.
- **lazy**: the five meta-tools in frozen order, plus pinned tools. A pinned tool whose exposed name
  collides with a meta name is dropped: the meta face must never be shadowed. Today every
  router-generated name contains `__` and cannot collide, but the rule is enforced, not assumed.

**The five meta-tool names and schemas are both ABI**: `status`, `search_tools`, `describe_tool`,
`call_tool`, `fetch_result`. Schemas are written as literals rather than marshaled from structs precisely
so those exact bytes are reviewable and golden-testable — agents are sensitive to wording. All meta-tool
arguments decode with `DisallowUnknownFields`: a misspelled argument must be a loud, recoverable error
and never a silently ignored field that lets the agent believe it got something it did not.

**`call_tool` and `fetch_result` have no handler in this package, by design.** Execution must go through
`internal/pipeline` and pagination through `internal/shaping`, so this package stops at `Resolve` and
`Parse`.

**Determinism is a contract.** Exposure set, ordering, summary truncation and every user-visible string
are frozen by golden tests. Ties break on ascending exposed name and never rely on map iteration order;
scores are integers precisely so a tie is exactly decidable.

### Intent variants

Lazy mode's single `call_tool` can be split into `call_tool_read` / `call_tool_write` /
`call_tool_destructive`, buying exactly one thing: clients whose permission UI can only allow or deny a
whole tool can allow `call_tool_read` while still requiring confirmation for writes.

Validation uses **equality**, not coverage. If the destructive variant also accepted read tools, each
variant would be a superset of those below it and allowing the top one would silently grant everything —
the exact property the split exists to make visible. Each tool has one correct door and the rejection
text names it; `callWithFor` is the only place making that choice and `ResolveCallVariant` enforces the
same derivation at the entry point, so the pointer given to the agent and the check it must pass cannot
disagree. Tools with no annotations fall to the destructive variant. The switch goes into `Key.Variants`,
because it changes `tools/list` output while touching neither generation nor scope hash.

### SearchGuard

A new top name sets streak 1; the same top increments it; no results, any non-search action, or a scope
change resets it. Escalation to an imperative message needs streak ≥ 3 **and** score ≥ 30. Two
asymmetries are deliberate:

- **A low-confidence top still advances the streak but never escalates.** Forcing an agent to call a tool
  the ranker itself doubts turns a weak guess into an instruction; if a later identical search scores
  above the line, the accumulated streak escalates immediately.
- **Escalation does not clear the streak.** Only doing something else clears it, because the guard tracks
  a loop and it is not over until the loop is over.

### The ranker and its budget

`weightName` 10, `weightServer` 4, `weightDesc` 2, multiplied by `exactFactor` 3 or `prefixFactor` 1,
plus a `coverageBonus` of 5 rewarding coverage of more **distinct** query terms. Each term contributes at
most once per field and occurrence counts are ignored, so stuffing repeated words into a description buys
nothing; `minPrefixLen` 2 keeps single-letter terms from matching everything. `ConfidenceThreshold` = 30
is calibrated so "confident" means "the query literally names this tool": an exact tool-name term match
scores 35, a description-only exact match 11, a name-prefix-only match 15. Zero-scoring candidates are
discarded.

**Query validation and privacy.** `MaxQueryBytes` 512, `MaxQueryWords` 64, `MaxDescriptionTokens` 256 on
the index side, so a malicious server cannot make every search expensive with a megabyte-long
description. Check order is fixed — empty, bytes, words — so a query violating two limits always reports
the same code. `Query` deliberately does not retain the original text, and `Trace` records only the
query's **metrics**: a search query is free text the agent composed and may carry secrets, paths or an
injected payload, while tool names and scores are safe because they come from the catalogue. Adding a
field to that struct is a privacy decision, and the golden test fails the moment someone adds a content
field.

**No hit carries a schema.** Each carries a one-line compact signature; rank 1 additionally carries the
full description, and the rest carry a 140-byte summary — a byte bound rather than a rune count, because
what is defended is a token budget and only a byte bound also holds for CJK, with truncation landing on a
rune boundary. The `lossy` flag is the pointer to "a describe round trip will tell you something new".

**`describe_tool`'s one error, no oracle.** Of the conceivable per-id errors — not found, invisible, not
offered by this server — only `not_found` is emitted: distinguishing them would turn `describe_tool` into
an oracle enumerating the part of the catalogue deliberately not shown. `fetch_result` follows the same
rule for cursors and `ResolveCall` for names. `MaxDescribeTools` = 5, and exceeding it is an error rather
than a silent truncation, which would let the agent believe it saw everything it asked for. A call where
none of the ids resolve still returns a **non-error** reply: the call was well-formed, and a protocol
error would deny the agent the remediation text.

### Tasks are unimplemented, and loudly so

MCP 2025-11-25 added tasks as an experimental feature: a tool may declare `execution.taskSupport`, and
where it is `"required"` a server MUST answer −32601 to a client that does not invoke it as a task.
AgentHub sends no `task` param and declares no `tasks` capability, so every call to such a tool earns
that −32601 — relayed upstream verbatim with its code, and logged at Warn.

That is the specification's own prescribed failure rather than a silent one, which is why **the tool is
still listed**: filtering it out would reproduce the worst failure this layer knows, a tool absent from
the catalogue with nothing in the log. What the catalogue does carry is `execution` itself, forwarded
raw, because it is the only thing that explains the refusal. The member exists in 2025-11-25 alone:
2026-07-28 moved tasks into a capability extension and dropped it from `Tool`, which is why its absence
reads as a missing feature and is not one.

## internal/discovery/toolsig

Renders a tool's JSON Schema into a one-line compact signature, dropping the cost of one search result
from a whole schema to one line. `Cache` memoizes by input fingerprint and `Shared()` is the
process-level instance catalogue indexing warms, so a session's first search pays a map lookup instead of
N schema walks.

**The grammar is frozen**, locked by `testdata/signatures.golden`, and two markers carry the meaning:

- **`?` marks optional parameters, not required ones.** The inverse would work too; optional is marked
  because they are the minority in practice, so the marker is rarer and the line shorter.
- **`~` is an honesty marker**: the signature cannot fully state this parameter — a collapsed nested
  object, a truncated enum, an oversized default, a union type, a surviving `$ref`, or a name in
  `required` with no schema at all. It is the pointer to "describe_tool will tell you more".

**Parameter order is the only deterministic choice available**: required parameters in the verbatim order
of the `required` array, then optional ones sorted by ascending bytes. JSON member order does not survive
decoding into a Go map, and `required` is the only ordering signal the schema carries.

**Nesting expands exactly one level** — `obj{key,key}` at the top with keys sorted and capped at four,
plain `obj` deeper, `arr<obj>` for an array of objects; both collapses set `~`. `$ref` is not resolved:
`internal/router` inlines refs first, and anything surviving renders as `any~` rather than being chased,
which would mean this package holding a schema store.

**Failure direction: better to say less than to say more.** Every unsupported construct becomes
`TypeAny` plus `lossy`, because a signature that says less than it knows is recoverable through
`describe_tool` and one that says more is not. An unparseable schema renders uniformly as
`name(~) -> type` — one shape, no guessing.

**The length budget truncates required-first**, closing with `…+N more`; since optional parameters sort
last, dropping from the tail drops them first. **The tool name is never truncated** — it is the key the
agent calls with, and a truncated key is worse than a long line.

**Cache eviction is clear-everything-when-full, not LRU.** The access pattern is "the same few hundred
fingerprints hit repeatedly until the catalogue changes": LRU bookkeeping costs more than an occasional
clear-and-re-render, and a clear cannot leak stale entries the way a mis-ordered LRU can.
