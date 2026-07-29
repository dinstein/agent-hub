package skills

import (
	"errors"
	"strings"
	"testing"
)

const userRules = "# my own rules\n\nAlways run tests.\n"

// TestSentinelPreservesForeignBytes is the SentinelBlock contract: install,
// update and remove must leave every byte the user wrote exactly where it
// was.
func TestSentinelPreservesForeignBytes(t *testing.T) {
	withBlock, err := upsertBlock(userRules, "pdf", "first body", "rules.mdc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(withBlock, userRules) {
		t.Fatalf("user content was disturbed:\n%s", withBlock)
	}
	updated, err := upsertBlock(withBlock, "pdf", "second body", "rules.mdc")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(updated, "first body") {
		t.Error("update left the old body behind")
	}
	if !strings.HasPrefix(updated, userRules) {
		t.Fatalf("update disturbed user content:\n%s", updated)
	}
	restored, removed, err := removeBlockFrom(updated, "pdf", "rules.mdc")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("remove reported nothing removed")
	}
	if restored != userRules {
		t.Errorf("round trip did not restore the file:\n%q\nwant\n%q", restored, userRules)
	}
}

// TestSentinelKeepsNeighbourBlocks: two skills sharing one file must not
// disturb each other.
func TestSentinelKeepsNeighbourBlocks(t *testing.T) {
	c, err := upsertBlock(userRules, "pdf", "pdf body", "rules.mdc")
	if err != nil {
		t.Fatal(err)
	}
	c, err = upsertBlock(c, "sql", "sql body", "rules.mdc")
	if err != nil {
		t.Fatal(err)
	}
	c, _, err = removeBlockFrom(c, "pdf", "rules.mdc")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c, "pdf body") {
		t.Error("pdf block survived its removal")
	}
	if !strings.Contains(c, "sql body") || !strings.HasPrefix(c, userRules) {
		t.Errorf("neighbour or user content lost:\n%s", c)
	}
}

// TestSentinelTrailingNewlineNormalization documents the ONE byte outside
// our markers this package may add.
func TestSentinelTrailingNewlineNormalization(t *testing.T) {
	got, err := upsertBlock("no trailing newline", "pdf", "body", "rules.mdc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "no trailing newline\n"+startMarker("pdf")) {
		t.Errorf("unexpected layout:\n%q", got)
	}
	back, _, err := removeBlockFrom(got, "pdf", "rules.mdc")
	if err != nil {
		t.Fatal(err)
	}
	if back != "no trailing newline\n" {
		t.Errorf("restored %q, want the documented newline normalization", back)
	}
}

// TestSentinelDamagedRefusesWrite is the load-bearing safety test: every
// damaged marker shape must refuse both the write and the removal, and both
// must classify as ErrConflict so callers fail closed uniformly.
func TestSentinelDamagedRefusesWrite(t *testing.T) {
	start, end := startMarker("pdf"), endMarker("pdf")
	cases := map[string]string{
		"start without end": userRules + start + "\nbody\n",
		"end without start": userRules + "body\n" + end + "\n",
		"end before start":  userRules + end + "\nbody\n" + start + "\n",
		"duplicate start":   userRules + start + "\nbody\n" + start + "\nmore\n" + end + "\n",
		"duplicate end":     userRules + start + "\nbody\n" + end + "\ntail\n" + end + "\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := upsertBlock(content, "pdf", "new body", "rules.mdc")
			if err == nil {
				t.Fatalf("damaged sentinels were overwritten:\n%s", out)
			}
			if !errors.Is(err, ErrConflict) {
				t.Errorf("err = %v, want ErrConflict", err)
			}
			var se *SentinelError
			if !errors.As(err, &se) {
				t.Errorf("err = %T, want *SentinelError", err)
			}
			if out != content {
				t.Error("content was modified despite the refusal")
			}
			if _, _, err := removeBlockFrom(content, "pdf", "rules.mdc"); !errors.Is(err, ErrConflict) {
				t.Errorf("remove err = %v, want ErrConflict", err)
			}
		})
	}
}

func TestContainsSentinelMarker(t *testing.T) {
	if !containsSentinelMarker("text " + endMarker("x") + " more") {
		t.Error("embedded end marker not detected")
	}
	if containsSentinelMarker("plain text") {
		t.Error("false positive")
	}
}

func TestFindBlockAbsent(t *testing.T) {
	span, found, err := findBlock(userRules, "pdf", "rules.mdc")
	if err != nil || found {
		t.Fatalf("found=%v err=%v span=%+v", found, err, span)
	}
}
