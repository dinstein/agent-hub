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

// rotatingVault is a resolver whose stored value can be replaced, the way
// another process replaces it: `agenthub auth login`, or the daemon's
// proactive refresher.
type rotatingVault struct {
	mu  sync.Mutex
	val string
}

func (r *rotatingVault) set(v string) {
	r.mu.Lock()
	r.val = v
	r.mu.Unlock()
}

func (r *rotatingVault) resolve(_ context.Context, ref secrets.Ref) (string, bool, error) {
	if ref.Key != secrets.KeyHTTPAuth {
		return "", false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.val, r.val != "", nil
}

// TestHTTPCredentialEpochDropsTheCachedBearer is the proactive half of the
// credential contract: a token ROTATED in the vault reaches a live
// connection because the epoch moved, WITHOUT the downstream having to
// reject the old one first.
//
// The control in the middle is the point — before the epoch moves, the
// rotated value must NOT be picked up, or the test would pass just as well
// against a round tripper that had no cache at all.
func TestHTTPCredentialEpochDropsTheCachedBearer(t *testing.T) {
	t.Parallel()
	f := newHTTPFake(t) // accepts any credential: no 401 may be involved
	vault := &rotatingVault{val: "v1"}

	var epoch atomic.Uint64
	var refreshes atomic.Int64
	auth := downstream.WithEpoch(
		downstream.NewVaultTokenSource("remote", vault.resolve, func(context.Context) (string, error) {
			refreshes.Add(1)
			return "", nil
		}),
		func() uint64 { return epoch.Load() },
	)

	srv, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:         "remote",
		Kind:       transport.StreamableHTTP,
		URL:        f.url(),
		Provenance: downstream.ProvenanceLocal,
	}, downstream.Deps{Secrets: vault.resolve, Auth: auth, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer srv.Close()

	if got := lastAuth(t, f); got != "Bearer v1" {
		t.Fatalf("first request carried %q, want Bearer v1", got)
	}

	// Rotated in the vault, but NOT announced: the cache must still hold.
	vault.set("v2")
	if err := srv.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := lastAuth(t, f); got != "Bearer v1" {
		t.Fatalf("an unannounced rotation was picked up (%q): the cache is not doing its job", got)
	}

	// Announced: the very next request carries the new credential.
	epoch.Add(1)
	if err := srv.Ping(context.Background()); err != nil {
		t.Fatalf("Ping after the announcement: %v", err)
	}
	if got := lastAuth(t, f); got != "Bearer v2" {
		t.Fatalf("after the announcement the request carried %q, want Bearer v2", got)
	}
	if got := refreshes.Load(); got != 0 {
		t.Errorf("refreshes = %d, want 0 — the rotation must not need a rejection to be noticed", got)
	}
}

// TestHTTPWithoutEpochKeepsTheOldContract: a TokenSource with no epoch
// behaves exactly as before — the cache is dropped by a 401 and by nothing
// else. The daemon and the CLI build their sources that way.
func TestHTTPWithoutEpochKeepsTheOldContract(t *testing.T) {
	t.Parallel()
	f := newHTTPFake(t)
	vault := &rotatingVault{val: "v1"}
	auth := downstream.NewVaultTokenSource("remote", vault.resolve, nil)

	srv, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:         "remote",
		Kind:       transport.StreamableHTTP,
		URL:        f.url(),
		Provenance: downstream.ProvenanceLocal,
	}, downstream.Deps{Secrets: vault.resolve, Auth: auth, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer srv.Close()

	vault.set("v2")
	if err := srv.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := lastAuth(t, f); got != "Bearer v1" {
		t.Fatalf("request carried %q, want the cached Bearer v1", got)
	}
}

// WithEpoch is a decorator, so it must be safe to hand it nothing.
func TestWithEpochIsANoOpWhenUnwired(t *testing.T) {
	t.Parallel()
	base := downstream.NewVaultTokenSource("remote", nil, nil)
	if got := downstream.WithEpoch(base, nil); got != base {
		t.Error("WithEpoch(ts, nil) must return the source untouched")
	}
	if got := downstream.WithEpoch(nil, func() uint64 { return 1 }); got != nil {
		t.Error("WithEpoch(nil, fn) must stay nil")
	}
}

func lastAuth(t *testing.T, f *httpFake) string {
	t.Helper()
	seen := f.authSeen()
	if len(seen) == 0 {
		t.Fatal("the fake downstream saw no request")
	}
	return seen[len(seen)-1]
}
