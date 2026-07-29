package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillMDGolden pins the parse-and-write contract: a manifest with
// frontmatter agenthub does not understand must survive a round trip with
// the unknown lines intact and the known ones in canonical order.
func TestSkillMDGolden(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "skillmd", "rich.input.md"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "skillmd", "rich.golden.md"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := ParseSkillMD(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if meta.Name != "PDF Tools" {
		t.Errorf("name = %q", meta.Name)
	}
	if meta.Description != "Extract text: even from scans" {
		t.Errorf("description = %q", meta.Description)
	}
	if meta.Version != "2.1.0" {
		t.Errorf("version = %q", meta.Version)
	}
	got := string(meta.Bytes())
	if got != string(want) {
		t.Errorf("golden mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// Round trip: writing then re-parsing must be a fixed point, otherwise
	// a second `skill update` would report a phantom change.
	again, err := ParseSkillMD(meta.Bytes())
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if string(again.Bytes()) != got {
		t.Errorf("write is not idempotent:\n%s", again.Bytes())
	}
}

func TestParseSkillMDNoFrontmatter(t *testing.T) {
	meta, err := ParseSkillMD([]byte("# Just a doc\n"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "" || meta.Body != "# Just a doc\n" {
		t.Errorf("meta = %+v", meta)
	}
}

// TestParseSkillMDUnterminated pins the fail direction: an opened but never
// closed frontmatter fence is an error, never "the whole file is metadata".
func TestParseSkillMDUnterminated(t *testing.T) {
	_, err := ParseSkillMD([]byte("---\nname: x\nbody without a fence\n"))
	if err == nil {
		t.Fatal("expected an error for unterminated frontmatter")
	}
	if !strings.Contains(err.Error(), "not terminated") {
		t.Errorf("error = %v", err)
	}
}

func TestParseSkillMDDuplicateKeyFirstWins(t *testing.T) {
	meta, err := ParseSkillMD([]byte("---\nname: first\nname: second\n---\n\nbody\n"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "first" {
		t.Errorf("name = %q, want the first occurrence", meta.Name)
	}
	if len(meta.Extra) != 1 || meta.Extra[0] != "name: second" {
		t.Errorf("extra = %q, want the shadowed line preserved", meta.Extra)
	}
}

func TestQuoteValueRoundTrip(t *testing.T) {
	for _, v := range []string{
		"plain", "with: colon", ` leading space`, `has "quotes"`, "-dash", "[bracket]", "#hash",
	} {
		if got := unquote(quoteValue(v)); got != v {
			t.Errorf("round trip %q -> %q -> %q", v, quoteValue(v), got)
		}
	}
}
