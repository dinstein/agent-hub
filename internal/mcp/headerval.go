package mcp

import (
	"encoding/base64"
	"strings"
)

// The sentinel MCP 2026-07-28 defines for a header value that could not be
// carried as plain ASCII. Both markers are case-sensitive and must appear
// exactly as written.
const (
	headerB64Prefix = "=?base64?"
	headerB64Suffix = "?="
)

// EncodeHeaderValue renders v for the Mcp-Name / Mcp-Param-* headers MCP
// 2026-07-28 mirrors body values into.
//
// RFC 9110 restricts a field value to visible ASCII, space and horizontal
// tab, and forbids leading or trailing whitespace; a value outside that set
// is carried as `=?base64?<base64 of the UTF-8 bytes>?=` instead. A
// plain-ASCII value that merely looks like the sentinel is encoded too,
// because the receiver has no other way to tell the two apart.
//
// This is not cosmetic. Go's net/http sends a non-ASCII value as raw UTF-8,
// which the receiver reads as mojibake and rejects as a header mismatch; it
// silently trims a padded one, so the header and body disagree; and it
// refuses to send a value containing a newline at all, failing the whole
// request. All three are reachable from a downstream tool name, which the
// specification only SHOULD-constrains to header-safe characters.
func EncodeHeaderValue(v string) string {
	if !needsHeaderEncoding(v) {
		return v
	}
	return headerB64Prefix + base64.StdEncoding.EncodeToString([]byte(v)) + headerB64Suffix
}

// DecodeHeaderValue returns the body value a header carries: the decoded
// bytes when it uses the sentinel, and the value unchanged when it does not.
// ok is false for a sentinel-shaped value whose payload is not valid base64
// — a receiver must never fall back to comparing the raw text there, because
// that is precisely how a mismatched header would slip past validation.
//
// Servers MUST decode before comparing a mirrored header to the body
// (2026-07-28, streamable-http "Server Validation"); comparing the encoded
// form would reject every conformant client that had to encode.
func DecodeHeaderValue(v string) (string, bool) {
	body, found := strings.CutPrefix(v, headerB64Prefix)
	if !found {
		return v, true
	}
	body, found = strings.CutSuffix(body, headerB64Suffix)
	if !found {
		// A value that opens the sentinel and never closes it is malformed,
		// not a literal: treating it as one would let a crafted name carry a
		// prefix the validator then compares against the wrong thing.
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// needsHeaderEncoding reports whether v must go out under the sentinel.
func needsHeaderEncoding(v string) bool {
	if v == "" {
		return false
	}
	if strings.HasPrefix(v, headerB64Prefix) && strings.HasSuffix(v, headerB64Suffix) {
		return true // would otherwise be read as an encoded value
	}
	if isHeaderSpace(v[0]) || isHeaderSpace(v[len(v)-1]) {
		return true // net/http trims these away, and the body keeps them
	}
	for i := range len(v) {
		if c := v[i]; c < 0x20 || c > 0x7e {
			return true // control byte, or a byte of a multi-byte rune
		}
	}
	return false
}

func isHeaderSpace(c byte) bool { return c == ' ' || c == '\t' }
