package discovery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// The five lazy-mode meta-tools (docs/flows.md plus the describe-in-two-
// steps layer of 7.2). The names are ABI: they appear in agent prompts, in
// client tool-allowlist UIs and in audit records, so they are frozen here
// and nowhere else — the gateway wires itself from these constants.
//
// docs/flows.md names the first one "status"; the M1 task brief writes it
// "retrieve_tools/status". One name has to win: "status" is the one in the
// design sequence diagram, and MetaStatus is the single source of truth for
// every caller.
const (
	// MetaStatus reports the surface itself: which servers are visible,
	// how many tools each has, and what to do next. It is the answer to
	// "the tool list only has five entries, now what?".
	MetaStatus = "status"
	// MetaSearchTools is the lexical search entry point.
	MetaSearchTools = "search_tools"
	// MetaDescribeTool is the second step of the describe-in-two-steps
	// layer (docs/subsystems/exposure.md): search hands back compact signatures, this
	// hands back the full schema for the few tools the agent settled on.
	MetaDescribeTool = "describe_tool"
	// MetaCallTool invokes a tool found through search. It is also the
	// single call entry of grouped mode.
	MetaCallTool = "call_tool"
	// MetaFetchResult pages through a truncated result by cursor.
	MetaFetchResult = "fetch_result"
)

// Search limits. A limit above MaxSearchLimit is clamped rather than
// rejected: the agent gets fewer results than it asked for, which is
// always safe, instead of an error it must learn to avoid.
const (
	DefaultSearchLimit = 5
	MaxSearchLimit     = 20
)

// Frozen JSON Schemas for the meta-tools. Written as literals rather than
// marshalled from structs so the exact bytes are reviewable and golden-
// testable (agents are sensitive to schema wording, canonical.md §6).
const (
	schemaStatus = `{"type":"object","properties":{},"additionalProperties":false}`

	schemaSearchTools = `{"type":"object","properties":{` +
		`"query":{"type":"string","description":"keywords describing the tool you need"},` +
		`"limit":{"type":"integer","minimum":1,"maximum":20,"description":"maximum results (default 5)"}},` +
		`"required":["query"],"additionalProperties":false}`

	schemaCallTool = `{"type":"object","properties":{` +
		`"tool":{"type":"string","description":"exposed tool name from a search result"},` +
		`"arguments":{"type":"object","description":"arguments for the target tool"}},` +
		`"required":["tool"],"additionalProperties":false}`

	schemaDescribeTool = `{"type":"object","properties":{` +
		`"tool":{"type":"string","description":"one exposed tool name from a search result"},` +
		`"tools":{"type":"array","items":{"type":"string"},"maxItems":5,` +
		`"description":"up to 5 exposed tool names (use instead of \"tool\" to describe several at once)"}},` +
		`"additionalProperties":false}`

	schemaFetchResult = `{"type":"object","properties":{` +
		`"cursor":{"type":"string","description":"cursor from a truncated result"},` +
		`"offset":{"type":"integer","minimum":0,"description":"character offset to resume at"},` +
		`"limit":{"type":"integer","minimum":1,"description":"maximum characters to return"}},` +
		`"required":["cursor"],"additionalProperties":false}`
)

// metaDefs is the frozen definition list, in the frozen order. Order is
// contract: it is the order tools/list emits in lazy mode.
var metaDefs = []mcp.ToolDef{
	{
		Name: MetaStatus,
		Description: "Report the agenthub tool surface: visible servers, how many tools each " +
			"exposes, and how to reach them. Call this first when you do not know what is available.",
		InputSchema: json.RawMessage(schemaStatus),
	},
	{
		Name: MetaSearchTools,
		Description: "Search the tools visible to this session by keyword. Every match comes back " +
			"with a compact one-line signature in its sig field; call describe_tool for the full " +
			"input schema. Invoke a result with the meta-tool named in its call_with field.",
		InputSchema: json.RawMessage(schemaSearchTools),
	},
	{
		Name: MetaDescribeTool,
		Description: "Return the full input schema of up to 5 tools found through search_tools. " +
			"Use it when a signature from a search result is not enough to build the arguments.",
		InputSchema: json.RawMessage(schemaDescribeTool),
	},
	{
		Name: MetaCallTool,
		Description: "Invoke a tool by the exposed name reported by search_tools, passing that " +
			"tool's own arguments.",
		InputSchema: json.RawMessage(schemaCallTool),
	},
	{
		Name: MetaFetchResult,
		Description: "Fetch the next page of a result that was truncated, using the cursor from " +
			"the truncation notice.",
		InputSchema: json.RawMessage(schemaFetchResult),
	},
}

// MetaDefs returns the five meta-tool definitions in their frozen order.
// The returned slice is a copy; the definitions themselves are immutable.
func MetaDefs() []mcp.ToolDef {
	out := make([]mcp.ToolDef, len(metaDefs))
	copy(out, metaDefs)
	return out
}

// MetaDefsFor returns the lazy-mode meta-tool definitions for a variant
// setting: the frozen five when variants are off (compatibility mode,
// ruling #18), and status / search_tools / describe_tool / the three call
// variants / fetch_result when they are on. Order is contract in both
// shapes.
func MetaDefsFor(variants bool) []mcp.ToolDef {
	if !variants {
		return MetaDefs()
	}
	out := make([]mcp.ToolDef, 0, len(metaDefs)+len(variantDefs)-1)
	for _, d := range metaDefs {
		if d.Name == MetaCallTool {
			out = append(out, variantDefs...)
			continue
		}
		out = append(out, d)
	}
	return out
}

// MetaNamesFor returns the lazy-mode meta-tool names for a variant setting,
// in the same order MetaDefsFor emits them.
//
// It is the only name-list accessor, on purpose. A sibling that spelled the
// five names out as a literal used to sit above this one, and a hand-written
// copy of a frozen list goes stale the first time the list moves — silently,
// because nothing compares the two. Deriving from MetaDefsFor cannot.
func MetaNamesFor(variants bool) []string {
	defs := MetaDefsFor(variants)
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

// IsMetaName reports whether name is RESERVED by the meta surface: the five
// meta-tools plus the three intent variants (variants.go). It does NOT
// consider the mode or the variant switch — it answers "is this name
// reserved", which is a property of the name alone, and reserving all eight
// unconditionally keeps a downstream tool from shadowing a door that a
// governance flip would open tomorrow. Surface.Classify applies the mode.
func IsMetaName(name string) bool {
	switch name {
	case MetaStatus, MetaSearchTools, MetaDescribeTool, MetaCallTool, MetaFetchResult:
		return true
	}
	return IsCallVariant(name)
}

// ---------------------------------------------------------------------------
// Argument parsing. Every meta-tool decodes with DisallowUnknownFields: a
// typo'd argument must be a loud, recoverable error, never a silently
// ignored field that makes the agent believe it asked for something it did
// not (fail-closed applied to arguments).
// ---------------------------------------------------------------------------

// SearchArgs is the decoded search_tools payload.
type SearchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

// ParseSearch decodes and bounds-checks search_tools arguments. The query
// itself is validated separately (Validate) so callers can record a trace
// for a rejected query.
func ParseSearch(raw json.RawMessage) (SearchArgs, error) {
	var a SearchArgs
	if err := decodeStrict(raw, &a); err != nil {
		return SearchArgs{}, err
	}
	switch {
	case a.Limit < 0:
		return SearchArgs{}, newError(CodeInvalidArgs, "limit must not be negative")
	case a.Limit == 0:
		a.Limit = DefaultSearchLimit
	case a.Limit > MaxSearchLimit:
		a.Limit = MaxSearchLimit
	}
	return a, nil
}

// CallToolArgs is the decoded call_tool payload. Arguments travel as raw
// JSON: this package never inspects, rewrites or logs them, and neither does
// anything downstream of it — what the caller sent is what the server
// receives.
type CallToolArgs struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ParseCallTool decodes call_tool arguments and checks their shape. It does
// not resolve the tool name; see Surface.ResolveCall.
func ParseCallTool(raw json.RawMessage) (CallToolArgs, error) {
	var a CallToolArgs
	if err := decodeStrict(raw, &a); err != nil {
		return CallToolArgs{}, err
	}
	if strings.TrimSpace(a.Tool) == "" {
		return CallToolArgs{}, newError(CodeInvalidArgs, "tool must be a non-empty tool name")
	}
	if len(a.Arguments) > 0 && !isJSONObject(a.Arguments) {
		return CallToolArgs{}, newError(CodeInvalidArgs, "arguments must be a JSON object")
	}
	return a, nil
}

// ResolveCall parses call_tool arguments and resolves the target against
// the VISIBLE set — the same set tools/list and search_tools use, so a
// call can never reach a tool the session was never shown (docs/model.md,
// third enforcement point; the pipeline's scope gate then re-checks the
// route on the execution path).
//
// An unresolvable name yields CodeUnknownTool with a message that does not
// distinguish "does not exist" from "not visible to you" — the same
// anti-probing rule fetch_result cursors follow (docs/flows.md).
func (s *Surface) ResolveCall(raw json.RawMessage) (Tool, json.RawMessage, error) {
	a, err := ParseCallTool(raw)
	if err != nil {
		return Tool{}, nil, err
	}
	t, ok := s.byExposed[a.Tool]
	if !ok {
		return Tool{}, nil, newError(CodeUnknownTool,
			fmt.Sprintf("no tool named %q is available; use %s to find one", a.Tool, MetaSearchTools))
	}
	return t, a.Arguments, nil
}

// FetchArgs is the decoded fetch_result payload. Cursor is opaque here:
// ownership and expiry are enforced by the shaping cache that minted it
// (session-bound, docs/flows.md) — this package only validates the shape.
type FetchArgs struct {
	Cursor string `json:"cursor"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// ParseFetchResult decodes fetch_result arguments.
func ParseFetchResult(raw json.RawMessage) (FetchArgs, error) {
	var a FetchArgs
	if err := decodeStrict(raw, &a); err != nil {
		return FetchArgs{}, err
	}
	if strings.TrimSpace(a.Cursor) == "" {
		return FetchArgs{}, newError(CodeInvalidArgs, "cursor must be a non-empty cursor value")
	}
	if a.Offset < 0 {
		return FetchArgs{}, newError(CodeInvalidArgs, "offset must not be negative")
	}
	if a.Limit < 0 {
		return FetchArgs{}, newError(CodeInvalidArgs, "limit must not be negative")
	}
	return a, nil
}

// decodeStrict decodes raw into v rejecting unknown fields. An absent
// payload decodes as the zero value (a meta-tool with only optional fields
// is legitimately callable with no arguments); a non-object payload is a
// typed error, never a panic.
func decodeStrict(raw json.RawMessage, v any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return newError(CodeInvalidArgs, "arguments are not valid for this tool")
	}
	return nil
}

// isJSONObject reports whether raw is a JSON object.
func isJSONObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '{' && json.Valid(t)
}

// ---------------------------------------------------------------------------
// Handlers. They compute the reply; the gateway performs the I/O. call_tool
// and fetch_result have no handler here BY DESIGN: executing a call must go
// through internal/pipeline (the single execute path) and paging must go
// through internal/shaping, so this package stops at Resolve/Parse.
// ---------------------------------------------------------------------------

// SearchRequest is the ranker's input after argument decoding.
type SearchRequest struct {
	Query string
	Limit int // 0 = DefaultSearchLimit
}

// request converts decoded arguments into a ranker request. The two types
// stay separate on purpose: SearchArgs is the wire shape (json tags, strict
// decoding), SearchRequest is the ranker's input, callable without going
// through JSON at all.
func (a SearchArgs) request() SearchRequest { return SearchRequest(a) }

// SearchResult is the structured outcome of one search: the projected
// hits, the guard verdict and the trace.
type SearchResult struct {
	Hits       []Hit
	Matched    int
	Truncated  bool
	Escalation Escalation
	Trace      Trace
}

// Search ranks the visible tools against a query and applies the budget
// projection and the SearchGuard.
//
// Order of operations is fixed and load-bearing: validate → rank over the
// SCOPE-FILTERED candidate set → guard → project. Guarding before
// projection means the escalation reply costs one line instead of a full
// result set; ranking over the pre-filtered set means search can never
// surface a tool the session cannot call.
//
// guard may be nil (stateless callers and tests). The returned Trace is
// always populated, including on validation failure.
func (s *Surface) Search(req SearchRequest, guard *SearchGuard) (*SearchResult, error) {
	q, err := Validate(req.Query)
	if err != nil {
		return &SearchResult{Trace: traceOfRejection(req.Query, codeOf(err))}, err
	}

	limit := req.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}

	cands := s.rank(q)
	res := &SearchResult{Matched: len(cands)}
	top, topScore := "", 0
	if len(cands) > 0 {
		top, topScore = cands[0].tool.Exposed, cands[0].score
	}
	res.Escalation = guard.ObserveSearch(top, topScore)

	if len(cands) > limit {
		cands = cands[:limit]
		res.Truncated = true
	}
	if !res.Escalation.Fire {
		res.Hits = project(cands, s.variants)
	}

	res.Trace = Trace{
		QueryBytes: q.Bytes,
		QueryWords: q.Words,
		Results:    hitNames(res.Hits),
		Matched:    res.Matched,
		TopScore:   topScore,
		Truncated:  res.Truncated,
		Escalated:  res.Escalation.Fire,
	}
	if res.Escalation.Fire {
		// The escalation reply names exactly one tool; the trace records
		// what the agent was actually told.
		res.Trace.Results = []string{res.Escalation.Tool}
	}
	return res, nil
}

func hitNames(hits []Hit) []string {
	if len(hits) == 0 {
		return nil
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Tool)
	}
	return out
}

// searchHint is the frozen closing instruction of a successful search.
// Two wordings, one per call-door shape: in variant mode the agent must be
// told to read call_with rather than to reach for a name it already knows,
// because the wrong door is a rejection (Surface.ResolveCallVariant).
const (
	searchHint = "each sig is the tool's compact signature; call " + MetaDescribeTool +
		"{tool} for the full schema when it is not enough. Call the tool you want with " +
		"call_tool{tool, arguments}; search again with different words if none of these fit"

	searchHintVariants = "each sig is the tool's compact signature; call " + MetaDescribeTool +
		"{tool} for the full schema when it is not enough. Call the tool you want with the " +
		"meta-tool named in its call_with field, passing {tool, arguments}; search again with " +
		"different words if none of these fit"
)

// hint returns the closing instruction for this surface's call-door shape.
func (s *Surface) hint() string {
	if s.variants {
		return searchHintVariants
	}
	return searchHint
}

// searchPayload is the JSON body of a search reply. Field order is the
// struct order — deterministic by construction.
type searchPayload struct {
	Results   []Hit  `json:"results"`
	Matched   int    `json:"matched"`
	Truncated bool   `json:"truncated,omitempty"`
	Hint      string `json:"hint"`
}

// HandleSearch is the search_tools handler: decode → search → MCP reply.
// It never returns a Go error — every failure is an isError CallResult the
// agent can recover from — and always returns a Trace for audit.
func (s *Surface) HandleSearch(raw json.RawMessage, guard *SearchGuard) (*mcp.CallResult, *SearchResult) {
	args, err := ParseSearch(raw)
	if err != nil {
		return ErrorResult(err), &SearchResult{Trace: traceOfRejection(argsQuery(raw), codeOf(err))}
	}
	res, err := s.Search(args.request(), guard)
	if err != nil {
		return ErrorResult(err), res
	}
	if res.Escalation.Fire {
		// Truncated to ONE imperative line: no results, no alternatives.
		return textResult(res.Escalation.Message), res
	}
	if len(res.Hits) == 0 {
		return textResult("no tool matches this query; try other words or call " +
			MetaStatus + " to see what is available"), res
	}
	body, err := json.Marshal(searchPayload{
		Results:   res.Hits,
		Matched:   res.Matched,
		Truncated: res.Truncated,
		Hint:      s.hint(),
	})
	if err != nil {
		return ErrorResult(newError(CodeInvalidArgs, "search results could not be encoded")), res
	}
	return &mcp.CallResult{
		Content:           textContent(string(body)),
		StructuredContent: body,
	}, res
}

// argsQuery best-effort extracts the query length source for a trace when
// argument decoding failed. It reads the "query" field only, and only to
// MEASURE it — the value never leaves this function.
func argsQuery(raw json.RawMessage) string {
	var probe struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Query
}

// codeOf extracts the stable code from a typed error.
func codeOf(err error) string {
	var e *Error
	if ok := asError(err, &e); ok {
		return e.Code
	}
	return CodeInvalidArgs
}

func asError(err error, target **Error) bool {
	if e, ok := err.(*Error); ok {
		*target = e
		return true
	}
	return false
}

// HandleStatus is the status handler: the orientation reply. It reports the
// mode, the visible servers with their tool counts, any pinned tools, and
// the next step. Arguments are ignored — status has no parameters, and
// failing a purely informational call over a stray field would be hostile.
//
// Only lazy mode EXPOSES status as a tool (Classify enforces that), but the
// reply is computable in every mode: the CLI and doctor render the same
// text for a session whose mode is grouped or full.
//
// Output is deterministic: servers sorted, counts exact, wording frozen.
//
// diagnosis is the caller's downstream connection report, appended verbatim
// after the visibility block. The Surface knows only what is VISIBLE, so
// without it a total connection failure and an empty registry both render as
// "0 server(s) visible" and the reader cannot tell which one they have. Empty
// = nothing to add (every server connected, or the caller tracks no state).
func (s *Surface) HandleStatus(diagnosis string) *mcp.CallResult {
	var b strings.Builder
	total := len(s.tools)
	fmt.Fprintf(&b, "agenthub discovery: mode=%s, %d server(s), %d tool(s) visible.\n",
		s.mode, len(s.serverIDs), total)
	for _, id := range s.serverIDs {
		fmt.Fprintf(&b, "  %s: %d tool(s)\n", id, len(s.byServer[id]))
	}
	if len(s.serverIDs) == 0 {
		b.WriteString("  (no server is visible to this session)\n")
	}
	b.WriteString(diagnosis)
	if len(s.pinned) > 0 {
		names := make([]string, 0, len(s.pinned))
		for _, t := range s.pinned {
			names = append(names, t.Exposed)
		}
		fmt.Fprintf(&b, "pinned (callable directly): %s\n", strings.Join(names, ", "))
	}
	switch s.mode {
	case ModeLazy:
		if s.variants {
			fmt.Fprintf(&b, "next: %s{query} to find a tool, then the %s meta-tool named in its "+
				"call_with field ({tool, arguments}) to run it.", MetaSearchTools, MetaCallTool+"_*")
			break
		}
		fmt.Fprintf(&b, "next: %s{query} to find a tool, then %s{tool, arguments} to run it.",
			MetaSearchTools, MetaCallTool)
	case ModeGrouped:
		fmt.Fprintf(&b, "next: call a <server>%s entry for that server's tools, then %s{tool, arguments} to run one.",
			groupSuffix, MetaCallTool)
	default:
		b.WriteString("next: every visible tool is listed directly; call it by name.")
	}
	return textResult(b.String())
}

// textResult wraps plain text in a single MCP text content block.
func textResult(text string) *mcp.CallResult {
	return &mcp.CallResult{Content: textContent(text)}
}

// ErrorResult renders a typed error as an isError tool result: the MCP
// shape an agent can read and retry against. Protocol-level JSON-RPC errors
// are reserved for protocol-level failures; a bad query is an application
// outcome.
func ErrorResult(err error) *mcp.CallResult {
	code, msg := CodeInvalidArgs, "invalid arguments"
	var e *Error
	if asError(err, &e) {
		code, msg = e.Code, e.Message
	}
	return &mcp.CallResult{
		Content: textContent(code + ": " + msg),
		IsError: true,
	}
}

// textContent marshals one text content block. Marshalling (rather than
// string concatenation) is what keeps a tool description containing quotes
// or newlines from producing malformed JSON.
func textContent(text string) json.RawMessage {
	type block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	raw, err := json.Marshal([]block{{Type: "text", Text: text}})
	if err != nil {
		// Unreachable for a string field; fail loud but well-formed.
		return json.RawMessage(`[{"type":"text","text":"agenthub: encoding failure"}]`)
	}
	return raw
}
