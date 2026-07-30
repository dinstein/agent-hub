package mcp

import (
	"errors"
	"fmt"
	"slices"
)

// Named version constants. Use these in version-conditional code paths rather
// than repeating string literals.
const (
	// Version2026 is the MCP 2026-07-28 specification: stateless per-request
	// _meta, server/discover, MRTR, subscriptions/listen, no Mcp-Session-Id.
	Version2026 = "2026-07-28"
	// Version2025 is the MCP 2025-11-25 specification: initialize handshake,
	// Mcp-Session-Id, server-initiated reverse RPCs, GET notification stream.
	Version2025 = "2025-11-25"
)

// ProtocolVersion is the MCP protocol version this client declares during
// handshake (canonical.md §5b). Pinned at 2025-11-25 while the 2026-07-28
// transport changes (stateless _meta, server/discover, MRTR) are being
// implemented; flipped to Version2026 as the final commit of Phase 1 — see
// docs/mcp-2026-07-28.md.
const ProtocolVersion = Version2025

// SupportedVersions lists every protocol version this facade accepts from a
// downstream server, newest first. NegotiateVersion and Handshake accept any
// version in this list; anything else fails the handshake.
var SupportedVersions = []string{
	Version2026,
	Version2025,
	"2025-06-18",
	"2025-03-26",
}

// ErrUnsupportedVersion is the decidable sentinel for a failed version
// negotiation.
var ErrUnsupportedVersion = errors.New("unsupported MCP protocol version")

// NegotiateVersion validates the protocolVersion a server returned from
// initialize. On success it returns the negotiated version (the server's).
// On failure the returned error satisfies
// errors.Is(err, ErrUnsupportedVersion).
func NegotiateVersion(serverVersion string) (string, error) {
	for _, v := range SupportedVersions {
		if v == serverVersion {
			return v, nil
		}
	}
	return "", fmt.Errorf("%w: server offered %q, supported: %v",
		ErrUnsupportedVersion, serverVersion, SupportedVersions)
}

// NegotiateHighest picks the newest version in SupportedVersions that the
// server also advertises. It is the multi-version counterpart of
// NegotiateVersion, used with the protocolVersions list a server/discover
// result carries (MCP 2026-07-28). On failure the returned error satisfies
// errors.Is(err, ErrUnsupportedVersion).
func NegotiateHighest(serverVersions []string) (string, error) {
	for _, v := range SupportedVersions {
		if slices.Contains(serverVersions, v) {
			return v, nil
		}
	}
	return "", fmt.Errorf("%w: server offered %v, supported: %v",
		ErrUnsupportedVersion, serverVersions, SupportedVersions)
}
