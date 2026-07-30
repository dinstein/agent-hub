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

// ProtocolVersion is the version declared where the STATEFUL (≤ 2025-11-25)
// protocol needs one: the legacy initialize handshake, the exposure side's
// default answer, and the HTTP header before negotiation. It stays at
// Version2025 deliberately — every context that reads it is definitionally
// pre-2026 (the legacy path only runs once server/discover has failed), and
// the 2026-07-28 declaration travels per-request in _meta instead, built by
// transport.BuildRequestMeta from Version2026 directly. See
// docs/mcp-2026-07-28.md §6.1 for the resolution of the original
// "flip this constant" plan.
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
