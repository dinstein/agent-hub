package oauthflow

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
)

// randRead is the entropy source. Tests replace it to prove the failure
// path; production always uses crypto/rand.
//
// It is deliberately a plain package variable and NOT part of any config
// struct: making the entropy source configurable at runtime would create
// exactly the downgrade path this package refuses to have.
var randRead = rand.Read

// newRandomToken returns a base64url (unpadded) token over n random bytes.
//
// Failure direction: a crypto/rand failure returns ErrEntropy. There is no
// fallback to math/rand, to a time seed, or to a shorter token. Every
// caller of this function is generating a value whose unpredictability is
// the whole security property (PKCE verifier, state, device nonce), so a
// degraded value is strictly worse than a failed login.
func newRandomToken(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("oauthflow: invalid token size %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(readerFunc(randRead), b); err != nil {
		return "", newFlowError(ErrorTypeEntropy, fmt.Errorf("%w: %v", ErrEntropy, err))
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// readerFunc adapts randRead to io.Reader so io.ReadFull enforces the full
// length (a short read must never silently shorten a verifier).
type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// correlationID returns a short diagnostic ID for FlowError. Unlike the
// security-bearing tokens above it MAY degrade: an unusable correlation ID
// must never turn a working login into a failure, so entropy failure
// yields a fixed placeholder that is obviously not a real ID.
func correlationID() string {
	b := make([]byte, 6)
	if _, err := randRead(b); err != nil {
		return "corr-unavailable"
	}
	return hex.EncodeToString(b)
}
