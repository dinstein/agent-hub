// Package fakemcp is a programmable fake downstream MCP server for tests
// (docs/conventions.md#engineering-conventions, "Test infrastructure").
//
// Every concurrency and safety invariant of downstream / router / pipeline /
// gateway is tested against this package, so programmability matters more
// than the fake's own polish: behavior is a declarative, JSON-serializable
// Script interpreted by Serve, which means the exact same fault script runs
// in-process (Connect) and as a real spawned child process (StdioConfig +
// transport.SpawnStdio re-executing the current binary via MaybeServe).
//
// A Script has three layers:
//
//   - handshake configuration (ServerInfo / ProtocolVersion / Capabilities,
//     and SupportedVersions, which decides whether the fake answers
//     server/discover at all and therefore which protocol generation it
//     speaks),
//   - a tool set served by default tools/list and tools/call handling,
//   - an ordered list of Rules. Each incoming request or notification is
//     matched against the rules (first match wins, by method and optionally
//     by Nth call); a matched rule's Actions replace default handling.
//
// Actions are the fault-injection primitives: slow responses, never
// responding, half-written and malformed frames, oversized (>16 MiB)
// payloads, crashing mid-handshake, list_changed storms, protocol
// violations (wrong response id, notification instead of response), and
// stderr noise. Version mismatch is scripted via Script.ProtocolVersion.
//
// Invariants:
//
//   - Script is pure data: json.Marshal/Unmarshal round-trips it exactly,
//     so the subprocess driver passes it through one environment variable.
//   - The interpreter is single-threaded per connection; scripted writes
//     never interleave within a frame.
//   - The fake never panics on hostile client input; malformed inbound
//     frames are ignored.
//
// Like everything that speaks MCP outside internal/mcp itself, this package
// uses only the internal/mcp facade (plus its transport subpackage) and the
// standard library.
package fakemcp

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// Script is the complete, JSON-serializable behavior specification of a
// fake server. The zero value (plus defaults applied at serve time) is a
// working MCP server with no tools.
type Script struct {
	// ServerInfo is returned from initialize. Empty Name/Version default to
	// "fakemcp" / "0.1.0".
	ServerInfo mcp.Implementation `json:"serverInfo"`
	// ProtocolVersion is echoed in the initialize result. Empty defaults to
	// mcp.ProtocolVersion. Set an unsupported value (e.g. "1999-01-01") to
	// script a version-negotiation failure.
	ProtocolVersion string `json:"protocolVersion,omitempty"`
	// SupportedVersions is what server/discover advertises — the field that
	// makes this fake a 2026-07-28 server rather than a ≤ 2025-11-25 one.
	//
	// EMPTY IS THE DEFAULT AND MEANS "no server/discover", answered with
	// method-not-found. That is what a pre-2026 server does, it is what makes
	// transport.Handshake fall back to the initialize handshake, and it is
	// what every script written before this field existed depends on. The
	// field is additive for exactly that reason: which generation a fake
	// speaks becomes scripted rather than assumed, and the assumption it
	// replaces stays the default.
	//
	// The list is advertised VERBATIM, including values this tree does not
	// support, so the negotiation outcomes are scriptable the same way
	// ProtocolVersion scripts one for the legacy path:
	//
	//	[]string{mcp.Version2026}   a stateless 2026 session
	//	[]string{mcp.Version2025}   discover answers, but the negotiated
	//	                            version still requires the stateful
	//	                            handshake, so initialize runs after it
	//	[]string{"1999-01-01"}      no mutual version: a fatal handshake
	SupportedVersions []string `json:"supportedVersions,omitempty"`
	// Capabilities is the raw capabilities object of the initialize result.
	// Empty defaults to {"tools":{"listChanged":true}}.
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	// Instructions is the optional initialize instructions field.
	Instructions string `json:"instructions,omitempty"`
	// Tools is served by tools/list; tools/call resolves against it.
	Tools []Tool `json:"tools,omitempty"`
	// PageSize, when positive, makes tools/list paginate at that many tools
	// per page. Zero serves every tool in one answer, which is what almost
	// every test wants.
	//
	// The cursor for the FIRST page boundary is deliberately the empty
	// string. The specification says an empty cursor is a valid position and
	// MUST NOT be read as the end of results, and a stub that never hands
	// one out cannot catch a client that reads it that way — which is
	// exactly the shape of the bug this knob was added for.
	PageSize int `json:"pageSize,omitempty"`
	// Rules override default handling. First matching rule wins; a message
	// matching no rule gets default handling.
	Rules []Rule `json:"rules,omitempty"`
	// StderrBanner is written to stderr once, before serving. Make it
	// larger than 4 KiB to exercise the transport stderr tail window.
	StderrBanner string `json:"stderrBanner,omitempty"`
}

// Tool is one served tool. A nil Result makes the tool an echo tool: it
// answers tools/call with a single text content item containing the raw
// argument JSON.
type Tool struct {
	Def    mcp.ToolDef     `json:"def"`
	Result *mcp.CallResult `json:"result,omitempty"`
}

// Rule matches incoming requests and notifications by method name and
// optionally by call sequence, and replaces default handling with Actions.
type Rule struct {
	// Method matches the JSON-RPC method (requests and notifications
	// alike). Empty matches every method.
	Method string `json:"method,omitempty"`
	// Call, when non-zero, restricts the rule to the Nth (1-based) message
	// carrying Method — or the Nth message overall when Method is empty.
	// Counting is per method name, independent of which rule matched.
	Call int `json:"call,omitempty"`
	// Actions run in order. An empty list consumes the message silently
	// (equivalent to a single hang action).
	Actions []Action `json:"actions"`
}

// ActionKind discriminates the Action union.
type ActionKind string

// Action kinds — the fault-injection primitives.
const (
	// ActRespond performs the built-in default handling for the request.
	ActRespond ActionKind = "respond"
	// ActResult responds with the literal Result payload.
	ActResult ActionKind = "result"
	// ActError responds with the JSON-RPC Error (default: internal error).
	ActError ActionKind = "error"
	// ActSleep pauses for Delay (aborted by ctx cancellation).
	ActSleep ActionKind = "sleep"
	// ActHang stops processing this message: no response, ever. The server
	// keeps reading, so later requests are still handled.
	ActHang ActionKind = "hang"
	// ActRaw writes Raw bytes verbatim to the stream.
	ActRaw ActionKind = "raw"
	// ActMalformed writes a complete line that is not valid JSON.
	ActMalformed ActionKind = "malformed"
	// ActHalfFrame writes the first Bytes bytes (default: half) of a valid
	// response frame, then never completes it; all further scripted writes
	// are suppressed (the stream is poisoned mid-frame). Close additionally
	// stops serving, closing the stream.
	ActHalfFrame ActionKind = "half-frame"
	// ActHuge writes a syntactically valid response frame padded with Bytes
	// filler bytes (default mcp.MaxFrameSize, i.e. just over the 16 MiB
	// bounded-read limit).
	ActHuge ActionKind = "huge"
	// ActCrash stops serving immediately: subprocess exits, in-process
	// stream closes. Attach to "initialize" for a mid-handshake crash.
	ActCrash ActionKind = "crash"
	// ActWrongID responds with a response whose id matches no request.
	ActWrongID ActionKind = "wrong-id"
	// ActNotifyInstead sends a notification (Method, default
	// notifications/tools/list_changed) instead of a response.
	ActNotifyInstead ActionKind = "notify-instead"
	// ActStorm sends Count (default 5) notifications of Method (default
	// notifications/tools/list_changed) spaced Delay apart.
	ActStorm ActionKind = "storm"
	// ActStderr writes Text to stderr.
	ActStderr ActionKind = "stderr"
	// ActInputRequired answers a tools/call with the MCP 2026-07-28
	// input_required interim result (MRTR): the client must resolve every
	// entry of inputRequests and retry the call — with a NEW JSON-RPC id —
	// carrying the answers plus the requestState echoed verbatim.
	//
	// Result carries the inputRequests object (default: one roots/list
	// request under the key "roots"); Text carries the requestState
	// (default: an opaque constant). Pair it with Rule.Call to answer the
	// first call this way and let the retry fall through to normal
	// handling:
	//
	//	Rule{Method: mcp.MethodToolsCall, Call: 1,
	//	     Actions: []Action{{Kind: ActInputRequired}}}
	//
	// The retry is CHECKED, not merely accepted: the fake refuses one whose
	// requestState is missing or altered, and echoes the answers back in
	// the result's structuredContent so a test can prove they arrived. A
	// fake that took any retry would pass a client that dropped either.
	ActInputRequired ActionKind = "input-required"
)

// Action is one step of a rule. Which fields are meaningful depends on
// Kind; see the ActionKind constants. Unknown kinds fail Serve loudly — a
// mistyped script must not silently pass.
type Action struct {
	Kind   ActionKind      `json:"kind"`
	Delay  Duration        `json:"delay,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *mcp.Error      `json:"error,omitempty"`
	Raw    string          `json:"raw,omitempty"`
	Bytes  int             `json:"bytes,omitempty"`
	Close  bool            `json:"close,omitempty"`
	Count  int             `json:"count,omitempty"`
	Method string          `json:"method,omitempty"`
	Text   string          `json:"text,omitempty"`
}

// Duration is a time.Duration that marshals as a duration string ("150ms")
// so scripts stay readable and env-var-portable. Plain JSON numbers are
// accepted as nanoseconds on unmarshal.
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("fakemcp: invalid duration: %w", err)
	}
	switch x := v.(type) {
	case string:
		dur, err := time.ParseDuration(x)
		if err != nil {
			return fmt.Errorf("fakemcp: invalid duration %q: %w", x, err)
		}
		*d = Duration(dur)
		return nil
	case float64:
		*d = Duration(time.Duration(x))
		return nil
	default:
		return fmt.Errorf("fakemcp: duration must be a string or number, got %T", v)
	}
}

// Minimal returns a normally-behaving script serving one echo tool per
// name (default: a single tool named "echo"). Echo tools answer tools/call
// with a text content item containing the raw argument JSON.
func Minimal(toolNames ...string) *Script {
	if len(toolNames) == 0 {
		toolNames = []string{"echo"}
	}
	tools := make([]Tool, 0, len(toolNames))
	for _, name := range toolNames {
		tools = append(tools, Tool{Def: mcp.ToolDef{
			Name:        name,
			Description: "echoes its arguments back as text",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}})
	}
	return &Script{Tools: tools}
}

// With appends rules and returns the script for chaining:
//
//	fakemcp.Minimal().With(fakemcp.CrashOn(mcp.MethodInitialize))
func (s *Script) With(rules ...Rule) *Script {
	s.Rules = append(s.Rules, rules...)
	return s
}

// ParseScript decodes a script from JSON.
func ParseScript(data []byte) (*Script, error) {
	var s Script
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("fakemcp: parse script: %w", err)
	}
	return &s, nil
}

// Convenience rule constructors for the common fault shapes. They are
// sugar only — everything is expressible as a literal Rule.

// SlowResponse answers method normally after a delay of d.
func SlowResponse(method string, d time.Duration) Rule {
	return Rule{Method: method, Actions: []Action{
		{Kind: ActSleep, Delay: Duration(d)},
		{Kind: ActRespond},
	}}
}

// NeverRespond consumes method and never answers it (until ctx / process
// exit); the server keeps serving other requests.
func NeverRespond(method string) Rule {
	return Rule{Method: method, Actions: []Action{{Kind: ActHang}}}
}

// CrashOn stops the server the moment method is received (for
// mcp.MethodInitialize: a mid-handshake crash / stdout close).
func CrashOn(method string) Rule {
	return Rule{Method: method, Actions: []Action{{Kind: ActCrash}}}
}

// MalformedResponse answers method with a frame that is not valid JSON.
func MalformedResponse(method string) Rule {
	return Rule{Method: method, Actions: []Action{{Kind: ActMalformed}}}
}

// HalfFrameResponse writes only prefixBytes bytes (0 = half) of the
// response frame for method; closeAfter additionally closes the stream.
func HalfFrameResponse(method string, prefixBytes int, closeAfter bool) Rule {
	return Rule{Method: method, Actions: []Action{
		{Kind: ActHalfFrame, Bytes: prefixBytes, Close: closeAfter},
	}}
}

// HugeResponse answers method with a valid frame padded by padBytes filler
// bytes (0 = mcp.MaxFrameSize, defeating the 16 MiB bounded read).
func HugeResponse(method string, padBytes int) Rule {
	return Rule{Method: method, Actions: []Action{{Kind: ActHuge, Bytes: padBytes}}}
}

// WrongIDResponse answers method with an id matching no outstanding call.
func WrongIDResponse(method string) Rule {
	return Rule{Method: method, Actions: []Action{{Kind: ActWrongID}}}
}

// NotificationInsteadOfResponse violates the protocol by answering the
// method request with a notification and no response.
func NotificationInsteadOfResponse(method string) Rule {
	return Rule{Method: method, Actions: []Action{{Kind: ActNotifyInstead}}}
}

// ListChangedStorm sends count tools/list_changed notifications spaced by
// interval when method arrives, then answers the request normally.
func ListChangedStorm(method string, count int, interval time.Duration) Rule {
	return Rule{Method: method, Actions: []Action{
		{Kind: ActStorm, Count: count, Delay: Duration(interval)},
		{Kind: ActRespond},
	}}
}

// StderrNoise writes text to stderr when method arrives, then answers the
// request normally (exercises the 4 KiB stderr tail window).
func StderrNoise(method, text string) Rule {
	return Rule{Method: method, Actions: []Action{
		{Kind: ActStderr, Text: text},
		{Kind: ActRespond},
	}}
}
