package logx

import (
	"strings"
	"testing"
)

// TestScrubRedactsEveryAuthScheme is the regression for the finding the
// 2026-07-31 sweep raised.
//
// The value pattern consumed an optional "bearer " prefix and nothing else,
// so every other RFC 7235 scheme had its NAME redacted and its credential
// printed: `Authorization: Basic dXNlcjpwYXNz` became
// `Authorization: [REDACTED] dXNlcjpwYXNz`. That is worse than an obvious
// leak, because the line reads as though the secret had been removed.
//
// The package header states the failure direction this restores: false
// positives are acceptable, a leaked credential is not.
func TestScrubRedactsEveryAuthScheme(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		secret string
	}{
		{"basic", `Authorization: Basic dXNlcjpwYXNzd29yZA==`, "dXNlcjpwYXNzd29yZA=="},
		{"negotiate", `Proxy-Authorization: Negotiate YIIZlgYGKwYBBQUCoIIZ`, "YIIZlgYGKwYBBQUCoIIZ"},
		{"ntlm", `authorization=NTLM TlRMTVNTUAABAAAA`, "TlRMTVNTUAABAAAA"},
		{"dpop", `Authorization: DPoP eyJhbGciOiJFUzI1NiJ9x`, "eyJhbGciOiJFUzI1NiJ9x"},
		{"digest, multi-part", `authorization=Digest username="admin", response=deadbeefcafe`, "deadbeefcafe"},
		{"bearer still works", `Authorization: Bearer sekrit-value`, "sekrit-value"},
		{"lower-cased scheme", `authorization: basic dXNlcjpwYXNz`, "dXNlcjpwYXNz"},
		{"no scheme at all", `api_key=abcdef123456`, "abcdef123456"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScrubString(tc.in)
			if strings.Contains(got, tc.secret) {
				t.Errorf("the credential survived scrubbing\n  in:  %s\n  out: %s", tc.in, got)
			}
			if !strings.Contains(got, Redacted) {
				t.Errorf("nothing was redacted at all\n  in:  %s\n  out: %s", tc.in, got)
			}
		})
	}
}

// TestScrubLeavesNonSecretsOnTheLine bounds the widened match. The scheme
// set is closed precisely so an ordinary sensitive key=value assignment
// keeps its whitespace boundary and the diagnostics beside it survive.
func TestScrubLeavesNonSecretsOnTheLine(t *testing.T) {
	in := `env AGENTHUB_SECRET_GITHUB=ghp_realsecretvalue loaded for server=github`
	got := ScrubString(in)
	if strings.Contains(got, "ghp_realsecretvalue") {
		t.Fatalf("the credential survived:\n  %s", got)
	}
	for _, keep := range []string{"loaded for", "server=github"} {
		if !strings.Contains(got, keep) {
			t.Errorf("scrubbing ate %q, which is not a secret:\n  in:  %s\n  out: %s", keep, in, got)
		}
	}
}

// TestScrubDoesNotEatTheRestOfALogLine is the same property for logfmt: a
// value with no scheme token stays whitespace-bounded, so the diagnostic
// fields after it are still readable.
func TestScrubDoesNotEatTheRestOfALogLine(t *testing.T) {
	in := `msg=refresh token_expires_at=2026-01-01 server=github attempt=3`
	got := ScrubString(in)
	for _, keep := range []string{"server=github", "attempt=3"} {
		if !strings.Contains(got, keep) {
			t.Errorf("scrubbing ate %s:\n  in:  %s\n  out: %s", keep, in, got)
		}
	}
}
