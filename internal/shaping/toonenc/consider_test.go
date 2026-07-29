package toonenc

import (
	"strings"
	"testing"
)

// The never-larger guarantee, stated as the property callers rely on: the
// string Consider returns is NEVER longer than the string it was given, for
// any input, at any settings.
func TestConsiderIsNeverLarger(t *testing.T) {
	inputs := []string{
		`{"a":1}`,                        // tiny: the header/None case
		`{}`,                             //
		`[]`,                             //
		`"x"`,                            //
		`{"a":"a,b","b":"- x","c":"42"}`, // quoting eats the saving
		`[{"id":1,"name":"a"},{"id":2,"name":"b"},{"id":3,"name":"c"}]`, // tabular: big win
		`not json at all`,
		``,
		`   `,
		strings.Repeat(`{"a":`, 40) + "1" + strings.Repeat("}", 40),
	}
	for _, in := range inputs {
		for _, opts := range []Options{{}, {Header: true}, {MinSavingsPct: -1}, {Budget: 32}} {
			out, d := Consider(in, opts)
			if len(out) > len(in) {
				t.Fatalf("Consider(%q, %+v) grew from %d to %d bytes", in, opts, len(in), len(out))
			}
			if d.BaselineBytes != len(in) || d.ActualBytes != len(out) {
				t.Fatalf("Consider(%q): decision %+v does not describe the strings", in, d)
			}
			if !d.Applied && out != in {
				t.Fatalf("Consider(%q): not applied but the text changed", in)
			}
			if d.SavedBytes() != max0(len(in)-len(out)) {
				t.Fatalf("SavedBytes = %d", d.SavedBytes())
			}
		}
	}
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// Reasons are stable audit values, and each one has a reachable input.
func TestConsiderReasons(t *testing.T) {
	tests := []struct {
		name string
		in   string
		opts Options
		want Reason
	}{
		{"tabular wins", `[{"id":1,"name":"alpha"},{"id":2,"name":"bravo"},{"id":3,"name":"chuck"}]`, Options{}, ReasonApplied},
		{"not json", `plain text result`, Options{}, ReasonNotJSON},
		{"blank", "   ", Options{}, ReasonEmpty},
		// Deep single-key nesting is the shape TOON LOSES on: every level
		// costs an indent where JSON costs one brace. The never-larger
		// guarantee is what keeps that case from reaching the agent.
		{"no gain", `{"a":{"b":{"c":{"d":{"e":1}}}}}`, Options{}, ReasonNoGain},
		{"gain below threshold", `{"aaaaaaaaaa":"bbbbbbbbbb"}`, Options{MinSavingsPct: 95}, ReasonNoGain},
	}
	for _, tc := range tests {
		out, d := Consider(tc.in, tc.opts)
		if d.Reason != tc.want {
			t.Fatalf("%s: reason = %q, want %q (out=%q)", tc.name, d.Reason, tc.want, out)
		}
		if (d.Reason == ReasonApplied) != d.Applied {
			t.Fatalf("%s: Applied=%v with reason %q", tc.name, d.Applied, d.Reason)
		}
	}
}

// The accept/reject boundary is integer arithmetic and must be exact.
func TestBeatsBoundary(t *testing.T) {
	tests := []struct {
		actual, baseline, pct int
		want                  bool
	}{
		{90, 100, 10, true},  // exactly 10%
		{91, 100, 10, false}, // 9%
		{100, 100, 0, false}, // never-larger dominates a 0% threshold
		{99, 100, 0, true},
		{0, 0, 10, false},
		{5, 0, 10, false},
		{101, 100, 10, false},
	}
	for _, tc := range tests {
		if got := beats(tc.actual, tc.baseline, tc.pct); got != tc.want {
			t.Fatalf("beats(%d,%d,%d) = %v, want %v", tc.actual, tc.baseline, tc.pct, got, tc.want)
		}
	}
}

// A truncated document is still applied — and says so on both channels.
func TestConsiderReportsTruncation(t *testing.T) {
	in := `[{"id":1,"label":"alpha"},{"id":2,"label":"bravo"},{"id":3,"label":"chuck"},{"id":4,"label":"delta"}]`
	out, d := Consider(in, Options{Budget: 40})
	if !d.Applied || !d.Truncated {
		t.Fatalf("decision = %+v, out = %q", d, out)
	}
	if !strings.Contains(out, "truncated by agenthub") {
		t.Fatalf("truncated output carries no marker:\n%s", out)
	}
	if _, d := Consider(in, Options{}); d.Truncated {
		t.Fatal("an unbudgeted encode reported truncation")
	}
}
