package oauthflow

import (
	"crypto/sha256"
	"encoding/base64"
)

// ChallengeMethodS256 is the only code_challenge_method this package will
// ever emit. RFC 7636 also defines "plain"; agenthub does not implement it,
// so there is no code path that could be talked into it by a hostile or
// merely sloppy authorization server metadata document.
const ChallengeMethodS256 = "S256"

// pkceVerifierBytes is the raw entropy behind a verifier. 32 bytes encode
// to 43 base64url characters, the RFC 7636 minimum length, at the maximum
// entropy the minimum length can carry.
const pkceVerifierBytes = 32

// stateBytes is the raw entropy behind the `state` parameter. state is the
// cross-flow/CSRF binding, checked locally in all three modes.
const stateBytes = 32

// PKCE is one authorization request's proof key.
type PKCE struct {
	// Verifier is the secret; it is sent only in the token request.
	Verifier string
	// Challenge is base64url(sha256(Verifier)), sent in the authorization
	// request.
	Challenge string
	// Method is always ChallengeMethodS256.
	Method string
}

// NewPKCE generates a fresh S256 proof key.
//
// Failure direction: an entropy failure returns a *FlowError wrapping
// ErrEntropy. It never returns a PKCE with Method "plain" and never returns
// a partially-populated value — callers may assume a non-nil result is
// fully valid.
func NewPKCE() (*PKCE, error) {
	v, err := newRandomToken(pkceVerifierBytes)
	if err != nil {
		return nil, err
	}
	return &PKCE{Verifier: v, Challenge: challengeS256(v), Method: ChallengeMethodS256}, nil
}

// NewState generates a fresh `state` value.
func NewState() (string, error) { return newRandomToken(stateBytes) }

func challengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// SupportsS256 reports whether metadata advertises S256. An AS that
// advertises only "plain" is refused rather than accommodated; an AS that
// advertises nothing is accepted, because omitting
// code_challenge_methods_supported is common and RFC 7636 servers are
// required to support S256.
func SupportsS256(md *AuthServerMetadata) bool {
	if md == nil || len(md.CodeChallengeMethodsSupported) == 0 {
		return true
	}
	for _, m := range md.CodeChallengeMethodsSupported {
		if m == ChallengeMethodS256 {
			return true
		}
	}
	return false
}
