# Result budget

> **Answers** how a large result is trimmed, how the remainder comes back, and why every failure here delivers more rather than less.
> **Not here** the gates → [execution.md](execution.md); what a truncated page costs a client → [../mcp-2026-07-28.md](../status/mcp-2026-07-28.md).
> **Kept true by** `internal/shaping`'s byte-accounting invariant tests and `test/e2e/cursororacle_test.go`.

`internal/shaping` trims a tool result to a byte budget and hands the remainder back through a
`fetch_result` cursor. `internal/shaping/toonenc` projects JSON into a compact display encoding. Both are
cost-saving mechanisms, not security boundaries, and both fail open.

## Three rulings fix the shape of the feature

1. **Truncation cuts at a rune offset inside a text content block.** Structured blocks are never split
   and are deferred whole — which means a truncated page carries no `structuredContent`, and if the tool
   declared an `outputSchema` that page does not satisfy it. Nothing is lost, since the payload is in the
   remainder, but it is a conformance cost with a product decision in front of both possible fixes: read
   [mcp-2026-07-28.md](../status/mcp-2026-07-28.md) §7.14 before changing this. Page 1 preserves the original
   block structure; page 2 onward is a plain-text slice of the retained payload.
2. **The recovery trailer is the last content block and is exempt from the budget** — neither truncated
   nor wrapped, because a recovery hint the agent cannot read is not a recovery hint. A page may
   therefore exceed the budget by exactly one trailer block.
3. **Cursor ids are an ordinary, guessable-by-design sequence** (`rc-%06d`, process-global). Owner
   validation is the only isolation, and unknown, expired and other people's ids all return the one
   message: distinguishable answers would turn a guessable id into a probing oracle.

**Owner comparison is constant-time**, because this is an isolation boundary and must not short-circuit
at the first differing byte. `Fetch` checks the owner **again** after `Store.Get`: the interface contract
requires implementations to validate, and both sides of an isolation boundary check.

## The budget is fail-open throughout

An unparseable content array, a missing cursor id, an absent owner, a remainder that cannot be stored —
every one delivers the **untruncated** result rather than destroying it. The closed direction belongs to
the gates; losing a caller's data to save tokens is a worse failure than spending more tokens.

**The never-larger guarantee.** The trailer is not free, so for a result barely over budget the trimmed
page can cost more than the original. `shape` compares once at the end and reverts when the shaped result
is not smaller — every dimension must improve: fewer bytes **and** no data withheld.

**Pagination walking rules.** `paginate` fills the budget in order, stops at the first segment that does
not fit, and defers everything after it; preserving order is what makes rune offsets after linearization
meaningful. Structured segments are all-or-nothing; text segments split, but a slice smaller than 16
bytes defers the whole block, because a two-character page plus a trailer costs more than it delivers.

**The byte accounting is exact.** `escapedRuneLen` reproduces `encoding/json`'s string escaping overhead,
including the HTML escaping on by default, with invariant tests aligning it to the standard library, so
emitted block sizes are predictable rather than estimated. All slicing lands on rune boundaries.

**`Fetch`'s boundary behaviour.** A negative offset clamps to 0; an offset at or past the end serves an
**empty page** — a success, not a miss, because offering a recovery hint when there is nothing left would
be a lie. `page` has an "always deliver at least one rune" backstop against a livelock that never
advances.

**Re-encode first, then bound.** `ShapeResult` calls `Reformat` and only then `shape`. The direction is
load-bearing: the budget is spent on the cheaper representation, so a result that fits once re-encoded is
delivered whole instead of paginated, and the retained remainder is the text the agent actually saw, so a
`fetch_result` page continues in the same notation. Rewrite scope is fixed too: only `text` blocks, only
those whose payload is a single JSON document, `structuredContent` is **never** re-encoded, and
`toonenc.HeaderLine` is emitted at most once per result.

## Two stores, one wired

`MemStore` makes the process the session, so cursor lifetime is aligned with the client connection by
construction. `FileStore` was written for a face where cursors must survive a restart within the session
TTL, and **is not wired**: `newGateway` assigns `MemStore` unconditionally, and the daemon's HTTP face
does not change that, since it builds one gateway per credential inside its process.

That makes a defence above look better exercised than it is. **Owner validation currently arbitrates
nothing in production**: no two sessions ever share a store, so another session's cursor misses because
this store never held it, not because the owner check refused it. The check, its constant-time comparison
and the double-check across the interface are all correct and are what would hold the line the moment a
shared store is wired — they are simply unreachable today.
`test/e2e/cursororacle_test.go` pins the property that IS reachable: every miss, whoever asks, is the
same bytes.

`FileStore`'s durable caching uses ordinary files, not an embedded database: the house rule of zero new
third-party dependencies, a single-key point lookup with no queries or transactions, and a corrupt entry
that must degrade to "lose one cursor", which per-file storage gives for free. The owner-hashed directory
keeps one session's cursors off another's path but is **not** the isolation — `Entry.Owner` is verified
on every read. `validID` is both a shape check and a path safety check, since an id becomes a filename.

## internal/shaping/toonenc

**TOON is a one-way projection.** The encoded document is for a language model to **read**; it does not
round-trip and no decoder is provided. Round-tripping would need a type tag on every scalar — a bare `1`
is indistinguishable from `"1"` — and those tags are exactly the tokens this encoding exists to save. So
the encoder quotes only where a reader might be misled, and the caller states the contract in place with
`HeaderLine`: the result is TOON, the arguments are **still** JSON. Anything that must round-trip —
`structuredContent`, tool arguments, cursors — stays JSON and never enters this package.

Two constructive guarantees:

1. **Never larger.** `Consider` re-encodes and compares, and a document that did not beat the JSON form
   by 10% is returned verbatim with a `Decision` explaining why, so a caller can always use the return
   value directly. Winning 1% is not worth teaching a model a second notation mid-conversation. All
   comparisons are integer arithmetic — floating point would make the accept/reject boundary depend on
   rounding, and that boundary is what golden tests assert.
2. **Numeric fidelity.** Decoding uses `json.Decoder.UseNumber`, so integers beyond 2^53 travel as
   literal text and are emitted byte for byte. No value passes through a `float64`.

Three of the table form's choices are contracts rather than details: the row delimiter is fixed and never
inferred; object keys sort by ascending bytes, because no Go decoder preserves JSON object order and
determinism is a contract; and truncation lands on line boundaries so a cut table stays parseable by eye.
Values beyond depth 12 render as one line of compact JSON: malicious input must not drive unbounded
recursion.

**Failure directions are all open.** Not a single JSON document, an encoder error, blank input — all
return the input verbatim. Mangling a tool result to save tokens is far worse than spending more tokens.

**This package reports only byte counts.** It has no notion of tokens; the ledger that once converted
bytes to tokens was removed, and nothing replaced it.

## Current assembly status: two unwired switches

- **`fetch_result`'s `limit` parameter is accepted and has no effect.** The field is in the frozen schema
  and `handleFetchResult` explicitly does not honour it — page size comes from the shaping budget of page
  1, stored alongside the entry. The field is retained so the wire shape does not change when it lands.
- **TOON output is a switch with no setter, not code with no caller.** `Reformat` runs on every delivered
  result and returns the input unchanged because `Options.Format` is never set to `FormatTOON`. What is
  unwired is the governance value: `ParseFormat` has no caller, and the `result_format` key its doc names
  does not exist in `GovernanceDoc`.
