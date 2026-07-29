package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// Argument self-healing (docs/modules/dataplane.md, "self-heal invalid_params" row, M1 scope
// per 7.12). The repair rules live here; the two places they run are:
//
//  1. PRE-CALL, inside the precheck gate (gates.go). Our own shallow
//     validator already sees most repairable shapes, so the repair happens
//     BEFORE the round trip: the downstream never receives the broken call
//     and there is nothing to retry. This is where "5" → 5 is fixed.
//  2. POST-CALL, in Pipeline.selfHeal. Reached when the downstream rejects
//     arguments our precheck accepted AND its rejection carries its OWN
//     inputSchema (the 7.2 error shape). That schema is new evidence the
//     precheck could not have had, so the call is re-issued ONCE against it.
//
// Three properties keep this from becoming a guessing machine:
//
//  1. ONLY provably-safe repairs. A missing required field is filled from
//     the schema's own `default`; a type mismatch is fixed only when the
//     conversion round-trips byte-for-byte ("5" → 5 → "5"). Anything that
//     would require inventing a value is left alone.
//  2. AT MOST ONE retry. A healed call that fails again is reported as it
//     is; there is no second heal, no ladder, no loop.
//  3. AUDITED. Every heal (pre-call repair and post-call retry alike) goes
//     to the OnSelfHeal seam, so a repaired call is never invisible: what
//     the agent sent and what actually ran must stay reconstructable.
//
// Failure direction: when in doubt, DO NOT heal. An unhealed invalid_params
// is a correct, informative error; a wrongly healed call is a side effect
// the caller never asked for.

// SelfHealEvent is one audit record of the self-heal path. It carries no
// argument VALUES — only the field names that were repaired and how — so it
// inherits the audit invariant that governance records never hold call
// arguments (internal/audit doc).
type SelfHealEvent struct {
	// Exposed / ServerID / RawTool identify the call.
	Exposed  string
	ServerID string
	RawTool  string
	// Fixes describes each repair as "<field>: <kind>" (see fix.String).
	// Never empty when Retried is true.
	Fixes []string
	// Retried reports that the downstream was called a second time.
	Retried bool
	// Recovered reports that the second call succeeded. Meaningless when
	// Retried is false.
	Recovered bool
}

// SelfHealFunc receives one SelfHealEvent. It must not block (it runs on the
// call path); the standard wiring hands it to an audit stream, whose append
// is non-blocking by construction.
type SelfHealFunc func(ctx context.Context, ev SelfHealEvent)

// CallWithArgs is the argument-parameterized form of CallFunc. Setting it on
// a CallRequest is what ENABLES self-healing: the pipeline can only re-issue
// a call whose arguments it controls, and CallFunc captures its arguments in
// a closure by construction. With only CallFunc set, an invalid_params
// answer passes through unchanged (fail-open toward the caller's own error,
// which is exactly the pre-self-heal behaviour).
type CallWithArgs func(ctx context.Context, args json.RawMessage) (*mcp.CallResult, error)

// fixKind names a repair for the audit record. The strings are contract
// (they appear in audit records and in golden tests); do not rename.
type fixKind string

const (
	fixDefault fixKind = "filled_from_schema_default"
	fixCoerce  fixKind = "type_coerced_losslessly"
)

// fix is one repair applied to one top-level argument.
type fix struct {
	field string
	kind  fixKind
}

func (f fix) String() string { return f.field + ": " + string(f.kind) }

// fixNames renders a repair list for the audit record.
func fixNames(fixes []fix) []string {
	out := make([]string, 0, len(fixes))
	for _, f := range fixes {
		out = append(out, f.String())
	}
	return out
}

// invalidParamsRe matches the parameter-error dialects downstream servers
// answer with when they do not use the JSON-RPC code. Deliberately narrow:
// see authSmellRe for what must never reach it.
var invalidParamsRe = regexp.MustCompile(`(?i)\b(invalid[_ -]?(params|arguments|input)|missing[_ -]?(required[_ -]?)?(param|parameter|argument|field|property)|required[_ -]?(param|parameter|argument|field|property)|parameter[_ -]?validation|schema[_ -]?validation|is not of type|expected .* but (got|received))`)

// authSmellRe is the DENY-LIST of docs/modules/dataplane.md: an authentication or
// transport failure that merely mentions a parameter must never be
// reclassified as invalid_params. Mislabeling a 401 that way makes the agent
// retry a call that can only ever fail — the exact anti-pattern the design
// calls out.
var authSmellRe = regexp.MustCompile(`(?i)\b(401|403|407|unauthorized|unauthenticated|forbidden|access denied|permission denied|invalid[_ -]?(token|credential|api[_ -]?key)|expired[_ -]?token|token[_ -]?expired|rate[_ -]?limit|429|timeout|timed out|deadline exceeded|connection (refused|reset)|network|tls|certificate)\b`)

// isInvalidParams reports whether the downstream outcome is a PARAMETER
// error. Two shapes count:
//
//   - a JSON-RPC error with code -32602 (the unambiguous signal), and
//   - an isError tool result / error text whose wording matches the
//     parameter dialect AND does not smell of auth or transport.
//
// The text path exists because most MCP servers report tool-level argument
// problems as an isError result, not as a protocol error.
func isInvalidParams(res *mcp.CallResult, callErr error) bool {
	var me *mcp.Error
	if errors.As(callErr, &me) && me.Code == mcp.CodeInvalidParams {
		return true
	}
	text := outcomeText(res, callErr)
	if text == "" {
		return false
	}
	if authSmellRe.MatchString(text) {
		return false // deny-list wins over the classifier, always
	}
	return invalidParamsRe.MatchString(text)
}

// outcomeText extracts the text a classifier may read: the error message on
// the error branch, the text content blocks of an isError result otherwise.
// A successful result is never classified (an empty string here means "not a
// candidate").
func outcomeText(res *mcp.CallResult, callErr error) string {
	if callErr != nil {
		return callErr.Error()
	}
	if res == nil || !res.IsError {
		return ""
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(res.Content, &blocks); err != nil {
		return string(res.Content) // unparsable content: classify the bytes
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// schemaFromRejection extracts the inputSchema a downstream attached to its
// invalid_params rejection (docs/modules/dataplane.md error shape:
// {error_type, tool, hint, input_schema}). Both the JSON-RPC error `data`
// object and an isError result's structuredContent are inspected, under
// either spelling of the key.
//
// Returns nil when the rejection carries no schema — which is the common
// case and must stay cheap and silent.
func schemaFromRejection(res *mcp.CallResult, callErr error) json.RawMessage {
	var me *mcp.Error
	if errors.As(callErr, &me) && len(me.Data) > 0 {
		if s := schemaField(me.Data); len(s) > 0 {
			return s
		}
	}
	if res != nil && res.IsError && len(res.StructuredContent) > 0 {
		return schemaField(res.StructuredContent)
	}
	return nil
}

// schemaField pulls input_schema / inputSchema out of a JSON object.
func schemaField(raw json.RawMessage) json.RawMessage {
	var obj struct {
		Snake json.RawMessage `json:"input_schema"`
		Camel json.RawMessage `json:"inputSchema"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	if len(obj.Snake) > 0 {
		return obj.Snake
	}
	return obj.Camel
}

// healArgs repairs args against schema and returns the repaired encoding
// plus the applied fixes. ok=false means "nothing provably fixable" — the
// caller must then NOT retry.
func healArgs(args, schema json.RawMessage) (json.RawMessage, []fix, bool) {
	obj, err := decodeArgsObject(args)
	if err != nil {
		return nil, nil, false // non-object args are not repairable
	}
	full, ok := decodeHealSchema(schema)
	if !ok {
		return nil, nil, false // no schema, no evidence, no repair
	}

	var fixes []fix
	for _, name := range full.Required {
		if _, present := obj[name]; present {
			continue
		}
		prop, declared := full.Properties[name]
		if !declared || len(prop.Default) == 0 {
			// A required field with no default cannot be invented. This is
			// the fail direction: report, never guess.
			return nil, nil, false
		}
		var def any
		dec := json.NewDecoder(bytes.NewReader(prop.Default))
		dec.UseNumber()
		if dec.Decode(&def) != nil {
			return nil, nil, false
		}
		obj[name] = def
		fixes = append(fixes, fix{field: name, kind: fixDefault})
	}

	// Deterministic order: the fix list is an audit record and a golden-test
	// subject, so it must not depend on map iteration order.
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		prop, declared := full.Properties[name]
		if !declared {
			continue
		}
		val := obj[name]
		if typeMatches(prop.Type, val) {
			continue
		}
		conv, converted := coerce(prop.Type, val)
		if !converted {
			return nil, nil, false // a mismatch we cannot fix losslessly
		}
		obj[name] = conv
		fixes = append(fixes, fix{field: name, kind: fixCoerce})
	}

	if len(fixes) == 0 {
		return nil, nil, false
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, nil, false
	}
	return out, fixes, true
}

// healSchema is the schema projection self-healing needs: the shallow
// projection of gates.go plus per-property defaults.
type healSchema struct {
	Required   []string               `json:"required"`
	Properties map[string]healPropDef `json:"properties"`
}

type healPropDef struct {
	Type    any             `json:"type"`
	Default json.RawMessage `json:"default"`
}

func decodeHealSchema(raw json.RawMessage) (healSchema, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return healSchema{}, false
	}
	var s healSchema
	if json.Unmarshal(trimmed, &s) != nil {
		return healSchema{}, false
	}
	return s, true
}

// coerce converts v to the declared type IF AND ONLY IF the conversion is
// lossless, i.e. converting back reproduces the original literal exactly.
// The whole point is that a healed argument carries the caller's value, not
// an approximation of it.
//
// Supported (docs/modules/dataplane.md "lossless type conversions such as \"5\"→5"):
//
//	string "5"      → integer/number 5      (round-trips)
//	string "true"   → boolean true          (round-trips)
//	number 5 / bool → string "5" / "true"   (the literal, verbatim)
//
// Explicitly NOT supported: "5.5" → integer (loses the fraction), "007" → 7
// (does not round-trip), scalar → array (a shape guess, not a conversion),
// anything → object.
func coerce(declared any, v any) (any, bool) {
	for _, name := range typeNames(declared) {
		switch name {
		case "integer", "number":
			s, ok := v.(string)
			if !ok {
				continue
			}
			n := json.Number(s)
			if name == "integer" {
				if _, err := n.Int64(); err != nil {
					continue
				}
			} else if _, err := n.Float64(); err != nil {
				continue
			}
			if n.String() != s {
				continue // not a byte-for-byte round trip
			}
			return n, true
		case "boolean":
			s, ok := v.(string)
			if !ok {
				continue
			}
			if s == "true" {
				return true, true
			}
			if s == "false" {
				return false, true
			}
		case "string":
			switch n := v.(type) {
			case json.Number:
				return n.String(), true
			case bool:
				return strconv.FormatBool(n), true
			}
		}
	}
	return nil, false
}

// typeNames flattens a JSON Schema type declaration into the names we can
// coerce to, in declaration order (first convertible wins).
func typeNames(declared any) []string {
	switch d := declared.(type) {
	case string:
		return []string{d}
	case []any:
		out := make([]string, 0, len(d))
		for _, alt := range d {
			if s, ok := alt.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// selfHeal is the POST-CALL half of the repair: the downstream answered
// invalid_params even though our own precheck accepted the arguments.
//
// The two halves are not redundant. The precheck gate repairs what OUR
// shallow model can see, before the round trip. This half exists for the
// case the docs/modules/dataplane.md error shape describes: the downstream's rejection
// carries its OWN inputSchema, which may say more than the schema cached
// from tools/list (a schema that changed, or a per-call schema). Healing
// against the schema the SERVER just handed us is the only repair the
// precheck could not have made.
//
// It returns the outcome to deliver; when no repair applies the inputs come
// back untouched.
func (p *Pipeline) selfHeal(ctx context.Context, req *CallRequest, res *mcp.CallResult, callErr error) (*mcp.CallResult, error) {
	if req.CallWithArgs == nil || !isInvalidParams(res, callErr) {
		return res, callErr
	}
	schema := schemaFromRejection(res, callErr)
	if len(schema) == 0 {
		// No fresh schema: anything repairable against the CACHED schema was
		// already repaired by the precheck gate, so retrying would just
		// replay the identical call.
		return res, callErr
	}
	healed, fixes, ok := healArgs(req.Args, schema)
	if !ok {
		return res, callErr
	}
	ev := SelfHealEvent{
		Exposed: req.Exposed, ServerID: req.ServerID, RawTool: req.RawTool,
		Fixes: fixNames(fixes), Retried: true,
	}
	res2, err2 := req.CallWithArgs(ctx, healed)
	ev.Recovered = err2 == nil && (res2 == nil || !res2.IsError)
	p.reportSelfHeal(ctx, ev)
	if !ev.Recovered {
		// The repair did not help: deliver the ORIGINAL outcome. The agent's
		// remediation hint must describe the call it actually made, not our
		// speculative rewrite.
		return res, callErr
	}
	return res2, nil
}

// reportSelfHeal hands the event to the audit seam, if one is wired.
func (p *Pipeline) reportSelfHeal(ctx context.Context, ev SelfHealEvent) {
	if p.onSelfHeal == nil {
		return
	}
	p.onSelfHeal(ctx, ev)
}
