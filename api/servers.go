package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// Transport values of ServerEntry.Transport (frozen wire values). The empty
// string means TransportStdio — an entry written before the field existed
// keeps its exact meaning.
const (
	// TransportStdio spawns a child process and speaks over its pipes.
	TransportStdio = "stdio"
	// TransportHTTP is MCP Streamable HTTP.
	TransportHTTP = "http"
	// TransportSSE is the legacy HTTP+SSE transport (read side only).
	TransportSSE = "sse"
)

// Runtime values of ServerEntry.Runtime. The empty string means RuntimeHost.
// Runtime is orthogonal to Transport: a docker-runtime server still speaks
// stdio, only the process on this host changes.
const (
	// RuntimeHost spawns the command directly on this machine.
	RuntimeHost = "host"
	// RuntimeDocker runs the command inside a container.
	RuntimeDocker = "docker"
)

// Provenance values of ServerEntry.Provenance. The empty string is treated
// as ProvenanceRemote — the screened direction.
const (
	// ProvenanceRemote screens the endpoint and refuses a private address.
	ProvenanceRemote = "remote"
	// ProvenanceLocal unblocks a LITERAL loopback endpoint only. It never
	// unblocks RFC1918 and never a hostname whose DNS answer claims to be
	// local: a DNS answer is a claim its owner can change at will.
	ProvenanceLocal = "local"
)

// DockerMount is one host directory exposed to a docker-runtime server.
//
// Failure direction: read-only is the zero value, so a form that never
// thinks about write access lands on the safe side.
type DockerMount struct {
	// Source is the absolute host path (required).
	Source string `json:"source"`
	// Target is the absolute container path; empty means "same as Source".
	Target string `json:"target,omitempty"`
	// Write mounts read-write instead of read-only.
	Write bool `json:"write,omitempty"`
}

// DockerRuntime is the container configuration of a docker-runtime server.
// Everything not stated here is denied by the spawner's defaults: no
// network, no mounts, no capabilities, no privileged mode.
//
// There is no Env field on purpose: the container's environment IS
// ServerEntry.Env — one place to look, one place to put a ${SECRET_X}
// placeholder.
type DockerRuntime struct {
	// Image is the container image reference (required).
	Image string `json:"image"`
	// Network is the docker network. Empty means "none" — a server that
	// needs the network has to say so.
	Network string `json:"network,omitempty"`
	// Mounts are the only host paths the container can see.
	Mounts []DockerMount `json:"mounts,omitempty"`
	// Memory is the `--memory` limit (e.g. "512m"); empty = unset.
	Memory string `json:"memory,omitempty"`
	// CPUs is the `--cpus` limit (e.g. "1.5"); empty = unset.
	CPUs string `json:"cpus,omitempty"`
	// User and Workdir map to `--user` / `--workdir`.
	User    string `json:"user,omitempty"`
	Workdir string `json:"workdir,omitempty"`
	// ExtraArgs are appended to `docker run` verbatim. They cannot
	// re-specify a flag the isolation defaults own, and the spawn guard
	// screens the generated run line like every other spawn.
	ExtraArgs []string `json:"extraArgs,omitempty"`
}

// OAuthHint is the configuration half of a server's OAuth setup: what a
// later login discovers against. It is optional — discovery works from the
// server URL alone (RFC 9728).
//
// There is deliberately no "needs auth" field: whether a server currently
// requires (or has lost) authorization is RUNTIME state, reported through
// the Health contract. Persisting it would create a second source of truth
// that goes stale the moment a token expires.
type OAuthHint struct {
	// Issuer pins the authorization server, skipping discovery.
	Issuer string `json:"issuer,omitempty"`
	// Scopes is the scope set to request, sent verbatim.
	Scopes []string `json:"scopes,omitempty"`
	// ResourceMetadataURL pins the RFC 9728 protected-resource document.
	ResourceMetadataURL string `json:"resourceMetadataUrl,omitempty"`
}

// ServerEntry is one downstream server's stored definition.
//
// CONTRACT: field names mirror internal/registry.ServerEntry, camelCase
// included. This is the registry DOCUMENT shape, not a control-plane
// projection of it (the same reasoning that keeps AuditRecord camelCase), so
// what a frontend edits is what lands on disk.
//
// NO FIELD IS `omitempty`, and that is load-bearing. The daemon merges a
// PATCH by key presence: a key present replaces that field, a key absent
// keeps the stored value. Marshaling every key therefore makes an update a
// true WHOLESALE replacement — which is the only correct semantics here,
// because an entry's fields are not independent (the transport decides which
// half of the struct is meaningful). With omitempty, clearing a field would
// be indistinguishable from not mentioning it, i.e. a leaked environment
// variable or a stale mount could never be removed through this API.
//
// Credentials red line: Env and Headers values may hold ${SECRET_X}
// placeholders and are stored VERBATIM. Resolution happens at connect time —
// a registry document must never hold a credential, so a frontend must never
// substitute one in before sending.
type ServerEntry struct {
	// Transport is one of the Transport* constants ("" == stdio).
	Transport string            `json:"transport"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Cwd       string            `json:"cwd"`
	// URL is the endpoint for the http and sse transports.
	URL string `json:"url"`
	// Headers are caller-owned request headers (http/sse). The OAuth path
	// does NOT set Authorization here: the access token is injected per
	// request so a refresh takes effect without reconnecting.
	Headers map[string]string `json:"headers"`
	OAuth   *OAuthHint        `json:"oauth"`
	// Provenance is one of the Provenance* constants.
	Provenance string `json:"provenance"`
	// Derive is the derived-instance policy: "none" (default), "root" or
	// "session". It is a CONNECTION-plane field: deriving changes which
	// process a call runs on, never what a session can see.
	Derive string `json:"derive"`
	// Runtime is one of the Runtime* constants ("" == host).
	Runtime string `json:"runtime"`
	// Docker is required when Runtime is RuntimeDocker, ignored otherwise.
	Docker  *DockerRuntime `json:"docker"`
	Enabled bool           `json:"enabled"`
	// Source records where the entry came from (cli / import / gui).
	Source string `json:"source"`
}

// ServerSpec is one server's id plus its definition: what Create sends and
// what a write answers with.
type ServerSpec struct {
	ID    string      `json:"id"`
	Entry ServerEntry `json:"entry"`
}

// ServerDetail is the stored definition read back for editing, plus the
// generation it was read at.
//
// The two travel together on purpose: that generation is what the following
// write sends as its expectedGeneration, which is what makes a
// read-modify-write safe against a concurrent writer. Reading the entry from
// one call and the generation from another would reintroduce exactly the
// window the precondition exists to close.
type ServerDetail struct {
	Generation uint64      `json:"generation"`
	ID         string      `json:"id"`
	Entry      ServerEntry `json:"entry"`
}

// ServerPatch is the PATCH body: a PARTIAL entry.
//
// Merge rule (daemon side): a key PRESENT in entry replaces that field
// wholesale, an absent key keeps the stored value. There is no deep merge —
// otherwise an env map could only ever grow and removing a leaked variable
// would be impossible.
//
// Entry is raw JSON because "partial" is a statement about which KEYS are
// present, which a typed struct with omitempty cannot express faithfully.
// Callers use Update (a complete entry: every key present, i.e. wholesale)
// or SetEnabled (exactly one key); a caller with a genuinely partial edit
// builds the object itself and passes it to Patch.
type ServerPatch struct {
	Entry json.RawMessage `json:"entry"`
}

// ServerWrite is the answer to every server mutation.
type ServerWrite struct {
	WriteResult
	ID string `json:"id"`
	// Entry is the definition as it now stands; absent after a delete.
	Entry   *ServerEntry `json:"entry,omitempty"`
	Deleted bool         `json:"deleted,omitempty"`
}

// ServerTestRequest asks the daemon to connect to a configured server and
// report what it finds. Tool, when set, is also called after the handshake.
type ServerTestRequest struct {
	// Tool is the ORIGINAL downstream tool name, not the exposed one.
	Tool string `json:"tool,omitempty"`
	// Args is the JSON arguments object for Tool.
	Args json.RawMessage `json:"args,omitempty"`
	// TimeoutMillis bounds the connection (0 = the downstream default).
	TimeoutMillis int64 `json:"timeout_ms,omitempty"`
	// Definitions asks for ToolDefs — the compact signature, the description
	// and the raw input schema of every tool — alongside the bare name list.
	//
	// It is opt-in because the handshake's schemas are unbounded: a server
	// with a hundred tools answers with far more bytes than the "does this
	// connect" question needs, and that question is the one this endpoint is
	// asked most often. The definitions cost nothing extra to produce — the
	// handshake already returned them — only to transmit.
	Definitions bool `json:"defs,omitempty"`
}

// ServerTestTool is one tool of the live handshake, present when the request
// set Definitions.
//
// Signature is the SAME compact grammar an agent is shown
// (internal/discovery/toolsig) rather than a second format invented for this
// endpoint: an operator debugging "why did the agent call this wrong" has to
// be looking at the string the agent saw.
type ServerTestTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Signature   string `json:"signature,omitempty"`
	// Lossy reports that the signature dropped information (a folded nested
	// object, a truncated parameter list). It is the cue that InputSchema
	// has the rest, and it is why a lossy signature is never presented as
	// the whole truth.
	Lossy bool `json:"lossy,omitempty"`
	// InputSchema is the downstream's own JSON Schema bytes, verbatim. An
	// empty schema is a fact about the server, and substituting "{}" would
	// hide it.
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// ServerTestCall is the outcome of the optional tool invocation.
type ServerTestCall struct {
	Tool string `json:"tool"`
	// IsError is a TOOL-level failure (a successful call whose tool
	// reported an error), which is a valid answer — not a transport failure.
	IsError bool `json:"is_error"`
	// Text is the concatenated text content, truncated. It is tool output,
	// never a credential: agenthub sends secrets, it does not render them.
	Text   string `json:"text,omitempty"`
	Millis int64  `json:"millis"`
}

// ServerTestResult is the "does this definition actually work" answer.
//
// This is how a credential is verified (docs/modules/controlplane.md rule 5): by making a
// REAL call, never by printing the secret back. The type therefore has no
// field a credential could be put in.
type ServerTestResult struct {
	Server    string `json:"server"`
	Transport string `json:"transport"`
	// ServerInfo and ProtocolVersion come from the initialize handshake:
	// proof the other side really answered, not just that a socket opened.
	ServerInfo      string   `json:"server_info,omitempty"`
	ProtocolVersion string   `json:"protocol_version,omitempty"`
	ConnectMillis   int64    `json:"connect_ms"`
	ToolCount       int      `json:"tool_count"`
	Tools           []string `json:"tools"`
	// ToolDefs is present only when the request set Definitions. Tools keeps
	// holding the bare names either way, so a caller written against the
	// older shape keeps working.
	ToolDefs []ServerTestTool `json:"tool_defs,omitempty"`
	// Call is present only when the request named a tool.
	Call *ServerTestCall `json:"call,omitempty"`
}

// enabledOnlyEntry is the one-key patch behind SetEnabled: the enable flag
// is the single entry field that is independent of the transport shape, so
// flipping it needs no round trip through a complete definition.
type enabledOnlyEntry struct {
	Enabled bool `json:"enabled"`
}

// Get returns one server's STORED definition together with the generation it
// was read at — the read half of a read-modify-write.
//
// GET /v1/servers (List) is a different answer on purpose: it reports
// runtime state and the Health display contract, which is what a dashboard
// needs and what the `servers` SSE topic pushes. Editing needs the stored
// entry, which only this call carries.
func (s *ServersService) Get(ctx context.Context, id string) (ServerDetail, error) {
	var out ServerDetail
	err := s.c.do(ctx, http.MethodGet, "/servers/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

// Create registers a new server. It refuses to overwrite: an existing id is
// a name conflict (E_SERVER_EXISTS at HTTP 409, which is NOT a stale
// precondition and must not be retried blindly), never a silent replacement.
func (s *ServersService) Create(
	ctx context.Context, spec ServerSpec, expectedGeneration uint64,
) (ServerWrite, error) {
	var out ServerWrite
	// ServerSpec IS the create body: {"id":…,"entry":{…}}.
	err := s.c.doWrite(ctx, http.MethodPost, "/servers", nil, expectedGeneration, spec, &out)
	return out, err
}

// Update replaces an existing server's definition WHOLESALE: ServerEntry
// marshals every key, so no stored field survives unmentioned.
//
// Pass the generation from Get as expectedGeneration. The daemon applies the
// merge outside its lock, so it substitutes the generation the entry was
// read at when the caller supplies none — a concurrent write then answers
// 409 instead of losing itself under a merge computed from stale bytes.
func (s *ServersService) Update(
	ctx context.Context, spec ServerSpec, expectedGeneration uint64,
) (ServerWrite, error) {
	raw, err := json.Marshal(spec.Entry)
	if err != nil {
		return ServerWrite{}, err
	}
	return s.Patch(ctx, spec.ID, raw, expectedGeneration)
}

// Patch applies a partial entry: only the keys present in entry are
// replaced. Callers that want "replace everything" use Update.
func (s *ServersService) Patch(
	ctx context.Context, id string, entry json.RawMessage, expectedGeneration uint64,
) (ServerWrite, error) {
	var out ServerWrite
	err := s.c.doWrite(ctx, http.MethodPatch, "/servers/"+url.PathEscape(id), nil,
		expectedGeneration, ServerPatch{Entry: entry}, &out)
	return out, err
}

// Delete removes a server definition.
//
// Profile and client references to it are deliberately NOT rewritten: a
// selector naming a server that no longer exists resolves to nothing, which
// is the fail-closed direction. Rewriting them would widen the surviving
// layers' effective sets as a side effect of a delete.
func (s *ServersService) Delete(ctx context.Context, id string, expectedGeneration uint64) (ServerWrite, error) {
	var out ServerWrite
	err := s.c.doWrite(ctx, http.MethodDelete, "/servers/"+url.PathEscape(id), nil, expectedGeneration, nil, &out)
	return out, err
}

// SetEnabled flips a server's global enable flag. Disabling removes it from
// every profile's effective set without discarding its definition, so the
// switch is reversible and no configuration is lost.
func (s *ServersService) SetEnabled(
	ctx context.Context, id string, enabled bool, expectedGeneration uint64,
) (ServerWrite, error) {
	raw, err := json.Marshal(enabledOnlyEntry{Enabled: enabled})
	if err != nil {
		return ServerWrite{}, err
	}
	return s.Patch(ctx, id, raw, expectedGeneration)
}

// Test connects to a configured server and reports the handshake, the tool
// list and (optionally) one call.
//
// It carries no precondition: it changes no configuration, so there is
// nothing for a concurrent writer to lose.
func (s *ServersService) Test(ctx context.Context, id string, req ServerTestRequest) (ServerTestResult, error) {
	var out ServerTestResult
	err := s.c.do(ctx, http.MethodPost, "/servers/"+url.PathEscape(id)+"/test", nil, req, &out)
	return out, err
}
