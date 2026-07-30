package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	// APIVersion is the control-plane protocol version this client speaks.
	// It is sent on every request as HeaderAPIVersion; the daemon rejects
	// incompatible clients with a structured error instead of guessing.
	APIVersion = "1"

	// HeaderRequestID is generated per request (overridable via
	// WithRequestID), echoed by the daemon on the response, carried in
	// error bodies and audit records (canonical.md §4: X-Request-Id
	// end-to-end).
	HeaderRequestID = "X-Request-Id"
	// HeaderAPIVersion carries APIVersion for version negotiation.
	HeaderAPIVersion = "X-Agenthub-Api-Version"

	// apiPrefix is the REST path prefix (/v1/*).
	apiPrefix = "/v1"
	// baseURL is a dummy origin: the transport dials the Unix socket, so
	// the host is never resolved.
	baseURL = "http://agenthub"

	// maxResponseBytes bounds non-streaming response bodies (mirrors the
	// 16MB bounded-read discipline used protocol-wide).
	maxResponseBytes = 16 << 20
)

// Client is the Go client for the agenthub control plane (REST + SSE over
// a Unix domain socket). Construct with New, Default or DialOrStart. A
// Client is safe for concurrent use.
type Client struct {
	hc         *http.Client
	socketPath string

	// Typed resource groups. Everything a frontend may do lives here:
	// there is no raw-request escape hatch, so "the GUI can do it" always
	// implies "an endpoint exists and the CLI can reach it too"
	// (docs/modules/controlplane.md).
	Servers  *ServersService
	Sessions *SessionsService
	Events   *EventsService
	Skills   *SkillsService
	Profiles *ProfilesService
	Scope    *ScopeService
	Config   *ConfigService
	Secrets  *SecretsService
	Tokens   *TokensService
	Clients  *ClientsService
	Auth     *AuthService
	Catalog  *CatalogService
	Parse    *ParseService
}

// New returns a Client that connects to the daemon control socket at
// socketPath. No I/O happens until the first call.
func New(socketPath string) *Client {
	c := &Client{
		socketPath: socketPath,
		hc: &http.Client{
			Transport: &http.Transport{
				// One dial per platform (dial_windows.go / dial_other.go): the
				// endpoint is a Unix socket, or on Windows a named pipe, and
				// "unix" is not a network name that reaches the latter.
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialEndpoint(ctx, socketPath)
				},
			},
			// No client-wide timeout: SSE subscriptions are long-lived.
			// Per-call deadlines come from the caller's context.
		},
	}
	c.Servers = &ServersService{c: c}
	c.Sessions = &SessionsService{c: c}
	c.Events = &EventsService{c: c, retryMin: 200 * time.Millisecond, retryMax: 5 * time.Second}
	c.Skills = &SkillsService{c: c}
	c.Profiles = &ProfilesService{c: c}
	c.Scope = &ScopeService{c: c}
	c.Config = &ConfigService{c: c}
	c.Secrets = &SecretsService{c: c}
	c.Tokens = &TokensService{c: c}
	c.Clients = &ClientsService{c: c}
	c.Auth = &AuthService{c: c}
	c.Catalog = &CatalogService{c: c}
	c.Parse = &ParseService{c: c}
	return c
}

// Default returns a Client for the platform-default control socket
// (AGENTHUB_SOCKET override honored, see DefaultSocketPath).
func Default() (*Client, error) {
	p, err := DefaultSocketPath()
	if err != nil {
		return nil, err
	}
	return New(p), nil
}

// SocketPath returns the socket path this client dials.
func (c *Client) SocketPath() string { return c.socketPath }

// Close releases idle connections. The Client must not be used afterwards.
func (c *Client) Close() { c.hc.CloseIdleConnections() }

// Ping probes the daemon and returns its version, pid and registry
// generation. A transport-level failure means the daemon is offline.
func (c *Client) Ping(ctx context.Context) (Hello, error) {
	var h Hello
	err := c.do(ctx, http.MethodGet, "/ping", nil, nil, &h)
	return h, err
}

type requestIDKey struct{}

// WithRequestID overrides the auto-generated X-Request-Id for calls made
// with the returned context (e.g. to propagate an id across process hops).
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// newRequestID returns 16 random bytes hex-encoded. crypto/rand.Read
// panics rather than returning an error since Go 1.24, so no error path.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// setCommonHeaders stamps the version-negotiation and request-id headers.
func (c *Client) setCommonHeaders(ctx context.Context, req *http.Request) {
	req.Header.Set(HeaderAPIVersion, APIVersion)
	id, _ := ctx.Value(requestIDKey{}).(string)
	if id == "" {
		id = newRequestID()
	}
	req.Header.Set(HeaderRequestID, id)
}

// do performs one REST call: path is relative to /v1, query is optional,
// in (when non-nil) is JSON-encoded as the request body, out (when
// non-nil) receives the envelope's data field.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("api: encoding request body: %w", err)
		}
		body = bytes.NewReader(b)
	}
	u := baseURL + apiPrefix + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return fmt.Errorf("api: building request: %w", err)
	}
	c.setCommonHeaders(ctx, req)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("api: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeEnvelope(resp, out)
}

// envelope is the uniform response shape shared with the CLI --json
// convention (docs/modules/controlplane.md).
type envelope struct {
	OK       bool            `json:"ok"`
	Data     json.RawMessage `json:"data"`
	Error    *errorWire      `json:"error"`
	Warnings []string        `json:"warnings"`
}

// errorWire is the failure body: the shared {code,message,hint} plus the
// fields only the transport carries. It mirrors internal/ctlapi's wireError
// (api cannot import internal/*).
//
// Generation exists ONLY for the lost-compare-and-swap 409, where a client
// that re-read blindly could loop forever; it is absent everywhere else.
type errorWire struct {
	ErrorBody
	RequestID  string `json:"requestId,omitempty"`
	Generation uint64 `json:"generation,omitempty"`
}

// decodeEnvelope parses a response into out.
//
// Failure direction: fail-closed. Any response that cannot be positively
// identified as a well-formed success envelope (ok:true, decodable data)
// is reported as an error — a garbled or half-written body must never be
// treated as success. Server error bodies pass through verbatim.
func decodeEnvelope(resp *http.Response, out any) error {
	reqID := resp.Header.Get(HeaderRequestID)
	bad := func(format string, args ...any) *Error {
		return &Error{
			ErrorBody: ErrorBody{Code: ErrCodeBadResponse, Message: fmt.Sprintf(format, args...)},
			Status:    resp.StatusCode,
			RequestID: reqID,
		}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return bad("reading response: %v", err)
	}
	if len(raw) > maxResponseBytes {
		return bad("response exceeds %d bytes", maxResponseBytes)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return bad("status %d with undecodable body: %v", resp.StatusCode, err)
	}
	if env.Error != nil {
		if reqID == "" {
			// The header is the primary source; the body's copy is the
			// fallback for a proxy that dropped it.
			reqID = env.Error.RequestID
		}
		return &Error{
			ErrorBody:  env.Error.ErrorBody,
			Status:     resp.StatusCode,
			RequestID:  reqID,
			Generation: env.Error.Generation,
		}
	}
	if !env.OK || resp.StatusCode >= 400 {
		return bad("status %d without error body", resp.StatusCode)
	}
	if out != nil {
		if len(env.Data) == 0 {
			return bad("success envelope missing data")
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			return bad("decoding data: %v", err)
		}
	}
	return nil
}
