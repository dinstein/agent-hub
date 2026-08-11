// Package agenthub ships one document: the SKILL.md an AI client loads to
// learn how to drive this CLI, compiled into the binary that it teaches.
//
// It exists because the document had exactly one distribution channel, the
// Homebrew tap that scripts/tap-sync.sh regenerates on every release. Nothing
// served a `go install` build, a downloaded binary, or a machine with no tap
// checked out — and a client reading a skill from a tap that was last synced
// two releases ago is told about flags the binary beside it does not have.
// Embedding makes the copy and the binary the same artifact.
//
// The bytes are embedded VERBATIM, and this package must not start generating
// frontmatter. The tap's copy is this file plus a banner and an injected
// `version:` field, written by tap-sync.sh; a second writer of that same YAML
// block is how a copy ends up carrying two version lines, with a parser
// picking whichever one disagrees with the release. Whoever prints these bytes
// says what release they came from out of band — `agenthub --skill --json`
// carries the binary's version beside the document (internal/cli/skilldoc.go).
//
// The package sits here, outside internal/, because the file cannot move to
// it: `//go:embed` reaches no further than its own directory, and
// scripts/tap-sync.sh reads skills/agenthub/SKILL.md by that path on every
// release. A package beside the file it ships beats a second copy of the file
// somewhere a build can reach. One exported string, and no API surface beyond
// it.
package agenthub

import _ "embed"

// SkillMD is skills/agenthub/SKILL.md, byte for byte. A string rather than a
// []byte so no caller can edit the document the process is serving.
//
//go:embed SKILL.md
var SkillMD string
