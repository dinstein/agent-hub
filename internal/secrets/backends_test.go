package secrets

import (
	"context"
	"errors"
	"testing"
)

// backendTestChain builds a chain whose keyring is a fake and whose enc file
// is active under an explicit key, so BOTH persistent backends are available
// in-process and a migration between them is a real round trip.
func backendTestChain(t *testing.T, env map[string]string) (*Chain, *fakeBackend) {
	t.Helper()
	fake := newFakeBackend(nil)
	return NewChain(ChainConfig{
		Dir:       t.TempDir(),
		Keyring:   fake,
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	}), fake
}

// TestBackendMigrateRoundTrip is the property the whole feature exists for:
// after migrating, the value resolves from the NEW backend with the old one
// removed from the picture entirely.
func TestBackendMigrateRoundTrip(t *testing.T) {
	ctx := context.Background()
	chain, _ := backendTestChain(t, map[string]string{EnvEncKey: "pass"})

	kr, err := chain.Backend(ctx, BackendKeyring)
	if err != nil {
		t.Fatalf("keyring backend: %v", err)
	}
	enc, err := chain.Backend(ctx, BackendEncFile)
	if err != nil {
		t.Fatalf("enc backend: %v", err)
	}

	ref := Ref{ServerID: "srv", Scope: DefaultScope, Key: "TOKEN"}
	if err := kr.Set(ctx, ref, "v1"); err != nil {
		t.Fatalf("seed keyring: %v", err)
	}

	results := Migrate(ctx, kr, enc, []Ref{ref})
	if len(results) != 1 || results[0].Err != nil || !results[0].Migrated {
		t.Fatalf("migrate = %+v, want one migrated ref with no error", results)
	}

	// Present in the destination...
	if v, ok, err := enc.Get(ctx, ref); err != nil || !ok || v != "v1" {
		t.Errorf("enc.Get = (%q, %v, %v), want the migrated value", v, ok, err)
	}
	// ...and gone from the source. This is the half that matters: a
	// migration that copies without removing leaves the credential in a
	// backend that may later disappear.
	if _, ok, err := kr.Get(ctx, ref); err != nil || ok {
		t.Errorf("keyring.Get after migrate = (ok=%v, err=%v), want absent", ok, err)
	}
}

// TestBackendMigrateKeepsBothOnVerifyFailure pins the fail-closed direction:
// when the destination cannot return what was written, the SOURCE entry must
// survive. A duplicated credential is recoverable; a dropped one is not.
func TestBackendMigrateKeepsBothOnVerifyFailure(t *testing.T) {
	ctx := context.Background()
	chain, _ := backendTestChain(t, map[string]string{EnvEncKey: "pass"})
	kr, err := chain.Backend(ctx, BackendKeyring)
	if err != nil {
		t.Fatalf("keyring backend: %v", err)
	}

	ref := Ref{ServerID: "srv", Scope: DefaultScope, Key: "TOKEN"}
	if err := kr.Set(ctx, ref, "v1"); err != nil {
		t.Fatalf("seed keyring: %v", err)
	}

	// A destination that accepts writes but returns nothing on read-back.
	results := Migrate(ctx, kr, blackHoleStore{}, []Ref{ref})
	if len(results) != 1 || results[0].Err == nil || results[0].Migrated {
		t.Fatalf("migrate = %+v, want a failed, unmigrated ref", results)
	}
	if v, ok, err := kr.Get(ctx, ref); err != nil || !ok || v != "v1" {
		t.Errorf("source after a failed verify = (%q, %v, %v), want the value still there",
			v, ok, err)
	}
}

// blackHoleStore accepts every write and reports every read as a miss — the
// shape that makes read-back verification earn its place.
type blackHoleStore struct{}

func (blackHoleStore) Get(context.Context, Ref) (string, bool, error) { return "", false, nil }
func (blackHoleStore) Set(context.Context, Ref, string) error         { return nil }
func (blackHoleStore) Delete(context.Context, Ref) error              { return nil }

// TestBackendIgnoresEnvironmentLevels is the reason Migrate demands
// backend-level stores. With AGENTHUB_SECRET_<KEY> set, Chain.Get would
// answer from the environment and satisfy a read-back verification that the
// destination backend never actually passed. A backend Store must not.
func TestBackendIgnoresEnvironmentLevels(t *testing.T) {
	ctx := context.Background()
	ref := Ref{ServerID: "srv", Scope: DefaultScope, Key: "TOKEN"}
	env := map[string]string{
		EnvEncKey:        "pass",
		EnvName(ref.Key): "value-from-the-environment",
	}
	chain, _ := backendTestChain(t, env)

	// The chain resolves it: level 1 hit.
	if v, ok, err := chain.Get(ctx, ref); err != nil || !ok || v != "value-from-the-environment" {
		t.Fatalf("chain.Get = (%q, %v, %v), want the environment value", v, ok, err)
	}
	// Every backend store reports a miss: nothing was ever stored.
	for _, kind := range BackendKinds() {
		st, err := chain.Backend(ctx, kind)
		if err != nil {
			t.Fatalf("%s backend: %v", kind, err)
		}
		if _, ok, err := st.Get(ctx, ref); err != nil || ok {
			t.Errorf("%s.Get = (ok=%v, err=%v), want a miss — an env var is not storage "+
				"and must never satisfy migrate's read-back verification", kind, ok, err)
		}
	}
}

// TestBackendEncFileUnavailableWithoutKey pins the eager availability check:
// with no key to open secrets.enc, the backend must refuse UP FRONT rather
// than fail partway through a migration.
func TestBackendEncFileUnavailableWithoutKey(t *testing.T) {
	ctx := context.Background()
	chain, _ := backendTestChain(t, nil) // no AGENTHUB_SECRET_KEY, no dev key file
	st, err := chain.Backend(ctx, BackendEncFile)
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("Backend(enc-file) err = %v, want ErrBackendUnavailable", err)
	}
	if st != nil {
		t.Error("an unavailable backend returned a usable Store")
	}
}

// TestBackendKeyringMaintainsKeyRegistry: OS keyrings cannot enumerate, so a
// value written without its registry entry is invisible to `secret ls` and to
// any LATER migration — it would be stranded in a backend nothing lists.
func TestBackendKeyringMaintainsKeyRegistry(t *testing.T) {
	ctx := context.Background()
	chain, _ := backendTestChain(t, nil)
	kr, err := chain.Backend(ctx, BackendKeyring)
	if err != nil {
		t.Fatalf("keyring backend: %v", err)
	}
	ref := Ref{ServerID: "srv", Scope: DefaultScope, Key: "TOKEN"}
	if err := kr.Set(ctx, ref, "v1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	refs, err := chain.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0].StorageKey() != ref.StorageKey() {
		t.Fatalf("List = %+v, want the keyring-written ref", refs)
	}
	// And the removal side: a deleted secret must leave no registry ghost.
	if err := kr.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if refs, err := chain.List(ctx); err != nil || len(refs) != 0 {
		t.Errorf("List after delete = (%+v, %v), want empty", refs, err)
	}
}

// TestBackendUnknownKindRejected: the backend spellings are frozen CLI
// surface, so an unrecognized one must be an error rather than a silent
// fallback to some default backend.
func TestBackendUnknownKindRejected(t *testing.T) {
	chain, _ := backendTestChain(t, nil)
	if _, err := chain.Backend(context.Background(), BackendKind("env")); err == nil {
		t.Error("Backend(\"env\") succeeded; environment levels are input, not storage")
	}
}
