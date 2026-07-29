package oauthlogin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/oauthflow"
)

// fakeFlow stands in for oauthflow.Flow. It calls the callbacks the real one
// would call, then blocks until the test releases it — which is what lets a
// test observe a session mid-flight, the state this package exists to model.
type fakeFlow struct {
	// announce runs before the flow blocks, with the request it was given.
	announce func(req oauthflow.LoginRequest)
	// release gates the return. nil returns immediately.
	release <-chan struct{}
	result  *oauthflow.LoginResult
	err     error

	mu   sync.Mutex
	got  oauthflow.LoginRequest
	ctxs []context.Context
}

func (f *fakeFlow) Login(
	ctx context.Context, req oauthflow.LoginRequest,
) (*oauthflow.LoginResult, error) {
	f.mu.Lock()
	f.got = req
	f.ctxs = append(f.ctxs, ctx)
	f.mu.Unlock()
	if f.announce != nil {
		f.announce(req)
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.result, f.err
}

func (f *fakeFlow) request() oauthflow.LoginRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got
}

func newManager(t *testing.T, flow Flow) *Manager {
	t.Helper()
	m, err := New(Config{Flows: func(bool) Flow { return flow }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// waitFor polls until cond holds, and FAILS HARD on timeout rather than
// returning quietly: a silent give-up here would report a stuck session as a
// passing test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStartReportsTheAuthorizationURLTheFlowProduced(t *testing.T) {
	release := make(chan struct{})
	flow := &fakeFlow{
		release: release,
		result:  &oauthflow.LoginResult{Mode: oauthflow.ModeLoopback, State: &oauthflow.State{}},
	}
	flow.announce = func(req oauthflow.LoginRequest) {
		// The real flow calls Open once it has an authorization URL.
		if err := req.Open("https://as.example/authorize?state=abc"); err != nil {
			t.Errorf("Open returned %v; the manager must report success or oauthflow "+
				"downgrades to the manual paste flow", err)
		}
	}
	m := newManager(t, flow)

	first, err := m.Start(Request{ServerID: "github", ResourceURL: "https://api.example/mcp"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if first.Phase != PhasePending {
		t.Fatalf("Start phase = %q, want pending", first.Phase)
	}
	if first.ID == "" {
		t.Fatal("Start returned no session id")
	}

	waitFor(t, "the authorization URL", func() bool {
		s, err := m.Get(first.ID)
		return err == nil && s.AuthorizationURL != ""
	})
	s, err := m.Get(first.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s.AuthorizationURL != "https://as.example/authorize?state=abc" {
		t.Errorf("AuthorizationURL = %q", s.AuthorizationURL)
	}
	if s.Mode != string(oauthflow.ModeLoopback) {
		t.Errorf("Mode = %q, want loopback", s.Mode)
	}
	if !s.Actionable() {
		t.Error("a session holding an authorization URL must be Actionable")
	}

	close(release)
	waitFor(t, "completion", func() bool {
		s, err := m.Get(first.ID)
		return err == nil && s.Phase == PhaseComplete
	})
}

func TestDeviceCodeIsReportedAndTheSecretIsNot(t *testing.T) {
	release := make(chan struct{})
	flow := &fakeFlow{
		release: release,
		result:  &oauthflow.LoginResult{Mode: oauthflow.ModeDevice, State: &oauthflow.State{}},
	}
	flow.announce = func(req oauthflow.LoginRequest) {
		req.OnDeviceCode(oauthflow.DeviceAuthorization{
			DeviceCode:              "SECRET-device-code",
			UserCode:                "WDJB-MJHT",
			VerificationURI:         "https://as.example/device",
			VerificationURIComplete: "https://as.example/device?code=WDJB-MJHT",
			ExpiresIn:               600,
		})
	}
	m := newManager(t, flow)
	started, err := m.Start(Request{ServerID: "linear", ResourceURL: "https://api.example/mcp"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the device code", func() bool {
		s, err := m.Get(started.ID)
		return err == nil && s.UserCode != ""
	})
	s, _ := m.Get(started.ID)
	if s.UserCode != "WDJB-MJHT" || s.VerificationURI != "https://as.example/device" {
		t.Errorf("device fields not reported: %+v", s)
	}
	if s.Mode != string(oauthflow.ModeDevice) {
		t.Errorf("Mode = %q, want device", s.Mode)
	}
	close(release)
}

// The device code polled with is a secret and has no field on Session at all,
// so no rendering mistake can put it on the wire. This is the type-level half
// of the rule; the test above is the behavioural half.
func TestSessionCarriesNoSecretFields(t *testing.T) {
	s := Session{}
	// A compile-time assertion in test form: adding DeviceCode, AccessToken
	// or RefreshToken to Session breaks this file, which is the point.
	if s.UserCode != "" || s.AuthorizationURL != "" {
		t.Fatal("zero Session is not zero")
	}
	if s.HasRefreshToken {
		t.Fatal("HasRefreshToken must be a boolean answer, never a token")
	}
}

func TestSecondLoginForTheSameServerJoinsTheFirst(t *testing.T) {
	release := make(chan struct{})
	flow := &fakeFlow{release: release, result: &oauthflow.LoginResult{State: &oauthflow.State{}}}
	m := newManager(t, flow)

	first, err := m.Start(Request{ServerID: "stripe", ResourceURL: "https://api.example/mcp"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	second, err := m.Start(Request{ServerID: "stripe", ResourceURL: "https://api.example/mcp"})
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second Start opened a new session (%s vs %s): two concurrent flows "+
			"would each bind a loopback port and race the same vault entry",
			second.ID, first.ID)
	}
	close(release)
}

func TestADifferentServerGetsItsOwnSession(t *testing.T) {
	release := make(chan struct{})
	flow := &fakeFlow{release: release, result: &oauthflow.LoginResult{State: &oauthflow.State{}}}
	m := newManager(t, flow)

	a, _ := m.Start(Request{ServerID: "a", ResourceURL: "https://a.example/mcp"})
	b, _ := m.Start(Request{ServerID: "b", ResourceURL: "https://b.example/mcp"})
	if a.ID == b.ID {
		t.Fatal("two servers shared one login session")
	}
	close(release)
}

func TestFailureIsReportedWithItsError(t *testing.T) {
	boom := errors.New("discovery failed")
	m := newManager(t, &fakeFlow{err: boom})
	started, err := m.Start(Request{ServerID: "notion", ResourceURL: "https://api.example/mcp"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "failure", func() bool {
		s, err := m.Get(started.ID)
		return err == nil && s.Phase == PhaseFailed
	})
	s, _ := m.Get(started.ID)
	if !errors.Is(s.Err, boom) {
		t.Errorf("Err = %v, want %v", s.Err, boom)
	}
	if s.Actionable() {
		t.Error("a failed session is not Actionable")
	}
}

func TestCompletionCarriesWhatWasStoredAndNoToken(t *testing.T) {
	m := newManager(t, &fakeFlow{
		result: &oauthflow.LoginResult{
			Mode:        oauthflow.ModeDevice,
			AccessToken: "at-secret",
			State: &oauthflow.State{
				Issuer:       "https://as.example",
				Scope:        "read",
				ExpiresAt:    1234,
				RefreshToken: "rt-secret",
			},
		},
	})
	started, _ := m.Start(Request{ServerID: "sentry", ResourceURL: "https://api.example/mcp"})
	waitFor(t, "completion", func() bool {
		s, err := m.Get(started.ID)
		return err == nil && s.Phase == PhaseComplete
	})
	s, _ := m.Get(started.ID)
	if s.Issuer != "https://as.example" || s.Scope != "read" || s.ExpiresAt != 1234 {
		t.Errorf("stored facts not reported: %+v", s)
	}
	if !s.HasRefreshToken {
		t.Error("HasRefreshToken should be true when a refresh token was stored")
	}
}

func TestCancelStopsTheFlowAndTheContextIsCancelled(t *testing.T) {
	// No release channel and no result: the fake blocks on ctx.Done() alone,
	// so only a real cancellation can end it.
	flow := &fakeFlow{release: make(chan struct{})}
	m := newManager(t, flow)
	started, _ := m.Start(Request{ServerID: "asana", ResourceURL: "https://api.example/mcp"})

	final, err := m.Cancel(started.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if final.Phase != PhaseFailed {
		t.Fatalf("phase after Cancel = %q, want failed", final.Phase)
	}
	if !errors.Is(final.Err, context.Canceled) {
		t.Errorf("Err = %v, want context.Canceled", final.Err)
	}
}

// Cancelling a login that just finished is what a user clicking "Cancel" at
// the moment the callback lands does. Answering that with a failure would
// report a credential that IS stored as one that was abandoned.
func TestCancelAfterCompletionKeepsTheResult(t *testing.T) {
	m := newManager(t, &fakeFlow{result: &oauthflow.LoginResult{State: &oauthflow.State{Issuer: "https://as.example"}}})
	started, _ := m.Start(Request{ServerID: "clerk", ResourceURL: "https://api.example/mcp"})
	waitFor(t, "completion", func() bool {
		s, err := m.Get(started.ID)
		return err == nil && s.Phase == PhaseComplete
	})
	final, err := m.Cancel(started.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if final.Phase != PhaseComplete || final.Issuer != "https://as.example" {
		t.Fatalf("Cancel rewrote a finished session: %+v", final)
	}
}

func TestUnknownSessionIsNotFound(t *testing.T) {
	m := newManager(t, &fakeFlow{})
	if _, err := m.Get("nope"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Get(unknown) = %v, want ErrNoSession", err)
	}
	if _, err := m.Cancel("nope"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Cancel(unknown) = %v, want ErrNoSession", err)
	}
}

func TestAFinishedSessionIsReadableThenSweptAway(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	m, err := New(Config{
		Flows:  func(bool) Flow { return &fakeFlow{result: &oauthflow.LoginResult{State: &oauthflow.State{}}} },
		Retain: time.Minute,
		Now:    clock,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	started, _ := m.Start(Request{ServerID: "time", ResourceURL: "https://api.example/mcp"})
	waitFor(t, "completion", func() bool {
		s, err := m.Get(started.ID)
		return err == nil && s.Phase == PhaseComplete
	})
	// Still inside the retention window: a poller one moment late must get
	// the outcome, not a 404 that reads as "no such login".
	now = now.Add(30 * time.Second)
	if _, err := m.Get(started.ID); err != nil {
		t.Fatalf("session dropped inside its retention window: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := m.Get(started.ID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("session survived its retention window: %v", err)
	}
}

func TestStartRefusesALoginItCannotAddress(t *testing.T) {
	m := newManager(t, &fakeFlow{})
	if _, err := m.Start(Request{ResourceURL: "https://api.example/mcp"}); err == nil {
		t.Error("a login with no server id must be refused")
	}
	// Neither a resource URL nor a pinned issuer: there is nothing to
	// discover against, and `auth login` refuses the same shape.
	if _, err := m.Start(Request{ServerID: "x"}); err == nil {
		t.Error("a login with no url and no issuer must be refused")
	}
}

func TestTheRequestReachesTheFlowIntact(t *testing.T) {
	release := make(chan struct{})
	flow := &fakeFlow{release: release, result: &oauthflow.LoginResult{State: &oauthflow.State{}}}
	m := newManager(t, flow)
	_, err := m.Start(Request{
		ServerID:              "github",
		Issuer:                "https://as.example",
		ResourceURL:           "https://api.example/mcp",
		ResourceMetadataURL:   "https://api.example/.well-known/x",
		AuthorizationEndpoint: "https://as.example/authorize/tenant1",
		Scopes:                []string{"read", "write"},
		CallbackPort:          8040,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the flow to be called", func() bool { return flow.request().ServerID != "" })
	got := flow.request()
	if got.Issuer != "https://as.example" || got.ResourceURL != "https://api.example/mcp" {
		t.Errorf("issuer/resource not forwarded: %+v", got)
	}
	if got.AuthorizationEndpoint != "https://as.example/authorize/tenant1" {
		t.Errorf("authorization endpoint not forwarded: %q", got.AuthorizationEndpoint)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "read" {
		t.Errorf("scopes not forwarded verbatim: %v", got.Scopes)
	}
	if got.FixedCallbackPort != 8040 {
		t.Errorf("FixedCallbackPort = %d, want 8040 — a provider matching the "+
			"redirect_uri byte for byte would refuse a fresh port", got.FixedCallbackPort)
	}
	// Paste must stay nil: manual mode reads from a terminal, and with a
	// non-nil Paste oauthflow may downgrade a failed browser-open to a
	// paste this API can never deliver.
	if got.Paste != nil {
		t.Error("Paste must be nil: there is no terminal on this API to paste into")
	}
	if got.Open == nil {
		t.Error("Open must be non-nil, or SelectMode falls through to manual")
	}
	close(release)
}

// The one behaviour that separates this caller from the CLI: Open records the
// URL and reports SUCCESS. A non-nil error is oauthflow's signal that the host
// has no browser, which downgrades the flow to a paste path that does not
// exist here.
func TestOpenReportsSuccessWithoutOpeningAnything(t *testing.T) {
	release := make(chan struct{})
	openErr := make(chan error, 1)
	flow := &fakeFlow{release: release, result: &oauthflow.LoginResult{State: &oauthflow.State{}}}
	flow.announce = func(req oauthflow.LoginRequest) {
		openErr <- req.Open("https://as.example/authorize")
	}
	m := newManager(t, flow)
	started, _ := m.Start(Request{ServerID: "github", ResourceURL: "https://api.example/mcp"})
	waitFor(t, "the URL", func() bool {
		s, err := m.Get(started.ID)
		return err == nil && s.AuthorizationURL != ""
	})
	select {
	case err := <-openErr:
		if err != nil {
			t.Fatalf("Open returned %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Open was never called")
	}
	close(release)
}
