package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// initializeLegacy performs the ≤ 2025-11-25 MCP handshake over an
// already-connected transport: it sends initialize declaring
// mcp.ProtocolVersion, validates the server's protocolVersion against
// mcp.SupportedVersions (downgrade accepted, anything else is a typed
// failure satisfying errors.Is(err, mcp.ErrUnsupportedVersion)), and on
// success sends the notifications/initialized notification.
//
// Handshake is the entry point; it lands here when the server does not
// speak 2026-07-28. The returned InitializeResult carries the negotiated
// (server) version in ProtocolVersion.
func initializeLegacy(ctx context.Context, t Transport, clientInfo mcp.Implementation) (*mcp.InitializeResult, error) {
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
// requests themselves; docs/status/mcp-2026-07-28.md §1.1).
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
// (fail closed; docs/status/mcp-2026-07-28.md §6.2).
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
	// ServerInfo is the server's self-reported identity: the top-level
	// serverInfo of a legacy initialize result, or the
	// io.modelcontextprotocol/serverInfo key of a discover result's _meta.
	// Both are self-reported and neither is verified; nothing may key a
	// security decision on it.
	ServerInfo mcp.Implementation
	// Instructions is the server's natural-language guidance. Both handshake
	// generations carry it — initialize as a top-level member, discover as
	// the DiscoverResult member of the same name.
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
	v, err := mcp.NegotiateHighest(dres.SupportedVersions)
	if err != nil {
		return nil, &Error{Class: ClassFatal, Err: err}
	}
	if v != mcp.Version2026 {
		return handshakeLegacy(ctx, t, clientInfo)
	}
	s, ok := t.(negotiatedSetter)
	if !ok {
		// The transport cannot inject the per-request _meta the stateless
		// protocol requires; sending bare requests would be rejected with
		// -32602 by a strict server. Refuse rather than degrade (the
		// "isolation claimed must be delivered or refused" direction).
		return nil, &Error{Class: ClassFatal, Err: fmt.Errorf(
			"transport %T cannot carry the per-request _meta MCP %s requires", t, v)}
	}
	s.setNegotiated(v, BuildRequestMeta(v, clientCapabilities2026(), clientInfo))
	return &HandshakeResult{
		Version:      v,
		Capabilities: dres.Capabilities,
		ServerInfo:   dres.ServerInfo(),
		Instructions: dres.Instructions,
	}, nil
}

// handshakeLegacy adapts the initializeLegacy result to a HandshakeResult.
func handshakeLegacy(ctx context.Context, t Transport, clientInfo mcp.Implementation) (*HandshakeResult, error) {
	res, err := initializeLegacy(ctx, t, clientInfo)
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
// (method-not-found from a well-behaved old server), or a ClassFatal HTTP
// 4xx (a 2025-11-25 streamable-http server rejects an unknown pre-session
// POST with 400 rather than a JSON-RPC error frame).
//
// An answered error is not by itself proof of an old server. The codes the
// 2026-07-28 specification allocates for itself are answers only a modern
// server knows how to give, and both transport bindings say so in as many
// words: "If the body contains a recognized modern JSON-RPC error, the
// server speaks a modern version of MCP — retry ... rather than falling
// back." Falling back on one of those turns a correctable request into a
// dead connection, because the initialize that follows is the one method
// such a server does not implement.
//
// Failure direction: everything else — connection loss, 5xx, oversized
// frame, context cancellation — propagates unchanged. Falling back there
// would hide a real failure from the circuit breaker behind a second
// handshake attempt (fail closed).
func discoverFallback(err error) bool {
	var me *mcp.Error
	if errors.As(err, &me) {
		return !mcp.IsSpecErrorCode(me.Code)
	}
	var te *Error
	if errors.As(err, &te) && te.Class == ClassFatal &&
		te.StatusCode >= 400 && te.StatusCode < 500 {
		// One 400 carries both a legacy server's opaque rejection and a
		// modern server's HeaderMismatch or UnsupportedProtocolVersion, so
		// the status cannot decide alone; the body's code can, when it has
		// one. No code parsed means no evidence, and the status stands.
		return !mcp.IsSpecErrorCode(te.RPCCode)
	}
	return false
}
