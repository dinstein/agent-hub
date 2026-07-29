package leakguard

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/dinstein/agent-hub/internal/guard/netguard"
)

// This file holds the false-positive gates. They are the reason inline
// redaction can be offered at all: a rule table without them would rewrite
// every README that mentions a token format.
//
// Failure direction of every validator: FAIL-TO-FALSE. Input that cannot be
// decoded, parsed or classified is NOT reported — the scanner stays quiet
// rather than guessing, because a noisy leak detector is one that gets
// switched off. The narrow exception is isInternalHost, which only chooses
// BETWEEN two rules of equal severity, so its uncertainty costs nothing.

// placeholderMarkers are substrings that mark a sample rather than a secret.
// Matching is case-insensitive. The risk of one of these appearing inside a
// random 40-character token is negligible; the risk of documentation samples
// flooding an audit stream is not.
var placeholderMarkers = []string{
	"example", "placeholder", "your-", "your_", "yourtoken", "yourkey",
	"changeme", "change_me", "redacted", "dummy", "sample", "insert",
	"todo", "fixme", "xxxxx", "notreal", "fake", "test-token", "testtoken",
	"my-secret", "mysecret", "secret-here", "<", "…", "***",
}

// placeholderWords are whole-value placeholders — the literal password every
// connection-string example in the world uses.
var placeholderWords = map[string]bool{
	"password": true, "passwd": true, "pass": true, "pwd": true,
	"secret": true, "changeme": true, "mypassword": true, "yourpassword": true,
	"user": true, "username": true, "admin": true, "root": true,
	"token": true, "apikey": true, "api_key": true, "hunter2": true,
	"redacted": true, "none": true, "null": true, "undefined": true,
}

// isPlaceholder reports whether s is documentation rather than a credential:
// a known marker, a known whole-value word, or a value with too few distinct
// characters to be random (ghp_xxxxxxxx…, AKIA0000000000000000).
func isPlaceholder(s string) bool {
	lower := strings.ToLower(s)
	if placeholderWords[lower] {
		return true
	}
	for _, marker := range placeholderMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	// A real credential's body is high-variety. Measure the body, not the
	// vendor prefix: `ghp_` and `AKIA` are constant by construction.
	body := s
	if i := strings.LastIndexAny(body, "_-"); i >= 0 && i < len(body)-8 {
		body = body[i+1:]
	}
	return distinctChars(body) < 5
}

// distinctChars counts distinct bytes in s (bounded by 256 by construction).
func distinctChars(s string) int {
	var seen [256]bool
	n := 0
	for i := 0; i < len(s); i++ {
		if !seen[s[i]] {
			seen[s[i]] = true
			n++
		}
	}
	return n
}

// isPasswordLike reports whether s is plausibly a real password rather than a
// placeholder, a port number or a fragment of a URL.
func isPasswordLike(s string) bool {
	if len(s) < 6 || isPlaceholder(s) {
		return false
	}
	if allDigits(s) {
		return false // a port, a phone number, an id — not a password
	}
	return distinctChars(s) >= 4
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// jwtHeaderHasAlg reports whether the first dot-separated segment of tok
// decodes (base64url, unpadded) to a JSON object carrying a non-empty string
// `alg`. That is the structural evidence that turns "three base64-ish runs"
// into "a token".
func jwtHeaderHasAlg(tok string) bool {
	head, _, ok := strings.Cut(tok, ".")
	if !ok || head == "" {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(head, "="))
	if err != nil {
		return false
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(raw, &hdr); err != nil {
		return false
	}
	return hdr.Alg != ""
}

// cardLengths are the payment-card number lengths in circulation.
var cardLengths = map[int]bool{13: true, 14: true, 15: true, 16: true, 19: true}

// isPlausibleCard reports whether run — a digit run with optional single
// space/dash separators — is a payment card number: uniform separators, a
// plausible length, a known issuer prefix, and a valid Luhn checksum.
//
// All four gates are needed. Luhn alone accepts roughly one in ten random
// digit runs, which in a tool result full of ids is a false-positive machine.
func isPlausibleCard(run string) bool {
	digits, ok := splitCardDigits(run)
	if !ok {
		return false
	}
	if !cardLengths[len(digits)] || !cardIssuerKnown(digits) {
		return false
	}
	return luhnValid(digits)
}

// splitCardDigits strips separators and rejects mixed separator styles
// ("4111-1111 1111 1111" is a concatenation artefact, not a card).
func splitCardDigits(run string) (digits string, ok bool) {
	var b strings.Builder
	b.Grow(len(run))
	var sep byte
	for i := 0; i < len(run); i++ {
		c := run[i]
		switch {
		case c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == ' ' || c == '-':
			if sep != 0 && sep != c {
				return "", false
			}
			sep = c
		default:
			return "", false
		}
	}
	return b.String(), true
}

// cardIssuerKnown reports whether digits start with an issuer prefix that is
// actually assigned (Visa, Mastercard incl. the 2-series, Amex, Discover,
// JCB, Diners, UnionPay, Maestro).
func cardIssuerKnown(d string) bool {
	switch {
	case d[0] == '4': // Visa
		return true
	case len(d) >= 2 && d[0] == '5' && d[1] >= '1' && d[1] <= '5': // Mastercard
		return true
	case len(d) >= 2 && d[0] == '3' && (d[1] == '4' || d[1] == '7'): // Amex
		return true
	case len(d) >= 2 && d[0] == '3' && (d[1] == '5' || d[1] == '6' || d[1] == '8'): // JCB / Diners
		return true
	case len(d) >= 3 && d[:3] >= "300" && d[:3] <= "305": // Diners
		return true
	case len(d) >= 4 && d[:4] == "6011", len(d) >= 2 && d[:2] == "65": // Discover
		return true
	case len(d) >= 2 && d[:2] == "62": // UnionPay
		return true
	case len(d) >= 4 && d[:4] >= "2221" && d[:4] <= "2720": // Mastercard 2-series
		return true
	case len(d) >= 4 && (d[:4] == "5018" || d[:4] == "5020" || d[:4] == "5038" || d[:4] == "6759"): // Maestro
		return true
	}
	return false
}

// luhnValid computes the Luhn checksum over an all-digit string.
func luhnValid(d string) bool {
	if len(d) < 2 {
		return false
	}
	sum := 0
	double := false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if double {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return sum%10 == 0
}

// internalSuffixes are the name spaces that only ever exist inside a network.
var internalSuffixes = []string{
	".internal", ".local", ".lan", ".corp", ".intranet", ".home", ".localdomain", ".private",
}

// isInternalHost reports whether host (possibly with :port) names something
// on a private network: a literal private address, a localhost name, a
// site-internal suffix, or a single-label name (no dot at all — "db",
// "redis", a Docker Compose service).
//
// DNS is never consulted: a scanner must not make network calls, and
// netguard.HostIsDefinitelyPrivate is the fail-to-false predicate for exactly
// this "classify, do not resolve" direction. Uncertainty therefore lands on
// credential-url rather than internal-credential-url — same severity, same
// redaction, only the audit label differs.
func isInternalHost(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[i+1:], ":") && !strings.Contains(h, "]") {
		if isPort(h[i+1:]) {
			h = h[:i]
		}
	}
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return false
	}
	if netguard.HostIsDefinitelyPrivate(h) {
		return true
	}
	for _, suffix := range internalSuffixes {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return !strings.Contains(h, ".")
}

func isPort(s string) bool { return s != "" && len(s) <= 5 && allDigits(s) }
