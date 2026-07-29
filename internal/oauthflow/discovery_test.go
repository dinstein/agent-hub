package oauthflow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// TestMetadataCandidateOrder is a golden test: the candidate order is a
// protocol contract (RFC 8414 §3.1 path insertion before the OIDC path
// appending form), not an implementation detail.
func TestMetadataCandidateOrder(t *testing.T) {
	cases := []struct {
		name   string
		issuer string
		want   []string
	}{
		{
			name:   "no path: two insertion forms",
			issuer: "https://as.example.com",
			want: []string{
				"https://as.example.com/.well-known/oauth-authorization-server",
				"https://as.example.com/.well-known/openid-configuration",
			},
		},
		{
			name:   "trailing slash is treated as no path",
			issuer: "https://as.example.com/",
			want: []string{
				"https://as.example.com/.well-known/oauth-authorization-server",
				"https://as.example.com/.well-known/openid-configuration",
			},
		},
		{
			name:   "single path segment: insertion twice, then appending",
			issuer: "https://as.example.com/tenant1",
			want: []string{
				"https://as.example.com/.well-known/oauth-authorization-server/tenant1",
				"https://as.example.com/.well-known/openid-configuration/tenant1",
				"https://as.example.com/tenant1/.well-known/openid-configuration",
			},
		},
		{
			name:   "multi segment path keeps the whole path",
			issuer: "https://as.example.com/a/b/",
			want: []string{
				"https://as.example.com/.well-known/oauth-authorization-server/a/b",
				"https://as.example.com/.well-known/openid-configuration/a/b",
				"https://as.example.com/a/b/.well-known/openid-configuration",
			},
		},
		{
			name:   "port is preserved",
			issuer: "https://as.example.com:8443/t",
			want: []string{
				"https://as.example.com:8443/.well-known/oauth-authorization-server/t",
				"https://as.example.com:8443/.well-known/openid-configuration/t",
				"https://as.example.com:8443/t/.well-known/openid-configuration",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.issuer)
			if err != nil {
				t.Fatal(err)
			}
			got := MetadataCandidates(u)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("candidates:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestProtectedResourceCandidateOrder(t *testing.T) {
	u, _ := url.Parse("https://mcp.example.com/servers/gh")
	want := []string{
		"https://mcp.example.com/.well-known/oauth-protected-resource/servers/gh",
		"https://mcp.example.com/servers/gh/.well-known/oauth-protected-resource",
		// Origin root, tried last: a deployment whose resource identifier
		// is the bare origin publishes only this document, and both
		// path-derived forms 404. Regression — omitting it made every such
		// server unauthorizable even though its 401 pointed straight here.
		"https://mcp.example.com/.well-known/oauth-protected-resource",
	}
	if got := ProtectedResourceCandidates(u); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}

	// A root resource URL must not repeat the same candidate.
	root, _ := url.Parse("https://mcp.example.com")
	if got := ProtectedResourceCandidates(root); !reflect.DeepEqual(got,
		[]string{"https://mcp.example.com/.well-known/oauth-protected-resource"}) {
		t.Fatalf("root: got %q", got)
	}
}

// TestDiscoveryWalksCandidatesInOrder proves the walk stops at the first
// candidate that answers, and that a 404 on an earlier candidate does not
// abort the chain.
func TestDiscoveryWalksCandidatesInOrder(t *testing.T) {
	as := newFakeAS(t)
	as.issuerPath = "tenant1"
	// Only the third candidate (OIDC path appending) is served.
	as.servedMetadata = map[string]bool{
		"/tenant1/.well-known/openid-configuration": true,
	}
	d := NewDiscoverer(as.client())
	res, err := d.DiscoverFromIssuer(context.Background(), as.issuer())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if res.Status != DiscoveryOK {
		t.Fatalf("status = %s", res.Status)
	}
	wantOrder := []string{
		"/.well-known/oauth-authorization-server/tenant1",
		"/.well-known/openid-configuration/tenant1",
		"/tenant1/.well-known/openid-configuration",
	}
	if got := as.hitList(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("server saw %q, want %q", got, wantOrder)
	}
	if res.Metadata.TokenEndpoint == "" {
		t.Fatal("metadata has no token endpoint")
	}
}

func TestDiscoveryFirstCandidateWins(t *testing.T) {
	as := newFakeAS(t)
	d := NewDiscoverer(as.client())
	if _, err := d.DiscoverFromIssuer(context.Background(), as.issuer()); err != nil {
		t.Fatal(err)
	}
	hits := as.hitList()
	if len(hits) != 1 || hits[0] != "/.well-known/oauth-authorization-server" {
		t.Fatalf("expected exactly the first candidate, got %q", hits)
	}
}

func TestDiscoveryFallsBackToDefaultEndpoints(t *testing.T) {
	as := newFakeAS(t)
	as.servedMetadata = map[string]bool{} // nothing is served
	d := NewDiscoverer(as.client())
	res, err := d.DiscoverFromIssuer(context.Background(), as.issuer())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if res.Status != DiscoveryDefaults {
		t.Fatalf("status = %s, want %s", res.Status, DiscoveryDefaults)
	}
	if res.Metadata.TokenEndpoint != as.issuer()+"/token" {
		t.Fatalf("token endpoint = %q", res.Metadata.TokenEndpoint)
	}

	d.AllowDefaultEndpoints = false
	if _, err := d.DiscoverFromIssuer(context.Background(), as.issuer()); !errors.Is(err, ErrDiscovery) {
		t.Fatalf("with fallback disabled, err = %v, want ErrDiscovery", err)
	}
}

// TestDiscoverFromResource walks the RFC 9728 hop and records the resource
// indicator that later binds the token (RFC 8707).
func TestDiscoverFromResource(t *testing.T) {
	as := newFakeAS(t)
	d := NewDiscoverer(as.client())
	res, err := d.DiscoverFromResource(context.Background(), as.srv.URL+"/mcp", "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if res.Protected == nil {
		t.Fatal("no protected resource metadata")
	}
	if res.Resource != as.srv.URL+"/mcp" {
		t.Fatalf("resource = %q", res.Resource)
	}
	if res.Metadata.TokenEndpoint != as.srv.URL+"/token" {
		t.Fatalf("token endpoint = %q", res.Metadata.TokenEndpoint)
	}
	// The advertised resource_metadata pointer must be tried FIRST.
	as2 := newFakeAS(t)
	d2 := NewDiscoverer(as2.client())
	ptr := as2.srv.URL + "/.well-known/oauth-protected-resource/custom"
	if _, err := d2.DiscoverFromResource(context.Background(), as2.srv.URL+"/mcp", ptr); err != nil {
		t.Fatal(err)
	}
	if got := as2.hitList()[0]; got != "/.well-known/oauth-protected-resource/custom" {
		t.Fatalf("first hit = %q, want the advertised pointer", got)
	}
}

// TestDiscoverFromResourceOrigin covers a real deployment shape that was
// undiscoverable before: the resource server publishes NO RFC 9728
// document, but does serve RFC 8414 metadata on its own origin. The
// endpoints are published and reachable, so discovery must find them rather
// than requiring an operator to pin them by hand.
func TestDiscoverFromResourceOrigin(t *testing.T) {
	as := newFakeAS(t)
	as.noProtectedResource = true
	as.authorizeSuffix = "/tenant1" // the per-resource endpoint
	d := NewDiscoverer(as.client())

	res, err := d.DiscoverFromResource(context.Background(), as.srv.URL+"/tenant1/mcp", "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if res.Status != DiscoveryResourceOrigin {
		t.Fatalf("status = %s, want %s", res.Status, DiscoveryResourceOrigin)
	}
	if res.Protected != nil {
		t.Fatal("no protected-resource document exists; Protected must stay nil")
	}
	// The whole point: the per-resource authorization_endpoint, not a
	// synthesized or generic one.
	if want := as.srv.URL + "/authorize/tenant1"; res.Metadata.AuthorizationEndpoint != want {
		t.Fatalf("authorization endpoint = %q, want %q", res.Metadata.AuthorizationEndpoint, want)
	}
	// The resource indicator still binds the token to the MCP endpoint.
	if res.Resource != as.srv.URL+"/tenant1/mcp" {
		t.Fatalf("resource = %q", res.Resource)
	}
	// The PRM candidates must all have been tried BEFORE this hop: the
	// fallback may never pre-empt a document the server actually advertises.
	hits := as.hitList()
	firstAS := -1
	for i, h := range hits {
		if strings.Contains(h, wellKnownProtectedResrc) && firstAS != -1 {
			t.Fatalf("protected-resource candidate %q tried after AS metadata: %q", h, hits)
		}
		if strings.Contains(h, wellKnownOAuthAS) && firstAS == -1 {
			firstAS = i
		}
	}
	if firstAS == -1 {
		t.Fatalf("AS metadata was never fetched from the resource origin: %q", hits)
	}
}

// TestDiscoverFromResourceOriginDoesNotSynthesize proves the fallback never
// invents endpoints on the resource server's origin. Sending a user's browser
// to a guessed /authorize on the RS host is worse than failing: no provider
// ever named that URL.
func TestDiscoverFromResourceOriginDoesNotSynthesize(t *testing.T) {
	as := newFakeAS(t)
	as.servedMetadata = map[string]bool{} // neither PRM nor AS metadata
	d := NewDiscoverer(as.client())       // AllowDefaultEndpoints stays true

	_, err := d.DiscoverFromResource(context.Background(), as.srv.URL+"/tenant1/mcp", "")
	if !errors.Is(err, ErrDiscovery) {
		t.Fatalf("err = %v, want ErrDiscovery", err)
	}
	var fe *FlowError
	if errors.As(err, &fe) && fe.Discovery != DiscoveryFailed {
		t.Fatalf("discovery status = %s, want %s", fe.Discovery, DiscoveryFailed)
	}
}

// TestDiscoverFromResourcePrefersAdvertisedMetadata pins the precedence: when
// the resource server DOES publish an RFC 9728 document, the origin fallback
// must not run at all.
func TestDiscoverFromResourcePrefersAdvertisedMetadata(t *testing.T) {
	as := newFakeAS(t)
	d := NewDiscoverer(as.client())
	res, err := d.DiscoverFromResource(context.Background(), as.srv.URL+"/mcp", "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if res.Status != DiscoveryOK {
		t.Fatalf("status = %s, want %s (the advertised path must win)", res.Status, DiscoveryOK)
	}
	if res.Protected == nil {
		t.Fatal("advertised protected-resource document was not used")
	}
}

func TestResourceMetadataURLParsing(t *testing.T) {
	cases := []struct {
		name   string
		header []string
		want   string
	}{
		{
			name:   "typical MCP 401",
			header: []string{`Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`},
			want:   "https://mcp.example.com/.well-known/oauth-protected-resource",
		},
		{
			name: "value containing a comma must not be split",
			header: []string{`Bearer realm="a,b", error="invalid_token", ` +
				`resource_metadata="https://x.example/.well-known/oauth-protected-resource"`},
			want: "https://x.example/.well-known/oauth-protected-resource",
		},
		{
			name:   "unquoted value",
			header: []string{`Bearer resource_metadata=https://x.example/prm`},
			want:   "https://x.example/prm",
		},
		{
			name:   "case-insensitive parameter name",
			header: []string{`Bearer Resource_Metadata="https://x.example/prm"`},
			want:   "https://x.example/prm",
		},
		{
			name:   "multiple header lines",
			header: []string{`Basic realm="x"`, `Bearer resource_metadata="https://y.example/prm"`},
			want:   "https://y.example/prm",
		},
		{
			name:   "escaped quote inside the value",
			header: []string{`Bearer error_description="he said \"no\"", resource_metadata="https://z.example/prm"`},
			want:   "https://z.example/prm",
		},
		{
			name:   "absent",
			header: []string{`Bearer realm="x", error="invalid_token"`},
			want:   "",
		},
		{
			name:   "no header at all",
			header: nil,
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResourceMetadataURL(tc.header); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("WWW-Authenticate", `Bearer resource_metadata="https://r.example/prm"`)
	if got := ResourceMetadataURLFromResponse(resp); got != "https://r.example/prm" {
		t.Fatalf("from response: %q", got)
	}
	if got := ResourceMetadataURLFromResponse(nil); got != "" {
		t.Fatalf("nil response: %q", got)
	}
}

func TestValidateMetadataRejectsUnusableDocument(t *testing.T) {
	err := validateMetadata(&AuthServerMetadata{AuthorizationEndpoint: "https://x/a", SourceURL: "u"})
	if err == nil {
		t.Fatal("metadata without a token endpoint must be rejected")
	}
	err = validateMetadata(&AuthServerMetadata{TokenEndpoint: "https://x/t", SourceURL: "u"})
	if err == nil {
		t.Fatal("metadata with no authorization or device endpoint must be rejected")
	}
	if err := validateMetadata(&AuthServerMetadata{
		TokenEndpoint:               "https://x/t",
		DeviceAuthorizationEndpoint: "https://x/d",
	}); err != nil {
		t.Fatalf("device-only metadata must be usable: %v", err)
	}
}

func TestSupportsDeviceFlow(t *testing.T) {
	if (&AuthServerMetadata{}).SupportsDeviceFlow() {
		t.Fatal("empty metadata must not claim device support")
	}
	if !(&AuthServerMetadata{DeviceAuthorizationEndpoint: "https://x/d"}).SupportsDeviceFlow() {
		t.Fatal("device endpoint must be detected")
	}
	var nilMD *AuthServerMetadata
	if nilMD.SupportsDeviceFlow() {
		t.Fatal("nil metadata must not claim device support")
	}
}

// TestDiscoverPrefersPinnedIssuer pins the precedence in Flow.discover: an
// explicitly configured issuer (`--issuer`, or ServerEntry.OAuth.Issuer)
// must win over the RFC 9728 resource route.
//
// Regression: ResourceURL used to be consulted first, which made the pin
// unreachable for every http/sse server — those always carry a URL, and the
// CLI always fills ResourceURL from it. A resource server that publishes no
// protected-resource metadata (FastMCP behind an external AS, e.g. the
// 401 + bare WWW-Authenticate shape) could therefore never be authorized:
// the pin existed for exactly that case and was silently ignored.
func TestDiscoverPrefersPinnedIssuer(t *testing.T) {
	as := newFakeAS(t)
	f := &Flow{Discoverer: NewDiscoverer(as.client())}

	// A resource server that serves no protected-resource metadata at all
	// (closed port). Resolving through it must fail, so a passing test
	// proves the issuer branch was taken rather than the resource one
	// merely succeeding too.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(dead.Close)
	deadResource := dead.URL + "/mcp"
	if _, err := f.discover(context.Background(), LoginRequest{
		ServerID:    "s",
		ResourceURL: deadResource,
	}); err == nil {
		t.Fatal("resource-only discovery unexpectedly succeeded; test cannot prove precedence")
	}

	res, err := f.discover(context.Background(), LoginRequest{
		ServerID:    "s",
		Issuer:      as.issuer(),
		ResourceURL: deadResource,
	})
	if err != nil {
		t.Fatalf("pinned issuer must skip the failing resource route: %v", err)
	}
	if res.Metadata == nil || res.Metadata.TokenEndpoint != as.srv.URL+"/token" {
		t.Fatalf("metadata = %+v, want the pinned AS", res.Metadata)
	}

	// With no pin, the resource route is still the one that runs.
	if _, err := f.discover(context.Background(), LoginRequest{
		ServerID:    "s",
		ResourceURL: as.srv.URL + "/mcp",
	}); err != nil {
		t.Fatalf("unpinned discovery must still take the RFC 9728 route: %v", err)
	}
}

func TestChallengeScopeParsing(t *testing.T) {
	cases := []struct {
		name   string
		header []string
		want   string
	}{
		{
			name:   "scope alongside resource_metadata",
			header: []string{`Bearer resource_metadata="https://x.example/prm", scope="files:read files:write"`},
			want:   "files:read files:write",
		},
		{
			name:   "insufficient_scope 403 challenge",
			header: []string{`Bearer error="insufficient_scope", scope="files:write", error_description="need write"`},
			want:   "files:write",
		},
		{
			name:   "unquoted single scope",
			header: []string{`Bearer scope=read`},
			want:   "read",
		},
		{
			name:   "absent",
			header: []string{`Bearer realm="FastMCP", error="invalid_token"`},
			want:   "",
		},
		{
			name:   "error_description containing the word scope must not match",
			header: []string{`Bearer error_description="a scope problem"`},
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChallengeScope(tc.header); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestScopeSetOrder pins MCP 2025-11-25's scope selection strategy.
//
// The third case is the one with teeth: with no challenge and no PRM we send
// NOTHING, rather than falling back to the authorization server's
// scopes_supported. The two documents answer different questions — the PRM
// says "what accessing me requires", AS metadata says "everything I can
// issue" — so that fallback would request every privilege a provider offers,
// write and admin included, for a resource that never asked for them.
func TestScopeSetOrder(t *testing.T) {
	asWide := &AuthServerMetadata{ScopesSupported: []string{"openid", "read", "write", "admin"}}

	cases := []struct {
		name string
		res  *DiscoveryResult
		want []string
	}{
		{
			name: "challenge wins over protected-resource metadata",
			res: &DiscoveryResult{
				ChallengeScope: "files:read",
				Protected:      &ProtectedResourceMetadata{ScopesSupported: []string{"read", "write"}},
				Metadata:       asWide,
			},
			want: []string{"files:read"},
		},
		{
			name: "protected-resource metadata when no challenge",
			res: &DiscoveryResult{
				Protected: &ProtectedResourceMetadata{ScopesSupported: []string{"read", "profile"}},
				Metadata:  asWide,
			},
			want: []string{"read", "profile"},
		},
		{
			name: "no challenge and no PRM sends nothing, NOT the AS scopes",
			res:  &DiscoveryResult{Metadata: asWide},
			want: nil,
		},
		{
			name: "PRM without scopes_supported sends nothing",
			res: &DiscoveryResult{
				Protected: &ProtectedResourceMetadata{},
				Metadata:  asWide,
			},
			want: nil,
		},
		{
			name: "multi-scope challenge splits on whitespace",
			res:  &DiscoveryResult{ChallengeScope: "  a   b\tc "},
			want: []string{"a", "b", "c"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.res.ScopeSet(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}

	// A nil result must not panic: discovery can fail before producing one.
	if got := (*DiscoveryResult)(nil).ScopeSet(); got != nil {
		t.Fatalf("nil result: got %q", got)
	}
}

// TestScopeSetDoesNotAliasPRM guards against handing callers the slice that
// lives inside the discovery result: a caller appending to it would mutate
// the document.
func TestScopeSetDoesNotAliasPRM(t *testing.T) {
	prm := &ProtectedResourceMetadata{ScopesSupported: []string{"read"}}
	res := &DiscoveryResult{Protected: prm}
	got := res.ScopeSet()
	got = append(got, "write") //nolint:staticcheck // the append is the test
	_ = got
	if len(prm.ScopesSupported) != 1 || prm.ScopesSupported[0] != "read" {
		t.Fatalf("ScopeSet aliased the document: %q", prm.ScopesSupported)
	}
}
