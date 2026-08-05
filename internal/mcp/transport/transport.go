// Package transport provides MCP transports beneath internal/downstream
// (; path ruled canonical in canonical.md — the old
// internal/downstream/transport name is dead).
//
// Three read-side transports live here (canonical.md §5b — the read side
// does all three, the exposure side offers streamable-http only):
// stdio, streamable-http (MCP 2025-11-25) and legacy HTTP+SSE. Like its
// parent package it depends on the standard library only
// (depguard-enforced), which is why SSRF screening is an injected
// HTTPConfig.DialContext rather than an import of internal/guard/netguard.
//
// Invariants owned by this package:
//
//   - bounded read: frames (and single SSE events) over mcp.MaxFrameSize
//     poison the connection,
//   - peer requests (server-initiated reverse RPC such as roots/list) are
//     answered inline from the read loop,
//   - context cancellation forwards notifications/cancelled downstream
//     (best-effort) before returning the context error,
//   - process exit / stdout close fails every pending call with
//     ClassUnavailable,
//   - the last 4 KiB of the child's stderr is retained for error reports.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// Kind names a transport implementation.
type Kind string

// Transport kinds. All four are implemented.
const (
	Stdio          Kind = "stdio"
	StreamableHTTP Kind = "http"
	SSE            Kind = "sse"
)

// ChangeMask is a bitmask of list_changed notification categories.
type ChangeMask uint32

// ChangeMask bits: TOOLS | RESOURCES | PROMPTS.
const (
	ChangeTools ChangeMask = 1 << iota
	ChangeResources
	ChangePrompts
)

// Has reports whether all bits of m2 are set in m.
func (m ChangeMask) Has(m2 ChangeMask) bool { return m&m2 == m2 }

func (m ChangeMask) String() string {
	var parts []string
	if m.Has(ChangeTools) {
		parts = append(parts, "tools")
	}
	if m.Has(ChangeResources) {
		parts = append(parts, "resources")
	}
	if m.Has(ChangePrompts) {
		parts = append(parts, "prompts")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "|")
}

// PeerHandler handles a server-initiated reverse RPC (roots/list, sampling,
// elicitation). It runs inline on the stdio read loop, so it must not block
// on the same Transport (no Call from inside a PeerHandler) and should
// return promptly. Returning an error (or a nil response) produces a
// JSON-RPC internal-error reply; the response id is forced to the request
// id by the transport.
type PeerHandler func(ctx context.Context, req *mcp.Request) (*mcp.Response, error)

// Transport is the interface every MCP transport implements.
type Transport interface {
	// Call performs one JSON-RPC request. params may be nil, a
	// json.RawMessage (sent verbatim), or any marshalable value. On a
	// JSON-RPC error response it returns *Error with ClassFatal wrapping
	// *mcp.Error; on connection failure *Error with ClassUnavailable; on
	// ctx cancellation it forwards notifications/cancelled (best-effort)
	// and returns the context error.
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)

	// Notify sends a JSON-RPC notification (no reply expected).
	Notify(ctx context.Context, method string, params any) error

	// OnPeerRequest registers the handler for server-initiated reverse
	// RPCs. Stdio answers inline in the read loop; SSE handles them on an
	// independent goroutine and POSTs the reply back. Without a handler,
	// peer requests are answered with method-not-found.
	OnPeerRequest(h PeerHandler)

	// OnListChanged registers the callback for list_changed notifications.
	// The callback runs on the read loop and must not block.
	OnListChanged(fn func(mask ChangeMask))

	// Stderr returns the tail (last 4 KiB) of the child process stderr for
	// error reporting. Non-stdio implementations return "".
	Stderr() string

	// Close tears the transport down. Pending calls fail with
	// ClassUnavailable wrapping ErrClosed. Close is idempotent.
	Close() error
}

// Class classifies a transport error for the circuit breaker and retry
// logic in internal/downstream.
type Class int

// Error classes. tools/call is not idempotent, so only errors that
// provably never reached the server (plus 429) may be ClassRetry.
const (
	// ClassFatal: an ordinary error response — does not count toward the
	// circuit breaker.
	ClassFatal Class = iota
	// ClassUnavailable: connection-level failure — counts toward the
	// circuit breaker.
	ClassUnavailable
	// ClassRetry: the request never reached the server, or the server
	// answered 429 — safe to retry.
	ClassRetry
)

func (c Class) String() string {
	switch c {
	case ClassFatal:
		return "fatal"
	case ClassUnavailable:
		return "unavailable"
	case ClassRetry:
		return "retry"
	default:
		return fmt.Sprintf("class(%d)", int(c))
	}
}

// ErrClosed reports use of a Transport after Close.
var ErrClosed = errors.New("transport closed")

// ErrDeadConnection marks a call REJECTED BEFORE IT WAS SENT because the
// connection had already failed. It is the pre-send half of
// ClassUnavailable, and the distinction is what makes an automatic
// reconnect sound: the request provably never reached the server, so
// replaying it on a fresh connection cannot double-execute a
// non-idempotent tools/call.
//
// A connection that dies AFTER the request was written must NOT carry this
// marker — the server may have executed the call and only the reply was
// lost. Wrap it only where nothing was put on the wire.
var ErrDeadConnection = errors.New("connection already failed")

// Error is the typed transport error. RetryAfter is only
// meaningful for ClassRetry (e.g. a 429 Retry-After hint, M1).
type Error struct {
	Class      Class
	RetryAfter time.Duration
	// StatusCode is the HTTP status that produced this error, or 0 when the
	// error did not come from an HTTP response (stdio, dial failures).
	//
	// It exists so callers can classify by STATUS rather than by grepping
	// Error() for "http 401". That substring search was the only route
	// available and it reads the response-body snippet too, so a proxy
	// answering 502 with a body that mentions an upstream 401 was classified
	// as "your credentials were rejected" — sending the operator to re-run
	// `auth login` for a problem no credential can fix.
	StatusCode int
	// RPCCode is the JSON-RPC error code the rejected HTTP body carried, or
	// 0 when it carried none. On MCP 2026-07-28 a 400 alone is ambiguous —
	// HeaderMismatch, UnsupportedProtocolVersion and
	// MissingRequiredClientCapability all use it — so the code is what tells
	// a caller whether the peer answered as a modern MCP server at all.
	RPCCode int
	Err     error
}

// StatusOf returns the HTTP status behind err, or 0 if it did not come from
// an HTTP response. It unwraps, because transport errors reach their callers
// wrapped in several layers of context.
func StatusOf(err error) int {
	var te *Error
	if errors.As(err, &te) {
		return te.StatusCode
	}
	return 0
}

// IsAuthStatus reports whether err is an HTTP rejection of our CREDENTIALS
// (401 or 403) rather than any other failure.
//
// It lives here because "which statuses mean the credential was refused" is a
// property of the protocol, not of any one front end — and it had been
// answered independently, by substring match, in both internal/cli and
// internal/ctlapi.
func IsAuthStatus(err error) bool {
	switch StatusOf(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	return false
}

func (e *Error) Error() string {
	return fmt.Sprintf("transport error (%s): %v", e.Class, e.Err)
}

// Unwrap exposes the cause so callers can errors.Is against the mcp
// sentinels (ErrFrameTooLarge, ErrMalformedFrame, ...).
func (e *Error) Unwrap() error { return e.Err }
