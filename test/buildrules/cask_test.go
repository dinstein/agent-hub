package buildrules

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The cask is rendered rather than read. Every check below is about what a
// user's `brew install` actually receives, and the heredoc that produces it
// interpolates four values — the artifact repo, the tap, the version and the
// build id — so reading the script's source would grade the template and not
// the file. scripts/homebrew-formula.sh's checks read source because the
// formula's URLs are assembled from a checksums file the same way; this one
// has an interpolated `version` in the URL as well, and only the rendered form
// shows whether that interpolation lands on the asset that was uploaded.
const (
	caskTag   = "v9.9.9"
	caskHash  = "abc1234"
	caskDMG   = "AgentHub-9.9.9-" + caskHash + "-macos-universal.dmg"
	caskSum   = "1e297600ea1f41510d035138117ebd594baf71463cb5088999f665a342eec5fc"
	caskTap   = "example/homebrew-agenthub"
	caskSrc   = "example/agent-hub"
	caskToken = "agenthub-gui"
)

// renderCask runs scripts/homebrew-cask.sh over a synthetic checksums file and
// returns the cask it wrote.
func renderCask(t *testing.T) string {
	t.Helper()
	out, err := runCask(t, caskSum+"  "+caskDMG+"\n", caskTag)
	if err != nil {
		t.Fatalf("rendering the cask: %v\n%s", err, out)
	}
	return out
}

// runCask is renderCask without the assertion, for the checks that want the
// script to refuse.
func runCask(t *testing.T, checksums, tag string) (string, error) {
	t.Helper()
	sums := filepath.Join(t.TempDir(), "checksums-macos.txt")
	if err := os.WriteFile(sums, []byte(checksums), 0o644); err != nil {
		t.Fatalf("writing the checksums file: %v", err)
	}
	script := filepath.Join(repoRoot(t), "scripts", "homebrew-cask.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("scripts/homebrew-cask.sh: %v; the release workflow runs it on every tag", err)
	}
	// Invoked through bash rather than through the shebang: the exec bit is
	// checked separately below, and a missing one there should read as the
	// permissions problem it is, not as every check in this file failing.
	cmd := exec.Command("bash", script, tag, sums)
	cmd.Env = append(os.Environ(),
		"HOMEBREW_SOURCE_REPO="+caskSrc,
		"HOMEBREW_TAP_REPO="+caskTap,
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// TestCaskURLResolvesToTheAssetThatWasUploaded is the cask's half of the
// agreement homebrew-formula.sh's checksum reading gives the formula.
//
// The URL is not written out; it is interpolated from `version`, whose two
// comma-separated fields are the release version and the build id parsed back
// out of the DMG's name. That indirection is what lets one line describe both,
// and it is also a second place where the asset name is composed — so this
// check performs the interpolation Homebrew would and asserts the result is
// the file the release actually published.
//
// Getting it wrong is invisible from here: the cask is valid Ruby, the sha256
// is real, `brew style` passes, and the 404 waits for the first `brew install`
// on a machine this project cannot see.
func TestCaskURLResolvesToTheAssetThatWasUploaded(t *testing.T) {
	cask := renderCask(t)

	version := caskStanza(t, cask, `version "([^"]+)"`)
	first, second, ok := strings.Cut(version, ",")
	if !ok {
		t.Fatalf("version %q has no build id; the URL interpolates version.csv.second, "+
			"which is empty for a version without a comma", version)
	}
	url := caskStanza(t, cask, `url "([^"]+)" \\\n\s*"([^"]+)"`)
	resolved := strings.NewReplacer(
		"#{version.csv.first}", first,
		"#{version.csv.second}", second,
	).Replace(url)

	if want := caskTag + "/" + caskDMG; !strings.HasSuffix(resolved, want) {
		t.Errorf("the cask's URL resolves to %q, but the release published %q.\n"+
			"Both jobs would pass and `brew install` would 404 on the first machine "+
			"that is not this one.", resolved, want)
	}
	if !strings.Contains(resolved, "github.com/"+caskSrc+"/") {
		t.Errorf("the cask's URL is %q, which is not on HOMEBREW_SOURCE_REPO (%s); "+
			"the DMG is uploaded there", resolved, caskSrc)
	}
	if got := caskStanza(t, cask, `sha256 "([^"]+)"`); got != caskSum {
		t.Errorf("sha256 is %q, not the %q the checksums file carries", got, caskSum)
	}
}

// TestCaskLeavesThePATHEntryToTheFormula pins the one stanza this cask must
// never grow.
//
// The .app carries its own agenthub — build/Taskfile.common.yml's layout
// contract requires it, because api/dialorstart.go resolves the daemon as the
// sibling of its own executable. Putting that copy on $PATH with a `binary`
// stanza is the obvious next thought and is wrong: $(brew --prefix)/bin/agenthub
// already belongs to the agenthub formula, and two packages claiming one path
// is an install-time error for everyone who has both. The Cask Cookbook
// documents `conflicts_with cask:` only, so there is no way to declare the
// collision away either.
//
// What the cask does instead is depend on the formula, fully qualified: a bare
// "agenthub" is resolved against every tap on the machine and against
// homebrew-core, so the day a core formula takes that name, installing this
// GUI starts pulling in a stranger's binary.
func TestCaskLeavesThePATHEntryToTheFormula(t *testing.T) {
	cask := renderCask(t)

	if regexp.MustCompile(`(?m)^\s*binary\s`).MatchString(commentless(cask)) {
		t.Error("the cask has a `binary` stanza. It links a second agenthub over the " +
			"formula's, which fails to install on every machine that has both — and " +
			"`conflicts_with formula:` is not a documented way out. Depend on the " +
			"formula instead and leave $PATH to it.")
	}

	dep := caskStanza(t, cask, `depends_on formula: "([^"]+)"`)
	if strings.Count(dep, "/") != 2 {
		t.Errorf("depends_on formula: %q is not tap-qualified (owner/tap/formula).\n"+
			"An unqualified name resolves against every tap on the user's machine "+
			"and against homebrew-core.", dep)
	}
	if want := "example/agenthub/agenthub"; dep != want {
		t.Errorf("depends_on formula: %q; HOMEBREW_TAP_REPO=%s should render %q",
			dep, caskTap, want)
	}
}

// TestCaskQuarantineOverrideCarriesItsReasoning keeps a Gatekeeper override
// from outliving the explanation of why it was acceptable.
//
// The postflight clears com.apple.quarantine because the bundle is ad-hoc
// signed and not notarized, and what stands in for Gatekeeper is the pinned
// sha256. That trade is defensible while it is written down beside the code
// that makes it and indefensible the moment it is not — a cask that silently
// disarms Gatekeeper is exactly the thing a reader is entitled to be told
// about, and the reader here is a user running `brew cat`.
//
// It cuts both ways on purpose. When notarization lands and the postflight
// goes, the prose must go with it: a cask that still warns about being
// unsigned after it is signed teaches people to ignore what it says.
func TestCaskQuarantineOverrideCarriesItsReasoning(t *testing.T) {
	cask := renderCask(t)
	strips := strings.Contains(commentless(cask), "com.apple.quarantine") ||
		strings.Contains(commentless(cask), `"/usr/bin/xattr"`)
	explains := strings.Contains(cask, "notariz") && strings.Contains(cask, "sha256")

	switch {
	case strips && !explains:
		t.Error("the cask clears quarantine without saying, in the file itself, that the " +
			"app is not notarized and that the pinned sha256 is what replaces Gatekeeper. " +
			"`brew cat` is where a user reads this.")
	case !strips && explains:
		t.Error("the cask no longer clears quarantine but still explains why it does. " +
			"If the app is notarized now, delete the explanation too — a warning that " +
			"outlives its cause trains people to ignore the next one.")
	}

	// The caveats are the half a user sees without asking; the header comment
	// is the half a reviewer sees. Neither substitutes for the other.
	if strips && !strings.Contains(caskCaveats(t, cask), "quarantine") {
		t.Error("the cask clears quarantine but its caveats do not mention it; that is " +
			"the only part of this file `brew install` prints")
	}
}

// TestCaskRefusesAReleaseItCannotDescribe proves the script's guards block
// rather than merely exist (canonical.md §6).
//
// Each of these produces a perfectly valid cask if the check is missing, and
// each fails only on someone else's machine: a 404 on install, or — for the
// dirty build — a tap serving an artifact that no commit can reproduce.
func TestCaskRefusesAReleaseItCannotDescribe(t *testing.T) {
	for _, tc := range []struct {
		name      string
		checksums string
		tag       string
		want      string
	}{
		{
			name:      "no DMG in the checksums",
			checksums: caskSum + "  agenthub-9.9.9-abc1234-linux-amd64.tar.gz\n",
			tag:       caskTag,
			want:      "release is incomplete",
		},
		{
			name:      "the DMG was built at another version",
			checksums: caskSum + "  AgentHub-9.9.8-abc1234-macos-universal.dmg\n",
			tag:       caskTag,
			want:      "is not named for",
		},
		{
			name:      "a dirty tree stamped the build id",
			checksums: caskSum + "  AgentHub-9.9.9-abc1234-dirty-macos-universal.dmg\n",
			tag:       caskTag,
			want:      "not a commit hash",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runCask(t, tc.checksums, tc.tag)
			if err == nil {
				t.Fatalf("the script rendered a cask anyway:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("refused, but not for the recorded reason: want %q in\n%s", tc.want, out)
			}
		})
	}
}

// TestCaskScriptIsExecutable — the release workflow invokes it directly.
func TestCaskScriptIsExecutable(t *testing.T) {
	info, err := os.Stat(filepath.Join(repoRoot(t), "scripts", "homebrew-cask.sh"))
	if err != nil {
		t.Fatalf("scripts/homebrew-cask.sh: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("scripts/homebrew-cask.sh is not executable; the release workflow runs it directly")
	}
}

// caskStanza returns the first submatch of pat, joined when the pattern has
// several — the URL is written over two lines and Ruby concatenates them.
func caskStanza(t *testing.T, cask, pat string) string {
	t.Helper()
	m := regexp.MustCompile(pat).FindStringSubmatch(cask)
	if m == nil {
		t.Fatalf("no %s in the rendered cask:\n%s", pat, cask)
	}
	return strings.Join(m[1:], "")
}

// caskCaveats returns the body of the caveats heredoc.
func caskCaveats(t *testing.T, cask string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)caveats <<~EOS\n(.*?)\n\s*EOS`).FindStringSubmatch(cask)
	if m == nil {
		t.Fatal("the cask has no caveats block; it is the only part of the file " +
			"`brew install` puts in front of a user")
	}
	return m[1]
}

// commentless strips Ruby comment lines, so prose about a stanza is not
// mistaken for the stanza — the same reason releaseWorkflow strips YAML
// comments.
func commentless(cask string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(cask, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
