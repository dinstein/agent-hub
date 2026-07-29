package clients

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
)

// JSONC: the settings files that are JSON with comments in them.
//
// Zed ships settings.json with a comment header, and VS Code's is JSONC by
// convention, so "a client agenthub cannot touch" was in practice "a client
// the user has not deleted the vendor's own comments out of yet".
//
// Two separate powers, and this file is careful about the difference:
//
//   - READING is a comment-blanking pass followed by encoding/json. Comments
//     and trailing commas are replaced by spaces of the SAME LENGTH, so every
//     byte offset in the blanked copy is also a byte offset in the original.
//
//   - WRITING never re-encodes the document. The offsets above locate our own
//     entry (or where it would go), and only those bytes are replaced. Comments,
//     key order, indentation and the user's own formatting survive because
//     nothing else is touched — that is what the "only rewrite documents that
//     round-trip losslessly" rule is protecting, and a splice loses nothing.
//
// The splice is then VERIFIED before anything reaches the disk: the result
// must parse, must differ from the original in exactly the entries agenthub
// meant to change, and must carry byte-identical comments. A locator bug
// therefore surfaces as a refusal, not as a mangled settings file.

// errJSONCUnsupported is the internal signal that a document is shaped in a
// way the splice does not model. Callers turn it into the ordinary refusal
// plus manual snippet — never into a rewrite.
var errJSONCUnsupported = errors.New("clients: this document's shape is not spliceable")

// blankJSONC returns a copy with comments and trailing commas replaced by
// spaces, preserving length, offsets and newlines.
func blankJSONC(data []byte) []byte {
	out := bytes.Clone(data)
	inString, escaped := false, false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
		case c == '/' && i+1 < len(out) && out[i+1] == '/':
			for ; i < len(out) && out[i] != '\n'; i++ {
				out[i] = ' '
			}
		case c == '/' && i+1 < len(out) && out[i+1] == '*':
			out[i], out[i+1] = ' ', ' '
			i += 2
			for ; i < len(out); i++ {
				if out[i] == '*' && i+1 < len(out) && out[i+1] == '/' {
					out[i], out[i+1] = ' ', ' '
					i++
					break
				}
				if out[i] != '\n' {
					out[i] = ' '
				}
			}
		}
	}
	blankTrailingCommas(out)
	return out
}

// blankTrailingCommas erases a comma that is followed by a closing bracket.
// JSONC allows it and encoding/json does not, and the point of the blanking
// pass is that what comes out of it is plain JSON.
func blankTrailingCommas(b []byte) {
	inString, escaped := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			continue
		}
		if c != ',' {
			continue
		}
		// Look past whitespace AND further commas: blanking one trailing
		// comma must not expose another, or the pass stops being
		// idempotent — and the writer runs it on bytes it has already
		// blanked once. (`,,]` is not valid JSON either way; what matters
		// is that the answer does not depend on how many times we look.)
		j := i + 1
		for j < len(b) && (isJSONSpace(b[j]) || b[j] == ',') {
			j++
		}
		if j < len(b) && (b[j] == '}' || b[j] == ']') {
			b[i] = ' '
		}
	}
}

func skipJSONSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// jsonMember is one key/value pair of an object, located in the source.
type jsonMember struct {
	name string
	// start is the opening quote of the key; end is one past the value.
	start, end int
}

// jsonObject is an object located in the source: the braces, and its
// members in document order.
type jsonObject struct {
	open, close int
	members     []jsonMember
}

func (o *jsonObject) member(name string) (jsonMember, bool) {
	for _, m := range o.members {
		if m.name == name {
			return m, true
		}
	}
	return jsonMember{}, false
}

// locateObject walks the blanked document to the object at path and returns
// its span. ok is false when the path does not exist or does not lead to an
// object — the caller must then create it rather than guess.
func locateObject(b []byte, path []string) (*jsonObject, bool) {
	i := skipJSONSpace(b, 0)
	obj, _, ok := parseJSONObject(b, i)
	if !ok {
		return nil, false
	}
	for _, key := range path {
		m, found := obj.member(key)
		if !found {
			return nil, false
		}
		vs := skipJSONSpace(b, valueStart(b, m))
		if vs >= len(b) || b[vs] != '{' {
			return nil, false
		}
		obj, _, ok = parseJSONObject(b, vs)
		if !ok {
			return nil, false
		}
	}
	return obj, true
}

// valueStart is the first byte of a member's value: past the key string and
// its colon.
func valueStart(b []byte, m jsonMember) int {
	i := m.start
	if i < len(b) && b[i] == '"' {
		if end, ok := scanJSONString(b, i); ok {
			i = end
		}
	}
	i = skipJSONSpace(b, i)
	if i < len(b) && b[i] == ':' {
		i++
	}
	return skipJSONSpace(b, i)
}

// parseJSONObject reads the object starting at b[i] == '{'.
func parseJSONObject(b []byte, i int) (*jsonObject, int, bool) {
	if i >= len(b) || b[i] != '{' {
		return nil, i, false
	}
	obj := &jsonObject{open: i}
	i = skipJSONSpace(b, i+1)
	for {
		if i >= len(b) {
			return nil, i, false
		}
		if b[i] == '}' {
			obj.close = i
			return obj, i + 1, true
		}
		if b[i] != '"' {
			return nil, i, false
		}
		start := i
		keyEnd, ok := scanJSONString(b, i)
		if !ok {
			return nil, i, false
		}
		var name string
		if json.Unmarshal(b[start:keyEnd], &name) != nil {
			return nil, i, false
		}
		i = skipJSONSpace(b, keyEnd)
		if i >= len(b) || b[i] != ':' {
			return nil, i, false
		}
		i = skipJSONSpace(b, i+1)
		end, ok := scanJSONValue(b, i)
		if !ok {
			return nil, i, false
		}
		obj.members = append(obj.members, jsonMember{name: name, start: start, end: end})
		i = skipJSONSpace(b, end)
		if i < len(b) && b[i] == ',' {
			i = skipJSONSpace(b, i+1)
		}
	}
}

// scanJSONValue returns one past the value starting at b[i].
func scanJSONValue(b []byte, i int) (int, bool) {
	if i >= len(b) {
		return i, false
	}
	switch b[i] {
	case '"':
		return scanJSONString(b, i)
	case '{':
		_, end, ok := parseJSONObject(b, i)
		return end, ok
	case '[':
		i++
		for {
			i = skipJSONSpace(b, i)
			if i >= len(b) {
				return i, false
			}
			if b[i] == ']' {
				return i + 1, true
			}
			end, ok := scanJSONValue(b, i)
			if !ok {
				return i, false
			}
			i = skipJSONSpace(b, end)
			if i < len(b) && b[i] == ',' {
				i++
			}
		}
	default:
		start := i
		for i < len(b) && !isJSONDelim(b[i]) {
			i++
		}
		if i == start {
			return i, false
		}
		return i, true
	}
}

func isJSONDelim(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', ',', '}', ']', ':':
		return true
	}
	return false
}

// scanJSONString returns one past the closing quote of the string at b[i].
func scanJSONString(b []byte, i int) (int, bool) {
	if i >= len(b) || b[i] != '"' {
		return i, false
	}
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1, true
		}
	}
	return i, false
}

// comments extracts every comment in the document, so a write can prove it
// kept them. It works by diffing against the blanked copy, which is exactly
// what "the comments" means here.
func comments(data []byte) []string {
	blanked := blankJSONC(data)
	var out []string
	for i := 0; i < len(data); {
		if data[i] == blanked[i] {
			i++
			continue
		}
		start := i
		for i < len(data) && data[i] != blanked[i] {
			i++
		}
		out = append(out, string(data[start:i]))
	}
	return out
}

// spliceMemberValue replaces one member's value in place.
func spliceMemberValue(src []byte, m jsonMember, value string) []byte {
	vs := valueStart(src, m)
	out := make([]byte, 0, len(src)+len(value))
	out = append(out, src[:vs]...)
	out = append(out, value...)
	return append(out, src[m.end:]...)
}

// spliceInsertMember adds a member as the object's first entry, indented
// like the members already there.
func spliceInsertMember(src []byte, obj *jsonObject, key, value string) []byte {
	indent := memberIndent(src, obj)
	text := "\n" + indent + `"` + key + `": ` + value
	if len(obj.members) > 0 {
		text += ","
	}
	at := obj.open + 1
	out := make([]byte, 0, len(src)+len(text))
	out = append(out, src[:at]...)
	out = append(out, text...)
	return append(out, src[at:]...)
}

// spliceRemoveMembers deletes members and the comma that joined them,
// leaving the surrounding lines untouched.
func spliceRemoveMembers(src []byte, obj *jsonObject, names []string) []byte {
	out := src
	// Right to left: earlier spans keep their offsets that way.
	for i := len(obj.members) - 1; i >= 0; i-- {
		m := obj.members[i]
		if !slices.Contains(names, m.name) {
			continue
		}
		start, end := m.start, m.end
		// Take the comma after it, or the one before it when it was last.
		if j := skipJSONSpace(out, end); j < len(out) && out[j] == ',' {
			end = j + 1
		} else {
			for k := start - 1; k >= 0; k-- {
				if out[k] == ',' {
					start = k
					break
				}
				if !isJSONSpace(out[k]) {
					break
				}
			}
		}
		// And the blank line it leaves behind.
		start = trimLineStart(out, start)
		end = trimLineEnd(out, end)
		out = append(append([]byte{}, out[:start]...), out[end:]...)
	}
	return out
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// trimLineStart walks back over the indentation preceding a member.
func trimLineStart(b []byte, i int) int {
	for i > 0 && (b[i-1] == ' ' || b[i-1] == '\t') {
		i--
	}
	if i > 0 && b[i-1] == '\n' {
		return i - 1
	}
	return i
}

// trimLineEnd walks forward over trailing spaces up to the newline.
func trimLineEnd(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t') {
		i++
	}
	return i
}

// memberIndent is the indentation of the object's first member, or the
// object's own line plus two spaces when it has none.
func memberIndent(src []byte, obj *jsonObject) string {
	anchor := obj.open
	if len(obj.members) > 0 {
		anchor = obj.members[0].start
	}
	lineStart := bytes.LastIndexByte(src[:anchor], '\n') + 1
	indent := ""
	for i := lineStart; i < anchor; i++ {
		if src[i] != ' ' && src[i] != '\t' {
			break
		}
		indent += string(src[i])
	}
	if len(obj.members) == 0 {
		indent += "  "
	}
	return indent
}

// indentJSON renders a value at the given indentation, for splicing into a
// document that is not ours to reformat.
func indentJSON(v any, indent string) (string, error) {
	b, err := json.MarshalIndent(v, indent, "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// verifySplice is what makes writing into a document agenthub cannot fully
// model safe: the result is proved, not trusted.
//
// Three properties, and a failure in any one leaves the file untouched:
//
//   - the result parses;
//   - it differs from the original in exactly the members named in changed —
//     so a locator that spliced into the wrong place is caught here, no
//     matter how plausible the bytes look;
//   - its comments are byte-identical, which is the thing the whole "do not
//     rewrite what you cannot round-trip" rule was protecting.
func verifySplice(before, after []byte, section []string, changed []string) error {
	var b, a any
	if err := json.Unmarshal(blankJSONC(before), &b); err != nil {
		return fmt.Errorf("clients: the document was not parseable to begin with: %w", err)
	}
	if err := json.Unmarshal(blankJSONC(after), &a); err != nil {
		return fmt.Errorf("clients: the edit did not produce a parseable document: %w", err)
	}
	if err := dropChanged(b, section, changed); err != nil {
		return err
	}
	if err := dropChanged(a, section, changed); err != nil {
		return err
	}
	if !reflect.DeepEqual(a, b) {
		return errors.New("clients: the edit changed something other than agenthub's own entry")
	}
	if bc, ac := comments(before), comments(after); !reflect.DeepEqual(bc, ac) {
		return errors.New("clients: the edit did not preserve the file's comments")
	}
	return nil
}

// dropChanged removes the entries an edit was allowed to touch, so that what
// is left is everything the edit had to leave alone.
//
// A section the edit CREATED is removed whole — but only after checking it
// holds nothing but those entries. A section that appeared with anything
// else inside it is a change nobody asked for, and saying so here is what
// stops "we created the object" from becoming a place to hide one.
// The walk keeps the LEAF SECTION'S PARENT rather than only the leaf,
// because dropping a created section whole means deleting a key out of the
// object above it.
func dropChanged(doc any, section, changed []string) error {
	parent, ok := doc.(map[string]any)
	if !ok {
		return errors.New("clients: document root is not an object")
	}
	for i, key := range section {
		next, found := parent[key]
		if !found {
			return nil // absent on this side; the other side is checked too
		}
		child, isObject := next.(map[string]any)
		if !isObject {
			return fmt.Errorf("clients: %q is not an object", key)
		}
		if i == len(section)-1 {
			for _, name := range changed {
				delete(child, name)
			}
			// Nothing but those entries was in it, so the section itself is
			// something this edit created and has to take away again.
			if len(child) == 0 {
				delete(parent, key)
			}
			return nil
		}
		parent = child
	}
	// No section at all: the entries sit at the document root.
	for _, name := range changed {
		delete(parent, name)
	}
	return nil
}

// spliceEntry returns the document with name's entry set to value, editing
// only the bytes that entry occupies (or, when it is not there yet, only
// the bytes needed to insert it).
//
// It refuses — errJSONCUnsupported — rather than guess: a section key that
// exists but is not an object, a document whose root is not an object, or
// anything else the locator cannot walk. The caller then reports the same
// refusal it always did, with the snippet to paste.
func spliceEntry(src []byte, section []string, name string, value any) ([]byte, error) {
	blanked := blankJSONC(src)
	if obj, ok := locateObject(blanked, section); ok {
		indent := memberIndent(src, obj)
		text, err := indentJSON(value, indent)
		if err != nil {
			return nil, err
		}
		if m, found := obj.member(name); found {
			return spliceMemberValue(src, m, text), nil
		}
		return spliceInsertMember(src, obj, name, text), nil
	}

	// The section is not there. Build the missing part as one literal and
	// insert it into the deepest object that does exist.
	depth := len(section)
	var host *jsonObject
	for depth > 0 {
		if obj, ok := locateObject(blanked, section[:depth-1]); ok {
			// A key that exists but is not an object is not ours to replace.
			if _, taken := obj.member(section[depth-1]); taken {
				return nil, errJSONCUnsupported
			}
			host = obj
			break
		}
		depth--
	}
	if host == nil {
		return nil, errJSONCUnsupported
	}
	// nested is the VALUE for section[depth-1]: the servers map, wrapped in
	// whichever section levels below it are also missing.
	var nested any = map[string]any{name: value}
	for i := len(section) - 1; i >= depth; i-- {
		nested = map[string]any{section[i]: nested}
	}
	text, err := indentJSON(nested, memberIndent(src, host))
	if err != nil {
		return nil, err
	}
	return spliceInsertMember(src, host, section[depth-1], text), nil
}

// spliceRemove returns the document with the named entries deleted, editing
// only their bytes.
func spliceRemove(src []byte, section []string, names []string) ([]byte, error) {
	blanked := blankJSONC(src)
	obj, ok := locateObject(blanked, section)
	if !ok {
		return nil, errJSONCUnsupported
	}
	return spliceRemoveMembers(src, obj, names), nil
}
