package downstream_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// deadlineSource is a TokenSource that renews inside Token, the way the
// gateway's proactive source does, and reports when the value it last handed
// out stops being worth sending.
//
// It deliberately never returns an error and never asks for a Refresh: the
// whole point of the deadline is to work on a downstream that issues no
// 401 at all.
type deadlineSource struct {
	mu       sync.Mutex
	tok      string
	notAfter time.Time
	// renew is called from Token when the deadline has passed; it returns
	// the successor credential and how long that one is good for.
	renew func() (string, time.Duration)
	loads atomic.Int64
}

func (d *deadlineSource) Token(context.Context) (string, bool, error) {
	d.loads.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.notAfter.IsZero() && !time.Now().Before(d.notAfter) {
		tok, ttl := d.renew()
		d.tok, d.notAfter = tok, time.Now().Add(ttl)
	}
	return d.tok, d.tok != "", nil
}

func (d *deadlineSource) Refresh(context.Context) (string, error) {
	panic("Refresh must not be reached: the deadline exists so no rejection is needed")
}

func (d *deadlineSource) NotAfter() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.notAfter
}

// TestHTTPCredentialDeadlineRenewsWithoutARejection is the rule the other
// three cache rules cannot cover: a credential that ages out inside a live
// connection, on a server that answers an expired token with 200 rather than
// 401 (they exist — docs/modules/oauth.md). Nothing rejects, nothing else
// writes the vault, and the token must still be replaced.
//
// The control in the middle is what makes it a test of the deadline rather
// than of a round tripper with no cache at all: before the deadline passes,
// the same credential must be reused.
func TestHTTPCredentialDeadlineRenewsWithoutARejection(t *testing.T) {
	t.Parallel()
	f := newHTTPFake(t) // accepts any credential: no 401 may be involved

	var renewals atomic.Int64
	auth := &deadlineSource{
		tok:      "v1",
		notAfter: time.Now().Add(time.Hour),
		renew: func() (string, time.Duration) {
			renewals.Add(1)
			return "v2", time.Hour
		},
	}

	srv, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:         "remote",
		Kind:       transport.StreamableHTTP,
		URL:        f.url(),
		Provenance: downstream.ProvenanceLocal,
	}, downstream.Deps{Auth: auth, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer srv.Close()

	if got := lastAuth(t, f); got != "Bearer v1" {
		t.Fatalf("first request carried %q, want Bearer v1", got)
	}

	// Still inside the deadline: the cache must hold, and the source must not
	// be consulted again.
	before := auth.loads.Load()
	if err := srv.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := lastAuth(t, f); got != "Bearer v1" {
		t.Fatalf("request carried %q, want the cached Bearer v1", got)
	}
	if got := auth.loads.Load(); got != before {
		t.Errorf("the source was consulted %d extra time(s) inside the deadline; the cache is not doing its job", got-before)
	}

	// Past the deadline: the next request re-reads, the source renews, and
	// the successor goes out — with no rejection anywhere in the story.
	auth.mu.Lock()
	auth.notAfter = time.Now().Add(-time.Second)
	auth.mu.Unlock()

	if err := srv.Ping(context.Background()); err != nil {
		t.Fatalf("Ping past the deadline: %v", err)
	}
	if got := lastAuth(t, f); got != "Bearer v2" {
		t.Fatalf("past the deadline the request carried %q, want Bearer v2", got)
	}
	if got := renewals.Load(); got != 1 {
		t.Fatalf("renewals = %d, want exactly 1", got)
	}

	// And the successor is cached under ITS deadline, not re-read every time:
	// caching under the pre-load deadline would make the very next request
	// throw the new credential away.
	before = auth.loads.Load()
	if err := srv.Ping(context.Background()); err != nil {
		t.Fatalf("Ping after the renewal: %v", err)
	}
	if got := auth.loads.Load(); got != before {
		t.Errorf("the renewed credential was not cached under its own deadline (%d extra load(s))", got-before)
	}
	if got := renewals.Load(); got != 1 {
		t.Errorf("renewals = %d after one expiry, want 1", got)
	}
}

// TestWithEpochForwardsTheDeadline pins the composition. WithEpoch embeds the
// TokenSource *interface*, so the wrapped value's other methods are not part
// of the decorator's method set — the gateway wraps a source that reports a
// deadline, and without the forward the round tripper's assertion would be
// made against the wrapper and silently find nothing.
func TestWithEpochForwardsTheDeadline(t *testing.T) {
	t.Parallel()
	want := time.Now().Add(time.Hour)
	wrapped := downstream.WithEpoch(&deadlineSource{tok: "v1", notAfter: want}, func() uint64 { return 0 })

	d, ok := wrapped.(interface{ NotAfter() time.Time })
	if !ok {
		t.Fatal("WithEpoch dropped the deadline face")
	}
	if got := d.NotAfter(); !got.Equal(want) {
		t.Errorf("NotAfter() = %v, want %v", got, want)
	}
}

// A source with no deadline of its own must still be safe to wrap, and must
// report the zero instant — which is what "no deadline" means, and what keeps
// the daemon's and the CLI's sources on exactly the old contract.
func TestWithEpochReportsNoDeadlineForASourceWithout(t *testing.T) {
	t.Parallel()
	plain := downstream.NewVaultTokenSource("remote", func(context.Context, secrets.Ref) (string, bool, error) {
		return "", false, nil
	}, nil)
	wrapped := downstream.WithEpoch(plain, func() uint64 { return 0 })

	d, ok := wrapped.(interface{ NotAfter() time.Time })
	if !ok {
		t.Fatal("WithEpoch must always carry the deadline face, reporting zero when there is none")
	}
	if got := d.NotAfter(); !got.IsZero() {
		t.Errorf("NotAfter() = %v, want the zero instant", got)
	}
}
