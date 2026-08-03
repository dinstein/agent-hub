// Package toonenc implements TOON (Token-Oriented Object Notation), the
// compact display encoding of docs/modules/dataplane.md "TOON encoding".
//
// # Ruling (canonical.md §7 #4): TOON is a ONE-WAY PROJECTION
//
// Appendix A.6 #4 left the grammar range and the golden corpus open because
// no Go implementation exists. RULED, and the first decision is the one every
// other rule follows from:
//
//	An encoded document is a DISPLAY projection for a language model to
//	read. It is NOT round-trippable and no decoder is provided.
//
// Round-tripping would require type tags on every scalar (a bare 1 is
// indistinguishable from "1", a bare true from "true"), and those tags cost
// exactly the tokens the encoding exists to save. Instead the encoder emits
// quotes only where a reader could be misled (see quoting below), and the
// caller states the contract in-band with HeaderLine: results are TOON,
// arguments are still JSON. Anything that needs to survive a round trip —
// structuredContent, tool arguments, cursors — stays JSON and never enters
// this package.
//
// Two constructive guarantees follow the design:
//
//   - Never larger. Consider re-encodes and compares; a document that does
//     not beat its JSON form by MinSavingsPct is returned unchanged, with a
//     Decision saying why. The caller can therefore always apply the result.
//   - Number fidelity. Decoding uses json.Decoder.UseNumber, so an integer
//     beyond 2^53 travels as its literal text and is emitted byte-identical.
//     No value is ever routed through float64.
//
// # Grammar (frozen; golden-tested in testdata/)
//
// A document is a sequence of lines joined by "\n", with no trailing
// newline. Indentation is Indent spaces per level (minimum and default 2 —
// list dashes occupy the first two columns of their level).
//
//	document  := [header] block
//	header    := "#toon/1 (display encoding; send tool arguments as JSON)"
//	block     := scalar | object | list | table
//	object    := ( key ":" SP scalar | key ":" NL block' | table )*
//	list      := ( "-" SP scalar | "-" NL block' )*
//	table     := key "[" count "]" "{" col ("," col)* "}:" NL row*
//	row       := field ("," field)*
//
// Rules:
//
//   - Scalars are emitted as their JSON text: numbers verbatim, true/false,
//     null, strings bare unless quoting is required.
//   - Object keys are sorted byte-ascending. JSON object order is not
//     preserved by any Go decoder, so sorting is the only deterministic
//     choice; determinism is contract (canonical.md §6).
//   - An empty object is "{}" and an empty list is "[]", on the key's own
//     line. They are the only inline aggregates.
//   - TABLE FORM is the whole point of the encoding. A list qualifies when
//     it has at least MinTableRows elements, every element is a non-empty
//     object, all elements share the identical key set, every value is a
//     scalar, and the column count is at most MaxTableCols. Columns are the
//     sorted key set; the header states the row count so a truncated table
//     is still self-describing. Rows are the values in column order joined
//     by "," — the separator is fixed, never inferred.
//   - Any other list uses "- " per element. A non-scalar element is written
//     as a block one level deeper whose first line's indent is overwritten
//     with "- ", the YAML shape readers already know.
//
// # Quoting
//
// A string is emitted bare unless it could be misread. It is quoted (Go
// strconv.Quote, so escapes are the familiar \n \t \" \\ and non-printable
// runes become \u escapes) when it is empty, has leading or trailing space,
// contains any of , : " \\ # or a control character, starts with "[", "{"
// or a list dash, or would read back as a number, true, false or null.
// Keys and column names additionally quote on any interior whitespace.
// Everything else — ordinary prose, paths, URLs — is bare, which is where
// the saving against JSON comes from.
//
// # Budget
//
// With Options.Budget > 0 the encoder truncates at a LINE boundary and
// appends the frozen TruncationMarker naming how many lines of how many
// survived. Truncation is honest and visible; it never cuts mid-row, so a
// truncated table is still parseable by eye.
//
// The package depends on the standard library only, and it reports byte
// counts. Nothing converts those into tokens: 48c7e94 removed the estimator
// once nothing read its number, so a byte count is the measurement rather
// than an input to one.
package toonenc
