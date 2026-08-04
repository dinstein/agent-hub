package buildrules

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The manifest is rendered rather than read. It is the only file the installer
// on a machine without Homebrew has to understand, and the heredoc-free
// printf sequence that produces it interpolates six values; reading the
// script's source would grade the template, and only the rendered form shows
// whether what comes out is JSON at all.
const (
	manifestTag    = "v9.9.9"
	manifestCommit = "abc1234"
	manifestSrc    = "example/agent-hub"
)

// manifestSums is a checksums-cli.txt as build-release-artifacts.sh writes it:
// four platforms the installer serves, plus the windows tarball it does not.
const manifestSums = `1e29760000000000000000000000000000000000000000000000000000000001  agenthub-9.9.9-abc1234-darwin-amd64.tar.gz
1e29760000000000000000000000000000000000000000000000000000000002  agenthub-9.9.9-abc1234-darwin-arm64.tar.gz
1e29760000000000000000000000000000000000000000000000000000000003  agenthub-9.9.9-abc1234-linux-amd64.tar.gz
1e29760000000000000000000000000000000000000000000000000000000004  agenthub-9.9.9-abc1234-linux-arm64.tar.gz
1e29760000000000000000000000000000000000000000000000000000000005  agenthub-9.9.9-abc1234-windows-amd64.tar.gz
`

// releaseManifest is the shape scripts/install.sh reads, and the shape this
// file asserts. Keeping it here rather than in a shipped package is
// deliberate: nothing in the binary consumes the manifest (canonical.md §7
// decision 6 — no update checker, no outbound request to a version manifest
// from internal/*), so a Go type for it would be a consumer that must not
// exist.
type releaseManifest struct {
	Schema  int    `json:"schema"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Channel string `json:"channel"`
	BaseURL string `json:"base_url"`
	CLI     []struct {
		OS     string `json:"os"`
		Arch   string `json:"arch"`
		Asset  string `json:"asset"`
		SHA256 string `json:"sha256"`
	} `json:"cli"`
}

// runManifest renders one manifest, returning the script's own failure rather
// than asserting, for the checks that want it to refuse.
func runManifest(t *testing.T, checksums, tag string) (string, error) {
	t.Helper()
	sums := filepath.Join(t.TempDir(), "checksums-cli.txt")
	if err := os.WriteFile(sums, []byte(checksums), 0o644); err != nil {
		t.Fatalf("writing the checksums file: %v", err)
	}
	script := filepath.Join(repoRoot(t), "scripts", "release-manifest.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("scripts/release-manifest.sh: %v; both release paths run it on every tag", err)
	}
	// Invoked through bash rather than through the shebang: the exec bit is
	// checked separately below, and a missing one there should read as the
	// permissions problem it is, not as every check in this file failing.
	cmd := exec.Command("bash", script, tag, sums)
	cmd.Env = append(os.Environ(), "HOMEBREW_SOURCE_REPO="+manifestSrc)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestReleaseManifestDescribesTheAssetsItWasGiven is the whole contract with
// the installer in one place: valid JSON, one record per platform it can
// install, and each record naming the asset and hash the release actually
// carries.
//
// The failure this guards is invisible from the release side. Every job stays
// green whatever this file contains — nothing in CI installs from it — and a
// manifest with a stray comma, a missing arch or a URL assembled for the wrong
// repository fails on a user's machine, at the one moment they have no working
// agenthub to diagnose it with.
func TestReleaseManifestDescribesTheAssetsItWasGiven(t *testing.T) {
	out, err := runManifest(t, manifestSums, manifestTag)
	if err != nil {
		t.Fatalf("rendering the manifest: %v\n%s", err, out)
	}

	var m releaseManifest
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("the manifest is not valid JSON: %v\n%s", err, out)
	}

	if m.Schema != 1 {
		t.Errorf("schema = %d, want 1; the installer refuses a schema it does not know", m.Schema)
	}
	if m.Version != strings.TrimPrefix(manifestTag, "v") {
		t.Errorf("version = %q, want %q", m.Version, strings.TrimPrefix(manifestTag, "v"))
	}
	if m.Commit != manifestCommit {
		t.Errorf("commit = %q, want %q — it is read back out of the asset names, so a "+
			"mismatch means the manifest describes a build the release does not hold",
			m.Commit, manifestCommit)
	}
	// A release artifact is built with -X main.channel=release; the default is
	// dev, which resolves the DEVELOPMENT data directory. The installer checks
	// the binary it just unpacked against this field, and the symptom of
	// getting it wrong is "all my servers disappeared".
	if m.Channel != "release" {
		t.Errorf("channel = %q, want \"release\"", m.Channel)
	}
	// The one place a download URL is assembled. install.sh appends an asset
	// name to it and constructs nothing itself, which is what keeps the two
	// from ever disagreeing about the layout of a Release.
	want := "https://github.com/" + manifestSrc + "/releases/download/" + manifestTag
	if m.BaseURL != want {
		t.Errorf("base_url = %q, want %q", m.BaseURL, want)
	}

	got := map[string]string{}
	for _, e := range m.CLI {
		if e.OS == "" || e.Arch == "" || e.Asset == "" || e.SHA256 == "" {
			t.Errorf("incomplete cli record: %+v", e)
		}
		got[e.OS+"/"+e.Arch] = e.Asset + " " + e.SHA256
	}
	for _, platform := range []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"} {
		if _, ok := got[platform]; !ok {
			t.Errorf("no %s record; install.sh has no other source for the asset name, so "+
				"that platform simply cannot install this release", platform)
		}
	}
	// Present in the checksums file, deliberately absent here: the installer
	// is a POSIX shell script. A windows record would be an offer nothing can
	// fulfil, and the user would find out after the download.
	if _, ok := got["windows/amd64"]; ok {
		t.Error("the manifest offers windows/amd64, which scripts/install.sh cannot install")
	}
	if want := "agenthub-9.9.9-abc1234-darwin-arm64.tar.gz 1e29760000000000000000000000000000000000000000000000000000000002"; got["darwin/arm64"] != want {
		t.Errorf("darwin/arm64 = %q, want %q — asset names and hashes are read back out of "+
			"the checksums file, never recomposed", got["darwin/arm64"], want)
	}
}

// TestReleaseManifestRefusesAnIncompleteRelease keeps a partial build from
// being described as a whole one.
//
// A missing platform costs nothing at release time and everything on the
// machine that has that platform: the installer finds no record, and the user
// reads "unsupported platform" about a platform this project supports.
func TestReleaseManifestRefusesAnIncompleteRelease(t *testing.T) {
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(manifestSums), "\n") {
		if strings.Contains(line, "linux-arm64") {
			continue
		}
		kept = append(kept, line)
	}
	out, err := runManifest(t, strings.Join(kept, "\n")+"\n", manifestTag)
	if err == nil {
		t.Fatalf("rendered a manifest with no linux/arm64 entry:\n%s", out)
	}
	if !strings.Contains(out, "linux/arm64") {
		t.Errorf("the refusal does not name the missing platform:\n%s", out)
	}
}

// TestReleaseManifestRefusesAnAssetBuiltAtAnotherVersion catches the mix-up
// that a checksums file cannot: a dist/ directory holding artifacts from two
// builds.
//
// The manifest reads its build id out of an asset name, so without this check
// a leftover tarball would be published under the new tag's version with the
// old tag's bytes — a download that succeeds, verifies, and installs the wrong
// binary.
func TestReleaseManifestRefusesAnAssetBuiltAtAnotherVersion(t *testing.T) {
	stale := strings.ReplaceAll(manifestSums, "agenthub-9.9.9-abc1234-darwin-arm64",
		"agenthub-9.9.8-def5678-darwin-arm64")
	out, err := runManifest(t, stale, manifestTag)
	if err == nil {
		t.Fatalf("rendered a manifest describing a 9.9.8 artifact as 9.9.9:\n%s", out)
	}
}

// TestReleaseManifestScriptIsExecutable — both release paths invoke it
// directly, so a lost exec bit fails the release rather than this test.
func TestReleaseManifestScriptIsExecutable(t *testing.T) {
	info, err := os.Stat(filepath.Join(repoRoot(t), "scripts", "release-manifest.sh"))
	if err != nil {
		t.Fatalf("scripts/release-manifest.sh: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("scripts/release-manifest.sh is not executable; both release paths run it directly")
	}
}
