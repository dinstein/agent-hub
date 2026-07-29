package mcp

import "encoding/json"

// MCP method and notification names used by M0. The facade owns these
// strings; no other package spells protocol method names.
const (
	MethodInitialize = "initialize"
	MethodPing       = "ping"
	MethodToolsList  = "tools/list"
	MethodToolsCall  = "tools/call"
	MethodRootsList  = "roots/list"

	NotificationInitialized          = "notifications/initialized"
	NotificationCancelled            = "notifications/cancelled"
	NotificationToolsListChanged     = "notifications/tools/list_changed"
	NotificationResourcesListChanged = "notifications/resources/list_changed"
	NotificationPromptsListChanged   = "notifications/prompts/list_changed"
	// NotificationRootsListChanged is sent by a client whose roots changed.
	// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28)
	NotificationRootsListChanged = "notifications/roots/list_changed"
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

// ClientCapabilities is the minimal capability set M0 declares. Unknown
// capability groups a future server might require are out of scope here;
// extend this struct rather than hand-writing JSON elsewhere.
type ClientCapabilities struct {
	Roots *RootsCapability `json:"roots,omitempty"`
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
// optional OutputSchema / Annotations) are passed through verbatim — this
// facade never re-encodes downstream JSON Schema.
type ToolDef struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
}

// ListToolsParams is the "tools/list" request payload.
type ListToolsParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// ListToolsResult is the "tools/list" response payload.
type ListToolsResult struct {
	Tools      []ToolDef `json:"tools"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

// CallToolParams is the "tools/call" request payload. Arguments are raw:
// the gateway routes them untouched from upstream client to downstream
// server.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CallResult is the "tools/call" response payload. Content (and optional
// structuredContent) are passed through verbatim; IsError distinguishes a
// tool-level failure (a successful RPC whose tool reported an error) from a
// protocol-level failure (a JSON-RPC error response).
type CallResult struct {
	Content           json.RawMessage `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// CancelledParams is the "notifications/cancelled" payload. RequestID names
// the in-flight request being cancelled; the receiver may still answer it
// (cancellation races are inherent, receivers must tolerate a late reply).
type CancelledParams struct {
	RequestID ID     `json:"requestId"`
	Reason    string `json:"reason,omitempty"`
}
