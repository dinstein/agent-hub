package clients

import (
	"strings"
	"testing"
)

// TestSpliceRefusesRatherThanGuess pins the failure direction of the write
// path: a document whose shape the locator cannot walk is REFUSED, and the
// refusal is what the caller turns into "add this by hand". Nothing about
// reading a file successfully entitles agenthub to edit it blind.
func TestSpliceRefusesRatherThanGuess(t *testing.T) {
	value := map[string]any{"command": "/bin/agenthub"}
	cases := []struct {
		name string
		src  string
	}{
		{"section is not an object", `{"mcpServers": [1, 2]}`},
		{"root is not an object", `[1, 2]`},
		{"root is a scalar", `"just a string"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := spliceEntry([]byte(tc.src), []string{"mcpServers"}, "agenthub", value); err == nil {
				t.Errorf("spliced into %s instead of refusing", tc.name)
			}
		})
	}
}

// TestVerifySpliceCatchesAWrongEdit is the check the whole design leans on.
// A splice that lands in the wrong place, or eats a comment, must be caught
// BEFORE the bytes reach the disk — so here the "edit" is deliberately bad.
func TestVerifySpliceCatchesAWrongEdit(t *testing.T) {
	before := []byte("// keep me\n{\n  \"mcpServers\": {},\n  \"other\": 1\n}\n")

	// Correct: only our entry appears.
	good, err := spliceEntry(before, []string{"mcpServers"}, "agenthub", map[string]any{"command": "x"})
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	if err := verifySplice(before, good, []string{"mcpServers"}, []string{"agenthub"}); err != nil {
		t.Fatalf("a correct splice was rejected: %v", err)
	}

	// Someone else's value changed as well.
	tampered := []byte(strings.Replace(string(good), `"other": 1`, `"other": 2`, 1))
	if err := verifySplice(before, tampered, []string{"mcpServers"}, []string{"agenthub"}); err == nil {
		t.Error("a splice that changed another key was accepted")
	}

	// A comment went missing.
	stripped := []byte(strings.Replace(string(good), "// keep me\n", "", 1))
	if err := verifySplice(before, stripped, []string{"mcpServers"}, []string{"agenthub"}); err == nil {
		t.Error("a splice that dropped a comment was accepted")
	}

	// The result does not parse.
	broken := append([]byte{}, good...)
	broken = append(broken, '{')
	if err := verifySplice(before, broken, []string{"mcpServers"}, []string{"agenthub"}); err == nil {
		t.Error("a splice that produced an unparseable document was accepted")
	}
}

// TestBlankJSONCLeavesStringsAlone: a `//` inside a value is not a comment,
// and blanking it would corrupt the very thing the caller wanted to read.
func TestBlankJSONCLeavesStringsAlone(t *testing.T) {
	src := []byte(`{"url": "https://x/y", /* c */ "a": "/* not a comment */"}`)
	out := blankJSONC(src)
	if !strings.Contains(string(out), `"https://x/y"`) {
		t.Errorf("a URL inside a string was blanked: %s", out)
	}
	if !strings.Contains(string(out), `"/* not a comment */"`) {
		t.Errorf("comment syntax inside a string was blanked: %s", out)
	}
	if strings.Contains(string(out), "/* c */") {
		t.Errorf("a real comment survived: %s", out)
	}
	if len(out) != len(src) {
		t.Errorf("length changed: %d -> %d", len(src), len(out))
	}
}

// TestVerifySpliceComparesNumbersExactly guards the two ways float64 was the
// wrong tool for proving two documents are the same.
//
// The collision half is the one that matters: the check exists to notice an
// edit that touched something it should not have, and under float64 two
// distinct integers become the same value, so the corruption it was built to
// catch would have compared equal. The overflow half is milder — a document
// read() accepts becoming one the writer refuses — but it is what a fuzz run
// actually found, and testdata carries the input.
func TestVerifySpliceComparesNumbersExactly(t *testing.T) {
	t.Parallel()

	t.Run("a changed value that rounds the same is still a change", func(t *testing.T) {
		// Both of these are 1e19 as a float64.
		before := []byte(`{"mcpServers": {}, "other": 10000000000000000001}`)
		after := []byte(`{"mcpServers": {"agenthub": {"command": "x"}}, "other": 10000000000000000002}`)
		if err := verifySplice(before, after, []string{"mcpServers"}, []string{"agenthub"}); err == nil {
			t.Error("an edit that rewrote a foreign number was accepted because it rounds the same")
		}
	})

	t.Run("a number beyond float64 is not a reason to refuse", func(t *testing.T) {
		big := strings.Repeat("9", 400)
		before := []byte(`{"mcpServers": {}, "other": ` + big + `}`)
		after, err := spliceEntry(before, []string{"mcpServers"}, "agenthub",
			map[string]any{"command": "x"})
		if err != nil {
			t.Fatalf("splice: %v", err)
		}
		if err := verifySplice(before, after, []string{"mcpServers"}, []string{"agenthub"}); err != nil {
			t.Errorf("a correct splice was refused over a number nobody edited: %v", err)
		}
	})

	t.Run("an untouched big number still compares equal to itself", func(t *testing.T) {
		big := strings.Repeat("7", 400)
		doc := []byte(`{"mcpServers": {"agenthub": {"command": "x"}}, "other": ` + big + `}`)
		if err := verifySplice(doc, doc, []string{"mcpServers"}, []string{"agenthub"}); err != nil {
			t.Errorf("a document compared unequal to itself: %v", err)
		}
	})
}
