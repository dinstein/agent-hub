package logx

import (
	"log/slog"
	"os"
)

// Mandatory field convention (docs/modules/foundation.md). Every log record
// that concerns a downstream server, a derived instance, a tool call, a
// client, a session, a registry generation or the writing process must use
// these exact keys so that log streams stay joinable across the gateway,
// daemon and CLI. Do not invent synonyms ("srv", "toolName", ...).
//
// The constants below are the whole convention. A reader told not to invent
// synonyms needs the list to be complete, or the field it fails to mention
// is exactly the one that gets a second spelling.
const (
	// FieldServer is the downstream MCP server name.
	FieldServer = "server"
	// FieldTool is the tool name (raw, per-server; not the namespaced form).
	FieldTool = "tool"
	// FieldClient is the client identifier (AGENTHUB_CLIENT_ID).
	FieldClient = "client"
	// FieldSession is the session identifier.
	//
	// Not every assembly has one, and that is not an omission: a stdio
	// gateway serves one terminal pipe with no id anywhere in it, so its
	// records are keyed by client and pid instead. The two that do are the
	// daemon-assigned session a gateway registers for over the control link,
	// and the Streamable HTTP session. Both go through this constant —
	// spelling it by hand is how a stream stops joining.
	FieldSession = "session"
	// FieldRev is the registry generation the record was produced under.
	FieldRev = "rev"
	// FieldPID is the OS process that produced the record.
	//
	// It exists because the log FILE is named after the client, not after the
	// process: every `agenthub connect --client claude-code` writes to
	// gateway-claude-code.log, and a user normally has several running at
	// once (one per editor window). Without it the interleaved lines of two
	// gateways read as one gateway behaving inexplicably — a server that
	// connects and fails at the same instant, a ladder that skips rungs — and
	// the reader has no way to tell which process said what.
	//
	// The pid is what `ps -eo pid,lstart,command | grep 'agenthub connect'`
	// prints, so a line in the log joins directly to a process on the machine.
	FieldPID = "pid"
	// FieldInstance is the derived instance a record belongs to (the derive
	// key), empty for a server's base connection.
	//
	// It is FieldServer's argument one level down. A derived instance keeps
	// its base server's id on purpose — that id is what RouteOf, the scope
	// intersection and the operator's config all name — so a server running
	// four derivations produces four connections' worth of lines under one
	// `server` value, and without this field a respawn or an opening circuit
	// cannot be attributed to the connection it happened on.
	//
	// The spelling matches TraceFrame's `inst`, which answers the same
	// question for the frame log; two names for one thing would mean a reader
	// joining the two streams has to know both.
	FieldInstance = "inst"
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

// Instance returns the derived-instance attr. Like PID it is meant to be
// bound once, on the connection's logger, rather than stamped per call site.
func Instance(key string) slog.Attr { return slog.String(FieldInstance, key) }

// PID returns the mandatory process attr for this process. It is meant to be
// attached ONCE, at logger construction (slog.Logger.With), not per record:
// every line of a process carries the same value, and stamping it at each
// call site is how one of them ends up missing it.
func PID() slog.Attr { return slog.Int(FieldPID, os.Getpid()) }
