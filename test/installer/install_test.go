//go:build !windows

// Package installer exercises scripts/install.sh end to end against a served
// release: manifest, tarball, checksums and all.
//
// It exists because that script is the ONLY install path for a machine with no
// Xcode Command Line Tools, and nothing else in this repository runs it. Every
// release job stays green whatever it does; the first report of a broken
// installer comes from someone who, by definition, has no working agenthub to
// diagnose it with.
//
// What is real here and what is fabricated matters. The binary inside the
// tarball is a genuine `go build` of cmd/agenthub with the release ldflags, so
// the script's self-check grades the same `--version` string a user's would;
// the manifest is rendered by scripts/release-manifest.sh, so the two files
// that must agree about asset names are the two that ship. Only base_url is
// rewritten, to point at this test's server instead of github.com.
package installer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	testVersion = "9.9.9"
	testCommit  = "abc1234"
	testTag     = "v" + testVersion
)

// platforms is every record release-manifest.sh insists on. All four are
// served with the same bytes: which one the script picks is a property of the
// machine running this test — including whether it is under Rosetta, where
// uname lies — and the test should exercise the script's choice rather than
// pin it.
var platforms = []string{"darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64"}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("locating the repository root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// buildAgenthub compiles cmd/agenthub the way a release does, or — with
// release=false — the way a build that forgot -X main.channel=release does.
// The second is not a hypothetical: that flag lives in one script, and a
// binary missing it silently resolves the DEVELOPMENT data directory.
func buildAgenthub(t *testing.T, release bool) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "agenthub")
	ldflags := "-X main.version=" + testVersion + "-" + testCommit
	if release {
		ldflags += " -X main.channel=release"
	}
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", out, "./cmd/agenthub")
	cmd.Dir = repoRoot(t)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building cmd/agenthub: %v\n%s", err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the built binary: %v", err)
	}
	return data
}

// tarball wraps a binary the way build-release-artifacts.sh does: one entry
// named agenthub, at the archive root.
func tarball(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	hdr := &tar.Header{Name: "agenthub", Mode: 0o755, Size: int64(len(binary))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatalf("tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return buf.Bytes()
}

// release is a served Release: a manifest at /manifest.json and one tarball
// per platform, all the same bytes.
type release struct {
	url      string // of the manifest
	manifest string
}

// serve renders the manifest with the real script and puts it behind an HTTP
// server together with the tarball. served is what every asset request
// answers with; sums is what the manifest pins, so passing different bytes for
// the two is how the checksum failure is provoked.
func serve(t *testing.T, served, sums []byte) *release {
	t.Helper()

	digest := sha256.Sum256(sums)
	var lines strings.Builder
	for _, p := range platforms {
		fmt.Fprintf(&lines, "%s  agenthub-%s-%s-%s.tar.gz\n",
			hex.EncodeToString(digest[:]), testVersion, testCommit, p)
	}
	// Windows is in a real checksums file and must not reach the manifest.
	fmt.Fprintf(&lines, "%s  agenthub-%s-%s-windows-amd64.tar.gz\n",
		hex.EncodeToString(digest[:]), testVersion, testCommit)

	sumsPath := filepath.Join(t.TempDir(), "checksums-cli.txt")
	if err := os.WriteFile(sumsPath, []byte(lines.String()), 0o644); err != nil {
		t.Fatalf("writing the checksums file: %v", err)
	}
	cmd := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "release-manifest.sh"), testTag, sumsPath)
	rendered, err := cmd.Output()
	if err != nil {
		t.Fatalf("rendering the manifest: %v", err)
	}

	r := &release{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/manifest.json":
			_, _ = w.Write([]byte(r.manifest))
		case strings.HasSuffix(req.URL.Path, ".tar.gz"):
			_, _ = w.Write(served)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)

	// The only edit to what the script produced. Everything the installer
	// reads to decide WHAT to download — asset names, hashes, version, commit,
	// channel — is the rendered file's own.
	r.manifest = regexp.MustCompile(`"base_url": "[^"]*"`).
		ReplaceAllString(string(rendered), `"base_url": "`+srv.URL+`"`)
	r.url = srv.URL + "/manifest.json"
	return r
}

// run invokes the installer as a user would, in an environment that cannot
// reach this machine's own agenthub: a bare PATH so `command -v agenthub`
// finds nothing, and HOME plus AGENTHUB_DATA_DIR pointed at scratch so a
// `daemon stop` inside the script cannot touch real state.
func run(t *testing.T, home string, rel *release, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{filepath.Join(repoRoot(t), "scripts", "install.sh")}, args...)...)
	cmd.Env = []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + home,
		"AGENTHUB_INSTALL_PREFIX=" + filepath.Join(home, ".local"),
		"AGENTHUB_INSTALL_MANIFEST_URL=" + rel.url,
		"AGENTHUB_DATA_DIR=" + filepath.Join(home, "data"),
		"TMPDIR=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func binPath(home string) string { return filepath.Join(home, ".local", "bin", "agenthub") }
func receiptPath(home string) string {
	return filepath.Join(home, ".local", "share", "agenthub", "install.json")
}

// TestInstallsAndUpdates covers the path every user takes and the one the
// project has no other way to check: a fresh install, then the same command
// again as an update.
//
// The second run is not a formality. It replaces a binary that already exists
// and may be executing, which is why the script stages inside the target
// directory and renames — a plain copy over the destination truncates a file
// another process is running.
func TestInstallsAndUpdates(t *testing.T) {
	tgz := tarball(t, buildAgenthub(t, true))
	rel := serve(t, tgz, tgz)
	home := t.TempDir()

	for _, pass := range []string{"install", "update"} {
		out, err := run(t, home, rel)
		if err != nil {
			t.Fatalf("%s: %v\n%s", pass, err, out)
		}
		got, err := exec.Command(binPath(home), "--version").Output()
		if err != nil {
			t.Fatalf("%s: running the installed binary: %v", pass, err)
		}
		if want := testVersion + "-" + testCommit; !strings.Contains(string(got), want) {
			t.Errorf("%s: installed binary reports %q, want %q", pass, got, want)
		}
		// The installed copy must be the release build. A dev one keeps its
		// state somewhere else entirely, and the user reads that as their
		// servers having disappeared.
		if strings.Contains(string(got), "(dev)") {
			t.Errorf("%s: installed a dev build: %q", pass, got)
		}
	}

	data, err := os.ReadFile(receiptPath(home))
	if err != nil {
		t.Fatalf("no install receipt: %v", err)
	}
	for _, want := range []string{`"method": "script"`, `"version": "` + testVersion + `"`, `"bin": "` + binPath(home) + `"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("receipt does not carry %s:\n%s", want, data)
		}
	}
}

// TestRefusesAChecksumMismatch is the one failure the installer must get right
// under any circumstance: the bytes are not the bytes the release describes.
//
// Refusing is half of it. The other half is that a refusal leaves NOTHING
// behind — no half-written binary at the target path, no receipt claiming a
// version that was never installed — because the machine reading that state
// next is the same one that has no working agenthub to check it with.
func TestRefusesAChecksumMismatch(t *testing.T) {
	honest := tarball(t, buildAgenthub(t, true))
	tampered := append(append([]byte{}, honest...), 0x00)
	rel := serve(t, tampered, honest)
	home := t.TempDir()

	out, err := run(t, home, rel)
	if err == nil {
		t.Fatalf("installed a tarball whose hash does not match the manifest:\n%s", out)
	}
	if !strings.Contains(out, "checksum mismatch") {
		t.Errorf("the refusal does not say what went wrong:\n%s", out)
	}
	if _, err := os.Stat(binPath(home)); err == nil {
		t.Error("a binary was installed despite the checksum mismatch")
	}
	if _, err := os.Stat(receiptPath(home)); err == nil {
		t.Error("a receipt was written despite the checksum mismatch")
	}
}

// TestRefusesADevBuild guards the flag that decides which data directory a
// shipped binary resolves.
//
// -X main.channel=release is set in exactly one place, and an artifact built
// without it passes every other check here: correct name, correct version,
// correct hash. What it does on a user's machine is quietly use the
// development data directory, which reads as every configured server having
// vanished. The binary is asked to answer for itself because nothing about
// the artifact's name or hash can.
func TestRefusesADevBuild(t *testing.T) {
	tgz := tarball(t, buildAgenthub(t, false))
	rel := serve(t, tgz, tgz)
	home := t.TempDir()

	out, err := run(t, home, rel)
	if err == nil {
		t.Fatalf("installed a dev build as a release:\n%s", out)
	}
	if !strings.Contains(out, "dev build") {
		t.Errorf("the refusal does not name the problem:\n%s", out)
	}
	if _, err := os.Stat(binPath(home)); err == nil {
		t.Error("a dev binary reached the install path")
	}
}

// TestRefusesAnUnknownSchema keeps a newer release from being installed by an
// older script on a guess. The installer is fetched from a branch, not pinned
// to a version, so an old copy on someone's disk WILL meet a manifest written
// after it.
func TestRefusesAnUnknownSchema(t *testing.T) {
	tgz := tarball(t, buildAgenthub(t, true))
	rel := serve(t, tgz, tgz)
	rel.manifest = strings.Replace(rel.manifest, `"schema": 1`, `"schema": 2`, 1)
	home := t.TempDir()

	out, err := run(t, home, rel)
	if err == nil {
		t.Fatalf("installed from a schema it does not understand:\n%s", out)
	}
	if !strings.Contains(out, "schema") {
		t.Errorf("the refusal does not mention the schema:\n%s", out)
	}
}

// TestUninstallLeavesTheDataAlone covers the asymmetry a user depends on:
// removing the program is not removing their servers and credentials.
func TestUninstallLeavesTheDataAlone(t *testing.T) {
	tgz := tarball(t, buildAgenthub(t, true))
	rel := serve(t, tgz, tgz)
	home := t.TempDir()

	if out, err := run(t, home, rel); err != nil {
		t.Fatalf("installing: %v\n%s", err, out)
	}
	data := filepath.Join(home, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatalf("creating the data directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(data, "servers.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("seeding the data directory: %v", err)
	}

	out, err := run(t, home, rel, "--uninstall")
	if err != nil {
		t.Fatalf("uninstalling: %v\n%s", err, out)
	}
	if _, err := os.Stat(binPath(home)); err == nil {
		t.Error("the binary survived --uninstall")
	}
	if _, err := os.Stat(receiptPath(home)); err == nil {
		t.Error("the receipt survived --uninstall")
	}
	if _, err := os.Stat(filepath.Join(data, "servers.json")); err != nil {
		t.Errorf("--uninstall deleted the data directory: %v", err)
	}

	// And --purge is the version that does mean it.
	if out, err := run(t, home, rel); err != nil {
		t.Fatalf("reinstalling: %v\n%s", err, out)
	}
	if out, err := run(t, home, rel, "--uninstall", "--purge"); err != nil {
		t.Fatalf("purging: %v\n%s", err, out)
	}
	if _, err := os.Stat(data); err == nil {
		t.Error("--purge left the data directory behind")
	}
}
