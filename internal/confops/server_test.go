package confops

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
)

func TestAddServerRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	res, err := AddServer(ctx, st, ServerSpec{ID: "github", Entry: stdio("gh-mcp")}, Precondition{})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(res.Servers) != 1 || res.Servers[0].ID != "github" {
		t.Fatalf("result = %+v", res.Servers)
	}
	entry, ok := st.Snapshot().Servers.V.Servers["github"]
	if !ok || entry.V.Command != "gh-mcp" || !entry.V.Enabled {
		t.Fatalf("entry = %+v (ok=%v)", entry.V, ok)
	}
}

// TestAddServersIsAtomic: pasting an mcpServers fragment must either land
// whole or not at all, so a conflict halfway through cannot leave a partial
// import behind.
func TestAddServersIsAtomic(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "dup")

	_, err := AddServers(ctx, st, []ServerSpec{
		{ID: "fresh", Entry: stdio("fresh-bin")},
		{ID: "dup", Entry: stdio("other-bin")},
	}, Precondition{})
	wantErrorKind(t, err, KindConflict, CodeServerExists)
	if _, ok := st.Snapshot().Servers.V.Servers["fresh"]; ok {
		t.Error("the first entry of a rejected batch was persisted")
	}
	if got := st.Snapshot().Servers.V.Servers["dup"].V.Command; got != "dup-bin" {
		t.Errorf("the existing entry was overwritten: command = %q", got)
	}
}

func TestAddServerPreconditionConflict(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	// Two writes so the "one generation behind" reading is not generation 0,
	// which means "do not check".
	seedServers(t, st, "a", "a2")
	gen := st.Snapshot().Generation

	_, err := AddServer(ctx, st, ServerSpec{ID: "b", Entry: stdio("b-bin")},
		Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
	if _, ok := st.Snapshot().Servers.V.Servers["b"]; ok {
		t.Error("a stale add wrote the entry anyway")
	}
}

// TestValidateServerSpecRefusesEveryBadShape. Each case is a combination
// that would otherwise be silently ignored at connect time — the failure
// nobody notices.
func TestValidateServerSpecRefusesEveryBadShape(t *testing.T) {
	cases := []struct {
		name string
		spec ServerSpec
		kind Kind
		code string
	}{
		{"no id", ServerSpec{Entry: stdio("x")}, KindUsage, CodeUsage},
		{"stdio without command", ServerSpec{ID: "s", Entry: registry.ServerEntry{}}, KindUsage, CodeUsage},
		{
			"stdio with a url",
			ServerSpec{ID: "s", Entry: registry.ServerEntry{Command: "c", URL: "https://x/mcp"}},
			KindUsage, CodeUsage,
		},
		{
			"http without a url",
			ServerSpec{ID: "s", Entry: registry.ServerEntry{Transport: registry.TransportHTTP}},
			KindUsage, CodeUsage,
		},
		{
			"http with a command",
			ServerSpec{ID: "s", Entry: registry.ServerEntry{
				Transport: registry.TransportHTTP, URL: "https://x/mcp", Command: "c",
			}},
			KindUsage, CodeUsage,
		},
		{
			"unknown transport",
			ServerSpec{ID: "s", Entry: registry.ServerEntry{Transport: "grpc"}},
			KindUsage, CodeUnsupportedTransport,
		},
		{
			"private endpoint without the local marker",
			ServerSpec{ID: "s", Entry: registry.ServerEntry{
				Transport: registry.TransportHTTP, URL: "http://127.0.0.1:9/mcp",
			}},
			KindUsage, CodeUsage,
		},
		{
			"local marker on a non-loopback host",
			ServerSpec{ID: "s", Entry: registry.ServerEntry{
				Transport: registry.TransportHTTP, URL: "https://example.com/mcp",
				Provenance: registry.ProvenanceLocal,
			}},
			KindUsage, CodeUsage,
		},
		{
			"unknown runtime",
			ServerSpec{ID: "s", Entry: registry.ServerEntry{Command: "c", Runtime: "dcoker"}},
			KindUsage, CodeUsage,
		},
		{
			"docker runtime without an image",
			ServerSpec{ID: "s", Entry: registry.ServerEntry{
				Command: "c", Runtime: registry.RuntimeDocker, Docker: &registry.DockerRuntime{},
			}},
			KindUsage, CodeUsage,
		},
		{
			// The spawn guard, not the shape check, recognises this one.
			"container mount of the host root",
			ServerSpec{ID: "s", Entry: registry.ServerEntry{
				Command: "node", Runtime: registry.RuntimeDocker,
				Docker: &registry.DockerRuntime{
					Image:  "alpine",
					Mounts: []registry.DockerMount{{Source: "/", Target: "/host"}},
				},
			}},
			KindDenied, CodeDenied,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantErrorKind(t, ValidateServerSpec(tc.spec), tc.kind, tc.code)
		})
	}
}

// TestValidateServerSpecAcceptsALiteralLoopbackMarkedLocal: --local is the
// narrow, explicit escape hatch, and it must actually work.
func TestValidateServerSpecAcceptsALiteralLoopbackMarkedLocal(t *testing.T) {
	err := ValidateServerSpec(ServerSpec{ID: "s", Entry: registry.ServerEntry{
		Transport: registry.TransportHTTP, URL: "http://127.0.0.1:9/mcp",
		Provenance: registry.ProvenanceLocal,
	}})
	if err != nil {
		t.Fatalf("a loopback endpoint marked local was refused: %v", err)
	}
}

// TestAddServerRejectsBeforeWriting: validation runs before the transaction,
// so a refused entry leaves no registry state behind.
func TestAddServerRejectsBeforeWriting(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	before := st.Snapshot().Generation

	_, err := AddServer(ctx, st, ServerSpec{ID: "bad", Entry: registry.ServerEntry{Transport: "grpc"}},
		Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUnsupportedTransport)
	if st.Snapshot().Generation != before {
		t.Error("a refused add bumped the generation")
	}
}

func TestUpdateServer(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a")

	res, err := UpdateServer(ctx, st, ServerSpec{ID: "a", Entry: stdio("new-bin")}, Precondition{})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !res.Changed {
		t.Error("a real edit reported no change")
	}
	if got := st.Snapshot().Servers.V.Servers["a"].V.Command; got != "new-bin" {
		t.Errorf("command = %q, want new-bin", got)
	}

	_, err = UpdateServer(ctx, st, ServerSpec{ID: "ghost", Entry: stdio("x")}, Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeServerNotFound)

	gen := st.Snapshot().Generation
	_, err = UpdateServer(ctx, st, ServerSpec{ID: "a", Entry: stdio("stale-bin")},
		Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
	if got := st.Snapshot().Servers.V.Servers["a"].V.Command; got != "new-bin" {
		t.Errorf("a stale update was applied anyway: command = %q", got)
	}
}

func TestRemoveServer(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a")

	if _, err := RemoveServer(ctx, st, "a", Precondition{}, RemoveOptions{}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := st.Snapshot().Servers.V.Servers["a"]; ok {
		t.Error("the entry survived removal")
	}

	_, err := RemoveServer(ctx, st, "a", Precondition{}, RemoveOptions{})
	wantErrorKind(t, err, KindNotFound, CodeServerNotFound)

	seedServers(t, st, "b")
	gen := st.Snapshot().Generation
	_, err = RemoveServer(ctx, st, "b", Precondition{Generation: gen - 1}, RemoveOptions{})
	wantStale(t, err, gen)
	if _, ok := st.Snapshot().Servers.V.Servers["b"]; !ok {
		t.Error("a stale removal deleted the entry anyway")
	}
}

// fakePurger is an in-memory CredentialPurger with fault injection.
type fakePurger struct {
	refs    []secrets.Ref
	deleted []secrets.Ref
	listErr error
	// failKey makes Delete fail for that entry key.
	failKey string
}

func (p *fakePurger) List(context.Context) ([]secrets.Ref, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	return append([]secrets.Ref(nil), p.refs...), nil
}

func (p *fakePurger) Delete(_ context.Context, ref secrets.Ref) error {
	if p.failKey != "" && ref.Key == p.failKey {
		return fmt.Errorf("injected delete failure for %s", ref.Key)
	}
	p.deleted = append(p.deleted, ref)
	return nil
}

// TestRemoveServerPurgesCredentials pins the default: removing a server
// removes its credentials, across EVERY scope and key, and touches no other
// server's entries.
//
// The cross-scope part is the substance. Deleting just the two well-known
// keys in _global would leave a scoped refresh token behind, and because the
// vault is keyed by server id, re-adding the same id would then silently
// revive it against a possibly different provider.
func TestRemoveServerPurgesCredentials(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a", "keepme")

	p := &fakePurger{refs: []secrets.Ref{
		{ServerID: "a", Scope: secrets.DefaultScope, Key: secrets.KeyOAuthState},
		{ServerID: "a", Scope: secrets.DefaultScope, Key: secrets.KeyHTTPAuth},
		{ServerID: "a", Scope: "team-b", Key: secrets.KeyHTTPAuth},
		{ServerID: "a", Scope: "team-b", Key: "CUSTOM_TOKEN"},
		{ServerID: "keepme", Scope: secrets.DefaultScope, Key: secrets.KeyHTTPAuth},
	}}

	res, err := RemoveServer(ctx, st, "a", Precondition{}, RemoveOptions{Credentials: p})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("clean purge warned: %q", res.Warnings)
	}
	if len(p.deleted) != 4 {
		t.Fatalf("deleted %d refs, want 4: %+v", len(p.deleted), p.deleted)
	}
	for _, ref := range p.deleted {
		if ref.ServerID != "a" {
			t.Errorf("purge crossed into another server: %+v", ref)
		}
	}
}

// TestRemoveServerKeepCredentials pins the escape hatch: --keep-credentials
// must not touch the vault at all, while still removing the entry.
func TestRemoveServerKeepCredentials(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a")

	p := &fakePurger{refs: []secrets.Ref{
		{ServerID: "a", Scope: secrets.DefaultScope, Key: secrets.KeyOAuthState},
	}}
	if _, err := RemoveServer(ctx, st, "a", Precondition{},
		RemoveOptions{Credentials: p, KeepCredentials: true}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(p.deleted) != 0 {
		t.Errorf("--keep-credentials deleted %+v", p.deleted)
	}
	if _, ok := st.Snapshot().Servers.V.Servers["a"]; ok {
		t.Error("the entry survived removal")
	}
}

// TestRemoveServerPurgeFailureIsAWarning pins the failure direction: a vault
// that cannot be read or written must not fail the removal. A locked OS
// keychain would otherwise make servers unremovable.
func TestRemoveServerPurgeFailureIsAWarning(t *testing.T) {
	ctx := context.Background()

	t.Run("list fails", func(t *testing.T) {
		st := newStore(t)
		seedServers(t, st, "a")
		p := &fakePurger{listErr: fmt.Errorf("keychain is locked")}
		res, err := RemoveServer(ctx, st, "a", Precondition{}, RemoveOptions{Credentials: p})
		if err != nil {
			t.Fatalf("a vault failure broke the removal: %v", err)
		}
		if _, ok := st.Snapshot().Servers.V.Servers["a"]; ok {
			t.Error("the entry survived removal")
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "auth logout") {
			t.Errorf("warnings = %q, want one naming the manual fallback", res.Warnings)
		}
	})

	t.Run("one delete fails", func(t *testing.T) {
		st := newStore(t)
		seedServers(t, st, "a")
		p := &fakePurger{
			refs: []secrets.Ref{
				{ServerID: "a", Scope: secrets.DefaultScope, Key: secrets.KeyOAuthState},
				{ServerID: "a", Scope: secrets.DefaultScope, Key: secrets.KeyHTTPAuth},
			},
			failKey: secrets.KeyOAuthState,
		}
		res, err := RemoveServer(ctx, st, "a", Precondition{}, RemoveOptions{Credentials: p})
		if err != nil {
			t.Fatalf("a vault failure broke the removal: %v", err)
		}
		// The surviving entry must be named, and the other one still deleted:
		// a partial purge must not abort the rest.
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], secrets.KeyOAuthState) {
			t.Errorf("warnings = %q, want one naming the surviving key", res.Warnings)
		}
		if len(p.deleted) != 1 || p.deleted[0].Key != secrets.KeyHTTPAuth {
			t.Errorf("deleted = %+v, want the other key to still be purged", p.deleted)
		}
	})
}

// TestRemoveServerStalePurgesNothing pins the ordering: a removal that fails
// its precondition must leave the vault alone. Purging first would destroy
// credentials for a delete that never happened.
func TestRemoveServerStalePurgesNothing(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	// Two seeds, not one: Precondition{Generation: 0} means "do not check",
	// so after a single write gen-1 == 0 would silently pass the check and
	// this test would assert nothing.
	seedServers(t, st, "other", "a")
	gen := st.Snapshot().Generation

	p := &fakePurger{refs: []secrets.Ref{
		{ServerID: "a", Scope: secrets.DefaultScope, Key: secrets.KeyOAuthState},
	}}
	_, err := RemoveServer(ctx, st, "a", Precondition{Generation: gen - 1},
		RemoveOptions{Credentials: p})
	wantStale(t, err, gen)
	if len(p.deleted) != 0 {
		t.Errorf("a stale removal purged %+v", p.deleted)
	}
	if _, ok := st.Snapshot().Servers.V.Servers["a"]; !ok {
		t.Error("a stale removal deleted the entry anyway")
	}
}

// TestRemoveServerNoVault covers the caller that has no vault wired: the
// purge is skipped silently rather than warning, because there is nothing
// the operator could do about it and a warning on every delete would train
// them to ignore warnings.
func TestRemoveServerNoVault(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a")
	res, err := RemoveServer(ctx, st, "a", Precondition{}, RemoveOptions{})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("a vault-less removal warned: %q", res.Warnings)
	}
}

func TestSetServerEnabled(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a")

	res, err := SetServerEnabled(ctx, st, "a", false, Precondition{})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if res.Servers[0].Entry.Enabled {
		t.Error("the returned entry is still enabled")
	}
	if st.Snapshot().Servers.V.Servers["a"].V.Enabled {
		t.Error("the stored entry is still enabled")
	}

	_, err = SetServerEnabled(ctx, st, "ghost", true, Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeServerNotFound)

	gen := st.Snapshot().Generation
	_, err = SetServerEnabled(ctx, st, "a", true, Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
	if st.Snapshot().Servers.V.Servers["a"].V.Enabled {
		t.Error("a stale toggle was applied anyway")
	}
}

// TestValidateOAuthHint pins the screen every writer of a login hint goes
// through — CLI flags, pasted JSON and the control plane all reach it via
// ValidateServerSpec, so a pin that discovery could never use is refused
// once, at the moment the operator can still fix what they typed.
//
// Failure direction: refuse. A malformed issuer that is merely carried
// surfaces much later as an `auth login` failure against a URL nobody sees.
func TestValidateOAuthHint(t *testing.T) {
	cases := []struct {
		name  string
		hint  *registry.OAuthHint
		local bool
		ok    bool
	}{
		{name: "nil is the norm", hint: nil, ok: true},
		{name: "empty block is a no-op", hint: &registry.OAuthHint{}, ok: true},
		{
			name: "full hint",
			hint: &registry.OAuthHint{
				Issuer:              "https://as.example.com",
				Scopes:              []string{"openid", "mcp:read"},
				ResourceMetadataURL: "https://rs.example.com/.well-known/oauth-protected-resource",
			},
			ok: true,
		},
		{name: "issuer is not a URL", hint: &registry.OAuthHint{Issuer: "as.example.com"}},
		{name: "issuer has no host", hint: &registry.OAuthHint{Issuer: "https:///path"}},
		{name: "plaintext issuer", hint: &registry.OAuthHint{Issuer: "http://as.example.com"}},
		{
			name:  "plaintext issuer is fine for a local server",
			hint:  &registry.OAuthHint{Issuer: "http://127.0.0.1:8080"},
			local: true, ok: true,
		},
		{name: "loopback issuer without --local", hint: &registry.OAuthHint{Issuer: "https://127.0.0.1:8080"}},
		{name: "private issuer", hint: &registry.OAuthHint{Issuer: "https://10.0.0.5"}},
		// RFC 8414 §2: metadata URLs are built by inserting a path into the
		// issuer, so a query string here silently produces a 404.
		{name: "issuer with a query", hint: &registry.OAuthHint{Issuer: "https://as.example.com/?x=1"}},
		{name: "issuer with a fragment", hint: &registry.OAuthHint{Issuer: "https://as.example.com/#f"}},
		{
			name: "resource metadata may carry a query",
			hint: &registry.OAuthHint{ResourceMetadataURL: "https://rs.example.com/prm?tenant=a"},
			ok:   true,
		},
		{name: "resource metadata is private", hint: &registry.OAuthHint{ResourceMetadataURL: "https://192.168.1.2/prm"}},
		{name: "empty scope", hint: &registry.OAuthHint{Scopes: []string{"openid", "  "}}},
		// RFC 6749 §3.3: scopes are space-delimited. One value holding two
		// scopes would be sent as a single unknown token.
		{name: "scope with a space", hint: &registry.OAuthHint{Scopes: []string{"openid profile"}}},
		{name: "scope with a tab", hint: &registry.OAuthHint{Scopes: []string{"openid\tprofile"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateOAuthHint("srv", tc.hint, tc.local)
			if tc.ok {
				if err != nil {
					t.Fatalf("ValidateOAuthHint = %v, want accepted", err)
				}
				return
			}
			if err == nil {
				t.Fatal("hint was accepted; a bad pin must be refused before it is stored")
			}
			wantErrorKind(t, err, KindUsage, CodeUsage)
		})
	}
}

// TestValidateServerSpecScreensOAuthOnEveryTransport: the hint is
// transport-independent (an stdio child may proxy to a remote authorization
// server), so the check must not sit inside one transport's branch.
func TestValidateServerSpecScreensOAuthOnEveryTransport(t *testing.T) {
	bad := &registry.OAuthHint{Issuer: "not a url"}

	e := stdio("gh-mcp")
	e.OAuth = bad
	wantErrorKind(t, ValidateServerSpec(ServerSpec{ID: "a", Entry: e}), KindUsage, CodeUsage)

	h := registry.ServerEntry{Transport: registry.TransportHTTP, URL: "https://x.example.com/mcp", OAuth: bad}
	wantErrorKind(t, ValidateServerSpec(ServerSpec{ID: "b", Entry: h}), KindUsage, CodeUsage)
}

// TestAddServerRefusesBadOAuthHint proves the refusal happens under the
// lock too, not only in the front end's pre-check: nothing is written.
func TestAddServerRefusesBadOAuthHint(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	e := stdio("gh-mcp")
	e.OAuth = &registry.OAuthHint{Issuer: "https://as.example.com", Scopes: []string{"a b"}}
	_, err := AddServer(ctx, st, ServerSpec{ID: "gh", Entry: e}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
	if _, ok := st.Snapshot().Servers.V.Servers["gh"]; ok {
		t.Error("a rejected entry was persisted anyway")
	}
}
