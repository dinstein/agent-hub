package fakemcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// errStop is the internal signal for a scripted stop (crash / half-frame
// close) or for the client having gone away. Serve translates it into a
// clean nil return.
var errStop = errors.New("fakemcp: stop serving")

// Serve runs the script interpreter over one connection: it reads
// newline-delimited JSON-RPC frames from in, writes responses and scripted
// garbage to out, and writes scripted noise to errOut. A nil script serves
// Minimal().
//
// Serve returns nil when the client closes in (EOF) or a scripted crash /
// close fires, ctx.Err() when ctx is cancelled during a sleep or storm, and
// a non-nil error for interpreter misuse (unknown action kind, oversized
// scripted result) or an unreadable input stream. Malformed inbound frames
// are ignored — the fake never panics on hostile input.
//
// The interpreter is strictly sequential: one message is fully handled
// (including its sleeps and storms) before the next frame is read.
func Serve(ctx context.Context, in io.Reader, out, errOut io.Writer, script *Script) error {
	if script == nil {
		script = Minimal()
	}
	s := &server{
		sc:          script,
		out:         out,
		errOut:      errOut,
		fw:          mcp.NewFrameWriter(out),
		byName:      make(map[string]*Tool, len(script.Tools)),
		methodCount: make(map[string]int),
	}
	for i := range script.Tools {
		s.byName[script.Tools[i].Def.Name] = &script.Tools[i]
	}
	if script.StderrBanner != "" {
		_, _ = io.WriteString(errOut, script.StderrBanner)
	}
	fr := mcp.NewFrameReader(in)
	for {
		line, err := fr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil // client closed the stream: normal shutdown
			}
			return fmt.Errorf("fakemcp: read: %w", err)
		}
		msg, perr := mcp.ParseMessage(line)
		if perr != nil {
			continue // hostile/garbage input is ignored, never fatal
		}
		if err := s.handle(ctx, msg); err != nil {
			if errors.Is(err, errStop) {
				return nil
			}
			return err
		}
	}
}

// server is the per-connection interpreter state.
type server struct {
	sc     *Script
	out    io.Writer
	errOut io.Writer
	fw     *mcp.FrameWriter

	byName      map[string]*Tool
	methodCount map[string]int // per-method arrival count (requests + notifications)
	total       int            // total arrival count

	// poisoned is set after a half-frame: the wire is stuck mid-frame, so
	// any further scripted write would corrupt framing in an unintended
	// way. All subsequent writes are suppressed.
	poisoned bool
}

func (s *server) handle(ctx context.Context, msg any) error {
	var (
		method string
		req    *mcp.Request
	)
	switch m := msg.(type) {
	case *mcp.Request:
		req, method = m, m.Method
	case *mcp.Notification:
		method = m.Method
	default:
		return nil // responses (reverse-RPC answers) are not scripted in M0
	}
	s.total++
	s.methodCount[method]++
	if r := s.matchRule(method); r != nil {
		return s.run(ctx, r.Actions, req)
	}
	if req == nil {
		return nil // default for notifications: ignore
	}
	return s.write(s.defaultResponse(req))
}

// matchRule returns the first rule matching method under the documented
// Call semantics (per-method arrival count; total count for method-less
// rules). Counting is independent of which rule matched.
func (s *server) matchRule(method string) *Rule {
	for i := range s.sc.Rules {
		r := &s.sc.Rules[i]
		if r.Method != "" && r.Method != method {
			continue
		}
		n := s.total
		if r.Method != "" {
			n = s.methodCount[method]
		}
		if r.Call != 0 && r.Call != n {
			continue
		}
		return r
	}
	return nil
}

// run executes a rule's actions in order. req is nil when the rule matched
// a notification; response-shaped actions are then skipped.
func (s *server) run(ctx context.Context, actions []Action, req *mcp.Request) error {
	for _, a := range actions {
		switch a.Kind {
		case ActRespond:
			if req == nil {
				continue
			}
			if err := s.write(s.defaultResponse(req)); err != nil {
				return err
			}
		case ActResult:
			if req == nil {
				continue
			}
			if err := s.write(mcp.NewResponse(req.ID, a.Result)); err != nil {
				return err
			}
		case ActError:
			if req == nil {
				continue
			}
			e := a.Error
			if e == nil {
				e = &mcp.Error{Code: mcp.CodeInternalError, Message: "fakemcp scripted error"}
			}
			if err := s.write(mcp.NewErrorResponse(req.ID, e)); err != nil {
				return err
			}
		case ActSleep:
			if err := sleepCtx(ctx, a.Delay); err != nil {
				return err
			}
		case ActHang:
			return nil // no response, remaining actions skipped, keep serving
		case ActRaw:
			if err := s.writeRaw([]byte(a.Raw)); err != nil {
				return err
			}
		case ActMalformed:
			if err := s.writeRaw([]byte("{this is not valid json\n")); err != nil {
				return err
			}
		case ActHalfFrame:
			if err := s.halfFrame(a, req); err != nil {
				return err
			}
		case ActHuge:
			if err := s.huge(a, req); err != nil {
				return err
			}
		case ActCrash:
			return errStop
		case ActWrongID:
			if req == nil {
				continue
			}
			resp := mcp.NewResponse(mcp.NewStringID("fakemcp-wrong-id"), json.RawMessage(`{}`))
			if err := s.write(resp); err != nil {
				return err
			}
		case ActNotifyInstead:
			method := a.Method
			if method == "" {
				method = mcp.NotificationToolsListChanged
			}
			if err := s.writeMsg(mcp.NewNotification(method, nil)); err != nil {
				return err
			}
		case ActStorm:
			if err := s.storm(ctx, a); err != nil {
				return err
			}
		case ActStderr:
			_, _ = io.WriteString(s.errOut, a.Text)
		default:
			return fmt.Errorf("fakemcp: unknown action kind %q", a.Kind)
		}
	}
	return nil
}

// halfFrame writes a prefix of a valid response frame and poisons the
// write side; with Close it also stops serving (closing the stream leaves
// the client with an unterminated partial frame → EOF).
func (s *server) halfFrame(a Action, req *mcp.Request) error {
	resp := mcp.NewResponse(mcp.NewIntID(1), json.RawMessage(`{}`))
	if req != nil {
		resp = s.defaultResponse(req)
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("fakemcp: half-frame encode: %w", err)
	}
	frame := append(data, '\n')
	n := a.Bytes
	if n <= 0 || n >= len(frame) {
		n = len(frame) / 2
	}
	if err := s.writeRaw(frame[:n]); err != nil {
		return err
	}
	s.poisoned = true
	if a.Close {
		return errStop
	}
	return nil
}

// huge writes a syntactically valid response frame padded with a.Bytes
// filler bytes (default mcp.MaxFrameSize, so the frame total exceeds the
// 16 MiB bounded-read limit and poisons the reading peer).
func (s *server) huge(a Action, req *mcp.Request) error {
	pad := a.Bytes
	if pad <= 0 {
		pad = mcp.MaxFrameSize
	}
	idJSON := []byte("1")
	if req != nil {
		var err error
		if idJSON, err = req.ID.MarshalJSON(); err != nil {
			return fmt.Errorf("fakemcp: huge frame id: %w", err)
		}
	}
	var buf bytes.Buffer
	buf.Grow(pad + 64)
	buf.WriteString(`{"jsonrpc":"2.0","id":`)
	buf.Write(idJSON)
	buf.WriteString(`,"result":"`)
	buf.Write(bytes.Repeat([]byte("a"), pad))
	buf.WriteString("\"}\n")
	return s.writeRaw(buf.Bytes())
}

// storm sends a burst of notifications spaced by a.Delay.
func (s *server) storm(ctx context.Context, a Action) error {
	count := a.Count
	if count <= 0 {
		count = 5
	}
	method := a.Method
	if method == "" {
		method = mcp.NotificationToolsListChanged
	}
	for i := 0; i < count; i++ {
		if i > 0 {
			if err := sleepCtx(ctx, a.Delay); err != nil {
				return err
			}
		}
		if err := s.writeMsg(mcp.NewNotification(method, nil)); err != nil {
			return err
		}
	}
	return nil
}

// write emits one well-formed frame. A write failure means the client is
// gone (stop serving quietly); an oversized scripted payload is a script
// bug and surfaces loudly (use ActHuge to send oversized frames on
// purpose).
func (s *server) write(resp *mcp.Response) error {
	return s.writeMsg(resp)
}

func (s *server) writeMsg(msg any) error {
	if s.poisoned {
		return nil
	}
	if err := s.fw.WriteFrame(msg); err != nil {
		if errors.Is(err, mcp.ErrFrameTooLarge) {
			return fmt.Errorf("fakemcp: scripted frame too large (use the %q action): %w", ActHuge, err)
		}
		return errStop
	}
	return nil
}

// writeRaw emits bytes verbatim, bypassing frame encoding (fault frames).
func (s *server) writeRaw(b []byte) error {
	if s.poisoned {
		return nil
	}
	if _, err := s.out.Write(b); err != nil {
		return errStop
	}
	return nil
}

// defaultResponse implements the built-in normal server behavior.
func (s *server) defaultResponse(req *mcp.Request) *mcp.Response {
	// The stateless protocol's per-request _meta is checked before the
	// method is even read, because on 2026-07-28 it is not payload — it is
	// the handshake, restated on every request. A fake that answered a bare
	// tools/call would certify a client that never learned to send one.
	if resp := s.checkRequestMeta(req); resp != nil {
		return resp
	}
	switch req.Method {
	case mcp.MethodDiscover:
		if len(s.sc.SupportedVersions) == 0 {
			// A pre-2026 server does not know the method, and this is the
			// branch transport.Handshake reads as "alive but old" before
			// falling back to initialize. It is load-bearing for every
			// legacy script in the tree, which is why the default is here
			// rather than in a knob somebody has to remember to unset.
			return methodNotFound(req)
		}
		return s.discover(req)
	case mcp.MethodInitialize:
		return okResponse(req.ID, mcp.InitializeResult{
			ProtocolVersion: s.protocolVersion(),
			Capabilities:    s.capabilities(),
			ServerInfo:      s.serverInfo(),
			Instructions:    s.sc.Instructions,
		})
	case mcp.MethodPing:
		return mcp.NewResponse(req.ID, json.RawMessage(`{}`))
	case mcp.MethodToolsList:
		return s.listTools(req)
	case mcp.MethodToolsCall:
		var p mcp.CallToolParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return mcp.NewErrorResponse(req.ID, &mcp.Error{
				Code:    mcp.CodeInvalidParams,
				Message: "invalid tools/call params: " + err.Error(),
			})
		}
		t, ok := s.byName[p.Name]
		if !ok {
			return mcp.NewErrorResponse(req.ID, &mcp.Error{
				Code:    mcp.CodeInvalidParams,
				Message: fmt.Sprintf("unknown tool %q", p.Name),
			})
		}
		if t.Result != nil {
			return okResponse(req.ID, *t.Result)
		}
		return okResponse(req.ID, echoResult(p.Arguments))
	default:
		return methodNotFound(req)
	}
}

// methodNotFound is the answer to a method this fake does not implement.
func methodNotFound(req *mcp.Request) *mcp.Response {
	return mcp.NewErrorResponse(req.ID, &mcp.Error{
		Code:    mcp.CodeMethodNotFound,
		Message: fmt.Sprintf("fakemcp does not implement %q", req.Method),
	})
}

// discover answers server/discover with the versions the script declared.
//
// The result is a CacheableResult and carries the server's identity in
// _meta rather than in a top-level member, because that is the shape MCP
// 2026-07-28 defines — and because a client reads the identity from exactly
// one of those two places. A fake that put it where the legacy handshake
// puts it would let a client that never learned the new location pass.
func (s *server) discover(req *mcp.Request) *mcp.Response {
	info := s.serverInfo()
	ttl := discoverTTLMs
	return okResponse(req.ID, mcp.DiscoverResult{
		ResultType:        mcp.ResultTypeComplete,
		SupportedVersions: s.sc.SupportedVersions,
		Capabilities:      s.capabilities(),
		Instructions:      s.sc.Instructions,
		Meta:              &mcp.ResultMeta{ServerInfo: &info},
		CacheableResult:   mcp.CacheableResult{TtlMs: &ttl, CacheScope: "public"},
	})
}

// discoverTTLMs is the freshness hint on the discover result — an hour, the
// same figure test/mcpstub uses. Nothing in this tree reads it; it is here
// because the member is required and a fake that omitted it would be
// advertising a result shape no conformant server produces.
var discoverTTLMs = int64(3_600_000)

// checkRequestMeta enforces the per-request _meta of MCP 2026-07-28, and
// returns nil when the request is fine or when this script is not a 2026
// server at all.
//
// WHICH REQUESTS ARE EXEMPT is the whole subtlety. server/discover carries
// its own _meta but arrives before anything is negotiated, and initialize
// arrives only when the client picked a version from this server's list
// that still requires the stateful handshake — a legacy request by
// construction, which carries no _meta and must not be refused for it.
// Everything else on a 2026 session must carry one.
//
// Failure direction: this fake only refuses when it ADVERTISED 2026. A
// script that never declared SupportedVersions is a pre-2026 server and has
// no _meta to demand, so the check is inert for every script that predates
// the field.
func (s *server) checkRequestMeta(req *mcp.Request) *mcp.Response {
	if !s.speaks2026() {
		return nil
	}
	switch req.Method {
	case mcp.MethodDiscover, mcp.MethodInitialize:
		return nil
	}
	var p struct {
		Meta *json.RawMessage `json:"_meta"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return mcp.NewErrorResponse(req.ID, &mcp.Error{
				Code: mcp.CodeInvalidParams, Message: "params: " + err.Error()})
		}
	}
	if p.Meta == nil {
		return mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code: mcp.CodeInvalidParams,
			Message: "missing required _meta (io.modelcontextprotocol/protocolVersion, " +
				"/clientCapabilities)"})
	}
	// Decoded separately from the presence check: RequestMeta's fields are
	// typed, and "the key is absent" and "the key decoded to its zero value"
	// are different failures that must not be reported as one.
	var meta struct {
		ProtocolVersion    string           `json:"io.modelcontextprotocol/protocolVersion"`
		ClientCapabilities *json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
	}
	if err := json.Unmarshal(*p.Meta, &meta); err != nil {
		return mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code: mcp.CodeInvalidParams, Message: "_meta: " + err.Error()})
	}
	if meta.ClientCapabilities == nil {
		// Required on EVERY request. An empty object is how a client says it
		// has no optional capabilities; omitting the key is not the same
		// statement, and a fake that accepted the omission would let a
		// client ship the difference.
		return mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code: mcp.CodeInvalidParams,
			Message: "_meta carries no io.modelcontextprotocol/clientCapabilities; " +
				"it is required on every request"})
	}
	if !slices.Contains(s.sc.SupportedVersions, meta.ProtocolVersion) {
		// -32022 carries its supported/requested payload: the client is
		// being told to retry with a version from the list, and a fake that
		// omitted the list would pass a client that cannot read one.
		return mcp.NewErrorResponse(req.ID, mcp.NewUnsupportedVersionError(
			meta.ProtocolVersion, s.sc.SupportedVersions,
			fmt.Sprintf("request declares protocol version %q; this server speaks %v",
				meta.ProtocolVersion, s.sc.SupportedVersions)))
	}
	return nil
}

// speaks2026 reports whether this script advertises the stateless protocol,
// which is what turns on every rule checkRequestMeta enforces.
func (s *server) speaks2026() bool {
	return slices.Contains(s.sc.SupportedVersions, mcp.Version2026)
}

func (s *server) serverInfo() mcp.Implementation {
	si := s.sc.ServerInfo
	if si.Name == "" {
		si.Name = "fakemcp"
	}
	if si.Version == "" {
		si.Version = "0.1.0"
	}
	return si
}

func (s *server) protocolVersion() string {
	if s.sc.ProtocolVersion != "" {
		return s.sc.ProtocolVersion
	}
	return mcp.ProtocolVersion
}

func (s *server) capabilities() json.RawMessage {
	if len(s.sc.Capabilities) > 0 {
		return s.sc.Capabilities
	}
	return json.RawMessage(`{"tools":{"listChanged":true}}`)
}

// okResponse marshals v as the result; a marshal failure (script bug)
// degrades to an in-band internal error rather than a panic.
func okResponse(id mcp.ID, v any) *mcp.Response {
	raw, err := json.Marshal(v)
	if err != nil {
		return mcp.NewErrorResponse(id, &mcp.Error{Code: mcp.CodeInternalError, Message: err.Error()})
	}
	return mcp.NewResponse(id, raw)
}

// echoResult builds the echo tool's answer: one text content item holding
// the raw argument JSON.
func echoResult(args json.RawMessage) mcp.CallResult {
	if len(args) == 0 {
		args = json.RawMessage("null")
	}
	text, err := json.Marshal(string(args))
	if err != nil {
		// Marshaling a string cannot fail.
		panic(err)
	}
	return mcp.CallResult{
		Content: json.RawMessage(fmt.Sprintf(`[{"type":"text","text":%s}]`, text)),
	}
}

// sleepCtx sleeps for d or until ctx is cancelled.
func sleepCtx(ctx context.Context, d Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(time.Duration(d))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// listTools answers tools/list, paginating when Script.PageSize is positive.
//
// The cursor is the index of the next tool, decimal — except at the first
// boundary, where it is the empty string. That is not an accident: the
// specification calls an empty cursor a valid position that MUST NOT be read
// as the end of the results, and a stub that only ever emits non-empty
// cursors would pass a client that stops on one.
func (s *server) listTools(req *mcp.Request) *mcp.Response {
	defs := make([]mcp.ToolDef, 0, len(s.sc.Tools))
	for _, t := range s.sc.Tools {
		defs = append(defs, t.Def)
	}
	size := s.sc.PageSize
	if size <= 0 {
		return okResponse(req.ID, mcp.ListToolsResult{Tools: defs})
	}
	var p mcp.ListToolsParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return mcp.NewErrorResponse(req.ID, &mcp.Error{
				Code: mcp.CodeInvalidParams, Message: "invalid tools/list params: " + err.Error()})
		}
	}
	start := 0
	if p.Cursor != nil {
		if *p.Cursor == "" {
			start = size // the empty cursor names the second page
		} else {
			n, err := strconv.Atoi(*p.Cursor)
			if err != nil || n < 0 || n > len(defs) {
				// The spec's own answer for a cursor the server cannot place.
				return mcp.NewErrorResponse(req.ID, &mcp.Error{
					Code: mcp.CodeInvalidParams, Message: "unknown cursor " + *p.Cursor})
			}
			start = n
		}
	}
	end := min(start+size, len(defs))
	res := mcp.ListToolsResult{Tools: defs[start:end]}
	if end < len(defs) {
		next := strconv.Itoa(end)
		if end == size {
			next = "" // the first boundary, deliberately empty
		}
		res.NextCursor = &next
	}
	return okResponse(req.ID, res)
}
