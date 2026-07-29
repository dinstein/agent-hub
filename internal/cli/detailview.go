package cli

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// The label/value layout the one-subject detail views render in — today
// `server inspect`, which is the command that has the most to say about the
// fewest things.
//
// WHY NOT A TABWRITER. The listings align columns across ROWS of the same
// kind, and tabwriter is exactly right for that. A detail view is the other
// shape: a fixed vocabulary of labels, values of wildly different lengths,
// and section breaks that a tabwriter treats as the end of a column block —
// so the alignment it computes drifts from section to section, and a value
// that happens to be long re-indents every label above it. A fixed label
// column is stable, which for a view people READ DOWN matters more than
// packing each section as tightly as it could go.
//
// WHY IT HOLDS THE ERROR. Every line of a detail view is a formatted write,
// and the alternative is one `if _, err := fmt.Fprintf(...); err != nil`
// per fact — at which point the check outweighs what is being checked, and
// the temptation to drop the ones that "cannot fail" is what leaves a
// renderer silently truncating on a closed pipe. The first error is kept and
// every later write becomes a no-op, so the caller still learns output was
// lost (the contract TestEveryHumanRendererHandlesTheZeroValue pins).
type detailWriter struct {
	w io.Writer
	// err is the FIRST write error. Later ones are dropped rather than
	// overwriting it: what a caller needs is the failure that started it.
	err error
}

// detailLabelWidth is the label column. It fits the longest label in use
// with a space to spare; a longer one simply pushes its own value right
// rather than misaligning the block.
const detailLabelWidth = 10

// line writes an unindented line — the identity header of a view.
func (d *detailWriter) line(format string, a ...any) {
	d.raw(fmt.Sprintf(format, a...) + "\n")
}

// section starts a titled block, preceded by a blank line.
func (d *detailWriter) section(title string) {
	d.raw("\n" + title + "\n")
}

// field writes one "label   value" pair inside a section.
func (d *detailWriter) field(label, format string, a ...any) {
	d.raw(fmt.Sprintf("  %-*s %s\n", detailLabelWidth, label, fmt.Sprintf(format, a...)))
}

// at writes the n-th value of a repeated field: the label is printed once
// and the rest line up under it, so ten mounts do not read as ten unrelated
// facts that happen to be adjacent.
func (d *detailWriter) at(n int, label, format string, a ...any) {
	if n == 0 {
		d.field(label, format, a...)
		return
	}
	d.cont(format, a...)
}

// cont writes a continuation line under the previous value: a repair hint, a
// health summary, the second of several values.
func (d *detailWriter) cont(format string, a ...any) {
	d.raw(fmt.Sprintf("  %-*s %s\n", detailLabelWidth, "", fmt.Sprintf(format, a...)))
}

// raw writes pre-rendered text (a JSON block, a nested table) verbatim.
func (d *detailWriter) raw(s string) {
	if d.err != nil {
		return
	}
	if _, err := io.WriteString(d.w, s); err != nil {
		d.err = err
	}
}

// fail records an error produced while rendering rather than while writing —
// a nested tabwriter's Flush, say.
func (d *detailWriter) fail(err error) {
	if d.err == nil {
		d.err = err
	}
}

// humanAge renders an elapsed duration at the resolution a reader of a
// diagnostic actually uses: "is this current, or from another era". Minutes
// past an hour and hours past a day are noise for that question, and a
// negative age (a clock that moved, a file from the future) is reported as
// what it is rather than as a large positive number.
func humanAge(d time.Duration) string {
	switch {
	case d < 0:
		return "in the future — check this machine's clock"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// dashOr renders a value with a stated default for the empty case, for the
// fields where "unset" has a meaning worth naming ("none", "same as source")
// rather than the bare "-" that dash gives.
func dashOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
