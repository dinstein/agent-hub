package mcp

import (
	"errors"
	"fmt"
)

// ProtocolVersion is the MCP protocol version this client declares in
// initialize (canonical.md §5b: target 2025-11-25).
const ProtocolVersion = "2025-11-25"

// SupportedVersions lists every protocol version this facade accepts from a
// server, newest first. A server answering initialize with any of these is
// accepted (downgrade negotiation); anything else fails the handshake.
var SupportedVersions = []string{
	"2025-11-25",
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
