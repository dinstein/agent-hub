// Package toolsig renders a downstream tool's JSON Schema as a ONE-LINE
// compact signature — the first layer of the token-saving stack in docs/subsystems/exposure.md
//
//	 ("compact signature").
//
//		read_file(path:str, encoding?:str="utf8", limit?:int) -> str
//
// A search result costs a signature instead of a schema; an agent that needs
// the schema asks for it with describe_tool. That is the "two-stage describe"
// split, and it caps the information a signature loses at exactly one extra
// round trip.
//
// # Grammar (frozen; golden-tested in testdata/signatures.golden)
//
//		signature := name "(" [param {", " param}] ")" " -> " type
//		param     := pname ["?"] ["~"] ":" type ["=" default]
//		type      := "str" | "int" | "num" | "bool" | "null" | "any"
//		           | "obj" | "obj{" key {"," key} "}"
//		           | "arr" | "arr<" type ">"
//		           | "enum{" value {"|" value} "}"
//
//	  - "?" marks an OPTIONAL parameter (one not listed in the schema's
//	    "required"). docs/subsystems/exposure.md sketched the inverse marker ("*" on the
//	    required ones); "?" was ruled instead because optional parameters are
//	    the minority in practice, so the marker is rarer and the line shorter.
//	    The lossy marker "~" is kept exactly as 7.2 defined it.
//	  - "~" marks a parameter the signature cannot state fully: a folded
//	    nested object, a truncated enum, an oversized default, a union type,
//	    a surviving $ref, or a name listed in "required" with no schema at
//	    all. It is an honesty marker, and it is what tells an agent that
//	    describe_tool would tell it more.
//	  - Parameter ORDER is: required parameters in the order the schema's
//	    "required" array lists them, then optional parameters sorted
//	    byte-ascending. JSON object member order does not survive decoding
//	    into a Go map, so "required" is the only ordering signal the schema
//	    actually carries; everything else must be sorted or it is not
//	    deterministic (canonical.md §6).
//	  - Nesting is expanded ONE level and then folded: a top-level object
//	    parameter renders as obj{key,key} (direct key names, sorted, capped at
//	    MaxObjectKeys) and anything deeper renders as plain obj. An array of
//	    objects is arr<obj>. Both fold cases set "~".
//	  - "$ref" is NOT resolved, by this package or by anything upstream of
//	    it — schemas reach here byte-identical to what the downstream sent.
//	    So a ref is not a rare survivor: every one renders as any~, and every
//	    signature carrying one is marked lossy. Chasing a ref would mean
//	    holding a schema store, and an absolute-URI ref would mean fetching
//	    it, which MCP 2026-07-28 says implementations must not do by default.
//	    describe_tool still returns the schema itself.
//	  - A schema that does not parse, or is not an object schema, renders as
//	    name(~) -> type. One shape, no guessing.
//
// # Length budget
//
// Over Options.MaxBytes the parameter list is cut and closed with "…+N more".
// The cut is deterministic and required-first: because optional parameters
// sort last, dropping from the tail drops optional ones first, and a
// signature that must lose required parameters says so in the same way.
//
// Post-condition: len(Signature.Text) <= MaxBytes whenever the skeleton
// (name + "(…+N more) -> type") fits in MaxBytes. The tool NAME is never
// truncated — it is the key the agent calls with, and a truncated key is
// worse than a long line.
//
// # Memoization
//
// Signatures are pure functions of (name, inputSchema, outputSchema,
// MaxBytes), so Cache memoizes them on a SHA-256 fingerprint of exactly
// those inputs. Shared() is the process-wide instance the catalog index
// warms; per docs/subsystems/exposure.md a second instance is legal but silently wastes
// that warm-up, so packages should reach for Shared() unless a test needs
// isolation.
package toolsig
