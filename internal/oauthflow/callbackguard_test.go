package oauthflow

import (
	"errors"
	"testing"
)

// TestStateIsCheckedOnAnErrorResponse: RFC 6749 §4.1.2.1 obliges an AS to
// echo state on an error response exactly as on a success, so a callback
// that fails the state check is not this flow's outcome whichever member it
// carries. Before this, the error branch ran first and skipped state
// entirely — anything that reached the loopback port during the flow could
// end it, with no state to guess, and put its own error_description into
// what the operator reads.
func TestStateIsCheckedOnAnErrorResponse(t *testing.T) {
	_, _, err := ParseManualCallback(
		"http://127.0.0.1:9/callback?error=access_denied&error_description=go+away&state=wrong", "right")
	if !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("err = %v, want ErrStateMismatch", err)
	}
	// And the AS's own text must not have become the failure.
	var te *TokenError
	if errors.As(err, &te) {
		t.Fatalf("a state-mismatched callback surfaced the AS error %+v", te)
	}
}

// TestIssIsCheckedBeforeAnErrorIsSurfaced pins the MUST NOT: on an iss
// mismatch a client must not act on or display error / error_description /
// error_uri. The AS's text is attacker-supplied and reaches the terminal and
// the event log, so the issuer error has to replace it, not follow it.
func TestIssIsCheckedBeforeAnErrorIsSurfaced(t *testing.T) {
	md := &AuthServerMetadata{
		Issuer: "https://honest.example",
		AuthorizationResponseIssParameterSupported: true,
	}
	_, iss, cbErr := ParseManualCallback(
		"http://127.0.0.1:9/callback?error=access_denied&error_description=go+away"+
			"&state=s&iss=https%3A%2F%2Fattacker.example", "s")
	if cbErr == nil {
		t.Fatal("the callback carried an error and parsed clean")
	}
	if iss != "https://attacker.example" {
		t.Fatalf("iss = %q; the parser must hand the caller what it cannot check itself", iss)
	}
	err := issThenCallback(md, iss, false, cbErr)
	if !errors.Is(err, ErrIssuerMismatch) {
		t.Fatalf("err = %v, want ErrIssuerMismatch to replace the AS's error", err)
	}
	var te *TokenError
	if errors.As(err, &te) {
		t.Fatalf("the AS's error survived an issuer mismatch: %+v", te)
	}
}

// TestIssCheckLeavesNonResponseFailuresAlone: a login that failed before any
// response arrived — no browser, refused endpoint, deadline — has no iss to
// validate, and replacing its cause with a fabricated issuer mismatch would
// send the operator after the wrong thing.
func TestIssCheckLeavesNonResponseFailuresAlone(t *testing.T) {
	md := &AuthServerMetadata{
		Issuer: "https://honest.example",
		AuthorizationResponseIssParameterSupported: true,
	}
	want := errors.New("no browser on this host")
	if got := issThenCallback(md, "", true, want); !errors.Is(got, want) {
		t.Fatalf("got %v, want the original failure untouched", got)
	}
}

// TestAnErrorResponseWithAGoodIssStillSurfaces: the guard must not swallow a
// genuine denial. The user pressing Deny is the common case.
func TestAnErrorResponseWithAGoodIssStillSurfaces(t *testing.T) {
	md := &AuthServerMetadata{
		Issuer: "https://honest.example",
		AuthorizationResponseIssParameterSupported: true,
	}
	_, iss, cbErr := ParseManualCallback(
		"http://127.0.0.1:9/callback?error=access_denied&state=s&iss=https%3A%2F%2Fhonest.example", "s")
	err := issThenCallback(md, iss, true, cbErr)
	if !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("err = %v, want the denial to reach the caller", err)
	}
}
