package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestFileTailNeverSplitsARune is the reason fileTail exists at all: it
// renders the daemon's stderr into the error shown when startup FAILED. That
// is the most important diagnostic the user gets, and it is the last place
// worth garbling.
//
// The tail is taken by slicing bytes from the end, which lands on an arbitrary
// byte — inside a rune for any non-ASCII log. This repository's own logs and
// messages are Chinese, so a mid-rune cut is the normal case rather than an
// exotic one, and it renders as a leading U+FFFD.
func TestFileTailNeverSplitsARune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stderr.log")

	// Every offset into a run of 3-byte runes, so the cut lands on each of
	// the three byte positions within a rune.
	body := strings.Repeat("下游连接失败", 40)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for limit := int64(10); limit < 60; limit++ {
		got := fileTail(path, limit)
		if !utf8.ValidString(got) {
			t.Fatalf("limit %d produced invalid UTF-8: %q", limit, got)
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Fatalf("limit %d produced a replacement character: %q", limit, got)
		}
		// Dropping a partial rune must cost at most the 3 orphaned bytes.
		if int64(len(got)) < limit-3 {
			t.Fatalf("limit %d returned %d bytes, more than a partial rune was dropped", limit, len(got))
		}
	}
}

func TestFileTail(t *testing.T) {
	dir := t.TempDir()

	// A missing file and an empty file both read as "<empty>": there is no
	// stderr to show either way, and the caller is already reporting why.
	if got := fileTail(filepath.Join(dir, "absent.log"), 1024); got != "<empty>" {
		t.Errorf("missing file = %q, want <empty>", got)
	}
	empty := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := fileTail(empty, 1024); got != "<empty>" {
		t.Errorf("empty file = %q, want <empty>", got)
	}

	// Under the limit the whole file comes back, trimmed.
	short := filepath.Join(dir, "short.log")
	if err := os.WriteFile(short, []byte("  boom  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := fileTail(short, 1024); got != "boom" {
		t.Errorf("short file = %q, want %q", got, "boom")
	}

	// Over the limit it is the TAIL that survives — the newest output is the
	// part that says why the process died.
	long := filepath.Join(dir, "long.log")
	if err := os.WriteFile(long, []byte("olddata-NEWEST"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := fileTail(long, 6)
	if got != "NEWEST" {
		t.Errorf("long file = %q, want the last bytes %q", got, "NEWEST")
	}
}
