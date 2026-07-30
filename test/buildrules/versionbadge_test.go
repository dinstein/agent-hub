package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// badgedReadmes are the files carrying a hand-maintained copy of the version.
// Both, not one: the copy that gets forgotten is always the one nobody was
// looking at, and for six releases running that was the English README while
// the Chinese one was not yet written.
var badgedReadmes = []string{"README.md", "README.zh-CN.md"}

// versionBadge matches the shields.io version badge and captures the number.
var versionBadge = regexp.MustCompile(`img\.shields\.io/badge/version-([0-9][0-9A-Za-z.\-]*)-`)

// TestReadmeBadgesMatchVERSION keeps the two hand-written copies of the
// version number in step with the one that is authoritative.
//
// releasing.md opens by saying the VERSION file is the one source and that
// changing it in one place makes three places agree. That is true of the
// three it names — the binary's -ldflags, the .app's Info.plist, and the
// Release title — and it is exactly why these two are dangerous: they look
// like they belong to that set and they do not derive from anything. They are
// edited by hand, or they are not.
//
// They were not. `git log -L` on the badge line shows it going 0.8.0 → 0.13.0
// in a single jump: releases 0.9.0, 0.10.0, 0.11.0, 0.12.0, 0.12.1, 0.12.2 and
// 0.12.3 all shipped with a README announcing 0.8.0. Seven consecutive
// releases, on the first screen of the project's front page, and the release
// commits of five of them touched VERSION and nothing else.
//
// Nothing else could have caught it. The badge is a static image URL, so it
// renders a wrong number as confidently as a right one; CI checks the tag
// against VERSION, which says nothing about prose; and a reader who compares
// them is a reader who already suspected something.
func TestReadmeBadgesMatchVERSION(t *testing.T) {
	root := repoRoot(t)

	raw, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		t.Fatalf("reading VERSION: %v", err)
	}
	want := strings.TrimSpace(string(raw))
	if want == "" {
		t.Fatal("VERSION is empty; the release toolchain reads this file, so fix that first")
	}

	for _, name := range badgedReadmes {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Errorf("reading %s: %v", name, err)
			continue
		}
		m := versionBadge.FindSubmatch(data)
		if m == nil {
			t.Errorf("%s has no version badge.\n"+
				"If it was removed on purpose, drop it from badgedReadmes in this test — "+
				"a check that silently stops checking is the failure it exists to prevent.", name)
			continue
		}
		if got := string(m[1]); got != want {
			t.Errorf("%s advertises version %s, VERSION says %s.\n"+
				"The badge does not derive from anything: bump it in the release commit, "+
				"in every README that carries one.", name, got, want)
		}
	}
}
