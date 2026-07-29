package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestProbeUsesReadNotWrite: the availability probe must be a Get — a Set
// probe would trigger the destructive macOS confirmation prompt.
func TestProbeUsesReadNotWrite(t *testing.T) {
	f := newFakeBackend(nil)
	h := newHardKeyring(f, "svc", time.Second)
	if !h.available(context.Background()) {
		t.Fatal("healthy backend probed unavailable")
	}
	get, set, del := f.counts()
	if get != 1 || set != 0 || del != 0 {
		t.Fatalf("probe calls get/set/del = %d/%d/%d, want 1/0/0", get, set, del)
	}
}

// TestProbeResultCached: the verdict is computed once per hardKeyring and
// reused — including the negative verdict.
func TestProbeResultCached(t *testing.T) {
	f := newFakeBackend(nil)
	f.setGetDelay(200 * time.Millisecond) // beyond the hard timeout
	h := newHardKeyring(f, "svc", 20*time.Millisecond)
	ctx := context.Background()
	if h.available(ctx) {
		t.Fatal("timed-out probe reported available")
	}
	for i := 0; i < 5; i++ {
		if h.available(ctx) {
			t.Fatal("cached probe verdict flipped")
		}
	}
	if get, _, _ := f.counts(); get != 1 {
		t.Fatalf("probe ran %d times, want 1 (cached)", get)
	}
}

// TestProbeNotFoundMeansAvailable: ErrKeyringNotFound proves the backend
// answers; only timeouts/other errors mark it unavailable.
func TestProbeNotFoundMeansAvailable(t *testing.T) {
	h := newHardKeyring(newFakeBackend(nil), "svc", time.Second)
	if !h.available(context.Background()) {
		t.Fatal("not-found probe must count as available")
	}
	f2 := newFakeBackend(nil)
	f2.getErr = errors.New("dbus: no session bus")
	h2 := newHardKeyring(f2, "svc", time.Second)
	if h2.available(context.Background()) {
		t.Fatal("erroring backend must count as unavailable")
	}
}

// TestOpHardTimeout: an operation exceeding the timeout returns
// ErrKeyringTimeout instead of hanging the caller.
func TestOpHardTimeout(t *testing.T) {
	f := newFakeBackend(map[string]string{bkey("svc", "u"): "v"})
	h := newHardKeyring(f, "svc", time.Second)
	if !h.available(context.Background()) {
		t.Fatal("probe failed")
	}
	f.setGetDelay(150 * time.Millisecond)
	h.timeout = 20 * time.Millisecond
	start := time.Now()
	_, err := h.get(context.Background(), "u")
	if !errors.Is(err, ErrKeyringTimeout) {
		t.Fatalf("get: got %v, want ErrKeyringTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 120*time.Millisecond {
		t.Fatalf("get blocked %v despite 20ms hard timeout", elapsed)
	}
	if err := h.set(context.Background(), "u", "v"); err != nil {
		t.Fatalf("set (no delay path): %v", err)
	}
}

// TestOpContextCancel: caller cancellation unblocks before the hard
// timeout fires.
func TestOpContextCancel(t *testing.T) {
	f := newFakeBackend(nil)
	f.setGetDelay(500 * time.Millisecond)
	h := newHardKeyring(f, "svc", 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := h.get(ctx, "u")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("get: got %v, want context deadline", err)
	}
}

func TestKeyRegistryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), keyRegistryFileName)
	keys, err := loadKeyRegistry(path)
	if err != nil || keys != nil {
		t.Fatalf("missing registry: got (%v, %v), want (nil, nil)", keys, err)
	}
	if err := saveKeyRegistry(path, []string{"b", "a", "b"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	keys, err = loadKeyRegistry(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(keys) != 2 || keys[0] != "a" || keys[1] != "b" {
		t.Fatalf("keys = %v, want sorted deduplicated [a b]", keys)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("registry permissions = %o, want 600", perm)
	}
}

// TestRealKeyringSmoke touches the actual OS keychain and therefore only
// runs under AGENTHUB_TEST_REAL_KEYRING=1 (manual invocation).
func TestRealKeyringSmoke(t *testing.T) {
	if os.Getenv("AGENTHUB_TEST_REAL_KEYRING") != "1" {
		t.Skip("set AGENTHUB_TEST_REAL_KEYRING=1 to run the real-keychain smoke test")
	}
	service := fmt.Sprintf("agenthub-test-%d", os.Getpid())
	user := "agenthub/v1/smoke/_global/token"
	b := systemBackend{}
	if err := b.Set(service, user, "smoke-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	defer func() { _ = b.Delete(service, user) }()
	v, err := b.Get(service, user)
	if err != nil || v != "smoke-value" {
		t.Fatalf("Get = (%q, %v)", v, err)
	}
	if err := b.Delete(service, user); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := b.Get(service, user); !errors.Is(err, ErrKeyringNotFound) {
		t.Fatalf("Get after delete: got %v, want ErrKeyringNotFound", err)
	}
}
