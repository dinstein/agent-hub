package downstream

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/dinstein/agent-hub/internal/guard/netguard"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
)

// headerAuthorization is the one header this package sets on behalf of the
// vault. Spelled once so the "an explicit header wins" rule below and the
// injection agree by construction.
const headerAuthorization = "Authorization"

// dialHTTP connects one HTTP downstream: expand the header placeholders,
// build the SSRF-screened + auth-injecting client, and hand it to the
// transport facade.
//
// The transport package is standard-library only and cannot import
// internal/guard/netguard (canonical.md §2 rule 2), so screening is
// injected from here — either as HTTPConfig.DialContext, or, when a
// credential is in play, inside the http.Client that also carries the
// bearer injection and the 401/403 retry.
func (d Deps) dialHTTP(ctx context.Context, spec Spec) (transport.Transport, error) {
	if strings.TrimSpace(spec.URL) == "" {
		return nil, fmt.Errorf("downstream %q: transport %q needs a url", spec.ID, spec.Kind)
	}
	if err := screenEndpoint(spec); err != nil {
		return nil, err
	}
	hdr, explicitAuth, err := d.buildHeader(ctx, spec)
	if err != nil {
		return nil, err
	}

	cfg := transport.HTTPConfig{
		URL:                spec.URL,
		Header:             hdr,
		NotificationStream: d.NotificationStream,
	}
	dial := d.DialContext
	if dial == nil {
		dial = dialContextFor(spec)
	}
	auth := d.authFor(spec)
	if auth != nil && !explicitAuth {
		// Client and DialContext are mutually exclusive in HTTPConfig (a
		// caller-supplied client owns its own screening), so the screened
		// dialer moves inside the client we build here — and with it the
		// redirect policy that keeps the credential on its own origin.
		endpoint, perr := url.Parse(strings.TrimSpace(spec.URL))
		if perr != nil {
			// screenEndpoint parsed this URL already, so a failure here is
			// not reachable through it. Refuse anyway rather than build a
			// credential-carrying client with no origin to compare against.
			return nil, fmt.Errorf("downstream %q: parse url: %w", spec.ID, perr)
		}
		cfg.Client = newAuthClient(dial, auth, endpoint)
	} else {
		cfg.DialContext = dial
	}

	switch spec.Kind {
	case transport.StreamableHTTP:
		return transport.DialStreamableHTTP(cfg)
	case transport.SSE:
		// DEPRECATED-UPSTREAM(http+sse, earliest-removal: deprecated
		// 2025-03-26) — read side only, kept so agenthub can attach to
		// servers that expose nothing else.
		return transport.DialHTTPSSE(ctx, cfg)
	default:
		return nil, fmt.Errorf("downstream %q: %q is not an http transport", spec.ID, spec.Kind)
	}
}

// buildHeader resolves the spec's headers against the vault and reports
// whether the operator set an Authorization header themselves.
//
// An explicit Authorization always wins over the vault credential: it is
// how a hand-pasted token (or a non-OAuth scheme like `Basic`) is
// configured, and silently overwriting it would make that configuration
// unusable with no diagnostic.
func (d Deps) buildHeader(ctx context.Context, spec Spec) (http.Header, bool, error) {
	resolved, err := expandSecretMap(ctx, spec.ID, spec.ScopeName, spec.Headers, d.Secrets)
	if err != nil {
		return nil, false, err
	}
	hdr := http.Header{}
	explicitAuth := false
	for k, v := range resolved {
		if strings.EqualFold(k, headerAuthorization) {
			explicitAuth = true
		}
		hdr.Set(k, v)
	}
	return hdr, explicitAuth, nil
}

// screenEndpoint refuses a URL before any connection is attempted.
//
// Failure direction: FAIL-CLOSED. This is the cheap hostname-level check
// that produces a readable error ("configure a public endpoint") instead of
// a dial failure; it is NOT the security boundary — DNS can flip between
// this check and the dial. dialContextFor is the boundary, because it
// screens the address the socket is actually about to connect to.
func screenEndpoint(spec Spec) error {
	u, err := url.Parse(strings.TrimSpace(spec.URL))
	if err != nil {
		return fmt.Errorf("downstream %q: parse url: %w", spec.ID, err)
	}
	if u.Host == "" {
		return fmt.Errorf("downstream %q: url %q has no host", spec.ID, spec.URL)
	}
	if spec.Provenance == ProvenanceLocal {
		if !isLiteralLoopbackHost(u.Hostname()) {
			return fmt.Errorf(
				"downstream %q: provenance %q only covers literal loopback endpoints, not %q",
				spec.ID, ProvenanceLocal, u.Host)
		}
		return nil
	}
	if netguard.HostIsPrivate(u.Host) {
		return fmt.Errorf(
			"downstream %q: %s resolves to a private or unresolvable address; mark it provenance=%q if it really is a local server",
			spec.ID, u.Host, ProvenanceLocal)
	}
	return nil
}

// dialContextFor returns the screened dialer for a spec: netguard.DialControl
// on the resolved address, plus the single loopback carve-out that
// ProvenanceLocal buys.
//
// The carve-out is deliberately narrower than "private": only literal
// loopback addresses pass. RFC1918, CGNAT and link-local stay blocked even
// for a local server, because those are the ranges cloud metadata services
// and intranet hosts live in.
func dialContextFor(spec Spec) transport.DialContextFunc {
	allowLoopback := spec.Provenance == ProvenanceLocal
	d := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, rc syscall.RawConn) error {
			if allowLoopback && isLiteralLoopbackAddr(address) {
				return nil
			}
			return netguard.DialControl(network, address, rc)
		},
	}
	return d.DialContext
}

// isLiteralLoopbackAddr reports whether a dial address (host:port, already
// resolved) is a literal loopback IP. Fail-to-false: anything unparsable
// falls through to netguard.
func isLiteralLoopbackAddr(address string) bool {
	host := address
	if h, _, err := net.SplitHostPort(address); err == nil {
		host = h
	}
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && a.Unmap().IsLoopback()
}

// isLiteralLoopbackHost reports whether a URL host is certainly loopback:
// an IP literal, or the RFC 6761 localhost tree (reserved for loopback by
// spec, so no zone owner can point it elsewhere). Hostnames are never
// resolved here — a DNS answer can deny trust but must never confer it.
func isLiteralLoopbackHost(host string) bool {
	h := strings.TrimSpace(strings.Trim(host, "[]"))
	if h == "" {
		return false
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Unmap().IsLoopback()
	}
	l := strings.ToLower(strings.TrimSuffix(h, "."))
	return l == "localhost" || strings.HasSuffix(l, ".localhost")
}

// sameOrigin reports whether u may carry the credential minted for base.
// Scheme and host (including port) must match exactly.
//
// Failure direction: FAIL-CLOSED. Anything unparsable, hostless, or merely
// different answers false, so the credential stays home. It is deliberately
// stricter than net/http's own redirect stripping, which keeps
// Authorization across a subdomain change and across an https→http
// downgrade; neither is a place this vault credential may go.
//
// transport.sameOrigin is the same predicate. It is duplicated rather than
// shared because internal/mcp is standard-library only (canonical.md §2
// rule 2) and this package must not reach into it for an unexported helper
// — and because the two are independent gates, which AGENTS.md says are
// never collapsed into one.
func sameOrigin(base, u *url.URL) bool {
	if base == nil || u == nil || base.Host == "" || u.Host == "" {
		return false
	}
	return strings.EqualFold(base.Scheme, u.Scheme) && strings.EqualFold(base.Host, u.Host)
}

// newAuthClient builds the http.Client the HTTP transports use when a
// credential is in play: the screened dialer at the bottom, the bearer
// injection and the one-shot 401/403 refresh on top, and a redirect policy
// that refuses to carry either off the configured origin.
//
// The redirect policy is load bearing, not hygiene. net/http strips
// Authorization when a redirect leaves the domain — but the header this
// package injects is not on the request when that stripping runs, because
// authRoundTripper sits BELOW the redirect loop and runs again for the
// redirected hop. Without this gate the stripping is undone by the very
// layer meant to protect it, and a downstream answering 3xx chooses where
// its own credential is delivered.
//
// No Client.Timeout is set, mirroring the transport facade: SSE streams are
// long-lived and every request already carries a bounding context.
func newAuthClient(dial transport.DialContextFunc, auth TokenSource, endpoint *url.URL) *http.Client {
	base := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 0, // SSE: headers may precede data by a lot
	}
	return &http.Client{
		Transport: newAuthRoundTripper(base, auth, endpoint),
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if !sameOrigin(endpoint, req.URL) {
				return fmt.Errorf(
					"downstream redirect to %s://%s refused: a credential-carrying request never leaves its configured origin (%s://%s)",
					req.URL.Scheme, req.URL.Host, endpoint.Scheme, endpoint.Host)
			}
			return nil
		},
	}
}
