package oauthflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/dinstein/agent-hub/internal/guard/netguard"
)

// Default bounds for every outbound OAuth request.
const (
	// DefaultHTTPTimeout bounds one metadata/registration/token request.
	DefaultHTTPTimeout = 30 * time.Second
	// maxBodyBytes bounds a metadata or token response body. Authorization
	// server documents are kilobytes; anything past this is either a
	// misrouted endpoint or an attempt to make the client chew memory.
	maxBodyBytes = 1 << 20 // 1 MiB
	// maxDiscoveryRedirects bounds redirect following for the *metadata*
	// GETs only. Credential POSTs follow zero (see credential client).
	maxDiscoveryRedirects = 3
)

// Config configures the HTTP face of a flow.
type Config struct {
	// AllowLoopback permits http:// and loopback destinations. Off by
	// default: an OAuth endpoint on 127.0.0.1 is normally a
	// misconfiguration or an SSRF probe. When on, ONLY literal loopback
	// addresses and the RFC 6761 localhost tree are permitted — RFC1918,
	// CGNAT and link-local stay blocked, and no hostname's DNS answer can
	// unlock the exception.
	AllowLoopback bool
	// Timeout bounds a single request. <= 0 uses DefaultHTTPTimeout.
	Timeout time.Duration
	// UserAgent is sent on every request. Empty uses defaultUserAgent.
	UserAgent string
	// Transport overrides the base transport. Tests use this only to reach
	// an httptest server through a custom dialer; production leaves it nil
	// so the netguard-screened transport is used.
	Transport http.RoundTripper
}

const defaultUserAgent = "agenthub-oauth/1"

// Client performs the HTTP half of an OAuth flow under the SSRF and
// redirect rules described in the package doc.
//
// It holds two http.Clients on purpose:
//
//   - discovery, which may follow a bounded number of redirects, each hop
//     re-screened by checkURL. Metadata documents are public and providers
//     do relocate them.
//   - credential, which follows ZERO redirects. Requests to the token and
//     registration endpoints carry a code_verifier, a refresh token or a
//     client secret; a 302 on one of those is an exfiltration primitive.
type Client struct {
	cfg        Config
	discovery  *http.Client
	credential *http.Client
}

// NewClient builds a Client. It never fails; misconfiguration surfaces at
// request time as a typed *FlowError.
func NewClient(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultHTTPTimeout
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	c := &Client{cfg: cfg}
	base := cfg.Transport
	if base == nil {
		base = c.newTransport()
	}
	c.discovery = &http.Client{
		Timeout:   cfg.Timeout,
		Transport: base,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxDiscoveryRedirects {
				return fmt.Errorf("oauthflow: too many redirects (%d)", len(via))
			}
			// Re-screen every hop: the first URL being safe says nothing
			// about where a redirect points.
			return c.checkURL(req.URL)
		},
	}
	c.credential = &http.Client{
		Timeout:   cfg.Timeout,
		Transport: base,
		// Zero redirects. ErrUseLastResponse stops the client from
		// following; postForm then rejects the 3xx as an error rather than
		// handing a redirect body to a JSON decoder.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return c
}

// WithLoopback returns a copy of this client with the loopback carve-out on,
// keeping every other setting.
//
// It exists because the carve-out is a PER-SERVER decision — a self-hosted
// provider the operator marked `provenance: local` — while a Client is built
// once and shared by every server a process renews. Building a second one
// from scratch at the call site would silently drop the timeout, user agent
// and transport the original was configured with, and the drop would be
// invisible until the day one of them mattered.
func (c *Client) WithLoopback() *Client {
	cfg := c.cfg
	cfg.AllowLoopback = true
	return NewClient(cfg)
}

// newTransport builds the screened transport: netguard.DialControl runs on
// the ACTUAL resolved address, so a hostname that passed checkURL and then
// flipped to a private answer is still refused.
func (c *Client) newTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	// The dialer is built per dial rather than once, because the carve-out
	// below is decided from the address the TRANSPORT WAS ASKED FOR — which
	// only this closure sees — and not from what that address resolved to.
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		d := &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Control:   c.dialControlFor(address),
		}
		return d.DialContext(ctx, network, address)
	}
	// A screened dialer is worthless if a pooled connection to a
	// previously-public address is reused after a rebind; short idle
	// lifetimes keep the screen close to the traffic.
	t.IdleConnTimeout = 30 * time.Second
	return t
}

// dialControlFor builds the net.Dialer.Control hook for ONE dial.
//
// `requested` is what http.Transport asked for: `host:port` taken from the
// URL, before any name resolution. The Control hook it returns sees the
// resolved address instead, and that difference is the whole point.
//
// The AllowLoopback carve-out needs BOTH to hold, and neither alone:
//
//   - the requested host is certainly loopback (`isLiteralLoopbackHost`, an
//     IP literal or the RFC 6761 localhost tree — never a name whose answer
//     comes from DNS), and
//   - the address actually being dialed is a loopback literal.
//
// Deciding it from the resolved address alone reopened the switch to DNS:
// a hostname that passed checkURL as public and then answered 127.0.0.1 was
// dialed without ever consulting netguard, delivering the discovery GET —
// or a postForm carrying code_verifier or a refresh token — to a service on
// this host's loopback interface. That contradicts what the switch says
// about itself: no hostname's DNS answer can unlock the exception.
// isLiteralLoopbackHost was written for exactly this and was never on the
// dial path.
//
// Requiring the second condition as well keeps a poisoned `localhost` (a
// name in the carve-out that answers with something else) on the
// netguard.DialControl branch.
func (c *Client) dialControlFor(requested string) func(string, string, syscall.RawConn) error {
	allow := c.cfg.AllowLoopback && isLiteralLoopbackHost(hostOnly(requested))
	return func(network, address string, rc syscall.RawConn) error {
		if allow && isLoopbackAddress(address) {
			return nil
		}
		return netguard.DialControl(network, address, rc)
	}
}

// hostOnly strips a port and brackets from a dial address. An address
// without a port is already a host.
func hostOnly(address string) string {
	host := address
	if h, _, err := net.SplitHostPort(address); err == nil {
		host = h
	}
	return strings.Trim(host, "[]")
}

// isLoopbackAddress reports whether a dial address is a loopback literal.
// Fail-to-false: anything unparsable is not loopback, and goes to netguard.
func isLoopbackAddress(address string) bool {
	a, err := netip.ParseAddr(hostOnly(address))
	return err == nil && a.Unmap().IsLoopback()
}

// checkURL screens a destination before the request is made.
//
// Failure direction: FAIL-CLOSED at every branch. An unparsable URL, a
// missing host, a non-https scheme outside the loopback carve-out, or
// netguard.HostIsPrivate answering true (which it also does for
// unresolvable names) all refuse.
func (c *Client) checkURL(u *url.URL) error {
	if u == nil {
		return newFlowError(ErrorTypeBlocked, fmt.Errorf("%w: empty destination", ErrBlocked))
	}
	// Scheme is checked before host so a file:// or data: URL reports what
	// is actually wrong with it rather than "empty host".
	loopback := c.cfg.AllowLoopback && isLiteralLoopbackHost(u.Hostname())
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback {
			e := newFlowError(ErrorTypeTransport,
				fmt.Errorf("%w: %s://%s is not https", ErrInsecureTransport, u.Scheme, u.Host))
			return e
		}
	default:
		return newFlowError(ErrorTypeTransport,
			fmt.Errorf("%w: unsupported scheme %q", ErrInsecureTransport, u.Scheme))
	}
	if u.Host == "" {
		return newFlowError(ErrorTypeBlocked, fmt.Errorf("%w: empty destination", ErrBlocked))
	}
	if loopback {
		return nil
	}
	if netguard.HostIsPrivate(u.Host) {
		return newFlowError(ErrorTypeBlocked,
			fmt.Errorf("%w: %s resolves to a private or unresolvable address", ErrBlocked, u.Host))
	}
	return nil
}

// isLiteralLoopbackHost reports whether host is certainly loopback.
//
// Failure direction: FAIL-TO-FALSE, and deliberately narrower than
// netguard.HostIsDefinitelyPrivate — that predicate also answers true for
// RFC1918 and link-local, which the AllowLoopback switch does NOT unlock.
// Only IP literals and the RFC 6761 localhost tree (reserved for loopback
// by spec, so no zone owner can point it elsewhere) qualify; every other
// name would need DNS, and a DNS answer can be changed at will.
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

// getJSON fetches a JSON document with the discovery client. A non-2xx
// status returns errStatus so discovery can distinguish "this candidate is
// absent" from "the network is broken".
func (c *Client) getJSON(ctx context.Context, rawURL string, out any) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return newFlowError(ErrorTypeTransport, fmt.Errorf("oauthflow: bad url %q: %w", rawURL, err))
	}
	if err := c.checkURL(u); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return newFlowError(ErrorTypeTransport, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	// MCP authorization requires the protocol version header on metadata
	// requests to multiplexed servers; harmless everywhere else.
	req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)

	resp, err := c.discovery.Do(req)
	if err != nil {
		return classifyTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readBounded(resp.Body)
	if err != nil {
		return newFlowError(ErrorTypeTransport, err)
	}
	if resp.StatusCode/100 != 2 {
		return &statusError{Status: resp.StatusCode, URL: rawURL, Header: resp.Header, Body: body}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return newFlowError(ErrorTypeDiscovery, fmt.Errorf("oauthflow: %s: invalid JSON: %w", rawURL, err))
	}
	return nil
}

// mcpProtocolVersion is the MCP revision agenthub speaks (docs/conventions.md 5b).
const mcpProtocolVersion = "2025-11-25"

// postForm performs a credential-bearing POST with ZERO redirects.
//
// Invariant: any 3xx is an error. The credential client is configured with
// http.ErrUseLastResponse so the redirect is not followed; this function
// then refuses to interpret the response at all. Treating the 3xx as a
// "failed request" instead of an error would be nearly as bad — the caller
// would retry against the same endpoint forever.
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values, hdr http.Header) ([]byte, error) {
	resp, err := c.postCredential(ctx, endpoint, "application/x-www-form-urlencoded", form.Encode(), hdr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if isRedirectStatus(resp.StatusCode) {
		loc := resp.Header.Get("Location")
		e := newFlowError(ErrorTypeTokenExchange,
			fmt.Errorf("%w: %s answered %d -> %s", ErrRedirect, endpoint, resp.StatusCode, redactLocation(loc)))
		e.Suggestion = "credential requests follow zero redirects; point the endpoint at its final URL"
		return nil, e
	}
	body, err := readBounded(resp.Body)
	if err != nil {
		return nil, newFlowError(ErrorTypeTransport, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, decodeTokenError(resp.StatusCode, body)
	}
	return body, nil
}

// postJSON is postForm's JSON-bodied sibling, used by dynamic client
// registration. Same zero-redirect rule.
func (c *Client) postJSON(ctx context.Context, endpoint string, payload any, hdr http.Header) ([]byte, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, newFlowError(ErrorTypeRegistration, err)
	}
	resp, err := c.postCredential(ctx, endpoint, "application/json", string(buf), hdr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if isRedirectStatus(resp.StatusCode) {
		e := newFlowError(ErrorTypeRegistration,
			fmt.Errorf("%w: %s answered %d", ErrRedirect, endpoint, resp.StatusCode))
		e.Suggestion = "credential requests follow zero redirects; point the endpoint at its final URL"
		return nil, e
	}
	body, err := readBounded(resp.Body)
	if err != nil {
		return nil, newFlowError(ErrorTypeTransport, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, &statusError{Status: resp.StatusCode, URL: endpoint, Header: resp.Header, Body: body}
	}
	return body, nil
}

// postCredential builds and sends one credential-bearing POST with the
// discipline every one of them owes: parse the endpoint, put it through
// checkURL (the SSRF screen), set the standard headers, add the caller's, and
// classify a transport failure. postForm and postJSON repeated all of it,
// which meant a third credential POST written by copying one of them could
// lose the screen without anything noticing.
//
// It deliberately stops at the response rather than also refusing the 3xx.
// Both callers refuse it, but with different errors — a token exchange
// redacts the Location it reports, because that header can echo back the
// credential we just sent, while a registration does not read it at all —
// and turning that into a parameter would make the zero-redirect rule read
// as something a caller chooses. It is not a choice.
func (c *Client) postCredential(ctx context.Context, endpoint, contentType, body string,
	hdr http.Header) (*http.Response, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, newFlowError(ErrorTypeTransport, fmt.Errorf("oauthflow: bad endpoint %q: %w", endpoint, err))
	}
	if err := c.checkURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(body))
	if err != nil {
		return nil, newFlowError(ErrorTypeTransport, err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.credential.Do(req)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	return resp, nil
}

func isRedirectStatus(code int) bool { return code >= 300 && code < 400 }

// redactLocation keeps scheme, host and path of a redirect target and drops
// the rest: a Location header on a credential request can echo back the
// query parameters we sent, and those are the credential.
func redactLocation(loc string) string {
	if loc == "" {
		return "(no Location)"
	}
	u, err := url.Parse(loc)
	if err != nil || u.Host == "" {
		return "(unparsable Location)"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

func readBounded(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBodyBytes {
		return nil, fmt.Errorf("oauthflow: response exceeds %d bytes", maxBodyBytes)
	}
	return b, nil
}

// statusError is a non-2xx response from a metadata or registration
// endpoint. Discovery treats it as "candidate absent" and moves on; the
// registrar surfaces it.
type statusError struct {
	Status int
	URL    string
	Header http.Header
	Body   []byte
}

func (e *statusError) Error() string {
	return fmt.Sprintf("oauthflow: %s answered HTTP %d", e.URL, e.Status)
}

// classifyTransportError turns a netguard dial rejection into a typed
// blocked FlowError so callers see ErrBlocked rather than a generic
// url.Error.
func classifyTransportError(err error) error {
	var blocked *netguard.BlockedError
	if errors.As(err, &blocked) {
		return newFlowError(ErrorTypeBlocked, fmt.Errorf("%w: %v", ErrBlocked, blocked))
	}
	// A FlowError raised inside CheckRedirect arrives wrapped in
	// *url.Error; unwrap so its type and suggestion survive.
	var flow *FlowError
	if errors.As(err, &flow) {
		return flow
	}
	// Everything else, context cancellation and deadlines included, is
	// transport: this package has no error type that separates "gave up
	// waiting" from "the request failed", and errors.Is on the wrapped cause
	// still tells a caller which it was.
	return newFlowError(ErrorTypeTransport, err)
}

// decodeTokenError parses an RFC 6749 §5.2 error body. A body that is not
// a recognizable error object still yields a *TokenError (with an empty
// Code) so callers have one type to handle.
func decodeTokenError(status int, body []byte) error {
	var raw struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
		URI         string `json:"error_uri"`
	}
	_ = json.Unmarshal(body, &raw)
	return &TokenError{
		Code:        raw.Error,
		Description: raw.Description,
		URI:         raw.URI,
		HTTPStatus:  status,
	}
}
