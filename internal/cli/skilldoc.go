package cli

import (
	"io"

	bundledskill "github.com/dinstein/agent-hub/skills/agenthub"
)

// `--skill` prints the document that teaches an AI client to drive this CLI,
// the one compiled into the binary (skills/agenthub/embed.go). It is how an
// agent bootstraps itself:
//
//	agenthub --skill > ~/.claude/skills/agenthub/SKILL.md
//
// A ROOT FLAG, not a member of the `skill` group. That group manages a library
// of skill packages the operator imports and materializes into clients; this
// prints one constant that ships with the binary and reads nothing on disk, so
// filing it there would put a command that cannot fail beside nine that depend
// on the library's state. It sits with `--version` instead, which it is
// shaped after: parse a flag, write a document, exit. It is also VISIBLE in a
// release build, where the `skill` group is not — the whole point is that a
// client which has never heard of agenthub can ask the binary what it does.
//
// The output is the embedded bytes verbatim, so `agenthub --skill` and the
// tap's SKILL.md are the same document (the tap's carries a generated banner
// and a `version:` field; see the embed package for why this path adds
// neither). Redirecting it into a client's skills directory has to produce a
// file that client will load, which is why nothing is prepended to it.
type SkillDocument struct {
	// Version is the binary that printed the document, not a version of the
	// document: the frontmatter deliberately carries no version field here,
	// and this is where a caller reads which release the text describes.
	Version string `json:"version"`
	Content string `json:"content"`
}

// Human writes the SKILL.md itself — no header, no trailing summary. Anything
// else would land in the file the caller is redirecting into.
func (d SkillDocument) Human(w io.Writer) error {
	_, err := io.WriteString(w, d.Content)
	return err
}

// emitSkill answers `agenthub --skill` through the normal printer, so --json
// wraps the document in the same envelope every other command uses rather
// than being silently ignored.
func (a *App) emitSkill() error {
	return a.printer().Emit(SkillDocument{Version: a.version, Content: bundledskill.SkillMD})
}
