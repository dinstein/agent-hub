package downstream

import (
	"strings"
	"testing"
)

// tailLines is the projection every handshake failure and every respawn line
// reports a dead child through, so its edge cases are the ones that decide
// whether a crash leaves usable evidence. The comment on it used to claim it
// was tested directly and nothing did.

func TestTailLinesKeepsTheLastNNonBlankLines(t *testing.T) {
	raw := "one\ntwo\n\nthree\n   \nfour\n"
	got := tailLines(raw, 2)
	want := []string{"three", "four"}
	if len(got) != len(want) {
		t.Fatalf("tailLines(...) = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTailLinesNormalizesCRLFAndTrimsTrailingSpace(t *testing.T) {
	got := tailLines("alpha  \r\nbeta\t\r\n", 5)
	want := []string{"alpha", "beta"}
	if len(got) != len(want) {
		t.Fatalf("tailLines(...) = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A downstream that dumps a stack trace on one line must not turn our error
// into that stack trace.
func TestTailLinesCapsOneLineAtStderrLineCap(t *testing.T) {
	got := tailLines(strings.Repeat("x", stderrLineCap+50), 1)
	if len(got) != 1 {
		t.Fatalf("tailLines(...) returned %d lines, want 1", len(got))
	}
	if !strings.HasSuffix(got[0], "…") {
		t.Errorf("an over-long line should be marked truncated, got %q", got[0])
	}
	if trimmed := strings.TrimSuffix(got[0], "…"); len(trimmed) != stderrLineCap {
		t.Errorf("kept %d bytes, want %d", len(trimmed), stderrLineCap)
	}
}

func TestTailLinesReturnsNothingForEmptyOrNonPositiveN(t *testing.T) {
	if got := tailLines("", 5); got != nil {
		t.Errorf("tailLines(\"\", 5) = %q, want nil", got)
	}
	if got := tailLines("a\nb", 0); got != nil {
		t.Errorf("tailLines(_, 0) = %q, want nil", got)
	}
	if got := tailLines("\n  \n\t\n", 5); len(got) != 0 {
		t.Errorf("a window of only blank lines should yield none, got %q", got)
	}
}

// Pins the gap documented on stderrTail: the first line of a byte-cut window
// is a fragment and is reported whole. This test asserts what the code does,
// not what it should do — if the cap is ever plumbed through so the fragment
// can be dropped, this test is the one that must change.
func TestTailLinesReportsALeadingFragmentAsAWholeLine(t *testing.T) {
	// What a 4 KiB cut through "...continued\nsecond\n" looks like.
	got := tailLines("tinued\nsecond\n", 5)
	if len(got) != 2 || got[0] != "tinued" {
		t.Fatalf("tailLines(...) = %q, want the fragment kept as line 0", got)
	}
}

func TestFormatStderrTailJoinsAndHandlesEmpty(t *testing.T) {
	if got := formatStderrTail(nil); got != "" {
		t.Errorf("formatStderrTail(nil) = %q, want empty", got)
	}
	if got := formatStderrTail([]string{"a", "b"}); got != "a | b" {
		t.Errorf("formatStderrTail = %q, want %q", got, "a | b")
	}
}
