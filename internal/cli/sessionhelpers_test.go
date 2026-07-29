package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/ctlapi"
)

// TestEscapePathSegmentNeutralizesSeparators is the client half of a
// two-package invariant. A session id goes into the request path, so an id
// carrying a slash would otherwise address a DIFFERENT endpoint than the one
// the command means to call.
//
// Neither half is sufficient alone, which is why this is worth pinning on both
// sides: internal/ctlapi matches on EscapedPath and refuses any segment still
// containing "/" (sessionPathID), so the escaping here is what keeps a
// legitimate id with odd bytes working, and that refusal is what keeps an
// unescaped client from reaching anything.
func TestEscapePathSegmentNeutralizesSeparators(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"sess-1", "sess-1"},
		{"a/b", "a%2Fb"},
		{"../admin", "..%2Fadmin"},
		{"a b", "a%20b"},
		{"a?b", "a%3Fb"},
		{"a#b", "a%23b"},
	} {
		if got := escapePathSegment(tt.in); got != tt.want {
			t.Errorf("escapePathSegment(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	// The property, stated independently of the exact encoding: whatever an
	// id contains, the result is one path segment.
	for _, hostile := range []string{"a/b", "../..", "x/../y", "//"} {
		if strings.Contains(escapePathSegment(hostile), "/") {
			t.Errorf("escapePathSegment(%q) left a separator: %q", hostile, escapePathSegment(hostile))
		}
	}
}

// TestDescribeScopeEditsNamesEveryEdit pins the summary an operator reads back
// after a scope change. It is the only feedback that the edit which landed is
// the edit they asked for, so a silently missing line is worse than a clumsy one.
func TestDescribeScopeEditsNamesEveryEdit(t *testing.T) {
	body := ctlapi.ScopeNarrowWire{Discovery: "lazy"}
	body.Reset = true
	body.DisableServers = []string{"gh", "jira"}
	body.Tools = map[string][]string{"fs": {"read", "list"}, "db": nil}

	got := describeScopeEdits(body)
	for _, want := range []string{
		"overlay reset to the static baseline",
		"server gh hidden",
		"server jira hidden",
		"server db tools blocked",
		"server fs narrowed to read,list",
		"discovery set to lazy",
	} {
		if !containsString(got, want) {
			t.Errorf("summary %v is missing %q", got, want)
		}
	}
	if len(got) != 6 {
		t.Errorf("summary = %v, want exactly the six edits above", got)
	}

	// An empty body describes nothing rather than inventing a line.
	if lines := describeScopeEdits(ctlapi.ScopeNarrowWire{}); len(lines) != 0 {
		t.Errorf("empty body described as %v", lines)
	}
}

// TestDescribeScopeEditsIsDeterministic: the tool edits come out of a map, so
// without sorting the summary would reorder between runs and two identical
// commands would print differently.
func TestDescribeScopeEditsIsDeterministic(t *testing.T) {
	body := ctlapi.ScopeNarrowWire{}
	body.Tools = map[string][]string{"c": {"x"}, "a": {"x"}, "b": {"x"}}

	first := strings.Join(describeScopeEdits(body), "|")
	for range 20 {
		if got := strings.Join(describeScopeEdits(body), "|"); got != first {
			t.Fatalf("summary is not deterministic: %q vs %q", got, first)
		}
	}
	if !strings.HasPrefix(first, "server a") {
		t.Errorf("summary %q is not sorted by server id", first)
	}
}

// TestClassifyScopeErrorMapsOntoTheFrozenExitCodes pins the exit-code table:
// a scope command's exit status is what a script branches on, so "unknown
// session" and "refused widening" must not collapse into the same code as an
// unrelated transport failure.
func TestClassifyScopeErrorMapsOntoTheFrozenExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		in       error
		wantCode string
		wantExit int
	}{
		{
			name:     "unknown session",
			in:       &ctlError{Status: http.StatusNotFound, Message: "no such session"},
			wantCode: CodeSessionNotFound,
			wantExit: ExitNotFound,
		},
		{
			name:     "refused widening",
			in:       &ctlError{Status: http.StatusForbidden, Message: "scope may only narrow"},
			wantCode: CodeTightenOnly,
			wantExit: ExitDenied,
		},
		{
			name:     "anything else keeps its own code",
			in:       &ctlError{Status: http.StatusInternalServerError, Code: "E_WHATEVER", Message: "boom"},
			wantCode: "E_WHATEVER",
			wantExit: ExitGeneral,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e *Error
			if !errors.As(classifyScopeError(tt.in), &e) {
				t.Fatalf("classifyScopeError returned %T, want *Error", classifyScopeError(tt.in))
			}
			if e.Code != tt.wantCode || e.ExitCode != tt.wantExit {
				t.Fatalf("code/exit = %s/%d, want %s/%d", e.Code, e.ExitCode, tt.wantCode, tt.wantExit)
			}
			if e.Message == "" {
				t.Error("the control plane's message was dropped")
			}
		})
	}

	// A non-ctlError is passed through untouched: classifying it would invent
	// a governance verdict for what may be a dial failure.
	plain := errNotCtl{}
	if got := classifyScopeError(plain); got != error(plain) {
		t.Errorf("a non-ctlError was rewritten to %v", got)
	}
}

type errNotCtl struct{}

func (errNotCtl) Error() string { return "not a control-plane error" }

// TestRemainingTextSaysExpiringRatherThanNegative: an approval whose deadline
// has passed must not render "-3s left".
func TestRemainingTextSaysExpiringRatherThanNegative(t *testing.T) {
	if got := remainingText(time.Now().Add(-time.Minute)); got != "expiring" {
		t.Errorf("past deadline = %q, want %q", got, "expiring")
	}
	if got := remainingText(time.Now()); got != "expiring" {
		t.Errorf("deadline now = %q, want %q", got, "expiring")
	}
	got := remainingText(time.Now().Add(90 * time.Second))
	if !strings.HasSuffix(got, "left") || strings.HasPrefix(got, "-") {
		t.Errorf("future deadline = %q, want a positive duration ending in 'left'", got)
	}
}

// TestCompactJSONFallsBackToTheRawBytes: the payload is shown to a human who
// is deciding whether to approve a call, so unparseable input must still be
// displayed rather than silently becoming empty.
func TestCompactJSONFallsBackToTheRawBytes(t *testing.T) {
	if got := compactJSON(json.RawMessage("{\n  \"a\": 1\n}")); got != `{"a":1}` {
		t.Errorf("compactJSON = %q, want compacted", got)
	}
	for _, bad := range []string{"", "{not json", "\x00"} {
		if got := compactJSON(json.RawMessage(bad)); got != bad {
			t.Errorf("compactJSON(%q) = %q, want the raw bytes back", bad, got)
		}
	}
}
