package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/cli/output"
	"github.com/dinstein/agent-hub/internal/discovery/toolsig"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/registry"
)

// `server test` is the "does this definition actually work" command
// (docs/modules/controlplane.md: verifying that a credential is correct is
// done by making a REAL call, never by printing the secret back).
//
// It connects DIRECTLY from the CLI process rather than through the daemon.
// The daemon-mediated form is what docs/modules/controlplane.md lists as online-only, but
// a direct dial is what makes this command usable for the case it exists
// for — debugging a server that does not work yet, on a machine where the
// daemon may itself be the thing that is broken. The connection is
// short-lived and closed before the result is rendered.
//
// --tools / --schema read the definitions of THIS handshake and nothing
// else. `server inspect --schema` answers the neighbouring question from the
// gateway's persisted tool cache, which only a real gateway session ever
// writes; a workflow of `server add` + `auth login` + `server test` leaves
// that cache absent, and the two commands must not paper over which source
// they are quoting. Nothing here is written to the cache: this command stays
// a direct dial with no persistent side effects.

// ServerTestResult is the `server test` result.
type ServerTestResult struct {
	Server    string `json:"server"`
	Transport string `json:"transport"`
	Target    string `json:"target"`
	// ServerInfo/ProtocolVersion come from the initialize handshake.
	ServerInfo      string   `json:"serverInfo,omitempty"`
	ProtocolVersion string   `json:"protocolVersion,omitempty"`
	ConnectMillis   int64    `json:"connectMillis"`
	ToolCount       int      `json:"toolCount"`
	Tools           []string `json:"tools"`
	// ToolDetails is present with --tools: the definitions the handshake
	// already returned, rendered as compact signatures. `tools` keeps
	// holding the bare names so a script written against the older shape
	// keeps working.
	ToolDetails []ServerTestTool `json:"toolDetails,omitempty"`
	// SchemaTool names the tool Schema belongs to, so the result says what
	// it is describing without the reader having to remember the flags.
	SchemaTool string `json:"schemaTool,omitempty"`
	// Schema is one tool's raw inputSchema, present with --schema. It is the
	// downstream's bytes verbatim: this command never re-encodes a schema.
	Schema json.RawMessage `json:"schema,omitempty"`
	// Call is present only when --tool was given.
	Call *ServerTestCall `json:"call,omitempty"`
}

// ServerTestTool is one tool of the live handshake.
//
// Signature is the SAME compact grammar `search_tools` shows an agent
// (internal/discovery/toolsig) rather than a second format invented for the
// CLI — an operator debugging "why did the agent call this wrong" has to be
// looking at the string the agent saw.
type ServerTestTool struct {
	Name      string `json:"name"`
	Signature string `json:"signature"`
	// Lossy reports that the signature dropped information (a folded nested
	// object, a truncated parameter list). It is the cue that --schema has
	// the rest, and it is why a "lossy" signature is never presented as the
	// whole truth.
	Lossy       bool   `json:"lossy,omitempty"`
	Description string `json:"description,omitempty"`
}

// ServerTestCall is the outcome of the optional --tool invocation.
type ServerTestCall struct {
	Tool    string `json:"tool"`
	IsError bool   `json:"isError"`
	// Text is the concatenated text content of the result, truncated. It is
	// tool output, never a credential: agenthub only ever sends secrets, it
	// does not render them back.
	Text string `json:"text,omitempty"`
	// Millis is the round trip of the call alone.
	Millis int64 `json:"millis"`
}

// maxTestTextBytes bounds the rendered call output. A tool that answers
// with a megabyte of JSON must not flood a terminal that asked "does this
// work".
const maxTestTextBytes = 2 << 10

// Human renders the connection report.
func (r ServerTestResult) Human(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "%s: connected in %dms via %s (%s)\n",
		r.Server, r.ConnectMillis, r.Transport, r.Target); err != nil {
		return err
	}
	if r.ServerInfo != "" {
		if _, err := fmt.Fprintf(w, "  server: %s, protocol %s\n", r.ServerInfo, r.ProtocolVersion); err != nil {
			return err
		}
	}
	if len(r.ToolDetails) > 0 {
		if _, err := fmt.Fprintf(w, "  tools (%d):\n", r.ToolCount); err != nil {
			return err
		}
		for _, t := range r.ToolDetails {
			if _, err := fmt.Fprintf(w, "    %s\n", t.Signature); err != nil {
				return err
			}
			if t.Description == "" {
				continue
			}
			if _, err := fmt.Fprintf(w, "      %s\n", oneLine(t.Description, descriptionColumnBytes)); err != nil {
				return err
			}
		}
	} else if _, err := fmt.Fprintf(w, "  tools (%d): %s\n", r.ToolCount, strings.Join(r.Tools, ", ")); err != nil {
		return err
	}
	if len(r.Schema) > 0 {
		if _, err := fmt.Fprintf(w, "  schema of %s:\n%s\n",
			r.SchemaTool, indent(indentJSON(r.Schema))); err != nil {
			return err
		}
	}
	if r.Call != nil {
		status := "ok"
		if r.Call.IsError {
			status = "tool reported an error"
		}
		if _, err := fmt.Fprintf(w, "  call %s: %s in %dms\n%s\n",
			r.Call.Tool, status, r.Call.Millis, indent(r.Call.Text)); err != nil {
			return err
		}
	}
	return nil
}

// indentJSON pretty-prints a schema for the human view only. The value in
// the --json envelope stays the downstream's verbatim bytes; unparsable
// input is returned untouched, because a schema this command could not
// re-indent is still the answer the operator asked for.
func indentJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

func indent(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

func (a *App) newServerTestCmd() *cobra.Command {
	var (
		toolName  string
		rawArgs   string
		timeout   time.Duration
		withTools bool
		schema    string
	)
	cmd := &cobra.Command{
		Use:   "test <id> [--tools] [--schema <tool>]",
		Short: "Actually connect to a server and see whether it works",
		Long: "Contacts the server now and prints the tools it offers, so you know a server is\n" +
			"usable before a client depends on it. If it reports that authorization is\n" +
			"needed, run 'agenthub auth login <id>' and test again.\n\n" +
			"You can also call one tool end to end, naming it as the server does:\n" +
			"  agenthub server test github --tool search_repositories --args '{\"query\":\"mcp\"}'",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			entry, warnings, err := a.serverEntry(id)
			if err != nil {
				return err
			}
			spec, err := downstream.SpecFromEntry(id, entry)
			if err != nil {
				return &Error{
					Code: CodeUnsupportedTransport, ExitCode: ExitGeneral,
					Message: err.Error(),
					Hint:    "fix the entry with 'agenthub server rm' + 'agenthub server add'",
				}
			}
			var toolArgs json.RawMessage
			if rawArgs != "" {
				if !json.Valid([]byte(rawArgs)) {
					return Usagef("--args must be a JSON object, got %q", rawArgs)
				}
				toolArgs = json.RawMessage(rawArgs)
			}
			if toolArgs != nil && toolName == "" {
				return Usagef("--args needs --tool")
			}

			deps, err := a.newOAuthDeps(entry.Provenance == registry.ProvenanceLocal)
			if err != nil {
				return err
			}
			ddeps := downstream.Deps{
				Secrets:        deps.chain.Resolver(),
				ConnectTimeout: timeout,
			}
			if spec.IsHTTP() {
				ddeps.Auth = deps.tokenSource(id)
			}

			p := a.printer()
			p.Progress(output.ProgressEvent{
				Event:   "connecting",
				Message: fmt.Sprintf("connecting to %s…", id),
				Fields:  map[string]any{"server": id, "transport": string(spec.Kind)},
			})

			start := time.Now()
			srv, err := downstream.Connect(cmd.Context(), spec, ddeps)
			if err != nil {
				return testConnectError(id, err)
			}
			defer srv.Close()
			connectMS := time.Since(start).Milliseconds()

			res := ServerTestResult{
				Server:        id,
				Transport:     entry.TransportName(),
				Target:        rowFor(id, entry).target(),
				ConnectMillis: connectMS,
				Tools:         []string{},
			}
			if ir := srv.InitializeResult(); ir != nil {
				res.ServerInfo = ir.ServerInfo.Name + " " + ir.ServerInfo.Version
				res.ProtocolVersion = ir.ProtocolVersion
			}
			// The handshake already returned every definition in full. The
			// bare name list is the default view; --tools and --schema
			// render the rest of what is ALREADY in hand rather than
			// reconnecting or reading the gateway's persisted cache (which
			// a `server test`-only workflow never writes).
			defs := srv.Tools()
			for _, t := range defs {
				res.Tools = append(res.Tools, t.Name)
			}
			res.ToolCount = len(res.Tools)
			if withTools {
				res.ToolDetails = make([]ServerTestTool, 0, len(defs))
				for _, t := range defs {
					sig := toolsig.Named(t.Name, t, toolsig.Options{})
					res.ToolDetails = append(res.ToolDetails, ServerTestTool{
						Name:        t.Name,
						Signature:   sig.Text,
						Lossy:       sig.Lossy,
						Description: t.Description,
					})
				}
			}
			if schema != "" {
				def, ok := findTool(defs, schema)
				if !ok {
					e := NotFoundf(CodeToolNotFound, "server %q has no tool %q", id, schema)
					e.Hint = "run 'agenthub server test " + id + " --tools' to see what it does have"
					return e
				}
				// Verbatim: an empty inputSchema is a fact about the server,
				// and substituting "{}" would hide it.
				res.SchemaTool = def.Name
				res.Schema = def.InputSchema
			}
			p.Progress(output.ProgressEvent{
				Event:   "connected",
				Message: fmt.Sprintf("connected (%d tools)", res.ToolCount),
				Fields:  map[string]any{"tools": res.ToolCount, "connect_ms": connectMS},
			})

			if toolName != "" {
				callStart := time.Now()
				out, cerr := srv.Call(cmd.Context(), toolName, toolArgs)
				if cerr != nil {
					return &Error{
						Code: CodeGeneral, ExitCode: ExitGeneral,
						// Message states the context only. Error() appends
						// Err, so formatting the cause in here printed the
						// whole chain twice.
						Message: fmt.Sprintf("server %q: calling %q failed", id, toolName),
						Err:     cerr,
					}
				}
				res.Call = &ServerTestCall{
					Tool:    toolName,
					IsError: out.IsError,
					Text:    truncate(contentText(out.Content), maxTestTextBytes),
					Millis:  time.Since(callStart).Milliseconds(),
				}
			}
			return p.Emit(res, warnings...)
		},
	}
	cmd.Flags().StringVar(&toolName, "tool", "", "also call this tool, named as the server itself names it")
	cmd.Flags().StringVar(&rawArgs, "args", "", "arguments for --tool, as a JSON object")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "how long to wait for a connection (default 120s: a first npx/uvx run has to download)")
	cmd.Flags().BoolVar(&withTools, "tools", false,
		"show each tool's arguments too, not just its name")
	cmd.Flags().StringVar(&schema, "schema", "", "print the full argument definition of one tool")
	return cmd
}

// testConnectError classifies a failed test connection. An authorization
// failure gets exit 5 and the login hint; everything else is a plain
// downstream failure.
func testConnectError(id string, err error) error {
	// By status, not by substring: the transport's message embeds the
	// response-body snippet, so matching "http 401" in it classified a proxy's
	// 502 whose body mentioned an upstream 401 as "your credentials were
	// rejected" — sending the operator to `auth login` for a problem no
	// credential can fix.
	if transport.IsAuthStatus(err) {
		return &Error{
			Code: CodeAuthFailed, ExitCode: ExitAuth,
			Message: fmt.Sprintf("server %q rejected the credentials", id),
			Hint:    fmt.Sprintf("run 'agenthub auth login %s'", id),
			Err:     err,
		}
	}
	return &Error{
		Code: CodeGeneral, ExitCode: ExitGeneral,
		Message: fmt.Sprintf("server %q did not connect", id),
		Err:     err,
	}
}

// contentText flattens the text items of a tools/call content array.
// Non-text items are named by type so a binary result is visible without
// being dumped.
func contentText(raw json.RawMessage) string {
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, it := range items {
		if it.Type == "text" {
			b.WriteString(it.Text)
			b.WriteByte('\n')
			continue
		}
		fmt.Fprintf(&b, "<%s content>\n", it.Type)
	}
	return b.String()
}

// truncate bounds s to max BYTES, cutting on a rune boundary.
//
// `s[:max]` alone splits whatever multi-byte rune straddles the cut, and the
// invalid fragment left behind renders as U+FFFD — in `--json` mode through
// encoding/json, in text mode through the terminal — so a tool answering in
// Chinese ends its output in a replacement character that reads as the TOOL's.
//
// internal/ctlapi/nonregtest.go holds the daemon's copy of the same cutting
// rule, for the same rendering done on the other side of the socket, and the
// rune-boundary behaviour must stay identical. That copy also REPORTS the
// cut on the wire, which this one has no need to: nothing reads `server
// test` output and has to explain the cut back to a person.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "… (truncated)"
}

// compile-time check that the result type renders in both modes.
var _ output.Data = ServerTestResult{}
