package discovery

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/dinstein/agent-hub/internal/discovery/toolsig"
	"github.com/dinstein/agent-hub/internal/mcp"
)

// describe_tool: step two of the two-step discovery of docs/modules/dataplane.md.
//
// search_tools hands back compact signatures. When a signature is not enough
// — a folded nested object, an enum whose tail was elided, a schema the
// grammar could not state — the agent asks describe_tool for the real thing.
// The split is what makes a search result cost one line per hit instead of
// one schema per hit, and the "lossy" flag on a hit is the pointer here.
//
// # Visibility is STRICTLY NOT WIDER than search
//
// describe resolves through Surface.byExposed — the same map tools/list,
// search_tools and ResolveCall read, which is the scope-filtered set
// (docs/architecture.md §7, "one scope, three enforcement points"). Structurally there
// is no other set it could reach, so describe can never reveal a tool search
// hid. That is a property of the code, not a rule someone has to remember.
//
// # One error, no oracle
//
// docs/modules/dataplane.md lists four per-id error kinds (not_found / invisible /
// quarantined / disabled). This implementation emits ONE: not_found. A tool
// that does not exist, one the scope hides, one integrity quarantined and one
// an operator disabled are indistinguishable in the reply — telling them
// apart would turn describe_tool into an enumeration oracle for the parts of
// the catalog a session was deliberately not shown. It is the same rule
// fetch_result follows for cursors and ResolveCall for names, and it is why
// the remediation text is a single frozen sentence.

// MaxDescribeTools bounds one describe_tool call (docs/modules/dataplane.md: "≤5").
// The point of the cap is that describe is the EXPENSIVE step: an agent that
// asks for twenty schemas has skipped the search that would have narrowed
// them. Over the cap is an error, not a silent clamp — a clamp would leave
// the agent believing it saw everything it asked for.
const MaxDescribeTools = 5

// Error codes and text for describe_tool. Frozen: agents key retry logic off
// codes, and prompts key off wording (canonical.md §6).
const (
	// CodeTooManyTools reports a describe_tool call over MaxDescribeTools.
	CodeTooManyTools = "too_many_tools"

	// DescribeErrNotFound is the ONE per-id error kind. See the note above.
	DescribeErrNotFound = "not_found"
)

// describeRemediation is the frozen per-id recovery instruction.
const describeRemediation = "run " + MetaSearchTools + " to find the tool you meant; " +
	"the exposed name in a search result is the name to pass here"

// describeHint is the frozen closing instruction of a describe_tool reply.
const describeHint = "build the arguments from the schema, then invoke the tool with the meta-tool " +
	"named in its call_with field"

// DescribeArgs is the decoded describe_tool payload.
//
// Two spellings are accepted because two designs asked for different ones:
// docs/modules/dataplane.md specifies a batch ("tool_ids:[≤5]") and the M1.5 brief
// specifies the singular convenience form. Both are declared fields, so
// strict decoding still rejects a typo; supplying both, or neither, is an
// error rather than a precedence rule nobody would remember.
type DescribeArgs struct {
	Tool  string   `json:"tool,omitempty"`
	Tools []string `json:"tools,omitempty"`
}

// names returns the requested tool names in call order, de-duplicated.
func (a DescribeArgs) names() []string {
	raw := a.Tools
	if a.Tool != "" {
		raw = append([]string{a.Tool}, raw...)
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, n := range raw {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// ParseDescribe decodes and bounds-checks describe_tool arguments.
func ParseDescribe(raw json.RawMessage) (DescribeArgs, error) {
	var a DescribeArgs
	if err := decodeStrict(raw, &a); err != nil {
		return DescribeArgs{}, err
	}
	if strings.TrimSpace(a.Tool) != "" && len(a.Tools) > 0 {
		return DescribeArgs{}, newError(CodeInvalidArgs,
			"pass either tool (one name) or tools (a list), not both")
	}
	names := a.names()
	if len(names) == 0 {
		return DescribeArgs{}, newError(CodeInvalidArgs,
			"tool must be a non-empty tool name, or tools a non-empty list of them")
	}
	if len(names) > MaxDescribeTools {
		return DescribeArgs{}, newError(CodeTooManyTools,
			"describe at most "+itoa(MaxDescribeTools)+" tools per call; narrow the list and call again")
	}
	return a, nil
}

// DescribeEntry is one successfully described tool: everything a caller needs
// to build a valid call, and nothing about provenance beyond the server id
// the search result already showed.
//
// Field order is the struct order, which is the wire order — deterministic by
// construction.
type DescribeEntry struct {
	Tool     string `json:"tool"`
	Server   string `json:"server"`
	CallWith string `json:"call_with"`
	// Sig is the same signature search_tools returned, repeated so a reply
	// read in isolation is self-contained.
	Sig   string `json:"sig"`
	Lossy bool   `json:"lossy,omitempty"`
	// Description is the FULL description — describe is the step that is
	// allowed to be expensive.
	Description string `json:"description"`
	// Schema is the full inputSchema, or the permissive default when the
	// downstream shipped none or shipped something unparsable (schemaOf).
	Schema json.RawMessage `json:"schema"`
	// OutputSchema is forwarded only when the downstream declared one.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
}

// DescribeError is one unresolvable id.
type DescribeError struct {
	Tool        string `json:"tool"`
	Error       string `json:"error"`
	Remediation string `json:"remediation"`
}

// describePayload is the JSON body of a describe_tool reply.
type describePayload struct {
	Tools  []DescribeEntry `json:"tools"`
	Errors []DescribeError `json:"errors,omitempty"`
	Hint   string          `json:"hint"`
}

// DescribeResult is the structured outcome of one describe_tool call: the
// resolved entries, the unresolvable names, and the savings the signature
// layer booked for the tools that WERE resolved (an agent that had to come
// back here spent the round trip the signature saved, so the caller can net
// the two against each other in the savings stream).
type DescribeResult struct {
	Entries []DescribeEntry
	Errors  []DescribeError
}

// Describe resolves the requested names against the VISIBLE set.
//
// Order is the caller's order for entries and errors alike: an agent that
// asked for [a, b, c] can zip the reply against its own list without
// matching on names.
func (s *Surface) Describe(args DescribeArgs) DescribeResult {
	names := args.names()
	res := DescribeResult{Entries: make([]DescribeEntry, 0, len(names))}
	for _, name := range names {
		t, ok := s.byExposed[name]
		if !ok {
			res.Errors = append(res.Errors, DescribeError{
				Tool:        name,
				Error:       DescribeErrNotFound,
				Remediation: describeRemediation,
			})
			continue
		}
		sig := toolsig.Shared().OfNamed(t.Exposed, t.Def)
		res.Entries = append(res.Entries, DescribeEntry{
			Tool:         t.Exposed,
			Server:       t.ServerID,
			CallWith:     callWithFor(t, s.variants),
			Sig:          sig.Text,
			Lossy:        sig.Lossy,
			Description:  describe(t),
			Schema:       schemaOf(t),
			OutputSchema: outputSchemaOf(t),
		})
	}
	return res
}

// outputSchemaOf forwards a declared outputSchema, dropping one that does not
// parse. A malformed schema is worse than an absent one: the agent would try
// to reason about it.
func outputSchemaOf(t Tool) json.RawMessage {
	raw := t.Def.OutputSchema
	if len(raw) == 0 || !json.Valid(raw) {
		return nil
	}
	return raw
}

// HandleDescribe is the describe_tool handler: decode → resolve → MCP reply.
// It never returns a Go error; every failure is an isError CallResult the
// agent can recover from, which is the same contract HandleSearch keeps.
//
// A call in which NO name resolved still returns a normal (non-error) reply
// carrying the per-id errors: the call itself was well-formed, and turning it
// into a protocol error would cost the agent the remediation text.
func (s *Surface) HandleDescribe(raw json.RawMessage) (*mcp.CallResult, DescribeResult) {
	args, err := ParseDescribe(raw)
	if err != nil {
		return ErrorResult(err), DescribeResult{}
	}
	res := s.Describe(args)
	body, err := json.Marshal(describePayload{
		Tools:  res.Entries,
		Errors: res.Errors,
		Hint:   describeHint,
	})
	if err != nil {
		return ErrorResult(newError(CodeInvalidArgs, "tool schemas could not be encoded")), res
	}
	return &mcp.CallResult{
		Content:           textContent(string(body)),
		StructuredContent: body,
	}, res
}

// DescribeNames reports every name this surface can describe, sorted. It
// exists for the CLI and doctor, which render the surface without going
// through the meta-tool wire shape.
func (s *Surface) DescribeNames() []string {
	out := make([]string, 0, len(s.byExposed))
	for name := range s.byExposed {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
