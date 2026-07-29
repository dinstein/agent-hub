package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeBackend is an in-memory keyring. Tests never touch the real OS
// keychain (package doc); the real backend is exercised only by the
// manual smoke test in keyring_test.go.
type fakeBackend struct {
	mu       sync.Mutex
	data     map[string]string
	getDelay time.Duration
	getErr   error // returned by every Get (probe breaker)

	getCalls int
	setCalls int
	delCalls int
}

func newFakeBackend(seed map[string]string) *fakeBackend {
	if seed == nil {
		seed = map[string]string{}
	}
	return &fakeBackend{data: seed}
}

func bkey(service, user string) string { return service + "\x00" + user }

func (f *fakeBackend) Get(service, user string) (string, error) {
	f.mu.Lock()
	delay, err := f.getDelay, f.getErr
	f.getCalls++
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[bkey(service, user)]
	if !ok {
		return "", ErrKeyringNotFound
	}
	return v, nil
}

func (f *fakeBackend) Set(service, user, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	f.data[bkey(service, user)] = secret
	return nil
}

func (f *fakeBackend) Delete(service, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delCalls++
	if _, ok := f.data[bkey(service, user)]; !ok {
		return ErrKeyringNotFound
	}
	delete(f.data, bkey(service, user))
	return nil
}

func (f *fakeBackend) setGetDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getDelay = d
}

func (f *fakeBackend) counts() (get, set, del int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls, f.setCalls, f.delCalls
}

func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func newTestChain(dir string, env map[string]string, b Backend) *Chain {
	return NewChain(ChainConfig{
		Dir:       dir,
		LookupEnv: envOf(env),
		Keyring:   b,
	})
}

// seedEnc writes ref=val into <dir>/secrets.enc under passphrase "pass".
func seedEnc(t *testing.T, dir string, ref Ref, val string) {
	t.Helper()
	c := newTestChain(dir, map[string]string{EnvEncKey: "pass"}, newFakeBackend(nil))
	if err := c.Set(context.Background(), ref, val); err != nil {
		t.Fatalf("seed enc: %v", err)
	}
}

// TestChainPriorityMatrix walks the four-level chain top-down: each row
// removes the previous winning level and asserts the next one wins.
func TestChainPriorityMatrix(t *testing.T) {
	ref := Ref{ServerID: "srv", Key: "token"}
	sk := ref.StorageKey()
	krSeed := map[string]string{bkey(defaultService, sk): "kr4"}

	cases := []struct {
		name    string
		env     map[string]string
		seedEnc bool
		want    string
		wantOK  bool
	}{
		{
			name: "level1 prefixed env wins over everything",
			env: map[string]string{
				"AGENTHUB_SECRET_TOKEN": "env1",
				"TOKEN":                 "env2",
				EnvAllowBare:            "1",
				EnvEncKey:               "pass",
			},
			seedEnc: true,
			want:    "env1", wantOK: true,
		},
		{
			name: "level2 bare env with opt-in",
			env: map[string]string{
				"TOKEN":      "env2",
				EnvAllowBare: "1",
				EnvEncKey:    "pass",
			},
			seedEnc: true,
			want:    "env2", wantOK: true,
		},
		{
			name: "level2 skipped without opt-in",
			env: map[string]string{
				"TOKEN":   "env2",
				EnvEncKey: "pass",
			},
			seedEnc: true,
			want:    "enc3", wantOK: true,
		},
		{
			name: "whitespace level1 counts as unset",
			env: map[string]string{
				"AGENTHUB_SECRET_TOKEN": " \t ",
				EnvEncKey:               "pass",
			},
			seedEnc: true,
			want:    "enc3", wantOK: true,
		},
		{
			name:    "level3 enc inactive without AGENTHUB_SECRET_KEY",
			env:     map[string]string{},
			seedEnc: true, // file exists but no key material and no dev key file
			want:    "kr4", wantOK: true,
		},
		{
			name: "level4 keyring when all above miss",
			env:  map[string]string{EnvEncKey: "pass"},
			want: "kr4", wantOK: true,
		},
		{
			name: "all levels miss",
			env:  map[string]string{},
			want: "", wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.seedEnc {
				seedEnc(t, dir, ref, "enc3")
			}
			seed := map[string]string{}
			for k, v := range krSeed {
				seed[k] = v
			}
			if tc.name == "all levels miss" {
				seed = nil
			}
			c := newTestChain(dir, tc.env, newFakeBackend(seed))
			got, ok, err := c.Get(context.Background(), ref)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("Get = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestEncKeyEnvNameReserved: a secret whose key maps to the reserved name
// AGENTHUB_SECRET_KEY must never resolve to the enc-file key material.
func TestEncKeyEnvNameReserved(t *testing.T) {
	dir := t.TempDir()
	ref := Ref{ServerID: "srv", Key: "key"} // EnvName("key") == EnvEncKey
	seedEnc(t, dir, ref, "from-enc")
	c := newTestChain(dir, map[string]string{EnvEncKey: "pass"}, newFakeBackend(nil))
	got, ok, err := c.Get(context.Background(), ref)
	if err != nil || !ok {
		t.Fatalf("Get: %v ok=%v", err, ok)
	}
	if got == "pass" {
		t.Fatal("chain leaked AGENTHUB_SECRET_KEY key material as a secret value")
	}
	if got != "from-enc" {
		t.Fatalf("Get = %q, want from-enc", got)
	}
}

// TestWrongEncKeyFailsClosed: a wrong AGENTHUB_SECRET_KEY is an error,
// never a silent fall-through to the keyring.
func TestWrongEncKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	ref := Ref{ServerID: "srv", Key: "token"}
	seedEnc(t, dir, ref, "enc-val")
	kr := newFakeBackend(map[string]string{bkey(defaultService, ref.StorageKey()): "kr-val"})
	c := newTestChain(dir, map[string]string{EnvEncKey: "WRONG"}, kr)
	_, _, err := c.Get(context.Background(), ref)
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Get with wrong key: got %v, want ErrDecrypt", err)
	}
}

// TestSetRoutesToKeyring: healthy keyring, no dev flags → writes land in
// the keyring and the self-managed registry mirrors the key.
func TestSetRoutesToKeyring(t *testing.T) {
	dir := t.TempDir()
	ref := Ref{ServerID: "srv", Scope: "work", Key: "token"}
	kr := newFakeBackend(nil)
	c := newTestChain(dir, map[string]string{}, kr)
	ctx := context.Background()

	if err := c.Set(ctx, ref, "v1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, set, _ := kr.counts(); set != 1 {
		t.Fatalf("keyring Set calls = %d, want 1", set)
	}
	got, ok, err := c.Get(ctx, ref)
	if err != nil || !ok || got != "v1" {
		t.Fatalf("Get = (%q, %v, %v), want v1", got, ok, err)
	}
	refs, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantRef := ref // List returns normalized scope; here explicit already
	if len(refs) != 1 || refs[0] != wantRef {
		t.Fatalf("List = %+v, want [%+v]", refs, wantRef)
	}
	if err := c.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	refs, err = c.List(ctx)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("List after delete = %+v, want empty", refs)
	}
	if _, ok, _ := c.Get(ctx, ref); ok {
		t.Fatal("Get after delete still returns a value")
	}
	// Deleting an absent secret is a no-op.
	if err := c.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete absent: %v", err)
	}
}

// TestDevFallbackOnProbeFailure (ruling A.6 #5): a broken keyring flips
// writes to secrets.enc under an auto-generated dev key, and reads
// resolve from there.
func TestDevFallbackOnProbeFailure(t *testing.T) {
	dir := t.TempDir()
	ref := Ref{ServerID: "srv", Key: "token"}
	kr := newFakeBackend(nil)
	kr.getErr = errors.New("keychain exploded") // probe fails
	c := newTestChain(dir, map[string]string{}, kr)
	ctx := context.Background()

	if err := c.Set(ctx, ref, "dev-val"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, set, _ := kr.counts(); set != 0 {
		t.Fatalf("keyring Set called %d times despite failed probe", set)
	}
	for _, name := range []string{encFileName, devKeyFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
		}
	}
	got, ok, err := c.Get(ctx, ref)
	if err != nil || !ok || got != "dev-val" {
		t.Fatalf("Get = (%q, %v, %v), want dev-val", got, ok, err)
	}
	// A fresh chain on the same dir (even with a healthy keyring now)
	// still resolves dev-enc data: both files exist.
	c2 := newTestChain(dir, map[string]string{}, newFakeBackend(nil))
	got, ok, err = c2.Get(ctx, ref)
	if err != nil || !ok || got != "dev-val" {
		t.Fatalf("fresh chain Get = (%q, %v, %v), want dev-val", got, ok, err)
	}
}

// TestDevModeEnvForcesEnc: AGENTHUB_DEV_SECRETS=1 routes writes to the
// enc file even when the keyring is healthy.
func TestDevModeEnvForcesEnc(t *testing.T) {
	dir := t.TempDir()
	ref := Ref{ServerID: "srv", Key: "token"}
	kr := newFakeBackend(nil)
	c := newTestChain(dir, map[string]string{EnvDevSecrets: "1"}, kr)
	ctx := context.Background()

	if err := c.Set(ctx, ref, "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, set, _ := kr.counts(); set != 0 {
		t.Fatalf("keyring Set called %d times in dev mode", set)
	}
	if _, err := os.Stat(filepath.Join(dir, encFileName)); err != nil {
		t.Fatalf("secrets.enc missing: %v", err)
	}
	got, ok, err := c.Get(ctx, ref)
	if err != nil || !ok || got != "v" {
		t.Fatalf("Get = (%q, %v, %v)", got, ok, err)
	}
}

// TestBareEnvNeverResolvesAgenthubVars: even with the bare opt-in, keys
// normalizing to AGENTHUB_* must not read our control variables.
func TestBareEnvNeverResolvesAgenthubVars(t *testing.T) {
	dir := t.TempDir()
	ref := Ref{ServerID: "srv", Key: "agenthub_dev_secrets"}
	env := map[string]string{
		EnvAllowBare:  "1",
		EnvDevSecrets: "1", // would be the bare match
	}
	c := newTestChain(dir, env, newFakeBackend(nil))
	got, ok, err := c.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatalf("bare lookup resolved AGENTHUB_* control variable: %q", got)
	}
}

// TestChainList aggregates enc entries and keyring registry entries.
func TestChainList(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	encRef := Ref{ServerID: "a", Key: "k1"}
	krRef := Ref{ServerID: "b", Scope: "work", Key: "k2"}

	kr := newFakeBackend(nil)
	// Write one secret via keyring...
	c := newTestChain(dir, map[string]string{}, kr)
	if err := c.Set(ctx, krRef, "v2"); err != nil {
		t.Fatalf("Set keyring: %v", err)
	}
	// ...and one via the enc file.
	cEnc := newTestChain(dir, map[string]string{EnvEncKey: "pass"}, kr)
	if err := cEnc.Set(ctx, encRef, "v1"); err != nil {
		t.Fatalf("Set enc: %v", err)
	}
	refs, err := cEnc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantEnc := Ref{ServerID: "a", Scope: DefaultScope, Key: "k1"}
	if len(refs) != 2 || refs[0] != wantEnc || refs[1] != krRef {
		t.Fatalf("List = %+v, want [%+v %+v]", refs, wantEnc, krRef)
	}
}

// TestResolverIsGet: the narrow face resolves identically to Get.
func TestResolverIsGet(t *testing.T) {
	dir := t.TempDir()
	ref := Ref{ServerID: "srv", Key: "token"}
	c := newTestChain(dir, map[string]string{"AGENTHUB_SECRET_TOKEN": "v"}, newFakeBackend(nil))
	r := c.Resolver()
	got, ok, err := r(context.Background(), ref)
	if err != nil || !ok || got != "v" {
		t.Fatalf("Resolver = (%q, %v, %v), want v", got, ok, err)
	}
}

// TestChainInvalidRef: all operations reject invalid refs up front.
func TestChainInvalidRef(t *testing.T) {
	c := newTestChain(t.TempDir(), map[string]string{}, newFakeBackend(nil))
	ctx := context.Background()
	bad := Ref{ServerID: "", Key: "k"}
	if _, _, err := c.Get(ctx, bad); err == nil {
		t.Error("Get: expected error")
	}
	if err := c.Set(ctx, bad, "v"); err == nil {
		t.Error("Set: expected error")
	}
	if err := c.Delete(ctx, bad); err == nil {
		t.Error("Delete: expected error")
	}
}
