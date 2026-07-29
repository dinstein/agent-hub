package oauthflow

import (
	"errors"
	"testing"
)

// TestParseManualCallbackMatrix is the paste-parsing matrix of docs/modules/oauth.md
// (b). The load-bearing rows are the state-mismatch ones: state is what
// stops a user from pasting a callback belonging to a different session
// (their own earlier attempt, or one somebody sent them).
func TestParseManualCallbackMatrix(t *testing.T) {
	const want = "state-good"
	cases := []struct {
		name     string
		input    string
		wantCode string
		wantErr  error
	}{
		{
			name:     "full loopback callback url",
			input:    "http://127.0.0.1:5731/callback?code=abc123&state=state-good",
			wantCode: "abc123",
		},
		{
			name:     "https callback url from another host",
			input:    "https://app.example.com/cb?code=abc123&state=state-good",
			wantCode: "abc123",
		},
		{
			name:     "parameter order does not matter",
			input:    "http://127.0.0.1:5731/callback?state=state-good&code=abc123",
			wantCode: "abc123",
		},
		{
			name:     "extra provider parameters are ignored",
			input:    "http://127.0.0.1:1/callback?code=abc123&state=state-good&iss=https%3A%2F%2Fas",
			wantCode: "abc123",
		},
		{
			name:     "url-encoded code is decoded",
			input:    "http://127.0.0.1:1/callback?code=a%2Fb%2Bc&state=state-good",
			wantCode: "a/b+c",
		},
		{
			name:     "leading question mark query fragment",
			input:    "?code=abc123&state=state-good",
			wantCode: "abc123",
		},
		{
			name:     "bare query string",
			input:    "code=abc123&state=state-good",
			wantCode: "abc123",
		},
		{
			name:     "surrounding whitespace is stripped",
			input:    "  http://127.0.0.1:1/callback?code=abc123&state=state-good \n",
			wantCode: "abc123",
		},
		{
			name:     "angle brackets from mail clients are stripped",
			input:    "<http://127.0.0.1:1/callback?code=abc123&state=state-good>",
			wantCode: "abc123",
		},
		{
			name:     "quotes are stripped",
			input:    `"http://127.0.0.1:1/callback?code=abc123&state=state-good"`,
			wantCode: "abc123",
		},
		{
			name:     "bare code is accepted (PKCE still protects it)",
			input:    "abc123",
			wantCode: "abc123",
		},
		{
			name:    "state mismatch is refused",
			input:   "http://127.0.0.1:1/callback?code=abc123&state=state-other",
			wantErr: ErrStateMismatch,
		},
		{
			name:    "missing state on a url is a mismatch, not a free pass",
			input:   "http://127.0.0.1:1/callback?code=abc123",
			wantErr: ErrStateMismatch,
		},
		{
			name:    "empty state on a url is a mismatch",
			input:   "http://127.0.0.1:1/callback?code=abc123&state=",
			wantErr: ErrStateMismatch,
		},
		{
			name:    "state mismatch outranks a present code",
			input:   "code=abc123&state=nope",
			wantErr: ErrStateMismatch,
		},
		{
			name:    "authorization server denial",
			input:   "http://127.0.0.1:1/callback?error=access_denied&error_description=user+said+no&state=state-good",
			wantErr: ErrAuthorizationDenied,
		},
		{
			name:    "authorization server error other than denial",
			input:   "http://127.0.0.1:1/callback?error=invalid_scope&state=state-good",
			wantErr: nil, // checked separately below
		},
		{
			name:    "url with no query at all",
			input:   "http://127.0.0.1:1/callback",
			wantErr: ErrMalformedCallback,
		},
		{
			name:    "nothing pasted",
			input:   "   ",
			wantErr: ErrMalformedCallback,
		},
		{
			name:    "mangled paste with whitespace inside",
			input:   "abc 123",
			wantErr: ErrMalformedCallback,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, err := ParseManualCallback(tc.input, want)
			switch {
			case tc.wantCode != "":
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if code != tc.wantCode {
					t.Fatalf("code = %q want %q", code, tc.wantCode)
				}
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v want %v", err, tc.wantErr)
				}
				if code != "" {
					t.Fatalf("a failed parse returned code %q", code)
				}
			default:
				if err == nil {
					t.Fatalf("expected an error, got code %q", code)
				}
			}
		})
	}
}

// TestManualStateMismatchIsLoud: the error must name the remedy, because a
// state mismatch in manual mode is almost always "pasted the wrong tab".
func TestManualStateMismatchIsLoud(t *testing.T) {
	_, err := ParseManualCallback("http://127.0.0.1:1/cb?code=c&state=other", "mine")
	var fe *FlowError
	if !errors.As(err, &fe) {
		t.Fatalf("not a FlowError: %v", err)
	}
	if fe.Suggestion == "" {
		t.Fatal("state mismatch must carry a suggestion")
	}
	if fe.Type != ErrorTypeAuthorization {
		t.Fatalf("type = %s", fe.Type)
	}
}

// TestManualBareCodeSkipsStateCheck documents the deliberate asymmetry: a
// bare code carries no state to check, and PKCE is what protects it.
func TestManualBareCodeSkipsStateCheck(t *testing.T) {
	code, err := ParseManualCallback("XYZ-987", "any-state-at-all")
	if err != nil {
		t.Fatal(err)
	}
	if code != "XYZ-987" {
		t.Fatalf("code = %q", code)
	}
}

func TestManualInstructionsCarryState(t *testing.T) {
	in := NewManualInstructions("https://as/authorize?x=1", ManualRedirectURI, "st")
	if in.AuthorizationURL == "" || in.RedirectURI == "" || in.State != "st" {
		t.Fatalf("instructions = %+v", in)
	}
	if CallbackPortOf(ManualRedirectURI) != 8642 {
		t.Fatalf("port = %d", CallbackPortOf(ManualRedirectURI))
	}
	if CallbackPortOf("not a url") != 0 {
		t.Fatal("unparsable redirect must yield port 0")
	}
}
