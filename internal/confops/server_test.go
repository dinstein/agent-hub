package confops

import (
	"context"
	"fmt"
	"slices"
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

// fakeForgetter is an in-memory StateForgetter with fault injection.
type fakeForgetter struct {
	name   string
	forgot []string
	err    error
}

func (f *fakeForgetter) ForgetServer(_ context.Context, id string) error {
	if f.err != nil {
		return f.err
	}
	f.forgot = append(f.forgot, id)
	return nil
}

func (f *fakeForgetter) StateName() string { return f.name }

// TestRemoveServerClearsState pins that out-of-registry state goes with the
// server. Each of these stores is keyed by server id, so anything left here
// is inherited by whatever is re-added under that id later — a credential,
// an integrity baseline or a standing approval earned by a different server.
func TestRemoveServerClearsState(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a")

	pins := &fakeForgetter{name: "tool pins"}
	grants := &fakeForgetter{name: "approval grants"}
	res, err := RemoveServer(ctx, st, "a", Precondition{}, RemoveOptions{
		State: []StateForgetter{pins, nil, grants}, // nil entries are skipped
	})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("a clean removal warned: %q", res.Warnings)
	}
	for _, f := range []*fakeForgetter{pins, grants} {
		if len(f.forgot) != 1 || f.forgot[0] != "a" {
			t.Errorf("%s cleared %v, want [a]", f.name, f.forgot)
		}
	}
}

// TestRemoveServerStateFailureIsAWarning pins the failure direction for the
// state cleanups: the server is already gone from the registry, so a store
// that will not open or write must warn rather than fail — and the warning
// has to NAME the store, because the operator's next move differs per store.
// One failing store must not stop the others.
func TestRemoveServerStateFailureIsAWarning(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a")

	broken := &fakeForgetter{name: "tool pins", err: fmt.Errorf("state file is locked")}
	ok := &fakeForgetter{name: "approval grants"}
	res, err := RemoveServer(ctx, st, "a", Precondition{},
		RemoveOptions{State: []StateForgetter{broken, ok}})
	if err != nil {
		t.Fatalf("a state-store failure broke the removal: %v", err)
	}
	if _, exists := st.Snapshot().Servers.V.Servers["a"]; exists {
		t.Error("the entry survived removal")
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "tool pins") {
		t.Errorf("warnings = %q, want one naming the store that survived", res.Warnings)
	}
	if len(ok.forgot) != 1 {
		t.Error("one failing store stopped the others")
	}
}

// TestRemoveServerForgetsReferences pins the reference rewrite and, more
// importantly, its DIRECTION. Profile.Servers is an allow list whose empty
// value means "no servers", so an emptied list must stay [] and never
// collapse to nil — nil means "every server", which would turn deleting a
// server into a silent widening of that profile.
func TestRemoveServerForgetsReferences(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a", "b")

	if err := st.Update(ctx, func(tx *registry.Tx) error {
		tx.Profiles.V.Profiles = map[string]registry.Doc[registry.Profile]{
			"mixed": {V: registry.Profile{
				Servers: []string{"a", "b"},
				Tools: map[string]registry.Doc[registry.ToolSelector]{
					"a": {V: registry.ToolSelector{Allow: []string{"x"}}},
					"b": {V: registry.ToolSelector{Allow: []string{"y"}}},
				},
			}},
			"only-a": {V: registry.Profile{Servers: []string{"a"}}},
			"untouched": {V: registry.Profile{
				Tools: map[string]registry.Doc[registry.ToolSelector]{
					"b": {V: registry.ToolSelector{Allow: []string{"y"}}},
				},
			}},
		}
		tx.Governance.V.ResultBudget = map[string]registry.Doc[registry.Budget]{
			"a": {V: registry.Budget{Bytes: 1}},
			"*": {V: registry.Budget{Bytes: 2}},
		}
		tx.Governance.V.RateLimits = []registry.Doc[registry.RateLimitRule]{
			{V: registry.RateLimitRule{Server: "a", Limit: 1, Window: "1m"}},
			{V: registry.RateLimitRule{Server: "*", Limit: 2, Window: "1m"}},
			{V: registry.RateLimitRule{Limit: 3, Window: "1m"}},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := RemoveServer(ctx, st, "a", Precondition{}, RemoveOptions{}); err != nil {
		t.Fatalf("remove: %v", err)
	}

	snap := st.Snapshot()
	profiles := snap.Profiles.V.Profiles
	if got := profiles["mixed"].V.Servers; !slices.Equal(got, []string{"b"}) {
		t.Errorf("mixed.Servers = %v, want [b]", got)
	}
	if _, ok := profiles["mixed"].V.Tools["a"]; ok {
		t.Error("mixed kept a tool selector for the removed server")
	}
	if _, ok := profiles["mixed"].V.Tools["b"]; !ok {
		t.Error("mixed lost the surviving server's selector")
	}
	// The whole point: emptied, not widened.
	onlyA := profiles["only-a"].V.Servers
	if onlyA == nil {
		t.Fatal("an emptied allow list collapsed to nil, which means 'all servers'")
	}
	if len(onlyA) != 0 {
		t.Errorf("only-a.Servers = %v, want empty", onlyA)
	}
	if _, ok := profiles["untouched"].V.Tools["b"]; !ok {
		t.Error("an unrelated profile was rewritten")
	}

	gov := snap.Governance.V
	if _, ok := gov.ResultBudget["a"]; ok {
		t.Error("the removed server kept its result budget")
	}
	if _, ok := gov.ResultBudget["*"]; !ok {
		t.Error("the machine-wide result budget was dropped")
	}
	if len(gov.RateLimits) != 2 {
		t.Fatalf("rate limits = %+v, want the two non-specific rules", gov.RateLimits)
	}
	for _, r := range gov.RateLimits {
		if r.V.Server == "a" {
			t.Errorf("a rule naming the removed server survived: %+v", r.V)
		}
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

// The trace switch is per server, persists, and refuses an unknown id the
// same way every other server reference does. A typo that silently created a
// traced ghost would leave an operator waiting on frames from a name nothing
// ever dials.
func TestSetServerTrace(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a")

	if st.Snapshot().Servers.V.Servers["a"].V.Trace {
		t.Fatal("a freshly seeded server is already traced; capture must be opt-in")
	}

	res, err := SetServerTrace(ctx, st, "a", true, Precondition{})
	if err != nil {
		t.Fatalf("trace on: %v", err)
	}
	if !res.Servers[0].Entry.Trace {
		t.Error("the returned entry is not traced")
	}
	if !st.Snapshot().Servers.V.Servers["a"].V.Trace {
		t.Error("the stored entry is not traced")
	}
	// Tracing must not disturb what the server IS. That the definition is
	// untouched is exactly what lets a running gateway pick the flip up
	// without reconnecting.
	if !st.Snapshot().Servers.V.Servers["a"].V.Enabled {
		t.Error("turning a trace on changed the enable flag")
	}

	_, err = SetServerTrace(ctx, st, "ghost", true, Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeServerNotFound)

	gen := st.Snapshot().Generation
	_, err = SetServerTrace(ctx, st, "a", false, Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
	if !st.Snapshot().Servers.V.Servers["a"].V.Trace {
		t.Error("a stale trace flip was applied anyway")
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

// TestSetServerToolsThreeStates is TestSetProfileToolsThreeStates one layer
// up, and deliberately asserts the same three outcomes: the two layers are
// one mechanism, so the state that fails open — block-all persisting as the
// EMPTY list rather than being dropped — has to be pinned at both altitudes
// or the shared resolver can regress on the untested one.
func TestSetServerToolsThreeStates(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "github")

	res, err := SetServerTools(ctx, st, "github",
		ToolSelection{Mode: ToolSelectOnly, Tools: []string{"list_prs", "create_pr", "list_prs"}}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Servers[0].Entry.Tools; len(got) != 2 || got[0] != "create_pr" {
		t.Errorf("--only rule = %v, want the deduplicated sorted subset", got)
	}

	res, err = SetServerTools(ctx, st, "github", ToolSelection{Mode: ToolSelectNone}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Servers[0].Entry.Tools; got == nil || len(got) != 0 {
		t.Errorf("block-all must store the EMPTY allow list, got %v", got)
	}
	// And it must SURVIVE a round trip through the document: omitzero keeps
	// an empty list on disk where omitempty would drop it, turning
	// expose-nothing into expose-everything on the next read.
	if got := st.Snapshot().Servers.V.Servers["github"].V.Tools; got == nil || len(got) != 0 {
		t.Errorf("block-all did not survive the round trip, got %v", got)
	}

	res, err = SetServerTools(ctx, st, "github", ToolSelection{Mode: ToolSelectAll}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Servers[0].Entry.Tools; got != nil {
		t.Errorf("all-tools must clear the rule, got %v", got)
	}

	// Validation, in the same order confops requires: arguments first, so an
	// unset mode is refused before the server is even looked up.
	_, err = SetServerTools(ctx, st, "github", ToolSelection{}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
	_, err = SetServerTools(ctx, st, "github", ToolSelection{Mode: ToolSelectOnly}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
	_, err = SetServerTools(ctx, st, "ghost", ToolSelection{Mode: ToolSelectAll}, Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeServerNotFound)
}
