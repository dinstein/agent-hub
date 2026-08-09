package toolsig

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// DefaultMaxBytes is the signature length budget. Calibration: a signature
// has to stay cheaper than the schema it replaces even for a tool with a
// dozen parameters, and it has to survive being printed in a search result
// list without wrapping in a terminal. 200 bytes does both; beyond it the
// elision rule takes over.
const DefaultMaxBytes = 200

// Options configures rendering. The zero value uses DefaultMaxBytes.
type Options struct {
	// MaxBytes bounds Signature.Text. <= 0 means DefaultMaxBytes.
	MaxBytes int
}

func (o Options) maxBytes() int {
	if o.MaxBytes <= 0 {
		return DefaultMaxBytes
	}
	return o.MaxBytes
}

// Signature is one rendered tool signature plus the honesty metadata that
// travels with it.
type Signature struct {
	// Text is the one-line signature. Never empty for a named tool.
	Text string
	// Lossy reports that the rendering dropped information — a folded
	// nested object, a truncated enum or parameter list, an unparsable
	// schema. It is the "lossy" field of the search result in
	// docs/modules/dataplane.md, and it is the agent's cue that describe_tool
	// has more.
	Lossy bool
	// Params is how many parameters the schema declares; Shown is how many
	// survived the length budget.
	Params int
	Shown  int
}

// Of renders the signature of def under its own name.
func Of(def mcp.ToolDef, opts Options) Signature { return Named(def.Name, def, opts) }

// Named renders the signature of def under an explicit name.
//
// The name is a parameter rather than def.Name because the name an agent
// CALLS is the namespaced exposed name, which lives on discovery.Tool; a
// definition that reached this package through some other path (a raw
// downstream listing, a test fixture) must not silently produce a signature
// with the wrong key in it.
//
// Determinism: the result is a pure function of (name, def.InputSchema,
// def.OutputSchema, opts.MaxBytes). Nothing else is read — not the
// description, not annotations — so a server that rewords its docs does not
// churn the search output.
func Named(name string, def mcp.ToolDef, opts Options) Signature {
	max := opts.maxBytes()
	var sig Signature

	ret := returnType(def)

	root, ok := parseNode(def.InputSchema)
	if !ok {
		// Unparsable or non-object schema: one frozen shape, no guessing.
		sig.Text = name + "(~) -> " + ret
		sig.Lossy = true
		return sig
	}

	params := paramsOf(root)
	sig.Params = len(params)
	rendered := make([]string, 0, len(params))
	for _, p := range params {
		text, lossy := p.render()
		rendered = append(rendered, text)
		sig.Lossy = sig.Lossy || lossy
	}

	text, shown := assemble(name, rendered, ret, max)
	sig.Text = text
	sig.Shown = shown
	if shown < len(rendered) {
		sig.Lossy = true
	}
	return sig
}

// returnType renders the declared outputSchema, or DefaultReturn when there
// is none. An outputSchema is rendered at depth 1 on purpose: the return
// value is not a parameter, so expanding its keys would spend the line
// budget on the half of the signature the agent does not have to construct.
func returnType(def mcp.ToolDef) string {
	if len(def.OutputSchema) == 0 {
		return DefaultReturn
	}
	t, _ := renderType(def.OutputSchema, 1)
	return t
}

// param is one rendered-ready parameter.
type param struct {
	name     string
	schema   []byte
	required bool
}

// render produces "name?~:type=default".
func (p param) render() (string, bool) {
	var b strings.Builder
	b.WriteString(p.name)

	t, lossy := TypeAny, true
	var def string
	hasDefault := false
	if n, ok := parseNode(p.schema); ok {
		t, lossy = renderNode(n, 0)
		if d, ok := renderDefault(n.Default); ok {
			def, hasDefault = d, true
		} else if len(n.Default) > 0 {
			lossy = true // a default too large to inline is still a fact
		}
	}

	if !p.required {
		b.WriteByte('?')
	}
	if lossy {
		b.WriteByte('~')
	}
	b.WriteByte(':')
	b.WriteString(t)
	if hasDefault {
		b.WriteByte('=')
		b.WriteString(def)
	}
	return b.String(), lossy
}

// paramsOf orders a schema's properties: required first in the order the
// schema's "required" array gives, then optional sorted byte-ascending.
//
// A name listed in "required" but absent from "properties" is still emitted,
// with type any — the caller must pass it, and hiding a mandatory argument
// because the server's schema is sloppy would produce calls that always fail.
func paramsOf(root *node) []param {
	seen := make(map[string]bool, len(root.Properties))
	out := make([]param, 0, len(root.Properties))
	for _, name := range root.Required {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, param{name: name, schema: root.Properties[name], required: true})
	}
	optional := make([]string, 0, len(root.Properties))
	for name := range root.Properties {
		if !seen[name] {
			optional = append(optional, name)
		}
	}
	slices.Sort(optional)
	for _, name := range optional {
		out = append(out, param{name: name, schema: root.Properties[name]})
	}
	return out
}

// assemble joins the rendered parameters into the final line, dropping from
// the TAIL until the budget is met and closing the cut with "…+N more".
//
// Dropping from the tail is what makes the rule "required first": optional
// parameters sort last, so they are always the first to go. When even the
// required ones do not fit they are dropped too, and the count says so —
// a signature that silently omitted a mandatory argument would be worse than
// one that admits it is incomplete.
//
// Post-condition: len(result) <= max whenever the skeleton fits in max. The
// tool name is never truncated.
func assemble(name string, params []string, ret string, max int) (string, int) {
	full := name + "(" + strings.Join(params, ", ") + ") -> " + ret
	if len(full) <= max || len(params) == 0 {
		return full, len(params)
	}
	for shown := len(params) - 1; shown >= 0; shown-- {
		head := params[:shown]
		tail := fmt.Sprintf(elisionFormat, len(params)-shown)
		joined := strings.Join(head, ", ")
		if shown > 0 {
			joined += ", "
		}
		out := name + "(" + joined + tail + ") -> " + ret
		if len(out) <= max {
			return out, shown
		}
	}
	// Even the skeleton is over budget. Emit it anyway: the name and the
	// parameter COUNT are the two things a caller cannot do without.
	return name + "(" + fmt.Sprintf(elisionFormat, len(params)) + ") -> " + ret, 0
}
