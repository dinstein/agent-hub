package cli

import (
	"encoding/json"
	"strings"
	"testing"

	bundledskill "github.com/dinstein/agent-hub/skills/agenthub"
)

// TestSkillFlagPrintsTheDocumentVerbatim is the whole contract of `--skill`:
// what comes out of stdout is a file a client will load.
//
// Byte equality, not a substring check. The failure this guards against is a
// helpful line — a header, a "wrote N bytes", a trailing hint — landing inside
// the SKILL.md the caller is redirecting into, where it is either invalid
// frontmatter or invisible prose the agent then follows.
func TestSkillFlagPrintsTheDocumentVerbatim(t *testing.T) {
	code, out, errOut := runCLI(t, "", "--skill")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if out != bundledskill.SkillMD {
		t.Errorf("stdout is not the embedded document: %d bytes out, %d embedded",
			len(out), len(bundledskill.SkillMD))
	}
	if !strings.HasPrefix(out, "---\n") {
		t.Error("the output does not open with YAML frontmatter; a client will not load it as a skill")
	}
	if errOut != "" {
		t.Errorf("stderr should stay empty, got %q", errOut)
	}
}

// TestSkillFlagJSONCarriesTheBinaryVersion pins the machine path: the document
// under "content", and the version of the binary that printed it beside it.
//
// The version is the point of the envelope. The embedded document carries no
// `version:` in its frontmatter — tap-sync.sh injects that into the tap's copy
// at release time, and this path deliberately does not become a second writer
// of that block — so out of band is the only place a caller can read which
// release the text describes.
func TestSkillFlagJSONCarriesTheBinaryVersion(t *testing.T) {
	code, out, errOut := runCLI(t, "", "--skill", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Version string `json:"version"`
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decoding the envelope: %v\n%s", err, out)
	}
	if !env.OK {
		t.Error("ok is false for a command that cannot fail")
	}
	if env.Data.Version != "1.2.3-test" {
		t.Errorf("version %q, want the binary's own", env.Data.Version)
	}
	if env.Data.Content != bundledskill.SkillMD {
		t.Errorf("content is not the embedded document: %d bytes, %d embedded",
			len(env.Data.Content), len(bundledskill.SkillMD))
	}
}

// TestSkillFlagIsRootOnly keeps the flag off every subcommand. It answers with
// a constant, so on a subcommand it can only be a mistake — and one that would
// otherwise be answered by printing 200 lines of markdown where the caller
// asked for a server listing.
func TestSkillFlagIsRootOnly(t *testing.T) {
	code, out, errOut := runCLI(t, "", "server", "ls", "--skill")
	if code != ExitUsage {
		t.Errorf("exit %d, want %d (usage); stdout %q stderr %q", code, ExitUsage, out, errOut)
	}
	if strings.Contains(out, "name: agenthub") {
		t.Error("a subcommand printed the skill document")
	}
}

// TestSkillFlagDoesNotSwallowAnUnknownCommand pins the precedence in the root
// RunE: `agenthub --skill srever` asked for two things and got one of them
// wrong, so it is a usage error rather than a document with a typo buried in
// the invocation that produced it.
func TestSkillFlagDoesNotSwallowAnUnknownCommand(t *testing.T) {
	code, out, _ := runCLI(t, "", "--skill", "srever")
	if code != ExitUsage {
		t.Errorf("exit %d, want %d (usage)", code, ExitUsage)
	}
	if strings.Contains(out, "name: agenthub") {
		t.Error("an unknown command was answered with the skill document")
	}
}

// TestSkillFlagShowsOnAReleasePage is the reason it is a root flag rather than
// a member of the withheld `skill` group: a client that has never heard of
// agenthub asks the shipped binary what it does, and a release build has to
// answer.
func TestSkillFlagShowsOnAReleasePage(t *testing.T) {
	code, out, errOut := runCLIReleaseHelp(t, "", "--help")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "--skill") {
		t.Error("a release build's help page does not mention --skill")
	}
	code, out, errOut = runCLIReleaseHelp(t, "", "--skill")
	if code != 0 || out != bundledskill.SkillMD {
		t.Errorf("a release build did not print the document: exit %d, %d bytes, stderr %q",
			code, len(out), errOut)
	}
}
