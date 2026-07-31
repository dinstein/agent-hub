package oauthflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestLoginRefusesAPrivateAuthorizationEndpoint is the regression for a
// DISCOVERED authorization_endpoint that was never screened.
//
// Only an operator-pinned endpoint went through checkURL. BuildAuthorizeURL
// merely url.Parsed whatever the metadata document said and LoopbackFlow.Run
// handed the result to Flow.Open, so an AS advertising
// `authorization_endpoint: https://10.0.0.5:8080/authorize` drove the user's
// browser — with its ambient intranet cookies — at an internal destination.
// cli/browser.go's scheme check closes the file:// half of this and nothing
// else, and Flow.Open is injectable, so other openers have no backstop at
// all.
func TestLoginRefusesAPrivateAuthorizationEndpoint(t *testing.T) {
	as := newFakeAS(t)
	// https, so the refusal comes from the ADDRESS rather than the scheme.
	as.authorizeOverride = "https://10.0.0.5:8080/authorize"
	f, v := newTestFlow(t, as)

	var opened atomic.Int64
	_, err := f.Login(context.Background(), LoginRequest{
		ServerID:   "gh",
		Issuer:     as.issuer(),
		Scopes:     []string{"read"},
		ClientName: "agenthub",
		Mode:       ModeLoopback,
		Open: func(string) error {
			opened.Add(1)
			return nil
		},
		Timeout: 2 * time.Second,
	})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("login = %v; want ErrBlocked", err)
	}
	if n := opened.Load(); n != 0 {
		t.Fatalf("the browser was opened %d time(s) at a private destination", n)
	}
	if got := v.writeLog(); len(got) != 0 {
		t.Fatalf("credentials were written for a refused flow: %v", got)
	}
}

// TestManualLoginRefusesAPrivateAuthorizationEndpoint: manual mode prints
// the URL for the user to paste into a browser themselves, which is the
// same destination reached by a different hand.
func TestManualLoginRefusesAPrivateAuthorizationEndpoint(t *testing.T) {
	as := newFakeAS(t)
	as.authorizeOverride = "https://192.168.1.10/authorize"
	f, _ := newTestFlow(t, as)

	var shown atomic.Int64
	_, err := f.Login(context.Background(), LoginRequest{
		ServerID:   "gh",
		Issuer:     as.issuer(),
		Scopes:     []string{"read"},
		ClientName: "agenthub",
		Mode:       ModeManual,
		Paste: func(context.Context, ManualInstructions) (string, error) {
			shown.Add(1)
			return "", nil
		},
		Timeout: 2 * time.Second,
	})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("login = %v; want ErrBlocked", err)
	}
	if n := shown.Load(); n != 0 {
		t.Fatalf("a private destination was shown to the user %d time(s)", n)
	}
}

// TestAuthorizeURLKeepsWorkingForAPublicEndpoint is the failure direction:
// the screen must not refuse the ordinary case. The fake AS is on loopback,
// so this also pins that the AllowLoopback carve-out still reaches the
// browser path.
func TestAuthorizeURLKeepsWorkingForAPublicEndpoint(t *testing.T) {
	as := newFakeAS(t)
	f, _ := newTestFlow(t, as)
	res, err := f.Login(context.Background(), LoginRequest{
		ServerID:   "gh",
		Issuer:     as.issuer(),
		Scopes:     []string{"read"},
		ClientName: "agenthub",
		Mode:       ModeLoopback,
		Open:       as.browserFor(t),
		Timeout:    10 * time.Second,
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.AccessToken == "" {
		t.Fatal("no token")
	}
}

// TestDeviceFlowRefusesAPrivateVerificationURI covers the other URL a
// device flow puts in front of a browser. It arrives in the AS's own
// response rather than its metadata, and cli/auth.go opens it directly.
func TestDeviceFlowRefusesAPrivateVerificationURI(t *testing.T) {
	as := newFakeAS(t)
	as.deviceEndpoint = true
	as.verificationURI = "https://10.1.2.3/activate"
	c := as.client()

	_, err := c.StartDevice(context.Background(), DeviceRequest{
		Metadata: as.metadata(),
		ClientID: "client-abc",
		Scopes:   []string{"read"},
	})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("StartDevice = %v; want ErrBlocked", err)
	}
}
