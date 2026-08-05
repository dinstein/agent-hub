package oauthflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Well-known suffixes (RFC 8414 §3, RFC 9728 §3, OpenID Connect Discovery
// 1.0 §4). These strings are protocol constants, not configuration.
const (
	wellKnownOAuthAS         = "oauth-authorization-server"
	wellKnownOpenIDConfig    = "openid-configuration"
	wellKnownProtectedResrc  = "oauth-protected-resource"
	wellKnownPrefix          = "/.well-known/"
	headerWWWAuthenticate    = "WWW-Authenticate"
	paramResourceMetadataKey = "resource_metadata"
	paramScopeKey            = "scope"
)

// AuthServerMetadata is the subset of RFC 8414 / OIDC Discovery metadata
// this package uses. Unknown members are ignored: agenthub is a client, and
// a client that fails on unrecognized metadata members breaks on every
// provider extension.
type AuthServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	DeviceAuthorizationEndpoint       string   `json:"device_authorization_endpoint"`
	RevocationEndpoint                string   `json:"revocation_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	// AuthorizationResponseIssParameterSupported is the RFC 9207 signal: an
	// AS that sets it promises an iss parameter on every authorization
	// response, which lets the client treat a MISSING iss as an attack
	// rather than as an old server (see validateIss).
	AuthorizationResponseIssParameterSupported bool `json:"authorization_response_iss_parameter_supported"`

	// SourceURL records which candidate produced this document. It is not
	// a protocol member; it exists so diagnostics can say *which* of the
	// candidate URLs answered.
	SourceURL string `json:"-"`
}

// SupportsDeviceFlow reports whether the AS advertises RFC 8628. `auth
// login` uses this for automatic mode selection (docs/modules/oauth.md).
func (m *AuthServerMetadata) SupportsDeviceFlow() bool {
	return m != nil && strings.TrimSpace(m.DeviceAuthorizationEndpoint) != ""
}

// ProtectedResourceMetadata is the RFC 9728 document a resource server
// publishes to point at its authorization servers.
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`

	SourceURL string `json:"-"`
}

// MetadataCandidates returns the RFC 8414 candidate URLs for issuer, in the
// order they must be tried. The order is a protocol contract and is
// golden-tested; do not reorder it for convenience.
//
// With a path component (issuer https://as.example.com/tenant1):
//
//  1. https://as.example.com/.well-known/oauth-authorization-server/tenant1
//     — RFC 8414 path INSERTION: the well-known segment goes between the
//     authority and the issuer path. This is the correct rule and the one
//     most often gotten wrong.
//  2. https://as.example.com/.well-known/openid-configuration/tenant1
//     — same insertion, OIDC document.
//  3. https://as.example.com/tenant1/.well-known/openid-configuration
//     — OIDC Discovery 1.0 path APPENDING, kept last because plenty of
//     deployed OIDC providers only implement this form.
//
// Without a path component only the two insertion forms exist (they are
// then indistinguishable from appending):
//
//  1. https://as.example.com/.well-known/oauth-authorization-server
//  2. https://as.example.com/.well-known/openid-configuration
//
// A trailing slash on the issuer path is treated as no path: RFC 8414
// issuers are compared by exact string, but "https://x/" and "https://x"
// name the same deployment in every implementation we have to interoperate
// with, and generating a "/.well-known/oauth-authorization-server/" URL
// helps nobody.
func MetadataCandidates(issuer *url.URL) []string {
	if issuer == nil {
		return nil
	}
	base := &url.URL{Scheme: issuer.Scheme, Host: issuer.Host}
	path := strings.Trim(issuer.EscapedPath(), "/")
	if path == "" {
		return []string{
			base.String() + wellKnownPrefix + wellKnownOAuthAS,
			base.String() + wellKnownPrefix + wellKnownOpenIDConfig,
		}
	}
	return []string{
		base.String() + wellKnownPrefix + wellKnownOAuthAS + "/" + path,
		base.String() + wellKnownPrefix + wellKnownOpenIDConfig + "/" + path,
		base.String() + "/" + path + wellKnownPrefix + wellKnownOpenIDConfig,
	}
}

// ProtectedResourceCandidates returns the RFC 9728 candidate URLs for a
// resource URL, same insertion-then-appending shape as MetadataCandidates.
// Used only when a 401 carried no resource_metadata parameter.
func ProtectedResourceCandidates(resource *url.URL) []string {
	if resource == nil {
		return nil
	}
	base := &url.URL{Scheme: resource.Scheme, Host: resource.Host}
	path := strings.Trim(resource.EscapedPath(), "/")
	if path == "" {
		return []string{base.String() + wellKnownPrefix + wellKnownProtectedResrc}
	}
	return []string{
		base.String() + wellKnownPrefix + wellKnownProtectedResrc + "/" + path,
		base.String() + "/" + path + wellKnownPrefix + wellKnownProtectedResrc,
		// Origin-root fallback. RFC 9728 §3 anchors the document at the
		// resource IDENTIFIER, which is frequently the bare origin even
		// when the MCP endpoint sits under a path: a deployment serving
		// {"resource":"https://host"} publishes only the root document, and
		// both path-derived candidates above 404. Tried last so a
		// path-scoped document still wins when one exists.
		base.String() + wellKnownPrefix + wellKnownProtectedResrc,
	}
}

// DefaultEndpoints synthesizes metadata from an issuer when every candidate
// 404s. Several real providers publish no metadata document at all but do
// serve /authorize, /token and /register relative to the issuer.
//
// This is a fallback, never a first choice, and it is recorded as
// DiscoveryDefaults in FlowError so a later failure is diagnosable: "DCR
// 403" against a synthesized /register means something very different from
// "DCR 403" against an advertised registration_endpoint.
func DefaultEndpoints(issuer *url.URL) *AuthServerMetadata {
	if issuer == nil {
		return nil
	}
	prefix := strings.TrimSuffix(issuer.String(), "/")
	return &AuthServerMetadata{
		Issuer:                      prefix,
		AuthorizationEndpoint:       prefix + "/authorize",
		TokenEndpoint:               prefix + "/token",
		RegistrationEndpoint:        prefix + "/register",
		DeviceAuthorizationEndpoint: "",
		SourceURL:                   defaultEndpointsSource,
	}
}

// defaultEndpointsSource marks metadata that was synthesized rather than
// fetched. Callers compare SourceURL against it to set DiscoveryDefaults.
const defaultEndpointsSource = "(default endpoints, no metadata document)"

// Attempt is one discovery candidate and what it answered.
//
// The URL alone was not enough to diagnose anything. A chain that tried four
// candidates and failed looked identical whether every one of them 404'd
// (the provider publishes nothing, so the issuer is wrong or the fallback is
// wanted), one parsed but was unusable (the provider is broken, and this is
// the URL to show them), or the first was refused by the SSRF screen (the
// rest were never tried at all, so the list reads as evidence of a search
// that did not happen).
//
// It is data rather than a log line for the reason scope.Step is: this
// package is a leaf with no logging dependency, so it reports what happened
// and lets its callers decide how to render it.
type Attempt struct {
	// URL is the candidate that was fetched.
	URL string
	// Outcome is one of the Attempt* constants.
	Outcome string
}

// Outcomes of one discovery candidate.
const (
	// AttemptOK: the document was fetched and is usable. The chain stops here.
	AttemptOK = "ok"
	// AttemptNoDocument: non-2xx or unparsable. NOT an error — providers
	// routinely 404 the candidate forms they do not implement, which is why
	// the chain moves on rather than failing.
	AttemptNoDocument = "no document"
	// AttemptUnusable: the document parsed but lacks the members a flow needs.
	// The chain STOPS: a broken provider is not a reason to keep guessing,
	// and the operator needs to see which URL was wrong.
	AttemptUnusable = "unusable document"
	// AttemptRefused: the SSRF screen (or another fatal condition) refused
	// this destination, so the chain aborts. Everything after it in the
	// candidate list was never tried — which is exactly what a bare URL list
	// could not say.
	AttemptRefused = "refused"
)

// attemptURLs renders attempts as their URLs, for the error messages that
// name what was tried.
func attemptURLs(attempts []Attempt) []string {
	out := make([]string, 0, len(attempts))
	for _, a := range attempts {
		out = append(out, a.URL)
	}
	return out
}

// DiscoveryResult is everything the discovery chain learned.
type DiscoveryResult struct {
	// Metadata is the authorization server metadata (never nil on success).
	Metadata *AuthServerMetadata
	// Protected is the RFC 9728 document, when the chain started from a
	// 401 or a resource URL. nil otherwise.
	Protected *ProtectedResourceMetadata
	// Resource is the RFC 8707 resource indicator to bind tokens to: the
	// canonical `resource` member of the protected-resource metadata when
	// present, else the caller-supplied resource URL. Empty means "do not
	// send a resource parameter".
	Resource string
	// Status records how the metadata was obtained.
	Status DiscoveryStatus
	// Attempted lists every candidate tried, in order, with what it
	// answered (see Attempt).
	Attempted []Attempt
	// ChallengeScope is the RFC 6750 §3 `scope` parameter from the 401 that
	// started this chain ("" = the server sent none). It is the first source
	// in the spec's scope selection order; see ScopeSet.
	ChallengeScope string
}

// ScopeSet returns the scope set to request, following MCP 2025-11-25's
// scope selection strategy:
//
//  1. the `scope` parameter from the 401's WWW-Authenticate challenge, which
//     the spec calls authoritative for the current request;
//  2. otherwise every scope in the PROTECTED RESOURCE metadata's
//     scopes_supported;
//  3. otherwise nothing — send no scope parameter at all.
//
// Step 2 reads the resource server's document, never the authorization
// server's. They answer different questions: the PRM says "what accessing ME
// requires", the AS metadata says "everything I can ever issue". Falling back
// to the latter when a resource publishes no PRM would silently request every
// privilege the provider offers — write and admin scopes included — for a
// resource that never asked for them. Requesting nothing is the fail-closed
// direction, and it is also what this package did before scope discovery
// existed, so a provider that works today keeps working.
func (r *DiscoveryResult) ScopeSet() []string {
	if r == nil {
		return nil
	}
	if s := strings.Fields(r.ChallengeScope); len(s) > 0 {
		return s
	}
	if r.Protected != nil && len(r.Protected.ScopesSupported) > 0 {
		return append([]string(nil), r.Protected.ScopesSupported...)
	}
	return nil
}

// Discoverer runs the discovery chain.
type Discoverer struct {
	client *Client
	// AllowDefaultEndpoints enables the DefaultEndpoints fallback when
	// every metadata candidate fails. Default true (set by NewDiscoverer):
	// refusing to log in to a provider that simply has no metadata
	// document is worse than proceeding with a recorded, diagnosable
	// assumption.
	AllowDefaultEndpoints bool
}

// NewDiscoverer builds a Discoverer over client.
func NewDiscoverer(client *Client) *Discoverer {
	return &Discoverer{client: client, AllowDefaultEndpoints: true}
}

// DiscoverFromIssuer walks MetadataCandidates in order and returns the
// first document that parses.
//
// Failure direction: a candidate that answers non-2xx or unparsable JSON is
// skipped (providers 404 the forms they do not implement). A candidate
// refused by the SSRF screen aborts the whole chain immediately — that is a
// security decision, not a "try the next one" condition, and continuing
// would just probe more private URLs.
func (d *Discoverer) DiscoverFromIssuer(ctx context.Context, issuer string) (*DiscoveryResult, error) {
	u, err := parseAbsoluteURL(issuer)
	if err != nil {
		return nil, err
	}
	res := &DiscoveryResult{Status: DiscoveryFailed}
	md, attempted, err := d.fetchMetadata(ctx, u)
	res.Attempted = attempted
	if err != nil {
		return nil, err
	}
	if md == nil {
		e := newFlowError(ErrorTypeDiscovery,
			fmt.Errorf("%w: tried %s", ErrDiscovery, strings.Join(attemptURLs(attempted), ", ")))
		e.Issuer = issuer
		e.Discovery = DiscoveryFailed
		e.Attempted = attempted
		return nil, e
	}
	res.Metadata = md
	res.Status = DiscoveryOK
	if md.SourceURL == defaultEndpointsSource {
		res.Status = DiscoveryDefaults
	}
	return res, nil
}

// DiscoverFromResource performs the full RFC 9728 → RFC 8414 chain: fetch
// the protected-resource metadata (from an explicit URL, e.g. one taken out
// of a 401's WWW-Authenticate header, or from the candidates derived from
// resourceURL), then discover the first advertised authorization server.
//
// resourceMetadataURL may be empty; resourceURL must not be.
//
// When no protected-resource document exists at all, the chain makes one last
// attempt against the resource server's own origin
// (fetchResourceOriginMetadata) before failing — some providers publish their
// per-resource metadata only there.
func (d *Discoverer) DiscoverFromResource(ctx context.Context, resourceURL, resourceMetadataURL string) (*DiscoveryResult, error) {
	ru, err := parseAbsoluteURL(resourceURL)
	if err != nil {
		return nil, err
	}
	candidates := ProtectedResourceCandidates(ru)
	if resourceMetadataURL != "" {
		// The advertised URL wins, but it is still screened by checkURL
		// like any other destination: it arrives from an unauthenticated
		// 401 response and is therefore attacker-influenceable.
		candidates = append([]string{resourceMetadataURL}, candidates...)
	}
	res := &DiscoveryResult{Status: DiscoveryFailed, Resource: canonicalResource(ru)}
	var prm *ProtectedResourceMetadata
	for _, c := range candidates {
		var doc ProtectedResourceMetadata
		err := d.client.getJSON(ctx, c, &doc)
		if err == nil {
			doc.SourceURL = c
			res.Attempted = append(res.Attempted, Attempt{URL: c, Outcome: AttemptOK})
			prm = &doc
			break
		}
		if isFatalDiscoveryError(err) {
			res.Attempted = append(res.Attempted, Attempt{URL: c, Outcome: AttemptRefused})
			return nil, err
		}
		res.Attempted = append(res.Attempted, Attempt{URL: c, Outcome: AttemptNoDocument})
	}
	if prm == nil {
		// No RFC 9728 document. Before giving up, look for AS metadata on the
		// resource server's OWN origin: providers exist that publish a
		// per-resource metadata document there and nothing at all under the
		// issuer's origin for that resource. Without this hop such a server is
		// undiscoverable — the correct endpoints are published and reachable,
		// but no candidate list we build from the issuer ever names them.
		md, attempted, err := d.fetchResourceOriginMetadata(ctx, ru)
		res.Attempted = append(res.Attempted, attempted...)
		if err != nil {
			return nil, err
		}
		if md != nil {
			res.Metadata = md
			res.Status = DiscoveryResourceOrigin
			return res, nil
		}
		e := newFlowError(ErrorTypeDiscovery,
			fmt.Errorf("%w: no protected-resource metadata; tried %s", ErrDiscovery,
				strings.Join(attemptURLs(res.Attempted), ", ")))
		e.Discovery = DiscoveryFailed
		e.Attempted = res.Attempted
		return nil, e
	}
	res.Protected = prm
	res.Status = DiscoveryProtected
	if strings.TrimSpace(prm.Resource) != "" {
		res.Resource = prm.Resource
	}
	if len(prm.AuthorizationServers) == 0 {
		e := newFlowError(ErrorTypeDiscovery,
			fmt.Errorf("%w: %s lists no authorization_servers", ErrDiscovery, prm.SourceURL))
		e.Discovery = DiscoveryProtected
		// The walk that reached this document is what says whether SourceURL
		// was the URL the 401 advertised or one of the forms guessed after it
		// — the difference between a provider publishing a broken document
		// and our having found a stale one it never pointed at.
		e.Attempted = res.Attempted
		return nil, e
	}
	// First advertised AS wins. Trying them all would multiply the number
	// of endpoints a hostile resource server can make us contact.
	issuer, err := parseAbsoluteURL(prm.AuthorizationServers[0])
	if err != nil {
		return nil, err
	}
	md, attempted, err := d.fetchMetadata(ctx, issuer)
	res.Attempted = append(res.Attempted, attempted...)
	if err != nil {
		return nil, err
	}
	if md == nil {
		e := newFlowError(ErrorTypeDiscovery,
			fmt.Errorf("%w: tried %s", ErrDiscovery, strings.Join(attemptURLs(attempted), ", ")))
		e.Issuer = prm.AuthorizationServers[0]
		e.Discovery = DiscoveryFailed
		e.Attempted = res.Attempted
		return nil, e
	}
	res.Metadata = md
	res.Status = DiscoveryOK
	if md.SourceURL == defaultEndpointsSource {
		res.Status = DiscoveryDefaults
	}
	return res, nil
}

// fetchResourceOriginMetadata looks for RFC 8414 metadata published on the
// resource server's own origin, for resource servers that serve no RFC 9728
// document. Both the path-scoped and the origin-root forms are tried, since
// a per-resource document lives under the resource's path while a
// deployment-wide one sits at the root.
//
// This is off-spec on purpose and therefore bounded:
//
//   - It runs ONLY after the whole protected-resource chain came up empty, so
//     it can never override a document the resource server did advertise.
//   - It never synthesizes endpoints. DefaultEndpoints is deliberately not
//     consulted here: guessing /authorize and /token on the resource server's
//     own origin would send the user's browser somewhere no provider ever
//     named, and every real instance of this shape publishes a document.
//   - The result is recorded as DiscoveryResourceOrigin, never DiscoveryOK.
//     The `issuer` member is unverified — the document came from a host the
//     spec does not designate as its publisher.
//
// Failure direction: fail-soft on absence (returns nil so the caller reports
// the original "no protected-resource metadata" error), fatal on an SSRF
// refusal, exactly like fetchMetadata.
func (d *Discoverer) fetchResourceOriginMetadata(ctx context.Context, resource *url.URL) (*AuthServerMetadata, []Attempt, error) {
	origin := &url.URL{Scheme: resource.Scheme, Host: resource.Host}
	candidates := MetadataCandidates(resource)
	if path := strings.Trim(resource.EscapedPath(), "/"); path != "" {
		candidates = append(candidates, MetadataCandidates(origin)...)
	}
	var attempted []Attempt
	record := func(url, outcome string) { attempted = append(attempted, Attempt{URL: url, Outcome: outcome}) }
	for _, c := range candidates {
		var md AuthServerMetadata
		err := d.client.getJSON(ctx, c, &md)
		if err == nil {
			md.SourceURL = c
			if verr := validateMetadata(&md); verr != nil {
				// Unlike fetchMetadata, an unusable document here is skipped
				// rather than fatal: this whole hop is a fallback over paths
				// the spec does not reserve, so a 200 that is not metadata is
				// an ordinary miss, not a broken provider.
				//
				// Recorded as unusable all the same. The chain continuing is
				// what the outcome does NOT say — that is the loop's business
				// — while "this URL answered 200 with something that was not
				// metadata" is exactly what a reader of the trace wants.
				record(c, AttemptUnusable)
				continue
			}
			record(c, AttemptOK)
			return &md, attempted, nil
		}
		if isFatalDiscoveryError(err) {
			record(c, AttemptRefused)
			return nil, attempted, err
		}
		record(c, AttemptNoDocument)
	}
	return nil, attempted, nil
}

// fetchMetadata walks the candidate list. It returns (nil, attempted, nil)
// when every candidate was absent — an outcome, not an error — and a
// non-nil error only for conditions that must abort the whole flow.
func (d *Discoverer) fetchMetadata(ctx context.Context, issuer *url.URL) (*AuthServerMetadata, []Attempt, error) {
	var attempted []Attempt
	record := func(url, outcome string) { attempted = append(attempted, Attempt{URL: url, Outcome: outcome}) }
	for _, c := range MetadataCandidates(issuer) {
		var md AuthServerMetadata
		err := d.client.getJSON(ctx, c, &md)
		if err == nil {
			md.SourceURL = c
			if err := validateMetadata(&md); err != nil {
				// A document that parses but is unusable is not a reason
				// to try the next candidate silently — it is a broken
				// provider and the operator needs to see it.
				record(c, AttemptUnusable)
				e := newFlowError(ErrorTypeDiscovery, err)
				e.Issuer = issuer.String()
				e.Discovery = DiscoveryFailed
				e.Attempted = attempted
				return nil, attempted, e
			}
			if err := validateIssuerMatch(&md, issuer); err != nil {
				// Same fatality as an unusable document, and for a stronger
				// reason: the next candidate is on the same host, so trying
				// it would only ask the same liar again.
				record(c, AttemptUnusable)
				e := newFlowError(ErrorTypeDiscovery, err)
				e.Issuer = issuer.String()
				e.Discovery = DiscoveryFailed
				e.Attempted = attempted
				e.Suggestion = "the authorization server's metadata claims an issuer other than the one it was fetched from; nothing was sent to it"
				return nil, attempted, e
			}
			record(c, AttemptOK)
			return &md, attempted, nil
		}
		if isFatalDiscoveryError(err) {
			// Refused, not absent — and the candidates after this one were
			// never reached, which is the distinction the outcome carries.
			record(c, AttemptRefused)
			return nil, attempted, err
		}
		record(c, AttemptNoDocument)
	}
	if d.AllowDefaultEndpoints {
		return DefaultEndpoints(issuer), attempted, nil
	}
	return nil, attempted, nil
}

// validateIssuerMatch enforces RFC 8414 §3.3 / OIDC Discovery §4.3: the
// `issuer` a metadata document declares MUST be identical to the issuer
// identifier the well-known URL was built from, and a document that fails
// it MUST NOT be used.
//
// This is the check the whole mix-up defence rests on, and its absence is
// invisible because everything downstream still looks like it is working.
// RFC 9207 has the client compare the authorization response's `iss` against
// the discovered issuer — but if the discovered issuer is whatever the
// fetched document said, then a host that serves both the document and the
// response satisfies that comparison against itself. The spec says as much:
// recording the issuer "provides no protection if the expected issuer was
// obtained from an unvalidated source". Validating here is what makes the
// later comparison mean anything.
//
// Comparison is NORMALISED, not exact, and that is a decision this file
// inherits rather than makes: docs/modules/oauth.md argued it out before the
// check existed. RFC 8414 says "identical", but a declared issuer differing
// only by a trailing slash or by host case is ordinary real-world sloppiness,
// and exact equality turns each instance into a provider that stops working.
// So: lower-case the host (DNS is case-insensitive, so this hands an attacker
// nothing), drop a single trailing slash, then require scheme, host and path
// to match exactly. Scheme is NOT normalised — http where https was expected
// is a downgrade, not sloppiness — and neither is the path, which is
// case-sensitive and names the tenant.
//
// A value that will not parse as an absolute URL is refused outright: it
// cannot be normalised, and comparing it as raw text is how a lax fallback
// becomes the hole the check was for.
func validateIssuerMatch(md *AuthServerMetadata, issuer *url.URL) error {
	if strings.TrimSpace(md.Issuer) == "" {
		return fmt.Errorf("oauthflow: %s declares no issuer; RFC 8414 requires it to match %s",
			md.SourceURL, issuer.String())
	}
	declared, err := parseAbsoluteURL(md.Issuer)
	if err != nil {
		return fmt.Errorf("oauthflow: %s declares issuer %q, which is not an absolute URL",
			md.SourceURL, md.Issuer)
	}
	if issuerKey(declared) == issuerKey(issuer) {
		return nil
	}
	return fmt.Errorf("oauthflow: %s declares issuer %q but was fetched as %q; refusing to use it",
		md.SourceURL, md.Issuer, issuer.String())
}

// issuerKey renders an issuer identifier for comparison: scheme and path as
// written, host lower-cased, one trailing slash removed.
func issuerKey(u *url.URL) string {
	return u.Scheme + "://" + strings.ToLower(u.Host) + strings.TrimSuffix(u.EscapedPath(), "/")
}

// validateMetadata enforces the two members without which no flow can run.
func validateMetadata(md *AuthServerMetadata) error {
	if strings.TrimSpace(md.AuthorizationEndpoint) == "" && strings.TrimSpace(md.DeviceAuthorizationEndpoint) == "" {
		return fmt.Errorf("oauthflow: %s has neither authorization_endpoint nor device_authorization_endpoint", md.SourceURL)
	}
	if strings.TrimSpace(md.TokenEndpoint) == "" {
		return fmt.Errorf("oauthflow: %s has no token_endpoint", md.SourceURL)
	}
	return nil
}

// isFatalDiscoveryError reports whether an error must abort the candidate
// walk instead of advancing to the next candidate. SSRF refusals and
// insecure-transport refusals are fatal (the next candidate has the same
// host, so it would just be refused again — loudly, N times), and so is a
// cancelled or expired context: the caller asked for the walk to end, and
// trying the next candidate would spend a request answering a question
// nobody is waiting for. What is NOT fatal, and is the reason a walk exists
// at all, is a candidate that is merely absent — an HTTP status error or a
// body that is not a metadata document.
func isFatalDiscoveryError(err error) bool {
	if errors.Is(err, ErrBlocked) || errors.Is(err, ErrInsecureTransport) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var se *statusError
	return !errors.As(err, &se) && !isDecodeError(err)
}

// isDecodeError reports a JSON-shaped discovery failure (candidate exists
// but is not a metadata document) — skippable, like a 404.
func isDecodeError(err error) bool {
	var fe *FlowError
	if errors.As(err, &fe) {
		return fe.Type == ErrorTypeDiscovery
	}
	return false
}

// parseAbsoluteURL parses and requires scheme+host.
func parseAbsoluteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, newFlowError(ErrorTypeDiscovery, fmt.Errorf("oauthflow: bad url %q: %w", raw, err))
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, newFlowError(ErrorTypeDiscovery, fmt.Errorf("oauthflow: url %q is not absolute", raw))
	}
	return u, nil
}

// canonicalResource renders the RFC 8707 resource indicator for a URL:
// scheme://host[:port]/path with no query and no fragment, trailing slash
// removed. RFC 8707 requires an absolute URI without a fragment.
//
// Host is lower-cased because the canonical form MCP prescribes uses
// lowercase scheme and host, and url.Parse folds only the scheme — a server
// configured as https://MCP.Example.com would otherwise put a non-canonical
// value in front of an authorization server that compares it literally. The
// PATH is left alone: it is case-sensitive, and folding it would name a
// different resource.
func canonicalResource(u *url.URL) string {
	c := &url.URL{Scheme: u.Scheme, Host: strings.ToLower(u.Host), Path: u.Path}
	s := c.String()
	if len(s) > 1 && strings.HasSuffix(s, "/") {
		s = strings.TrimSuffix(s, "/")
	}
	return s
}

// ResourceMetadataURLFromResponse pulls the RFC 9728 `resource_metadata`
// pointer out of a 401 response's WWW-Authenticate header. This is how an
// MCP resource server tells an unauthenticated client where to start the
// discovery chain (docs/modules/oauth.md).
//
// The value is attacker-influenceable — it arrives on an unauthenticated
// response — so it is a HINT, not an instruction: it is screened by
// Client.checkURL like every other destination before it is fetched, and
// DiscoverFromResource still falls back to the candidates derived from the
// resource URL if it does not answer.
func ResourceMetadataURLFromResponse(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	return ResourceMetadataURL(resp.Header.Values(headerWWWAuthenticate))
}

// ResourceMetadataURL extracts the RFC 9728 `resource_metadata` parameter
// from a WWW-Authenticate challenge, as sent by an MCP resource server in
// its 401. It returns "" when absent.
//
// The header grammar (RFC 9110 §11.6.1) allows several comma-separated
// challenges, each with comma-separated auth-params whose values may be
// tokens or quoted strings — which means a naive strings.Split on "," is
// wrong for any value containing a comma, and quoted URLs do contain them.
// This is a small purpose-built scanner over that grammar.
func ResourceMetadataURL(headerValues []string) string {
	for _, h := range headerValues {
		if v := scanAuthParam(h, paramResourceMetadataKey); v != "" {
			return v
		}
	}
	return ""
}

// scanAuthParam finds `name=value` (bare or quoted) anywhere in a
// WWW-Authenticate value. It scans character by character so a comma or an
// equals sign inside a quoted string cannot terminate a value early.
func scanAuthParam(h, name string) string {
	i := 0
	for i < len(h) {
		// Skip separators and whitespace.
		for i < len(h) && (h[i] == ',' || h[i] == ' ' || h[i] == '\t') {
			i++
		}
		start := i
		for i < len(h) && h[i] != '=' && h[i] != ',' && h[i] != ' ' && h[i] != '\t' {
			i++
		}
		key := h[start:i]
		// Skip optional whitespace before '='.
		j := i
		for j < len(h) && (h[j] == ' ' || h[j] == '\t') {
			j++
		}
		if j >= len(h) || h[j] != '=' {
			// A bare token: an auth-scheme name ("Bearer") or a param with
			// no value. Move on.
			i = j
			continue
		}
		i = j + 1
		for i < len(h) && (h[i] == ' ' || h[i] == '\t') {
			i++
		}
		var val string
		if i < len(h) && h[i] == '"' {
			i++
			var b strings.Builder
			for i < len(h) && h[i] != '"' {
				if h[i] == '\\' && i+1 < len(h) {
					i++
				}
				b.WriteByte(h[i])
				i++
			}
			if i < len(h) {
				i++ // closing quote
			}
			val = b.String()
		} else {
			s := i
			for i < len(h) && h[i] != ',' && h[i] != ' ' && h[i] != '\t' {
				i++
			}
			val = h[s:i]
		}
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.TrimSpace(val)
		}
	}
	return ""
}

// ProbeResourceMetadataURL asks the resource server where its
// protected-resource metadata lives, by making one unauthenticated request
// and reading the RFC 9728 `resource_metadata` parameter out of the 401's
// WWW-Authenticate header.
//
// Without this, discovery can only GUESS the document's location from a
// candidate list, and a server that publishes it anywhere unconventional is
// simply unreachable — the server told us the answer and we never listened.
//
// Failure direction: fail-soft, deliberately. Every failure — the server is
// down, it answers 200, it omits the parameter, the URL is screened out —
// returns "" so discovery falls back to the candidate list it would have
// used anyway. The pointer arrives on an UNAUTHENTICATED response and is
// therefore attacker-influenceable, so it never bypasses checkURL: it is
// prepended to the candidates and screened like any other destination.
func (d *Discoverer) ProbeResourceMetadataURL(ctx context.Context, resourceURL string) string {
	c, _ := d.ProbeChallenge(ctx, resourceURL)
	return c
}

// ProbeChallenge makes the one unauthenticated request and returns BOTH
// RFC 9728 hints the 401 carries: the `resource_metadata` pointer and the
// RFC 6750 §3 `scope` parameter.
//
// They come from the same response on purpose. Asking twice would double the
// unauthenticated requests for two values the server already sent together,
// and the scope challenge is the FIRST source in the spec's scope selection
// order — dropping it, as this package did, made that order start at step 2.
//
// Failure direction: fail-soft in both components, independently. A missing
// or unparsable value yields "" and the caller falls back exactly as it did
// before. Both arrive on an UNAUTHENTICATED response and are therefore
// attacker-influenceable: the URL is screened by checkURL before any fetch,
// and the scope string is only ever echoed back to the authorization server,
// which applies its own policy to it.
func (d *Discoverer) ProbeChallenge(ctx context.Context, resourceURL string) (metadataURL, scope string) {
	u, err := parseAbsoluteURL(resourceURL)
	if err != nil {
		return "", ""
	}
	if err := d.client.checkURL(u); err != nil {
		return "", ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("User-Agent", d.client.cfg.UserAgent)
	req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)

	resp, err := d.client.discovery.Do(req)
	if err != nil {
		return "", ""
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return ResourceMetadataURLFromResponse(resp), ChallengeScope(resp.Header.Values(headerWWWAuthenticate))
}

// ChallengeScope extracts the RFC 6750 §3 `scope` parameter from a
// WWW-Authenticate challenge. It returns "" when absent.
//
// MCP 2025-11-25 makes this the FIRST source for the scope set, ahead of the
// protected-resource document's scopes_supported, and says clients "MUST
// treat the scopes provided in the challenge as authoritative for satisfying
// the current request" — with no assumable set relationship to
// scopes_supported. So this value is used verbatim, never intersected with
// or subtracted from anything else we discovered.
func ChallengeScope(headerValues []string) string {
	for _, h := range headerValues {
		if v := scanAuthParam(h, paramScopeKey); v != "" {
			return v
		}
	}
	return ""
}
