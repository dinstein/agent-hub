package downstream

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/eventlog"
)

// A missing secret and a refused connection are both "this server did not
// connect", and they send an operator to opposite places: one is a setup
// step nobody finished, the other is a server or a network. The closed
// vocabulary is what carries that difference into a timeline, so the
// classification is pinned here rather than left to whichever caller looks
// at the error next.
func TestConnectFailureSeparatesSetupFromNetwork(t *testing.T) {
	missing := &UnresolvedSecretError{ServerID: "linear", Key: "LINEAR_TOKEN"}
	if got := connectFailure(missing).Kind; got != eventlog.KindSecretsMissing {
		t.Errorf("an unresolved secret classified as %q, want secrets_missing", got)
	}
	// Wrapped, because the dial path never returns the bare error. errors.As
	// is the contract UnresolvedSecretError documents; matching the message
	// text is what it forbids, and a wrapper is what would break that.
	wrapped := fmt.Errorf("downstream %q: dial: %w", "linear", missing)
	if got := connectFailure(wrapped).Kind; got != eventlog.KindSecretsMissing {
		t.Errorf("a wrapped unresolved secret classified as %q, want secrets_missing", got)
	}
	if got := connectFailure(ErrNoResolver).Kind; got != eventlog.KindSecretsMissing {
		t.Errorf("a missing resolver classified as %q; it blocks on the same setup step", got)
	}
	plain := errors.New("dial tcp 127.0.0.1:9: connect: connection refused")
	if got := connectFailure(plain).Kind; got != eventlog.KindConnectFailed {
		t.Errorf("a refused connection classified as %q, want connect_failed", got)
	}
	// The key names the vault entry so an operator knows what to set. The
	// VALUE is never in the error and so can never reach the file.
	if d := connectFailure(missing).Detail; !strings.Contains(d, "LINEAR_TOKEN") {
		t.Errorf("Detail = %q, want the key", d)
	}
}

// A dead credential fails every request, so a per-failure event would let one
// broken server fill a file every gateway shares. Only the flip is recorded —
// the same rule the breaker and the health tracker follow.
//
// It reads back through a real stream rather than counting calls, because
// what matters is what LANDS in the file: a test that re-derives the emit
// decision from the flag would keep passing if the emit moved to the wrong
// side of it.
func TestOAuthRefreshFailureIsRecordedOncePerFlip(t *testing.T) {
	path := filepath.Join(t.TempDir(), eventlog.FileName)
	st, err := eventlog.Open(path, eventlog.Options{})
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer func() { _ = st.Close() }()

	rt := &authRoundTripper{events: serverEvents{stream: st, server: "linear"}}
	failures := func() int {
		st.Sync()
		res, err := eventlog.Read(path, eventlog.Query{
			Kinds: []eventlog.Kind{eventlog.KindOAuthRefreshFailed},
		})
		if err != nil {
			t.Fatalf("read event stream: %v", err)
		}
		return len(res.Records)
	}

	boom := errors.New("refresh token rejected")
	rt.noteRefresh(false, boom)
	rt.noteRefresh(false, boom)
	rt.noteRefresh(false, boom)
	if got := failures(); got != 1 {
		t.Fatalf("three consecutive failures wrote %d records, want 1", got)
	}
	// Recovery is tracked but not announced: the kind is
	// `oauth_refresh_failed`, and a record of that name reporting a fix would
	// be a row whose name and content disagree.
	rt.noteRefresh(true, nil)
	if got := failures(); got != 1 {
		t.Fatalf("a recovery wrote an oauth_refresh_failed record (now %d)", got)
	}
	// It must re-arm, or a credential that breaks, is fixed and breaks again
	// reports only the first outage.
	rt.noteRefresh(false, boom)
	if got := failures(); got != 2 {
		t.Fatalf("a second outage wrote %d records in total, want 2", got)
	}
}
