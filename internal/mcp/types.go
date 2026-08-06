package mcp

import (
	"encoding/json"
	"strings"
)

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
// optional OutputSchema / Annotations / Icons / Execution / Meta) are passed
// through verbatim — this facade never re-encodes downstream JSON Schema,
// and an aggregating proxy that drops a member a downstream sent has
// degraded that server's tool rather than relayed it.
//
// There is deliberately NO gate on the session's negotiated version, even
// though OutputSchema and Title postdate 2025-03-26 and the gateway will
// negotiate that revision with a client that asks for it. Neither older
// schema sets additionalProperties: false and neither says a receiver must
// reject an unrecognized member, so forwarding a later revision's optional
// fields is the additive behaviour both revisions permit — while stripping
// them would be the degradation the paragraph above rules out. ResultType
// and the freshness hints ARE gated, and the difference is real: those
// change how a result must be READ, these only add to it.
//
// THE MEMBER LIST IS PER REVISION, and a count taken from one of them is not
// a check. 2026-07-28's Tool has eight members; 2025-11-25's has nine, the
// extra one being Execution. This struct was audited against the eight once
// already and came away complete, because the ninth belongs to a revision
// the audit was not reading — and mcp.SupportedVersions promises both.
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
	// Execution is the 2025-11-25 `execution` object, whose one member is
	// `taskSupport`: "forbidden" (the default), "optional" or "required".
	// It says whether the tool may — or must — be invoked as a task.
	//
	// AgentHub implements no part of tasks and declares no `tasks`
	// capability, so a conformant client MUST NOT augment a call through
	// this hub whatever this field says: the capability gate decides, and
	// this is a refinement inside an already-enabled capability. Forwarding
	// it is therefore inert today and correct anyway — it describes the
	// downstream's TOOL, not this hop, and for a "required" tool it is the
	// only thing that explains the -32601 every call to it earns.
	//
	// Raw, and ungated, for the reason the paragraph above gives: it exists
	// in 2025-11-25 alone. 2026-07-28 moved tasks out of the core schema
	// into a capability extension and dropped the member from Tool, which
	// is why this reads as a missing feature and is not one.
	Execution json.RawMessage `json:"execution,omitempty"`
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
	RequestState   *RequestState   `json:"requestState,omitzero"`
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
	// Meta is the result's own _meta, which every revision's base Result
	// carries. It is here for the reason ToolDef.Meta is: a downstream may
	// address extension data to a client this hub only forwards for, and
	// the raw bytes are dropped at decode, so a member with no field can
	// never reappear.
	//
	// It does NOT reach the upstream client verbatim. gateway.replyResult
	// removes the io.modelcontextprotocol/ namespace first — see
	// StripReservedMetaKeys, which explains why this hop owns it.
	Meta json.RawMessage `json:"_meta,omitempty"`
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
	RequestState  *RequestState `json:"requestState,omitzero"`
}

// RequestState is the opaque blob a server hands a client to echo back on
// the MRTR retry. It is a POINTER wherever it appears, because the rule is
// three-state: present means echo this exact value, ABSENT means send no
// requestState at all, and a plain string cannot tell absent from empty.
// Clients must not inspect, parse, modify or assume anything about it —
// which is also why nothing here gives it any structure.
type RequestState = string

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

// ReservedMetaPrefix is the _meta key namespace the specification reserves
// for itself. Keys under it have defined meanings; every other key belongs
// to whoever wrote it.
const ReservedMetaPrefix = "io.modelcontextprotocol/"

// StripReservedMetaKeys removes the specification's own key namespace from a
// _meta object a downstream sent, leaving every other key untouched. It
// returns nil when nothing survives, so an object emptied by the strip is
// omitted rather than emitted as `{}`.
//
// THE HOP OWNS ITS OWN NAMESPACE, and that is the whole rule. A reserved key
// describes the exchange it travels on, so relaying one across a hop
// restates it about a different exchange. On a tools/call result exactly one
// reserved key can legitimately appear — io.modelcontextprotocol/serverInfo
// — and forwarding it would tell the upstream client that the server which
// produced the response was the downstream. On this hop that server is
// agenthub, and after internal/shaping truncates or reformats, the bytes
// being attributed were never the downstream's at all. The namespace is
// stripped rather than that one key, because the reservation is what makes
// the answer wrong and a later reserved key would be wrong the same way.
//
// Everything outside the namespace is forwarded verbatim: it is the
// extension data the downstream addressed to a client this hub only
// forwards for, and nothing here is entitled to read it.
//
// FAIL-CLOSED on malformed input: a _meta that is not a JSON object is not
// something this hub can vouch for, and it is dropped rather than passed on.
// The schema requires an object, so nothing conformant is lost.
func StripReservedMetaKeys(meta json.RawMessage) json.RawMessage {
	if len(meta) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(meta, &obj); err != nil {
		return nil
	}
	kept := false
	for k := range obj {
		if strings.HasPrefix(k, ReservedMetaPrefix) {
			delete(obj, k)
			continue
		}
		kept = true
	}
	if !kept {
		return nil
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return out
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
