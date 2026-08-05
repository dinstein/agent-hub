package mcp

import "encoding/json"

// MCP method and notification names. The facade owns these strings; no other
// package spells protocol method names.
const (
	MethodInitialize = "initialize"
	// MethodPing is removed from the core protocol in MCP 2026-07-28.
	// DEPRECATED-UPSTREAM(ping, earliest-removal: 2027-07-28)
	MethodPing      = "ping"
	MethodToolsList = "tools/list"
	MethodToolsCall = "tools/call"
	MethodRootsList = "roots/list"

	// MethodSamplingCreate and MethodElicitationCreate name server-initiated
	// requests: reverse RPCs on ≤ 2025-11-25, InputRequest methods inside an
	// InputRequiredResult on 2026-07-28 (MRTR).
	//
	// DEPRECATED-UPSTREAM(sampling, earliest-removal: 2027-07-28): the
	// constant stays because it is what mrtr matches on to REFUSE the
	// method; nothing here offers sampling. Elicitation is not deprecated.
	MethodSamplingCreate    = "sampling/createMessage"
	MethodElicitationCreate = "elicitation/create"

	// MethodDiscover is the server/discover RPC introduced in MCP 2026-07-28.
	// Servers MUST implement it; clients MAY call it before any other request
	// to negotiate the highest mutually supported protocol version.
	MethodDiscover = "server/discover"

	// MethodSubscriptionsListen is the 2026-07-28 replacement for the HTTP
	// GET notification stream and resources/subscribe. A single long-lived
	// POST-response SSE stream delivers all opted-in change notifications.
	MethodSubscriptionsListen = "subscriptions/listen"

	// NotificationInitialized is sent after a successful initialize handshake
	// (MCP ≤ 2025-11-25). Removed in 2026-07-28 (stateless protocol).
	// DEPRECATED-UPSTREAM(initialize-handshake, earliest-removal: 2027-07-28)
	NotificationInitialized          = "notifications/initialized"
	NotificationCancelled            = "notifications/cancelled"
	NotificationToolsListChanged     = "notifications/tools/list_changed"
	NotificationResourcesListChanged = "notifications/resources/list_changed"
	NotificationPromptsListChanged   = "notifications/prompts/list_changed"
	// NotificationRootsListChanged is sent by a client whose roots changed.
	// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28)
	NotificationRootsListChanged = "notifications/roots/list_changed"

	// NotificationSubscriptionsAcknowledged is the first message a server
	// sends on a subscriptions/listen stream (MCP 2026-07-28), reporting the
	// subset of the requested filter it will honour.
	NotificationSubscriptionsAcknowledged = "notifications/subscriptions/acknowledged"
)

// ResultType values for the required resultType field introduced in MCP
// 2026-07-28. Servers speaking earlier protocol versions omit the field;
// clients MUST treat an absent resultType as ResultTypeComplete.
const (
	// ResultTypeComplete signals a normal, finished result.
	ResultTypeComplete = "complete"
	// ResultTypeInputRequired signals that the server needs more information
	// from the client before it can complete the request (see InputRequiredResult).
	ResultTypeInputRequired = "input_required"
)

// Root is one entry of a roots/list result.
// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28): kept per
// canonical.md §5b; the gateway RootSource seam absorbs a future removal.
type Root struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

// ListRootsResult is the "roots/list" response payload.
// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28)
type ListRootsResult struct {
	Roots []Root `json:"roots"`
}

// Implementation identifies a client or server ("clientInfo"/"serverInfo").
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// RootsCapability advertises roots support.
// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28): kept per
// canonical.md §5b; the RootSource seam absorbs a future removal.
type RootsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ClientCapabilities is the capability set this client declares. Extend this
// struct rather than hand-writing JSON elsewhere. Extensions carries any
// opt-in extension capabilities negotiated during the handshake (MCP 2026-07-28+).
type ClientCapabilities struct {
	// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28)
	Roots      *RootsCapability           `json:"roots,omitempty"`
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

// RequestMeta carries the per-request protocol metadata introduced in MCP
// 2026-07-28 under the io.modelcontextprotocol/* _meta key namespace.
// It is injected into the _meta field of every outgoing request when the
// negotiated protocol version is Version2026.
//
// Clients MUST include ProtocolVersion and ClientCapabilities on every
// request; ClientInfo SHOULD be included.
type RequestMeta struct {
	ProtocolVersion    string             `json:"io.modelcontextprotocol/protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"io.modelcontextprotocol/clientCapabilities"`
	ClientInfo         *Implementation    `json:"io.modelcontextprotocol/clientInfo,omitempty"`
	// LogLevel, when set, asks the server to emit log messages at or above
	// this level for this request only (replaces logging/setLevel).
	LogLevel string `json:"io.modelcontextprotocol/logLevel,omitempty"`
}

// InitializeParams is the "initialize" request payload.
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// InitializeResult is the "initialize" response payload. Capabilities are
// kept as raw JSON: M0 does not interpret server capabilities, and raw
// passthrough guarantees nothing a server declared is silently dropped.
type InitializeResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities"`
	ServerInfo      Implementation  `json:"serverInfo"`
	Instructions    string          `json:"instructions,omitempty"`
}

// ToolDef is one entry of a tools/list result. InputSchema (and the
// optional OutputSchema / Annotations / Icons / Meta) are passed through
// verbatim — this facade never re-encodes downstream JSON Schema, and an
// aggregating proxy that drops a member a downstream sent has degraded that
// server's tool rather than relayed it.
type ToolDef struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
	// Icons is the tool's optional icon set. Raw, like the schemas: nothing
	// here interprets an icon, and re-encoding one could only lose detail.
	Icons json.RawMessage `json:"icons,omitempty"`
	// Meta is the tool's own _meta. It may carry extension data addressed to
	// a client this hub only forwards for, so it travels untouched.
	Meta json.RawMessage `json:"_meta,omitempty"`
}

// Cursor is an opaque pagination position. It is a POINTER wherever it
// appears, because absent and empty are different answers: the
// specification says an empty string is a valid cursor and MUST NOT be read
// as the end of results, and a plain string cannot tell the two apart.
// Clients must not parse, modify, or decide anything from the value beyond
// whether one was provided.
type Cursor = string

// ListToolsParams is the "tools/list" request payload.
type ListToolsParams struct {
	Cursor *Cursor `json:"cursor,omitzero"`
}

// CacheableResult carries the freshness hint fields that MCP 2026-07-28
// requires on all list and read results (tools/list, resources/list,
// prompts/list, resources/read, resources/templates/list).
// TtlMs is a client-side cache TTL in milliseconds; CacheScope controls
// whether shared intermediaries may cache the response ("public"/"private").
type CacheableResult struct {
	TtlMs      *int64 `json:"ttlMs,omitempty"`
	CacheScope string `json:"cacheScope,omitempty"`
}

// ListToolsResult is the "tools/list" response payload.
type ListToolsResult struct {
	Tools []ToolDef `json:"tools"`
	// NextCursor is nil when the server said nothing, and non-nil — possibly
	// pointing at "" — when it handed out a cursor. Only nil means the end
	// of the results; see Cursor.
	NextCursor *Cursor `json:"nextCursor,omitzero"`
	// ResultType is "complete" for a normal result (MCP 2026-07-28+).
	// An absent field from older servers is treated as "complete".
	ResultType string `json:"resultType,omitempty"`
	CacheableResult
}

// CallToolParams is the "tools/call" request payload. Arguments are raw:
// the gateway routes them untouched from upstream client to downstream
// server.
//
// RequestState and InputResponses only appear on an MRTR retry
// (MCP 2026-07-28): RequestState is the opaque blob echoed VERBATIM from
// the InputRequiredResult (never inspected, never modified — servers own
// its integrity), and InputResponses carries the collected answers keyed
// like the originating inputRequests.
type CallToolParams struct {
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments,omitempty"`
	RequestState   string          `json:"requestState,omitempty"`
	InputResponses InputResponses  `json:"inputResponses,omitempty"`
}

// CallResult is the "tools/call" complete response payload. Content (and
// optional structuredContent) are passed through verbatim; IsError
// distinguishes a tool-level failure from a protocol-level failure.
// ResultType is "complete" for a finished result; check for
// InputRequiredResult before unmarshalling as CallResult (MCP 2026-07-28+).
type CallResult struct {
	ResultType        string          `json:"resultType,omitempty"`
	Content           json.RawMessage `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// InputRequiredResult is the "tools/call" (and "prompts/get",
// "resources/read") interim response when the server needs additional input
// from the client before it can complete the request (MCP 2026-07-28 MRTR).
//
// The client must:
//  1. Collect the responses for every key in InputRequests.
//  2. Retry the original request with a NEW JSON-RPC id, carrying
//     InputResponses and the echoed RequestState.
//
// Clients MUST NOT inspect, parse, or modify RequestState; servers own its
// integrity (HMAC/AEAD if it influences auth or resource access).
type InputRequiredResult struct {
	ResultType    string        `json:"resultType"` // always ResultTypeInputRequired
	InputRequests InputRequests `json:"inputRequests,omitempty"`
	RequestState  string        `json:"requestState,omitempty"`
}

// InputRequests is a map of server-assigned string keys to input request
// objects. Keys are unique within the scope of one InputRequiredResult.
// Values are InputRequest objects carrying the method and params of each
// server-initiated request (elicitation/create, sampling/createMessage,
// or roots/list).
type InputRequests map[string]InputRequest

// InputRequest is one entry in an InputRequests map: the method and raw
// params of a server-initiated request the client must fulfill.
type InputRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// InputResponses is a map of client responses keyed by the same string keys
// as the originating InputRequests. It is carried in the retry of the
// original request.
type InputResponses map[string]json.RawMessage

// SubscriptionFilter names the notifications a subscriptions/listen stream
// may carry (MCP 2026-07-28). It is an allow list and the server MUST NOT
// send a type absent from it: every field left false or nil is "do not
// send", never "send anything".
//
// ResourceSubscriptions is the replacement for the resources/subscribe RPC,
// so nil and [] differ as they do for every selector in this tree — nil is
// "no resource subscriptions", [] is an explicit empty set — which is why it
// carries omitzero rather than omitempty.
type SubscriptionFilter struct {
	ToolsListChanged      bool     `json:"toolsListChanged,omitempty"`
	PromptsListChanged    bool     `json:"promptsListChanged,omitempty"`
	ResourcesListChanged  bool     `json:"resourcesListChanged,omitempty"`
	ResourceSubscriptions []string `json:"resourceSubscriptions,omitzero"`
}

// SubscriptionsListenParams is the "subscriptions/listen" request payload
// (MCP 2026-07-28): the long-lived POST-response SSE stream that replaces
// both the HTTP GET notification stream and resources/subscribe.
// Notifications is the required opt-in filter.
type SubscriptionsListenParams struct {
	Notifications SubscriptionFilter `json:"notifications"`
	Meta          *RequestMeta       `json:"_meta,omitempty"`
}

// SubscriptionsAcknowledgedParams is the
// "notifications/subscriptions/acknowledged" payload: the subset of the
// requested filter the server agreed to honour. A type the server does not
// support is simply omitted, so this is the only place a client learns that
// something it asked for will never arrive — silence on the stream looks
// identical to a quiet server.
type SubscriptionsAcknowledgedParams struct {
	Notifications SubscriptionFilter `json:"notifications"`
	Meta          *NotificationMeta  `json:"_meta,omitempty"`
}

// NotificationMeta carries the per-notification protocol metadata MCP
// 2026-07-28 defines. SubscriptionID identifies the subscriptions/listen
// request whose stream delivered the notification, and equals that
// request's JSON-RPC id; it is absent on notifications that did not arrive
// on such a stream.
type NotificationMeta struct {
	SubscriptionID ID `json:"io.modelcontextprotocol/subscriptionId,omitzero"`
}

// DiscoverParams is the "server/discover" request payload (MCP 2026-07-28).
// Meta carries the required per-request protocol metadata — discover is
// itself a stateless request, so it declares the client's version and
// capabilities like any other. No other fields exist yet.
type DiscoverParams struct {
	Meta *RequestMeta `json:"_meta,omitempty"`
}

// ResultMeta carries the per-result protocol metadata MCP 2026-07-28 defines
// under the io.modelcontextprotocol/* _meta key namespace. Servers SHOULD
// echo ServerInfo on every result; it is where a server's identity travels
// now that no initialize handshake carries it.
type ResultMeta struct {
	ServerInfo *Implementation `json:"io.modelcontextprotocol/serverInfo,omitempty"`
}

// DiscoverResult is the "server/discover" response payload. It advertises
// the server's supported protocol versions, capabilities, and identity.
// Clients pick the highest mutually supported version from SupportedVersions
// before sending their first real request.
//
// The result is a CacheableResult: ttlMs and cacheScope are required members
// of the specification's shape, and the server's identity lives in _meta
// rather than in a top-level member (see ServerInfo).
type DiscoverResult struct {
	ResultType        string          `json:"resultType,omitempty"`
	SupportedVersions []string        `json:"supportedVersions"`
	Capabilities      json.RawMessage `json:"capabilities,omitempty"`
	Instructions      string          `json:"instructions,omitempty"`
	Meta              *ResultMeta     `json:"_meta,omitempty"`
	CacheableResult
}

// ServerInfo returns the identity the result carries in
// _meta.io.modelcontextprotocol/serverInfo, or the zero Implementation when
// the server omitted it — echoing it is a SHOULD, not a MUST, so absence is
// normal and never an error.
func (r *DiscoverResult) ServerInfo() Implementation {
	if r == nil || r.Meta == nil || r.Meta.ServerInfo == nil {
		return Implementation{}
	}
	return *r.Meta.ServerInfo
}

// CancelledParams is the "notifications/cancelled" payload. RequestID names
// the in-flight request being cancelled; the receiver may still answer it
// (cancellation races are inherent, receivers must tolerate a late reply).
type CancelledParams struct {
	RequestID ID     `json:"requestId"`
	Reason    string `json:"reason,omitempty"`
}
