package cli

import (
	"strings"
	"testing"
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
