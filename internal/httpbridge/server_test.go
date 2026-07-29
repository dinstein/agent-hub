package httpbridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
	"github.com/dinstein/agent-hub/internal/tier"
)

// recordingDispatcher is the MCP half of the face under test. It records
// what the transport handed it and answers a canned result — the point of
// these tests is the ingress, the credentials and the session binding, not
// the MCP logic behind them.
type recordingDispatcher struct {
	mu       sync.Mutex
	requests []*mcp.Request
	callers  []*Caller
	notifies int
	// exec, when set, runs the request through a pipeline so a test can
	// prove the caller's tier actually reaches the governance chain.
	exec func(ctx context.Context, c *httpbridge.Caller, req *mcp.Request) *mcp.Response
}

// Caller is a local copy of the fields a test asserts on (the recorder
// keeps the values, not the pointer, so a later request cannot rewrite an
// earlier observation).
type Caller struct {
	Kind    httpbridge.CallerKind
	Token   string
	Tier    tier.Tier
	Servers []string
	Profile string
}

func (d *recordingDispatcher) Dispatch(ctx context.Context, c *httpbridge.Caller, _ *httpbridge.Session, req *mcp.Request) *mcp.Response {
	d.mu.Lock()
	d.requests = append(d.requests, req)
	d.callers = append(d.callers, &Caller{
		Kind: c.Kind, Token: c.Token, Tier: c.Tier, Servers: c.Servers, Profile: c.Profile,
	})
	d.mu.Unlock()
	if d.exec != nil {
		return d.exec(ctx, c, req)
	}
	return mcp.NewResponse(req.ID, json.RawMessage(`{"ok":true}`))
}

func (d *recordingDispatcher) Notify(context.Context, *httpbridge.Caller, *httpbridge.Session, *mcp.Notification) {
	d.mu.Lock()
	d.notifies++
	d.mu.Unlock()
}

func (d *recordingDispatcher) lastCaller() *Caller {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.callers) == 0 {
		return nil
	}
	return d.callers[len(d.callers)-1]
}

func (d *recordingDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.requests)
}

type harness struct {
	srv    *httptest.Server
	store  *httpbridge.Store
	disp   *recordingDispatcher
	bridge *httpbridge.Server
}

func newHarness(t *testing.T, auth *httpbridge.Authenticator, opts httpbridge.Options) *harness {
	t.Helper()
	disp := &recordingDispatcher{}
	if opts.Dispatcher == nil {
		opts.Dispatcher = disp
	} else if d, ok := opts.Dispatcher.(*recordingDispatcher); ok {
		disp = d
	}
	opts.Auth = auth
	bridge, err := httpbridge.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(bridge.Handler())
	t.Cleanup(srv.Close)
	return &harness{srv: srv, store: auth.Tokens, disp: disp, bridge: bridge}
}

// post sends one JSON-RPC frame with the given bearer and session id.
func (h *harness) post(t *testing.T, bearer, session, body string, headers ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+httpbridge.DefaultPath, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if session != "" {
		req.Header.Set(httpbridge.SessionHeader, session)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

const initFrame = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
const callFrame = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"srv__rm"}}`

// initSession performs the handshake and returns the session id.
func (h *harness) initSession(t *testing.T, bearer string) string {
	t.Helper()
	res := h.post(t, bearer, "", initFrame)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200", res.StatusCode)
	}
	id := res.Header.Get(httpbridge.SessionHeader)
	if id == "" {
		t.Fatal("initialize did not return a session id")
	}
	return id
}

func errorCode(t *testing.T, res *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("rejection body is not the frozen envelope: %s", body)
	}
	return env.Error.Code
}

// --- authentication --------------------------------------------------------

// Fail-closed is the headline property: no credential, no access, and the
// dispatcher is never reached.
func TestUnauthenticatedRequestIsRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &httpbridge.Authenticator{}, httpbridge.Options{})
	res := h.post(t, "", "", initFrame)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	if got := res.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
	}
	if code := errorCode(t, res); code != httpbridge.CodeUnauthorized {
		t.Errorf("code = %q, want %q", code, httpbridge.CodeUnauthorized)
	}
	if h.disp.count() != 0 {
		t.Fatal("the dispatcher was reached without a credential")
	}
}

// Prefix dispatch is exclusive in both directions.
func TestPrefixDispatchIsExclusive(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, agentValue := mustCreate(t, store, httpbridge.CreateSpec{Name: "agent", Tier: tier.Write})
	h := newHarness(t, &httpbridge.Authenticator{AdminToken: "adminsecret", Tokens: store}, httpbridge.Options{})

	if res := h.post(t, "adminsecret", "", initFrame); res.StatusCode != http.StatusOK {
		t.Fatalf("admin token status = %d, want 200", res.StatusCode)
	}
	if got := h.disp.lastCaller(); got.Kind != httpbridge.CallerAdmin || got.Tier != tier.Destructive {
		t.Errorf("admin caller = %+v, want admin/destructive", got)
	}

	if res := h.post(t, agentValue, "", initFrame); res.StatusCode != http.StatusOK {
		t.Fatalf("agent token status = %d, want 200", res.StatusCode)
	}
	if got := h.disp.lastCaller(); got.Kind != httpbridge.CallerAgent || got.Token != "agent" || got.Tier != tier.Write {
		t.Errorf("agent caller = %+v, want agent/agent/write", got)
	}

	// An agent-shaped bearer is NEVER compared against the admin token, so
	// an admin token that happened to start with the prefix cannot be used
	// to probe the store either.
	if res := h.post(t, httpbridge.TokenPrefix+"deadbeef", "", initFrame); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("bogus agent token status = %d, want 401", res.StatusCode)
	}
	if res := h.post(t, "wrongadmin", "", initFrame); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong admin token status = %d, want 401", res.StatusCode)
	}
}

// The escape hatch only excuses a MISSING credential; a caller that
// presented one has claimed an identity and must prove it.
func TestInsecureLoopbackDoesNotExcuseABadToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &httpbridge.Authenticator{InsecureLoopback: true, Tokens: newStore(t)}, httpbridge.Options{})
	if res := h.post(t, "", "", initFrame); res.StatusCode != http.StatusOK {
		t.Fatalf("anonymous status = %d, want 200 under --insecure-loopback", res.StatusCode)
	}
	if got := h.disp.lastCaller(); got.Kind != httpbridge.CallerLoopback {
		t.Errorf("caller kind = %q, want %q", got.Kind, httpbridge.CallerLoopback)
	}
	if res := h.post(t, httpbridge.TokenPrefix+"nope", "", initFrame); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("invalid token under --insecure-loopback: status = %d, want 401", res.StatusCode)
	}
}

// A revoked token stops working at its next request, without restarting the
// server: authentication reads the store, it does not cache a decision.
func TestRevokedTokenStopsWorkingImmediately(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "agent", Tier: tier.Write})
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{})

	session := h.initSession(t, value)
	if _, err := store.Revoke(context.Background(), "agent", time.Now()); err != nil {
		t.Fatal(err)
	}
	res := h.post(t, value, session, callFrame)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status after revocation = %d, want 401", res.StatusCode)
	}
}

// The token's constraints travel to the dispatcher intact — they are what
// the assembly feeds into the scope intersection and the tier gate.
func TestCallerCarriesTheTokenConstraints(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{
		Name: "ci", Tier: tier.Read, Servers: []string{"git"}, Profile: "readonly",
	})
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{})
	h.initSession(t, value)

	got := h.disp.lastCaller()
	if got.Tier != tier.Read || got.Profile != "readonly" ||
		len(got.Servers) != 1 || got.Servers[0] != "git" {
		t.Fatalf("caller = %+v, want tier read, profile readonly, servers [git]", got)
	}
}

// The whole point of the tier: an HTTP caller holding a read token is
// stopped by the pipeline's token tier gate, with the gate's own code.
func TestReadTokenIsBlockedByTheTierGate(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, readValue := mustCreate(t, store, httpbridge.CreateSpec{Name: "ro", Tier: tier.Read})
	_, fullValue := mustCreate(t, store, httpbridge.CreateSpec{Name: "rw", Tier: tier.Destructive})

	pipe := pipeline.New(pipeline.Options{})
	disp := &recordingDispatcher{}
	disp.exec = func(ctx context.Context, c *httpbridge.Caller, req *mcp.Request) *mcp.Response {
		if req.Method != mcp.MethodToolsCall {
			return mcp.NewResponse(req.ID, json.RawMessage(`{}`))
		}
		_, err := pipe.Execute(ctx, pipeline.CallRequest{
			Exposed: "srv__rm", ServerID: "srv", RawTool: "rm",
			// No annotations at all: the fail-closed destructive case.
			CallerTier: c.Tier,
			Call: func(context.Context) (*mcp.CallResult, error) {
				return &mcp.CallResult{}, nil
			},
		})
		if err != nil {
			var be *pipeline.BlockedError
			if errors.As(err, &be) {
				return mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeInvalidRequest, Message: be.Code})
			}
			return mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeInternalError, Message: err.Error()})
		}
		return mcp.NewResponse(req.ID, json.RawMessage(`{"ok":true}`))
	}
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{Dispatcher: disp})

	rejected := decodeRPC(t, h.post(t, readValue, h.initSession(t, readValue), callFrame))
	if rejected.Error == nil || rejected.Error.Message != pipeline.CodeTokenTierDenied {
		t.Fatalf("read token: response = %+v, want %s", rejected, pipeline.CodeTokenTierDenied)
	}
	allowed := decodeRPC(t, h.post(t, fullValue, h.initSession(t, fullValue), callFrame))
	if allowed.Error != nil {
		t.Fatalf("destructive token: response error = %+v, want success", allowed.Error)
	}
}

func decodeRPC(t *testing.T, res *http.Response) *mcp.Response {
	t.Helper()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out mcp.Response
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("response is not JSON-RPC: %s", body)
	}
	return &out
}

// --- sessions --------------------------------------------------------------

func TestSessionBindingAndOwnership(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, a := mustCreate(t, store, httpbridge.CreateSpec{Name: "a", Tier: tier.Write})
	_, b := mustCreate(t, store, httpbridge.CreateSpec{Name: "b", Tier: tier.Write})
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{})

	session := h.initSession(t, a)
	if res := h.post(t, a, session, callFrame); res.StatusCode != http.StatusOK {
		t.Fatalf("owner request status = %d, want 200", res.StatusCode)
	}
	// A different token may not ride this session, and the miss is the same
	// frozen 404 as an unknown id (no probing oracle).
	foreign := h.post(t, b, session, callFrame)
	if foreign.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign session status = %d, want 404", foreign.StatusCode)
	}
	unknown := h.post(t, b, "deadbeef", callFrame)
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session status = %d, want 404", unknown.StatusCode)
	}
	if errorCode(t, foreign) != errorCode(t, unknown) {
		t.Error("a foreign session and an unknown session must answer identically")
	}
	// The probe must not have destroyed the owner's session.
	if res := h.post(t, a, session, callFrame); res.StatusCode != http.StatusOK {
		t.Fatalf("owner request after a foreign probe = %d, want 200", res.StatusCode)
	}
	// A non-initialize request without a session id is a miss too.
	if res := h.post(t, a, "", callFrame); res.StatusCode != http.StatusNotFound {
		t.Fatalf("sessionless call status = %d, want 404", res.StatusCode)
	}
}

func TestSessionDeleteAndTTL(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "a", Tier: tier.Write})
	now := time.Now()
	clock := func() time.Time { return now }
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store, Now: clock},
		httpbridge.Options{SessionTTL: time.Minute, Now: clock})

	session := h.initSession(t, value)
	if h.bridge.Sessions() != 1 {
		t.Fatalf("Sessions = %d, want 1", h.bridge.Sessions())
	}
	// Idle past the TTL: the session is gone and the answer is the frozen 404.
	now = now.Add(2 * time.Minute)
	if res := h.post(t, value, session, callFrame); res.StatusCode != http.StatusNotFound {
		t.Fatalf("expired session status = %d, want 404", res.StatusCode)
	}

	now = time.Now()
	session = h.initSession(t, value)
	req, _ := http.NewRequest(http.MethodDelete, h.srv.URL+httpbridge.DefaultPath, nil)
	req.Header.Set("Authorization", "Bearer "+value)
	req.Header.Set(httpbridge.SessionHeader, session)
	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", res.StatusCode)
	}
	if res := h.post(t, value, session, callFrame); res.StatusCode != http.StatusNotFound {
		t.Fatalf("status after DELETE = %d, want 404", res.StatusCode)
	}
}

func TestSessionCapacityShedsRatherThanEvicts(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "a", Tier: tier.Write})
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{MaxSessions: 2})

	first := h.initSession(t, value)
	h.initSession(t, value)
	res := h.post(t, value, "", initFrame)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status at capacity = %d, want 503", res.StatusCode)
	}
	// The existing sessions survive: capacity pressure must not turn into
	// errors for callers that were already connected.
	if res := h.post(t, value, first, callFrame); res.StatusCode != http.StatusOK {
		t.Fatalf("existing session status = %d, want 200", res.StatusCode)
	}
}

// --- ingress ---------------------------------------------------------------

func TestBodyLimit(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "a", Tier: tier.Write})
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{})

	huge := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"pad":"` +
		strings.Repeat("x", httpbridge.MaxBodyBytes) + `"}}`
	res := h.post(t, value, "", huge)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", res.StatusCode)
	}
	if code := errorCode(t, res); code != httpbridge.CodePayloadTooBig {
		t.Errorf("code = %q, want %q", code, httpbridge.CodePayloadTooBig)
	}
	if h.disp.count() != 0 {
		t.Fatal("an oversized body reached the dispatcher")
	}
	// A body just under the limit still works.
	pad := httpbridge.MaxBodyBytes - len(initFrame) - 64
	ok := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"pad":"` +
		strings.Repeat("x", pad) + `"}}`
	if res := h.post(t, value, "", ok); res.StatusCode != http.StatusOK {
		t.Fatalf("under-limit body status = %d, want 200", res.StatusCode)
	}
}

// The in-flight ceiling sheds load; the shed happens BEFORE authentication,
// because that is the work an anonymous caller can otherwise cause.
func TestInFlightCeilingSheds(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "a", Tier: tier.Write})

	release := make(chan struct{})
	blocking := &recordingDispatcher{}
	blocking.exec = func(ctx context.Context, _ *httpbridge.Caller, req *mcp.Request) *mcp.Response {
		<-release
		return mcp.NewResponse(req.ID, json.RawMessage(`{}`))
	}
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store},
		httpbridge.Options{Dispatcher: blocking, MaxInFlight: 1})
	defer close(release)

	started := make(chan struct{})
	go func() {
		close(started)
		_ = h.post(t, value, "", initFrame)
	}()
	<-started
	// Wait until the slot is actually taken.
	deadline := time.Now().Add(2 * time.Second)
	for blocking.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	res := h.post(t, value, "", initFrame)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
	if code := errorCode(t, res); code != httpbridge.CodeOverloaded {
		t.Errorf("code = %q, want %q", code, httpbridge.CodeOverloaded)
	}
}

// Browser-originated cross-site requests are refused, and no CORS header is
// ever emitted — not even on the rejection.
func TestCrossSiteAndCORS(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "a", Tier: tier.Write})
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{})

	cases := []struct {
		name    string
		headers []string
		want    int
	}{
		{"cross-site fetch metadata", []string{"Sec-Fetch-Site", "cross-site"}, http.StatusForbidden},
		{"same-site fetch metadata", []string{"Sec-Fetch-Site", "same-site"}, http.StatusForbidden},
		{"same-origin fetch metadata", []string{"Sec-Fetch-Site", "same-origin"}, http.StatusOK},
		{"address-bar navigation", []string{"Sec-Fetch-Site", "none"}, http.StatusOK},
		{"foreign origin", []string{"Origin", "https://evil.example"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		res := h.post(t, value, "", initFrame, tc.headers...)
		if res.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, res.StatusCode, tc.want)
		}
		for _, header := range []string{
			"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials",
			"Access-Control-Allow-Headers", "Access-Control-Allow-Methods",
		} {
			if got := res.Header.Get(header); got != "" {
				t.Errorf("%s: %s = %q, want no CORS header at all", tc.name, header, got)
			}
		}
	}
	// A same-Host Origin is a local UI, not a cross-origin page.
	sameHost := h.post(t, value, "", initFrame, "Origin", h.srv.URL)
	if sameHost.StatusCode != http.StatusOK {
		t.Errorf("same-host Origin status = %d, want 200", sameHost.StatusCode)
	}
}

// No SSE exposure face (canonical.md §5b), and an unknown path answers the
// same frozen 404 as an unknown session.
func TestVerbsAndPaths(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "a", Tier: tier.Write})
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{})

	get, _ := http.NewRequest(http.MethodGet, h.srv.URL+httpbridge.DefaultPath, nil)
	get.Header.Set("Authorization", "Bearer "+value)
	res, err := h.srv.Client().Do(get)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405 (no SSE exposure face)", res.StatusCode)
	}

	other, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/admin", strings.NewReader(initFrame))
	other.Header.Set("Authorization", "Bearer "+value)
	res2, err := h.srv.Client().Do(other)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res2.Body.Close() }()
	if res2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", res2.StatusCode)
	}
	if code := errorCode(t, res2); code != httpbridge.CodeNotFound {
		t.Errorf("code = %q, want %q", code, httpbridge.CodeNotFound)
	}
}

func TestMalformedAndNotificationFrames(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "a", Tier: tier.Write})
	h := newHarness(t, &httpbridge.Authenticator{Tokens: store}, httpbridge.Options{})
	session := h.initSession(t, value)

	for _, body := range []string{`{`, `{"jsonrpc":"1.0","id":1,"method":"x"}`, `[]`} {
		if res := h.post(t, value, session, body); res.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, res.StatusCode)
		}
	}
	res := h.post(t, value, session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202", res.StatusCode)
	}
	if h.disp.notifies != 1 {
		t.Errorf("notifies = %d, want 1", h.disp.notifies)
	}
}

// New refuses to build a face without an authenticator: an MCP endpoint with
// no credential check is the exact fail-open this package prevents.
func TestNewRequiresItsCollaborators(t *testing.T) {
	t.Parallel()
	if _, err := httpbridge.New(httpbridge.Options{Auth: &httpbridge.Authenticator{}}); err == nil {
		t.Error("New accepted a nil Dispatcher")
	}
	if _, err := httpbridge.New(httpbridge.Options{Dispatcher: &recordingDispatcher{}}); err == nil {
		t.Error("New accepted a nil Authenticator")
	}
}

// The head-side limits cannot be set from a handler, so the constructor that
// carries them is the one assemblies must use.
func TestHTTPServerCarriesTheHeadLimits(t *testing.T) {
	t.Parallel()
	bridge, err := httpbridge.New(httpbridge.Options{
		Dispatcher: &recordingDispatcher{}, Auth: &httpbridge.Authenticator{},
	})
	if err != nil {
		t.Fatal(err)
	}
	hs := bridge.HTTPServer()
	if hs.MaxHeaderBytes != httpbridge.MaxHeaderBytes {
		t.Errorf("MaxHeaderBytes = %d, want %d", hs.MaxHeaderBytes, httpbridge.MaxHeaderBytes)
	}
	if hs.ReadHeaderTimeout != httpbridge.HeaderReadTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", hs.ReadHeaderTimeout, httpbridge.HeaderReadTimeout)
	}
}
