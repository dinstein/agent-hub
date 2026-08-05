package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Version is the only JSON-RPC protocol version MCP uses.
const Version = "2.0"

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// MCP-specification error codes in the reserved range -32020 to -32099.
// Codes -32000 to -32019 are legacy (grandfathered SDK allocations); new
// allocations must not use that sub-range. See MCP 2026-07-28 §error-codes.
const (
	// CodeHeaderMismatch is returned when a required protocol header is
	// present but its value conflicts with the request body (e.g. the
	// Mcp-Method header names a different method than the JSON-RPC body).
	CodeHeaderMismatch = -32020

	// CodeMissingRequiredClientCapability is returned when processing a
	// request requires a client capability not declared in the request's
	// _meta.io.modelcontextprotocol/clientCapabilities field.
	CodeMissingRequiredClientCapability = -32021

	// CodeUnsupportedProtocolVersion is returned when the protocol version
	// in _meta.io.modelcontextprotocol/protocolVersion is not supported by
	// the server.
	CodeUnsupportedProtocolVersion = -32022
)

// Bounds of the sub-range MCP 2026-07-28 reserves for codes the
// specification itself allocates. -32000 to -32019 stays implementation
// defined and is deliberately excluded: an SDK's own code in that band says
// nothing about which protocol generation the peer speaks.
const (
	specErrorCodeMax = -32020
	specErrorCodeMin = -32099
)

// UnsupportedVersionData is the data payload a CodeUnsupportedProtocolVersion
// error carries. The specification declares it required, and the reason is
// operational rather than formal: the backward-compatibility flow tells a
// client that meets this error to retry with a version from Supported, which
// it cannot do if the list never arrives.
type UnsupportedVersionData struct {
	// Supported lists the versions this peer can serve, for the client to
	// choose from.
	Supported []string `json:"supported"`
	// Requested echoes the version that was refused.
	Requested string `json:"requested"`
}

// NewUnsupportedVersionError builds the complete -32022 error, payload
// included. Use it rather than assembling an Error by hand: a bare -32022
// tells a client it must change something without telling it to what.
func NewUnsupportedVersionError(requested string, supported []string, message string) *Error {
	data, err := json.Marshal(UnsupportedVersionData{Supported: supported, Requested: requested})
	if err != nil {
		// Marshaling a []string and a string cannot fail.
		panic(err)
	}
	return &Error{Code: CodeUnsupportedProtocolVersion, Message: message, Data: data}
}

// IsSpecErrorCode reports whether code falls in that reserved sub-range.
//
// It answers one question for its callers: could only a peer that knows the
// 2026-07-28 specification have produced this? True is proof the peer is
// modern, which is why the backward-compatibility probe must not read such a
// reply as "old server" and fall back to initialize (see the transport
// package's discoverFallback). Codes the specification has not allocated yet
// are included on purpose — a future one is still an answer no pre-2026 peer
// would know how to give.
func IsSpecErrorCode(code int) bool {
	return code <= specErrorCodeMax && code >= specErrorCodeMin
}

// ErrMalformedFrame is the decidable sentinel for a frame that is not a
// valid JSON-RPC 2.0 message (bad JSON, wrong "jsonrpc" version, invalid id
// type, or none of request/response/notification). Errors returned by
// ParseMessage satisfy errors.Is(err, ErrMalformedFrame).
var ErrMalformedFrame = errors.New("malformed jsonrpc frame")

// ID is a JSON-RPC request id. The spec allows strings and numbers; the raw
// JSON text is preserved so ids received from a peer are echoed back
// byte-for-byte (including number formatting beyond float64 precision).
// The zero value is "unset" and marshals as null.
type ID struct {
	raw string
}

// NewIntID returns a numeric ID.
func NewIntID(n int64) ID { return ID{raw: strconv.FormatInt(n, 10)} }

// NewStringID returns a string ID.
func NewStringID(s string) ID {
	b, err := json.Marshal(s)
	if err != nil {
		// Marshaling a string cannot fail.
		panic(err)
	}
	return ID{raw: string(b)}
}

// IsSet reports whether the ID carries a value. Response ids for pending-call
// matching must be set; an unset ID marshals as null (used only for
// protocol-level error responses that cannot name a request).
func (id ID) IsSet() bool { return id.raw != "" }

// Key returns a stable map key. String and number ids never collide because
// string keys keep their JSON quotes.
func (id ID) Key() string { return id.raw }

// String returns the raw JSON text of the id (or "<unset>").
func (id ID) String() string {
	if !id.IsSet() {
		return "<unset>"
	}
	return id.raw
}

// MarshalJSON implements json.Marshaler.
func (id ID) MarshalJSON() ([]byte, error) {
	if !id.IsSet() {
		return []byte("null"), nil
	}
	return []byte(id.raw), nil
}

// UnmarshalJSON implements json.Unmarshaler. Only strings and numbers are
// accepted; null yields the unset ID.
func (id *ID) UnmarshalJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("%w: invalid id: %v", ErrMalformedFrame, err)
	}
	switch v.(type) {
	case nil:
		*id = ID{}
		return nil
	case string, float64:
		*id = ID{raw: string(data)}
		return nil
	default:
		return fmt.Errorf("%w: id must be a string or number, got %T", ErrMalformedFrame, v)
	}
}

// Error is a JSON-RPC 2.0 error object. It implements the error interface so
// a peer's error response can travel through Go error chains.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// Request is a JSON-RPC 2.0 request (id is always set; see Notification).
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      ID              `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response. Exactly one of Result / Error is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      ID              `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Notification is a JSON-RPC 2.0 notification (no id, never answered).
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// NewRequest builds a request with the jsonrpc version filled in.
func NewRequest(id ID, method string, params json.RawMessage) *Request {
	return &Request{JSONRPC: Version, ID: id, Method: method, Params: params}
}

// NewNotification builds a notification with the jsonrpc version filled in.
func NewNotification(method string, params json.RawMessage) *Notification {
	return &Notification{JSONRPC: Version, Method: method, Params: params}
}

// NewResponse builds a success response. A nil result is encoded as null so
// the response always carries a "result" member.
func NewResponse(id ID, result json.RawMessage) *Response {
	if result == nil {
		result = json.RawMessage("null")
	}
	return &Response{JSONRPC: Version, ID: id, Result: result}
}

// NewErrorResponse builds an error response.
func NewErrorResponse(id ID, e *Error) *Response {
	return &Response{JSONRPC: Version, ID: id, Error: e}
}

// ParseMessage classifies one frame into *Request, *Response, or
// *Notification. Any shape violation returns an error satisfying
// errors.Is(err, ErrMalformedFrame); ParseMessage never panics on hostile
// input. Callers (the transport read loop) decide whether a malformed frame
// closes the connection — a single bad frame must not crash the process.
func ParseMessage(data []byte) (any, error) {
	var probe struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   *Error          `json:"error"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedFrame, err)
	}
	if probe.JSONRPC != Version {
		return nil, fmt.Errorf("%w: jsonrpc version %q (want %q)", ErrMalformedFrame, probe.JSONRPC, Version)
	}
	var id ID
	if len(probe.ID) > 0 {
		if err := id.UnmarshalJSON(probe.ID); err != nil {
			return nil, err
		}
	}
	switch {
	case probe.Method != "" && id.IsSet():
		return &Request{JSONRPC: probe.JSONRPC, ID: id, Method: probe.Method, Params: probe.Params}, nil
	case probe.Method != "":
		// A null id on a method-bearing message is treated as a notification.
		return &Notification{JSONRPC: probe.JSONRPC, Method: probe.Method, Params: probe.Params}, nil
	case probe.Result != nil || probe.Error != nil:
		return &Response{JSONRPC: probe.JSONRPC, ID: id, Result: probe.Result, Error: probe.Error}, nil
	default:
		return nil, fmt.Errorf("%w: neither request, response, nor notification", ErrMalformedFrame)
	}
}
