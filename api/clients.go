package api

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// ClientDetected is one discovered AI-client configuration file.
//
// Detection reports STATS ONLY — never file content. On macOS, reading
// another application's configuration triggers a privacy prompt, and a bulk
// scan that prompts a dozen times is worse than no scan at all. Content is
// read only by the single-client actions, where a prompt is expected and
// explainable.
type ClientDetected struct {
	// Client is the stable client id ("claude", "cursor", ...).
	Client string `json:"client"`
	Name   string `json:"name"`
	// Placement is where the file sits ("user", "project", ...).
	Placement string `json:"placement"`
	// Shape is the configuration format family the adapter recognised.
	Shape    string `json:"shape"`
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
	Size     int64  `json:"size"`
	// Modified is the file mtime: metadata from the stat, not content.
	Modified time.Time `json:"modified"`
	Note     string    `json:"note,omitempty"`
	// Denied marks a location that exists but may not be inspected.
	// "The client is not installed" and "you may not look" call for
	// opposite user actions, so a frontend must render the two differently
	// and never fold this into "not found".
	Denied bool `json:"denied,omitempty"`
	// Remediation is the operator-facing fix for a denied location.
	Remediation string `json:"remediation,omitempty"`
}

// ClientDetectResult is the answer to Detect.
type ClientDetectResult struct {
	Found []ClientDetected `json:"found"`
	// Supported lists every client agenthub can write directly, so the
	// answer to "why is my client missing" ships with the listing.
	Supported []string `json:"supported"`
}

// GatewayEntry is the MCP server entry agenthub writes into a client
// configuration: it makes the client spawn this binary as its single MCP
// server.
type GatewayEntry struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// ClientConnectRequest is the body of a connect.
type ClientConnectRequest struct {
	// Profile binds this client to a profile ("" = none).
	Profile string `json:"profile,omitempty"`
	// Path overrides the configuration file to write. It names the file
	// outright and therefore may not be combined with Placement.
	Path string `json:"path,omitempty"`
	// Placement picks between the client's own "user" and "project"
	// configuration files. Empty means user-level, which is the default
	// because the entry carries the absolute path of one machine's agenthub
	// binary and project-level files are meant to be committed. A client
	// that has no such file answers E_BAD_REQUEST rather than writing to
	// the other one.
	Placement string `json:"placement,omitempty"`
	// Bin overrides the agenthub binary written into the entry.
	Bin string `json:"bin,omitempty"`
	// DryRun previews the entry and writes nothing.
	DryRun bool `json:"dry_run,omitempty"`
}

// ClientConnection is the outcome of a connect (or its dry-run preview).
type ClientConnection struct {
	Client  string       `json:"client"`
	Profile string       `json:"profile,omitempty"`
	DryRun  bool         `json:"dry_run"`
	Entry   GatewayEntry `json:"entry"`
	Path    string       `json:"path,omitempty"`
	// Backup is the copy taken before the file was rewritten. A frontend
	// should surface it: it is the operator's undo.
	Backup string `json:"backup,omitempty"`
	// Changed is false on an idempotent re-connect (the file already said
	// exactly this). Not a failure.
	Changed bool `json:"changed,omitempty"`
}

// ClientDisconnected is the outcome of a disconnect.
type ClientDisconnected struct {
	Client string `json:"client"`
	Path   string `json:"path"`
	// Removed names the entries that were deleted. Only entries agenthub
	// itself wrote are ever removed — ownership decides, never the name
	// alone, so a hand-written server that happens to share a name survives.
	Removed []string `json:"removed"`
	Backup  string   `json:"backup,omitempty"`
}

// ClientsService detects AI clients on this machine and wires them to the
// gateway.
//
// These calls carry no expectedGeneration: a client configuration file is
// not the registry, so there is no shared document to lose a
// compare-and-swap against.
type ClientsService struct{ c *Client }

// Detect scans for known client configurations.
func (s *ClientsService) Detect(ctx context.Context) (ClientDetectResult, error) {
	var out ClientDetectResult
	err := s.c.do(ctx, http.MethodGet, "/clients", nil, nil, &out)
	return out, err
}

// Connect writes the gateway entry into one client's configuration.
//
// A configuration file this process may not read answers 403 with
// E_FORBIDDEN, deliberately distinct from the uniform 404: "not installed"
// and "you may not look" call for opposite user actions.
func (s *ClientsService) Connect(
	ctx context.Context, client string, req ClientConnectRequest,
) (ClientConnection, error) {
	var out ClientConnection
	err := s.c.do(ctx, http.MethodPost, "/clients/"+url.PathEscape(client)+"/connect", nil, req, &out)
	return out, err
}

// Disconnect removes agenthub's gateway entry from one client's
// configuration. The client's other MCP servers, if any, are left exactly as
// they were.
//
// It targets the same default file a connect writes and, only when that file
// holds nothing agenthub owns, the client's other location — so an entry
// written back when connect defaulted to the project level is still removed
// rather than reported as missing.
func (s *ClientsService) Disconnect(ctx context.Context, client string) (ClientDisconnected, error) {
	var out ClientDisconnected
	err := s.c.do(ctx, http.MethodDelete, "/clients/"+url.PathEscape(client)+"/connect", nil, nil, &out)
	return out, err
}
