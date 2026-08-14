package downstream

import (
	"strings"

	"github.com/dinstein/agent-hub/internal/mcp/transport"
)

// StderrRingLines is how many trailing stderr LINES are embedded in an
// initialization failure ("a 20-line ring buffer of child stderr is embedded
// in the initialization failure — otherwise a startup crash leaves nothing
// but deadline exceeded").
const StderrRingLines = 20

// stderrLineCap bounds one embedded line. A downstream that dumps a 1 MB
// stack trace on one line must not turn our error into that stack trace.
const stderrLineCap = 400

// stderrTail projects the transport's byte tail window (the 4 KiB retained
// by internal/mcp/transport) onto the last n non-blank LINES.
//
// Why a projection and not a second buffer: the transport is the only place
// that owns the child's stderr pipe, it is standard-library only, and the
// byte window is already the authoritative capture. Reading lines out of it
// gives the line-level ring the error text needs without a second copy of
// the child's output — the only loss is that a line older than 4 KiB of
// stderr is gone, which is exactly what the byte window promises anyway.
//
// KNOWN GAP: a full window has been cut at a byte boundary, so its first line
// is a fragment, and nothing here drops it — the fragment is reported as
// though it were a whole line. This function cannot see the condition: it is
// handed the window's contents and not its capacity, so "the window was full"
// is not a question it can ask. Fixing it means plumbing the cap through, a
// behaviour change to the error text and therefore not a tidy-night edit;
// docs/subsystems/downstream.md carries the item.
func stderrTail(tr transport.Transport, n int) []string {
	if tr == nil {
		return nil
	}
	return tailLines(tr.Stderr(), n)
}

// tailLines is the pure half of stderrTail: it keeps the LAST n non-blank
// lines, trims trailing whitespace, and caps each at stderrLineCap. It does
// not drop a leading fragment — see the gap on stderrTail.
func tailLines(raw string, n int) []string {
	if raw == "" || n <= 0 {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	out := make([]string, 0, n)
	for _, l := range lines {
		l = strings.TrimRight(l, "\r\t ")
		if strings.TrimSpace(l) == "" {
			continue
		}
		if len(l) > stderrLineCap {
			l = l[:stderrLineCap] + "…"
		}
		out = append(out, l)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// formatStderrTail renders the ring for embedding in an error message.
func formatStderrTail(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, " | ")
}
