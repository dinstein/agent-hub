package logx

import "log/slog"

// Mandatory field convention (docs/modules/foundation.md). Every log record that
// concerns a downstream server, a tool call, a client or a session must
// use these exact keys so that log streams stay joinable across the
// gateway, daemon and CLI. Do not invent synonyms ("srv", "toolName", ...).
const (
	// FieldServer is the downstream MCP server name.
	FieldServer = "server"
	// FieldTool is the tool name (raw, per-server; not the namespaced form).
	FieldTool = "tool"
	// FieldClient is the client identifier (AGENTHUB_CLIENT_ID).
	FieldClient = "client"
	// FieldSession is the session identifier.
	FieldSession = "session"
	// FieldRev is the registry generation the record was produced under.
	FieldRev = "rev"
)

// Server returns the mandatory server attr.
func Server(name string) slog.Attr { return slog.String(FieldServer, name) }

// Tool returns the mandatory tool attr.
func Tool(name string) slog.Attr { return slog.String(FieldTool, name) }

// Client returns the mandatory client attr.
func Client(id string) slog.Attr { return slog.String(FieldClient, id) }

// Session returns the mandatory session attr.
func Session(id string) slog.Attr { return slog.String(FieldSession, id) }

// Rev returns the mandatory registry-generation attr.
func Rev(gen uint64) slog.Attr { return slog.Uint64(FieldRev, gen) }
