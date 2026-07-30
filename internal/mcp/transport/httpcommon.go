package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// HTTP header and media-type names shared by both HTTP transports. The
// facade owns these strings; no other package spells them.
// Note: net/http canonicalises header names on the wire, so
// headerProtocolVersion goes out as "Mcp-Protocol-Version" rather than the
// spec's "MCP-Protocol-Version". Header names are case-insensitive
// (RFC 9110 §5.1) and the golden files pin the canonical form.
const (
	headerSessionID       = "Mcp-Session-Id"
	headerProtocolVersion = "MCP-Protocol-Version"
	headerLastEventID     = "Last-Event-ID"
	headerRetryAfter      = "Retry-After"
	headerAccept          = "Accept"
	headerContentType     = "Content-Type"
	// Required on every POST once 2026-07-28 is negotiated: the JSON-RPC
	// method, and the tool/resource/prompt name when the params carry one.
	headerMcpMethod = "Mcp-Method"
	headerMcpName   = "Mcp-Name"

	mediaJSON = "application/json"
	mediaSSE  = "text/event-stream"
)

// maxErrBodySnippet bounds how much of a non-2xx body is folded into the
// error message. Downstream servers put the useful part first, and an
// unbounded snippet would turn a hostile server into a memory amplifier.
const maxErrBodySnippet = 1 << 10

// peerReplyTimeout bounds the POST that carries a reverse-RPC answer back
// to the server. It is independent of any call context: the answer must
// still be delivered when the call that observed the stream is gone.
const peerReplyTimeout = 30 * time.Second

// maxPeerWorkers bounds concurrent reverse-RPC handlers per transport. A
// server that floods requests gets backpressure (the stream reader stalls)
// rather than an unbounded goroutine fan-out.
const maxPeerWorkers = 8

// Typed HTTP-layer failures, decidable with errors.Is through *Error.
var (
	// ErrEndpointMoved reports HTTP 410 Gone: the MCP endpoint is
	// permanently gone and the configured URL must be changed by a human.
	// It is NEVER retried and never resumed.
	ErrEndpointMoved = errors.New("mcp endpoint gone (HTTP 410): the server URL must be updated")

	// ErrSessionExpired reports HTTP 404 on a request that carried an
	// Mcp-Session-Id: the server dropped the session. The transport clears
	// its session id; recovering means a fresh initialize, which is the
	// caller's (internal/downstream) decision, not this package's.
	ErrSessionExpired = errors.New("mcp session expired (HTTP 404)")

	// ErrHTTPProtocol reports a server that answered with something the
	// MCP HTTP binding does not allow (wrong content type, missing
	// endpoint event, cross-origin endpoint redirect, ...).
	ErrHTTPProtocol = errors.New("mcp http protocol violation")
)

// DialContextFunc is the SSRF screening seam.
//
// internal/mcp is standard-library only (canonical.md §2 rule 2), so this
// package cannot import internal/guard/netguard. The dependency is
// inverted instead: the caller (internal/downstream) injects a dialer —
// typically (&net.Dialer{Control: netguard.DialControl}).DialContext,
// which screens the *resolved* address and so closes the DNS-rebind TOCTOU
// window — and this package dials only through it.
type DialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// HTTPConfig configures both read-side HTTP transports (streamable-http
// and legacy HTTP+SSE).
//
// Failure direction on SSRF: with neither DialContext nor Client set there
// is NO address screening whatsoever — that combination exists for tests
// and explicitly trusted loopback endpoints only. Production callers must
// inject one of the two.
type HTTPConfig struct {
	// URL is the MCP endpoint. For streamable-http every JSON-RPC message
	// is POSTed here; for legacy HTTP+SSE this is the GET stream URL and
	// the POST address comes from the server's endpoint event.
	URL string

	// Header carries caller-owned headers (Authorization, User-Agent, ...)
	// applied to every request. Protocol headers set by this package
	// (Accept, Content-Type, Mcp-Session-Id, MCP-Protocol-Version) are
	// applied afterwards and always win.
	Header http.Header

	// DialContext screens outbound connections (see DialContextFunc).
	// Mutually exclusive with Client.
	DialContext DialContextFunc

	// Client, when set, is used verbatim — the caller then owns SSRF
	// screening, redirect policy and timeouts. Mutually exclusive with
	// DialContext so a guarded dialer can never be silently discarded.
	Client *http.Client

	// MaxFrame bounds a single JSON-RPC message (request body, JSON
	// response body, or one SSE event payload). Zero means
	// mcp.MaxFrameSize (16 MiB).
	MaxFrame int

	// NotificationStream opens the optional server→client GET SSE stream
	// after a successful initialize (streamable-http only). Servers that
	// answer 405 are simply left alone.
	NotificationStream bool

	// DisableResume turns off best-effort Last-Event-ID resumption of a
	// POST stream that died before delivering its response.
	DisableResume bool

	// retryBase is the first backoff step of the notification-stream
	// reconnect loop; tests shorten it. Zero means defaultRetryBase.
	retryBase time.Duration
}

const (
	defaultRetryBase = 500 * time.Millisecond
	maxRetryBackoff  = 5 * time.Second
)

// httpBase is the state both HTTP transports share: endpoint, client,
// caller headers and the frame bound.
type httpBase struct {
	endpoint *url.URL
	client   *http.Client
	header   http.Header
	maxFrame int
}

// newHTTPBase validates cfg and builds the shared state. Configuration
// errors are ClassFatal: no retry can fix a bad URL.
func newHTTPBase(cfg HTTPConfig) (httpBase, error) {
	var b httpBase
	if cfg.Client != nil && cfg.DialContext != nil {
		return b, &Error{Class: ClassFatal, Err: fmt.Errorf(
			"http transport: Client and DialContext are mutually exclusive (a caller-supplied client owns its own SSRF screening)")}
	}
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil {
		return b, &Error{Class: ClassFatal, Err: fmt.Errorf("http transport: parse url: %w", err)}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return b, &Error{Class: ClassFatal, Err: fmt.Errorf("http transport: url scheme %q must be http or https", u.Scheme)}
	}
	if u.Host == "" {
		return b, &Error{Class: ClassFatal, Err: fmt.Errorf("http transport: url %q has no host", cfg.URL)}
	}
	b.endpoint = u
	b.maxFrame = cfg.MaxFrame
	if b.maxFrame <= 0 {
		b.maxFrame = mcp.MaxFrameSize
	}
	b.header = cfg.Header.Clone()
	if b.header == nil {
		b.header = http.Header{}
	}
	b.client = cfg.Client
	if b.client == nil {
		b.client = newHTTPClient(cfg.DialContext)
	}
	return b, nil
}

// newHTTPClient builds the default client. It deliberately sets no
// Client.Timeout: SSE streams are long-lived and every request already
// carries a context that bounds it.
func newHTTPClient(dial DialContextFunc) *http.Client {
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dial,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          8,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			ResponseHeaderTimeout: 0, // SSE: headers may precede data by a lot
		},
	}
}

// newRequest builds a request with the caller headers applied. Protocol
// headers are set by the caller of this helper, after it returns, so they
// always override caller-supplied ones.
func (b *httpBase) newRequest(ctx context.Context, method string, u *url.URL, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		// *bytes.Reader gives http.NewRequest an exact ContentLength and a
		// GetBody, so a 307/308 replay never silently drops the payload.
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, err
	}
	for k, vs := range b.header {
		for i, v := range vs {
			if i == 0 {
				req.Header.Set(k, v)
			} else {
				req.Header.Add(k, v)
			}
		}
	}
	return req, nil
}

// httpError converts a non-2xx response into a typed transport error and
// drains+closes the body so the connection can be reused.
//
// Classification — tools/call is not idempotent, so
// only "provably never reached the server" and 429 may be ClassRetry:
//
//	410 → ErrEndpointMoved, ClassUnavailable, never retried, never resumed
//	404 → ErrSessionExpired, ClassUnavailable (caller re-initializes)
//	429 → ClassRetry with the Retry-After hint
//	5xx → ClassUnavailable — the request DID reach the server, so a
//	      non-idempotent call must not be replayed; the breaker counts it
//	other 4xx → ClassFatal: our request was rejected on its merits, which
//	      says nothing about server health, so it must not trip the breaker
func httpError(resp *http.Response) *Error {
	snippet := drainSnippet(resp)
	where := resp.Request.Method + " " + resp.Request.URL.Redacted()
	// Every branch carries StatusCode: callers classify by status (see
	// StatusOf / IsAuthStatus) rather than by grepping the message, which
	// also contains the response-body snippet.
	switch {
	case resp.StatusCode == http.StatusGone:
		return &Error{Class: ClassUnavailable, StatusCode: resp.StatusCode,
			Err: fmt.Errorf("%w: %s%s", ErrEndpointMoved, where, snippet)}
	case resp.StatusCode == http.StatusNotFound:
		return &Error{Class: ClassUnavailable, StatusCode: resp.StatusCode,
			Err: fmt.Errorf("%w: %s%s", ErrSessionExpired, where, snippet)}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &Error{
			Class:      ClassRetry,
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get(headerRetryAfter)),
			Err:        fmt.Errorf("%s: http 429 too many requests%s", where, snippet),
		}
	case resp.StatusCode >= 500:
		return &Error{Class: ClassUnavailable, StatusCode: resp.StatusCode,
			Err: fmt.Errorf("%s: http %d%s", where, resp.StatusCode, snippet)}
	default:
		return &Error{Class: ClassFatal, StatusCode: resp.StatusCode,
			Err: fmt.Errorf("%s: http %d%s", where, resp.StatusCode, snippet)}
	}
}

// parseRetryAfter reads a Retry-After value in either allowed form
// (delta-seconds or HTTP-date). An unparsable or negative value yields 0,
// which means "use the caller's own backoff" — never a hard failure.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// drainSnippet reads a bounded prefix of the body for the error message and
// closes it. The snippet is flattened to one line: error strings end up in
// audit records and logs, where an embedded newline forges a record.
func drainSnippet(resp *http.Response) string {
	if resp.Body == nil {
		return ""
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySnippet))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrBodySnippet))
	_ = resp.Body.Close()
	s := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, string(data))
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return ": " + s
}

// drainClose consumes a bounded prefix of a body we do not care about and
// closes it, so the connection returns to the idle pool.
func drainClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrBodySnippet))
	_ = resp.Body.Close()
}

// requestError classifies a client-side round-trip failure.
//
// ClassRetry is granted only when the failure provably happened before any
// byte reached the server (DNS resolution, dial). Everything else — TLS
// after connect, a broken response read — may have executed a
// non-idempotent tools/call, so it is ClassUnavailable.
//
// Note: an SSRF rejection from an injected DialContext surfaces here as a
// dial failure and therefore as ClassRetry. That is honest (nothing
// reached any server) and harmless: the guard is fail-closed, so each
// bounded retry is rejected identically before the caller gives up.
func requestError(err error) *Error {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return &Error{Class: ClassRetry, Err: fmt.Errorf("dns: %w", err)}
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return &Error{Class: ClassRetry, Err: fmt.Errorf("dial: %w", err)}
	}
	return &Error{Class: ClassUnavailable, Err: fmt.Errorf("http request: %w", err)}
}

// responseMediaType returns the lower-cased media type of a response with
// parameters stripped (charset etc.). An absent or unparsable header
// yields "".
func responseMediaType(resp *http.Response) string {
	ct := resp.Header.Get(headerContentType)
	if ct == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	}
	return strings.ToLower(mt)
}

// encodeMessage marshals one outgoing JSON-RPC message, enforcing the same
// bound as the stdio framer. An oversized body is ClassFatal: nothing was
// written, and retrying the identical payload can never succeed.
func encodeMessage(msg any, max int) ([]byte, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, &Error{Class: ClassFatal, Err: fmt.Errorf("encode message: %w", err)}
	}
	if len(data) > max {
		return nil, &Error{Class: ClassFatal, Err: fmt.Errorf("%w: outgoing body is %d bytes", mcp.ErrFrameTooLarge, len(data))}
	}
	return data, nil
}

// readBounded reads at most max bytes, reporting mcp.ErrFrameTooLarge
// rather than buffering an unbounded body.
func readBounded(r io.Reader, max int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > max {
		return nil, fmt.Errorf("%w: response body larger than %d bytes", mcp.ErrFrameTooLarge, max)
	}
	return data, nil
}

// decodeMessages splits a JSON body into one or more JSON-RPC messages.
// Batching was removed from MCP in 2025-06-18, but a 2025-03-26 server may
// still answer with an array, so the read side accepts both shapes (the
// write side only ever emits a single message).
func decodeMessages(data []byte) ([]any, error) {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: empty json body", mcp.ErrMalformedFrame)
	}
	if trimmed[0] != '[' {
		msg, err := mcp.ParseMessage(trimmed)
		if err != nil {
			return nil, err
		}
		return []any{msg}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("%w: %v", mcp.ErrMalformedFrame, err)
	}
	out := make([]any, 0, len(items))
	for _, it := range items {
		msg, err := mcp.ParseMessage(it)
		if err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, nil
}

// changeMaskFor maps a list_changed notification method to its bit.
// Unknown notifications yield ok=false and are ignored, never fatal.
func changeMaskFor(method string) (ChangeMask, bool) {
	switch method {
	case mcp.NotificationToolsListChanged:
		return ChangeTools, true
	case mcp.NotificationResourcesListChanged:
		return ChangeResources, true
	case mcp.NotificationPromptsListChanged:
		return ChangePrompts, true
	default:
		return 0, false
	}
}

// invokePeer runs a PeerHandler and normalises the outcome into a response
// whose id is forced to the request id, mirroring the stdio contract. A
// missing handler answers method-not-found; a handler error or nil
// response answers internal-error. It never returns nil.
func invokePeer(ctx context.Context, h PeerHandler, req *mcp.Request) *mcp.Response {
	if h == nil {
		return mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code:    mcp.CodeMethodNotFound,
			Message: fmt.Sprintf("no peer handler for %q", req.Method),
		})
	}
	r, err := h(ctx, req)
	switch {
	case err != nil:
		return mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeInternalError, Message: err.Error()})
	case r == nil:
		return mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code:    mcp.CodeInternalError,
			Message: "peer handler returned no response",
		})
	default:
		r.JSONRPC = mcp.Version
		r.ID = req.ID
		return r
	}
}

// backoffFor returns the reconnect delay for attempt n (0-based),
// doubling from base and capped at maxRetryBackoff.
func backoffFor(base time.Duration, n int) time.Duration {
	if base <= 0 {
		base = defaultRetryBase
	}
	d := base
	for i := 0; i < n && d < maxRetryBackoff; i++ {
		d *= 2
	}
	if d > maxRetryBackoff {
		d = maxRetryBackoff
	}
	return d
}

// sameOrigin reports whether u may receive requests carrying the caller's
// headers, given the configured endpoint. Scheme and host (including port)
// must match exactly.
//
// Failure direction: FAIL-CLOSED. A server-supplied endpoint that points
// elsewhere would exfiltrate the Authorization header, so it is refused
// rather than followed.
func sameOrigin(base, u *url.URL) bool {
	return strings.EqualFold(base.Scheme, u.Scheme) && strings.EqualFold(base.Host, u.Host)
}
