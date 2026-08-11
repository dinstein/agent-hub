package cli

import (
	"io"

	"github.com/spf13/cobra"

	bundledskill "github.com/dinstein/agent-hub/skills/agenthub"
)

// `agenthub manual` prints the document that teaches an AI client to drive
// this CLI, the one compiled into the binary (skills/agenthub/embed.go). It is
// how an agent bootstraps itself:
//
//	agenthub manual > ~/.claude/skills/agenthub/SKILL.md
//
// A COMMAND OF ITS OWN, and deliberately not `skill doc`. The `skill` group
// manages a library of packages the operator imports from elsewhere, and every
// one of its invariants is built for text agenthub did not write: imports
// refuse symlinks, `enabled` reads fail-closed to disabled, a library copy is
// pinned by fingerprint and goes Tampered when it drifts, and the whole
// skills-over-MCP face is off by default because it is "a new supply channel
// of untrusted text" (internal/gateway/skills.go). This document is the
// opposite of all that — it is the binary describing itself, as trustworthy as
// the code that prints it. Filing it in the library would need a third
// SourceKind, an exception to that fail-closed default, and a meaning for
// `update` and `rm` on something that ships compiled in. Same noun, opposite
// trust model; a different name is what keeps the library's rules from having
// to bend around one entry.
//
// It is VISIBLE in a release build, where the `skill` group is not, and that
// is the whole point: a client which has never heard of agenthub asks the
// shipped binary what it is.
//
// The output is the embedded bytes verbatim, so `agenthub manual` and the
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

// newManualCmd builds `agenthub manual`. It reads a compiled-in constant and
// touches no state, so it cannot fail — the printer is still the one every
// other command uses, which is what makes --json wrap the document in the
// same envelope rather than being silently ignored.
func (a *App) newManualCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "manual",
		Short: "Print the SKILL.md that teaches an AI client to drive this CLI",
		Long: "Print the skill document compiled into this binary, so an AI client can be\n" +
			"taught to drive the CLI it is actually running against:\n\n" +
			"  agenthub manual > ~/.claude/skills/agenthub/SKILL.md\n\n" +
			"The output is the document and nothing else, so it can be redirected straight\n" +
			"into a client's skills directory. Use --json to read the printing binary's\n" +
			"version beside it.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.printer().Emit(SkillDocument{Version: a.version, Content: bundledskill.SkillMD})
		},
	}
}
