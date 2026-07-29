package toolsig

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// Type abbreviations. They are ABI: they appear in every search result an
// agent reads, so they are declared once here and nowhere else.
const (
	TypeString  = "str"
	TypeInteger = "int"
	TypeNumber  = "num"
	TypeBoolean = "bool"
	TypeNull    = "null"
	TypeObject  = "obj"
	TypeArray   = "arr"
	TypeAny     = "any"
)

// DefaultReturn is the return type of a tool that declares no outputSchema —
// the overwhelming majority. MCP delivers results as content blocks, and the
// text block is the one an agent reads, so "str" is the honest default.
const DefaultReturn = TypeString

// Rendering caps. Each one has a fallback that sets the lossy marker, so a
// cap can shorten a signature but never silently change its meaning.
const (
	// MaxObjectKeys is how many direct keys a top-level object parameter
	// expands to before it folds to a bare obj.
	MaxObjectKeys = 4
	// MaxEnumValues is how many enum members are listed before the tail is
	// elided.
	MaxEnumValues = 6
	// MaxDefaultBytes bounds a rendered default. A longer one is dropped:
	// a 300-byte default value is a schema to fetch, not a hint to inline.
	MaxDefaultBytes = 24
	// maxTypeDepth bounds recursion through items/properties. Hostile input
	// must not be able to steer unbounded work.
	maxTypeDepth = 4
)

// elision closes a truncated parameter list. Frozen text.
const elisionFormat = "…+%d more"

// node is the subset of JSON Schema this package reads. Everything outside
// it (allOf, patternProperties, format, descriptions) is deliberately
// ignored: a signature is a shape reminder, not a validator, and the
// validator lives in internal/pipeline where it belongs.
type node struct {
	Type       json.RawMessage            `json:"type"`
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
	Items      json.RawMessage            `json:"items"`
	Enum       []json.RawMessage          `json:"enum"`
	Default    json.RawMessage            `json:"default"`
	Ref        string                     `json:"$ref"`
	AnyOf      []json.RawMessage          `json:"anyOf"`
	OneOf      []json.RawMessage          `json:"oneOf"`
}

// parseNode decodes one schema node. A node that is not a JSON object (the
// boolean schemas `true`/`false`, or garbage) reports false.
func parseNode(raw json.RawMessage) (*node, bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '{' {
		return nil, false
	}
	var n node
	if err := json.Unmarshal(t, &n); err != nil {
		return nil, false
	}
	return &n, true
}

// typeNames returns the declared type(s) with "null" filtered out. JSON
// Schema allows both a string and an array of strings; nullability is not
// part of this grammar, so a ["string","null"] union renders as str.
func (n *node) typeNames() []string {
	if len(n.Type) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(n.Type, &one); err == nil {
		if one == "null" {
			return []string{"null"}
		}
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(n.Type, &many); err != nil {
		return nil
	}
	out := make([]string, 0, len(many))
	for _, t := range many {
		if t != "null" {
			out = append(out, t)
		}
	}
	if len(out) == 0 && len(many) > 0 {
		return []string{"null"}
	}
	return out
}

// renderType renders a schema node's type at the given depth and reports
// whether anything was folded away.
//
// Failure direction: every unreadable or unsupported construct becomes
// TypeAny with lossy=true. A signature that claims less than it knows is
// recoverable through describe_tool; one that claims more is not.
func renderType(raw json.RawMessage, depth int) (string, bool) {
	n, ok := parseNode(raw)
	if !ok {
		return TypeAny, true
	}
	return renderNode(n, depth)
}

func renderNode(n *node, depth int) (string, bool) {
	if n.Ref != "" {
		// Refs are inlined by internal/router before a definition gets
		// here; one that survived is a schema this package will not chase.
		return TypeAny, true
	}
	if len(n.Enum) > 0 {
		return renderEnum(n.Enum)
	}
	names := n.typeNames()
	if len(names) == 0 {
		// Infer from structure — a schema with "properties" and no "type"
		// is an object everywhere in practice.
		switch {
		case len(n.Properties) > 0:
			names = []string{"object"}
		case len(n.Items) > 0:
			names = []string{"array"}
		case len(n.AnyOf) > 0 || len(n.OneOf) > 0:
			return TypeAny, true
		default:
			return TypeAny, false // a genuinely unconstrained value
		}
	}
	if len(names) > 1 {
		return TypeAny, true // a real union; the signature will not pick one
	}

	switch names[0] {
	case "string":
		return TypeString, false
	case "integer":
		return TypeInteger, false
	case "number":
		return TypeNumber, false
	case "boolean":
		return TypeBoolean, false
	case "null":
		return TypeNull, false
	case "array":
		return renderArray(n, depth)
	case "object":
		return renderObject(n, depth)
	default:
		return TypeAny, true // an unknown type keyword; say so
	}
}

// renderArray renders arr / arr<elem>. A tuple schema (items as an array)
// folds to plain arr with the lossy marker: naming only the first member's
// type would be a lie about the rest.
func renderArray(n *node, depth int) (string, bool) {
	if len(n.Items) == 0 || depth >= maxTypeDepth {
		return TypeArray, len(n.Items) > 0
	}
	if t := bytes.TrimSpace(n.Items); len(t) > 0 && t[0] == '[' {
		return TypeArray, true
	}
	elem, lossy := renderType(n.Items, depth+1)
	return TypeArray + "<" + elem + ">", lossy
}

// renderObject expands ONE level (obj{a,b}) and folds everything deeper.
// The expansion is what makes a nested-config parameter readable at all; the
// fold is what keeps a signature one line.
func renderObject(n *node, depth int) (string, bool) {
	if len(n.Properties) == 0 {
		return TypeObject, false // a free-form object loses nothing
	}
	if depth > 0 {
		return TypeObject, true // deeper than one level: fold
	}
	keys := slices.Sorted(maps.Keys(n.Properties))
	lossy := true // the sub-property TYPES are always dropped
	if len(keys) > MaxObjectKeys {
		keys = keys[:MaxObjectKeys]
	}
	return TypeObject + "{" + strings.Join(keys, ",") + "}", lossy
}

// renderEnum renders enum{a|b}. Members that are plain identifiers are
// written bare; anything else keeps its JSON form so the "|" separator can
// never be confused with a member's own bytes.
func renderEnum(values []json.RawMessage) (string, bool) {
	lossy := false
	shown := values
	if len(shown) > MaxEnumValues {
		shown = shown[:MaxEnumValues]
		lossy = true
	}
	parts := make([]string, 0, len(shown))
	for _, v := range shown {
		parts = append(parts, renderEnumValue(v))
	}
	return "enum{" + strings.Join(parts, "|") + "}", lossy
}

func renderEnumValue(raw json.RawMessage) string {
	compacted := compact(raw)
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && isBareToken(s) {
		return s
	}
	return compacted
}

// isBareToken reports whether a string can be written without quotes inside
// enum{…}: no separator, no whitespace, no emptiness.
func isBareToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == ':', r == '/':
		default:
			return false
		}
	}
	return true
}

// renderDefault renders a "default" keyword, or reports false when it is too
// large to inline (the caller then marks the parameter lossy).
func renderDefault(raw json.RawMessage) (string, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", false
	}
	c := compact(raw)
	if c == "" || len(c) > MaxDefaultBytes {
		return "", false
	}
	return c, true
}

// compact normalises a JSON value to its whitespace-free encoding so the
// signature is a byte-deterministic function of the schema's VALUES, not of
// the server's formatting.
func compact(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return ""
	}
	return buf.String()
}
