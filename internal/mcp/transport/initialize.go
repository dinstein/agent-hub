package transport

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// Initialize performs the MCP handshake over an already-connected
// transport: it sends initialize declaring mcp.ProtocolVersion, validates
// the server's protocolVersion against mcp.SupportedVersions (downgrade
// accepted, anything else is a typed failure satisfying
// errors.Is(err, mcp.ErrUnsupportedVersion)), and on success sends the
// notifications/initialized notification.
//
// The returned InitializeResult carries the negotiated (server) version in
// ProtocolVersion. Handshake failures are ClassFatal: retrying the same
// handshake cannot succeed, so they must not trip the circuit breaker.
func Initialize(ctx context.Context, t Transport, clientInfo mcp.Implementation) (*mcp.InitializeResult, error) {
	params := mcp.InitializeParams{
		ProtocolVersion: mcp.ProtocolVersion,
		Capabilities: mcp.ClientCapabilities{
			// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28)
			Roots: &mcp.RootsCapability{ListChanged: true},
		},
		ClientInfo: clientInfo,
	}
	raw, err := t.Call(ctx, mcp.MethodInitialize, params)
	if err != nil {
		return nil, err // already a typed transport / context error
	}
	var res mcp.InitializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, &Error{Class: ClassFatal, Err: fmt.Errorf("decode initialize result: %w", err)}
	}
	if _, err := mcp.NegotiateVersion(res.ProtocolVersion); err != nil {
		return nil, &Error{Class: ClassFatal, Err: err}
	}
	if err := t.Notify(ctx, mcp.NotificationInitialized, nil); err != nil {
		return nil, err
	}
	return &res, nil
}
