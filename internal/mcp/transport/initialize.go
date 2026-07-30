package transport

import (
	"context"
	"encoding/json"
	"errors"
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

// BuildRequestMeta produces the per-request _meta payload that every
// outgoing request must carry once the negotiated protocol version is
// mcp.Version2026 (stateless protocol — the handshake state moved into the
// requests themselves; docs/mcp-2026-07-28.md §1.1).
func BuildRequestMeta(version string, caps mcp.ClientCapabilities, info mcp.Implementation) *mcp.RequestMeta {
	return &mcp.RequestMeta{
		ProtocolVersion:    version,
		ClientCapabilities: caps,
		ClientInfo:         &info,
	}
}

// clientCapabilities2026 is what this client declares in 2026-07-28 request
// _meta. Empty until the MRTR coordinator (Phase 2) can actually answer
// input requests: a capability declared here invites input_required results,
// so declaring one we cannot serve would fail mid-call rather than up front
// (fail closed; docs/mcp-2026-07-28.md §6.2).
func clientCapabilities2026() mcp.ClientCapabilities {
	return mcp.ClientCapabilities{}
}

// Discover calls server/discover (MCP 2026-07-28) and returns the server's
// advertised versions, capabilities, and identity. Errors pass through from
// Transport.Call untouched so the caller can distinguish "server answered
// with an error" (old server — see Handshake) from connection failure.
func Discover(ctx context.Context, t Transport, clientInfo mcp.Implementation) (*mcp.DiscoverResult, error) {
	params := mcp.DiscoverParams{
		Meta: BuildRequestMeta(mcp.Version2026, clientCapabilities2026(), clientInfo),
	}
	raw, err := t.Call(ctx, mcp.MethodDiscover, params)
	if err != nil {
		return nil, err
	}
	var res mcp.DiscoverResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, &Error{Class: ClassFatal, Err: fmt.Errorf("decode discover result: %w", err)}
	}
	return &res, nil
}

// HandshakeResult is the outcome of Handshake, normalized across the
// discover (2026-07-28) and initialize (≤ 2025-11-25) paths.
type HandshakeResult struct {
	// Version is the negotiated protocol version. Every version-conditional
	// code path in this package gates on it; nothing above
	// internal/downstream needs to know which protocol is in use.
	Version string
	// Capabilities is the server's capability object, passed through raw.
	Capabilities json.RawMessage
	ServerInfo   mcp.Implementation
	// Instructions is only set by the legacy initialize path; the discover
	// result has no equivalent field.
	Instructions string
}

// Handshake negotiates the protocol version with an already-connected
// transport. It tries server/discover first: a 2026-07-28 server answers it
// (the version becomes whatever mcp.NegotiateHighest picks), while a server
// that rejects it — see discoverFallback for exactly what counts — gets the
// legacy Initialize handshake instead. A discover result that negotiates
// ≤ 2025-11-25 also runs Initialize afterward: those versions require the
// stateful handshake regardless of how the version was learned.
//
// Handshake failures are ClassFatal, same as Initialize: retrying the same
// handshake cannot succeed, so they must not trip the circuit breaker.
func Handshake(ctx context.Context, t Transport, clientInfo mcp.Implementation) (*HandshakeResult, error) {
	dres, err := Discover(ctx, t, clientInfo)
	if err != nil {
		if discoverFallback(err) {
			return handshakeLegacy(ctx, t, clientInfo)
		}
		return nil, err
	}
	v, err := mcp.NegotiateHighest(dres.ProtocolVersions)
	if err != nil {
		return nil, &Error{Class: ClassFatal, Err: err}
	}
	if v != mcp.Version2026 {
		return handshakeLegacy(ctx, t, clientInfo)
	}
	return &HandshakeResult{
		Version:      v,
		Capabilities: dres.Capabilities,
		ServerInfo:   dres.ServerInfo,
	}, nil
}

// handshakeLegacy adapts the Initialize result to a HandshakeResult.
func handshakeLegacy(ctx context.Context, t Transport, clientInfo mcp.Implementation) (*HandshakeResult, error) {
	res, err := Initialize(ctx, t, clientInfo)
	if err != nil {
		return nil, err
	}
	return &HandshakeResult{
		Version:      res.ProtocolVersion,
		Capabilities: res.Capabilities,
		ServerInfo:   res.ServerInfo,
		Instructions: res.Instructions,
	}, nil
}

// discoverFallback reports whether a server/discover failure means "alive
// but pre-2026", i.e. the legacy initialize handshake should run. True only
// when the server provably answered the request: a JSON-RPC error reply
// (method-not-found from a well-behaved old server, but any error object
// counts — a real 2026-07-28 server MUST implement discover, so an error
// reply is proof of an old one), or a ClassFatal HTTP 4xx (a 2025-11-25
// streamable-http server rejects an unknown pre-session POST with 400
// rather than a JSON-RPC error frame).
//
// Failure direction: everything else — connection loss, 5xx, oversized
// frame, context cancellation — propagates unchanged. Falling back there
// would hide a real failure from the circuit breaker behind a second
// handshake attempt (fail closed).
func discoverFallback(err error) bool {
	var me *mcp.Error
	if errors.As(err, &me) {
		return true
	}
	var te *Error
	if errors.As(err, &te) && te.Class == ClassFatal &&
		te.StatusCode >= 400 && te.StatusCode < 500 {
		return true
	}
	return false
}
