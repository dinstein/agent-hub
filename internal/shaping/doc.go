// Package shaping bounds tool-call results to a byte budget and hands the
// remainder back through fetch_result cursors (docs/flows.md and 7.2
// "truncation and pagination").
//
// The shape of the feature is fixed by three design rulings:
//
//   - Truncation splits TEXT content blocks at a character (rune) offset.
//     A structured block is never split: it is deferred whole. Page 1 keeps
//     the original block structure; pages 2+ are plain text slices of the
//     retained payload.
//   - The recovery trailer is appended as the LAST content block and is
//     exempt from the budget — it is never truncated and never wrapped.
//     Same rule as pipeline's injection trailer (internal/pipeline/shape.go):
//     a recovery hint the agent cannot read is not a recovery hint.
//   - fetch_result cursor ids are a plain sequence and are GUESSABLE BY
//     DESIGN. Owner (session) verification is the only isolation, and an
//     unknown id, an expired id and another session's id all return the ONE
//     message in notFoundText — a distinguishable answer would turn a
//     guessable id into a probe oracle.
//
// Budgeting is an economy mechanism, not a security boundary. Every
// unexpected input (unparsable content array, missing cursor id, absent
// owner) fails OPEN: the untruncated result is delivered rather than
// destroyed. The closed direction belongs to the gates in internal/pipeline;
// losing a caller's data to save tokens would be a worse failure than
// spending the tokens.
//
// # Ruling: the durable cache is plain files, not an embedded database
//
// Appendix A.6 #2 asked bbolt vs plain files for the daemon-side cache.
// RULED: plain files.
//
//	<data>/cache/shaping/<sha256(owner)>/<cursor>.json
//
// written atomically (same-directory temp → chmod 0600 → fsync → rename),
// swept by TTL at construction and on demand. Rationale: M0–M1 shipped with
// zero new third-party dependencies and that is the house style; the access
// pattern is a single-key point lookup with no queries, no transactions and
// no cross-key consistency requirement; and a corrupt entry must degrade to
// one lost cursor, which per-file storage gives for free (skip the file)
// while a single-file database gives only with recovery machinery. The
// owner-hash directory keeps one session's cursors off another session's
// path, but it is NOT the isolation — Entry.Owner is verified on every read.
//
// Concurrency: Store implementations are safe for concurrent use. Shape and
// Fetch are pure functions over their arguments plus the store.
package shaping
