// Package jsonl is the append-only JSONL writer shared by every stream this
// product keeps on disk: the per-server wire trace (internal/downstream), the
// control-plane event stream (internal/eventlog), and — through LineWriter —
// the process logs slog writes (daemon.log, gateway-<client>.log).
//
// It was extracted from internal/audit, which owned it while the governance
// streams existed. The streams went; the write discipline they were built on
// did not, because it is the same discipline every JSONL file here needs:
//
//   - Multi-writer safety. N gateway processes plus the daemon append to the
//     same file. Every file is opened O_APPEND, and every record is exactly
//     one write(2) of one line bounded by DefaultMaxLineBytes, so two
//     processes can never tear each other's lines.
//   - Rotation renames the active file to a new segment; the active file is
//     never read back and truncated, which is what makes the above safe
//     across processes that did not agree to rotate at the same moment.
//   - Backpressure drops, never blocks. Appends funnel through a single
//     writer goroutine behind a buffered channel; overflow is counted
//     (Dropped) and discarded. A record on the way to disk must never be
//     able to slow down or fail the call that produced it.
//
// A line that cannot fit the bound is replaced by an OversizeMarker rather
// than truncated mid-JSON: a reader can then tell "this record was too big"
// from "this file is corrupt", which a half-line cannot.
//
// Segments and Prune are the reading and retention halves of the rotation
// scheme. They live here because segmentPath is what names a rotated file:
// a caller that composed the glob itself would be a second place that scheme
// lives, and the way that goes wrong is a reader opening only the active file
// and reporting "nothing happened" for everything rotation moved aside.
//
// Dependency budget: standard library only.
package jsonl
