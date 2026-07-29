package clients

import (
	"strings"
)

// TOML DETECTION, NOT TOML PARSING — and the difference is the point.
//
// agenthub does not rewrite TOML: a re-encoder drops comments, key order
// and formatting, which is a config-destroying machine wearing a helpful
// hat (see probe.go). That rule is about WRITING, and it left reading in a
// bad place: `client ls` could only say "?" for codex, and doctor went
// further and reported "no agenthub gateway entry" for a file it had never
// opened — an absence claimed from no evidence.
//
// So this reads, and only reads. It recognises the one construct that
// matters, `[mcp_servers.NAME]` tables and the handful of keys inside them,
// and refuses the whole file the moment it meets anything it does not
// model. That refusal is the contract: scanTOMLServers returns ok=false,
// the caller reports "unknown", and nobody ever converts a construct this
// scanner cannot read into "the entry is not there".
//
// It reads untrusted bytes by hand, so it is fuzzed (FuzzScanTOMLServers).

// tomlEntry is one server table, reduced to the fields agenthub uses.
type tomlEntry struct {
	command  string
	args     []string
	url      string
	disabled bool
}

// scanTOMLServers extracts the tables under the named top-level table.
//
// ok is false for any document this scanner does not fully understand:
// unterminated strings or arrays, an array-of-tables or inline table where
// the servers live, a key it cannot read. A partial answer is never
// returned — half a server map would be indistinguishable from a complete
// one, and the caller would report "not connected" on the strength of a
// table this code simply failed to reach.
func scanTOMLServers(data []byte, table string) (map[string]tomlEntry, bool) {
	s := &tomlScanner{src: data}
	out := map[string]tomlEntry{}
	// current is the server name whose table we are inside, "" when the
	// current table is not one of ours.
	current := ""
	for {
		s.skipIgnorable()
		if s.eof() {
			return out, true
		}
		switch s.peek() {
		case '[':
			if s.hasPrefix("[[") {
				// An array-of-tables. If it is anywhere near our table
				// the shape is one we do not model at all.
				keys, ok := s.tableHeader()
				if !ok || (len(keys) > 0 && keys[0] == table) {
					return nil, false
				}
				current = ""
				continue
			}
			keys, ok := s.tableHeader()
			if !ok {
				return nil, false
			}
			current = ""
			// [mcp_servers.NAME] is a server; [mcp_servers.NAME.env] and
			// deeper are that server's sub-tables, whose keys are not ours
			// to read but which must not be attributed to the server
			// either.
			if len(keys) == 2 && keys[0] == table {
				current = keys[1]
				if _, seen := out[current]; !seen {
					out[current] = tomlEntry{}
				}
			}
		default:
			key, val, ok := s.keyValue()
			if !ok {
				return nil, false
			}
			// A root-level `mcp_servers = {...}`: the servers exist, in a
			// form this scanner does not read. Refuse the file rather than
			// report an empty map.
			if len(key) > 0 && key[0] == table && current == "" {
				return nil, false
			}
			if current == "" || len(key) != 1 {
				continue
			}
			e := out[current]
			switch key[0] {
			case "command":
				if val.kind != tomlString {
					return nil, false
				}
				e.command = val.str
			case "url":
				if val.kind != tomlString {
					return nil, false
				}
				e.url = val.str
			case "args":
				if val.kind != tomlArray {
					return nil, false
				}
				e.args = val.list
			case "enabled":
				if val.kind != tomlBool {
					return nil, false
				}
				e.disabled = !val.truth
			case "disabled":
				if val.kind != tomlBool {
					return nil, false
				}
				e.disabled = val.truth
			}
			out[current] = e
		}
	}
}

type tomlKind int

const (
	tomlOther tomlKind = iota // a value we skipped over intact
	tomlString
	tomlArray
	tomlBool
)

// tomlValue is as much of a value as agenthub needs. list is filled only
// for an array whose elements are all strings; a mixed array reads as
// tomlOther, which the caller rejects where it wanted args.
type tomlValue struct {
	kind  tomlKind
	str   string
	list  []string
	truth bool
}

type tomlScanner struct {
	src []byte
	i   int
}

func (s *tomlScanner) eof() bool  { return s.i >= len(s.src) }
func (s *tomlScanner) peek() byte { return s.src[s.i] }

func (s *tomlScanner) hasPrefix(p string) bool {
	return strings.HasPrefix(string(s.src[s.i:]), p)
}

// skipIgnorable consumes whitespace, newlines and comments. A comment runs
// to the end of its line; it cannot contain a string, so nothing inside it
// is ever interpreted.
func (s *tomlScanner) skipIgnorable() {
	for !s.eof() {
		switch s.peek() {
		case ' ', '\t', '\r', '\n', ',':
			s.i++
		case '#':
			for !s.eof() && s.peek() != '\n' {
				s.i++
			}
		default:
			return
		}
	}
}

// skipInlineSpace consumes spaces and tabs only, stopping at a newline.
func (s *tomlScanner) skipInlineSpace() {
	for !s.eof() && (s.peek() == ' ' || s.peek() == '\t') {
		s.i++
	}
}

// tableHeader reads `[a.b.c]` or `[[a.b]]` and returns the dotted key.
func (s *tomlScanner) tableHeader() ([]string, bool) {
	double := s.hasPrefix("[[")
	s.i++
	if double {
		s.i++
	}
	keys, ok := s.dottedKey(']')
	if !ok {
		return nil, false
	}
	s.skipInlineSpace()
	if s.eof() || s.peek() != ']' {
		return nil, false
	}
	s.i++
	if double {
		if s.eof() || s.peek() != ']' {
			return nil, false
		}
		s.i++
	}
	return keys, true
}

// keyValue reads `key = value` (dotted keys included).
func (s *tomlScanner) keyValue() ([]string, tomlValue, bool) {
	keys, ok := s.dottedKey('=')
	if !ok || len(keys) == 0 {
		return nil, tomlValue{}, false
	}
	s.skipInlineSpace()
	if s.eof() || s.peek() != '=' {
		return nil, tomlValue{}, false
	}
	s.i++
	s.skipInlineSpace()
	val, ok := s.value()
	if !ok {
		return nil, tomlValue{}, false
	}
	return keys, val, true
}

// dottedKey reads key parts separated by dots, stopping before stop. Parts
// are bare (letters, digits, _ and -) or quoted.
func (s *tomlScanner) dottedKey(stop byte) ([]string, bool) {
	var keys []string
	for {
		s.skipInlineSpace()
		if s.eof() {
			return nil, false
		}
		var part string
		switch c := s.peek(); {
		case c == '"' || c == '\'':
			v, ok := s.stringValue()
			if !ok {
				return nil, false
			}
			part = v
		case isBareKeyByte(c):
			start := s.i
			for !s.eof() && isBareKeyByte(s.peek()) {
				s.i++
			}
			part = string(s.src[start:s.i])
		default:
			return nil, false
		}
		keys = append(keys, part)
		s.skipInlineSpace()
		if s.eof() {
			return nil, false
		}
		if s.peek() == '.' {
			s.i++
			continue
		}
		if s.peek() == stop {
			return keys, true
		}
		return nil, false
	}
}

func isBareKeyByte(c byte) bool {
	return c == '_' || c == '-' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// value reads one value. Anything agenthub does not interpret is still
// consumed exactly, so scanning continues in the right place — a value
// skipped by guesswork is how a scanner starts reading a string's contents
// as configuration.
func (s *tomlScanner) value() (tomlValue, bool) {
	if s.eof() {
		return tomlValue{}, false
	}
	switch c := s.peek(); {
	case c == '"' || c == '\'':
		str, ok := s.stringValue()
		return tomlValue{kind: tomlString, str: str}, ok
	case c == '[':
		return s.arrayValue()
	case c == '{':
		return s.inlineTable()
	case strings.HasPrefix(string(s.src[s.i:]), "true"):
		s.i += 4
		return tomlValue{kind: tomlBool, truth: true}, true
	case strings.HasPrefix(string(s.src[s.i:]), "false"):
		s.i += 5
		return tomlValue{kind: tomlBool}, true
	}
	// A scalar of a type agenthub has no use for (number, date, ...).
	// It ends at the first delimiter; TOML forbids all of these inside one.
	start := s.i
	for !s.eof() {
		c := s.peek()
		if c == '\n' || c == ',' || c == ']' || c == '}' || c == '#' {
			break
		}
		s.i++
	}
	if s.i == start {
		return tomlValue{}, false
	}
	return tomlValue{kind: tomlOther}, true
}

// stringValue reads all four TOML string forms. The multi-line ones are
// why this scanner is not line-based: a `"""` block may contain lines that
// look exactly like table headers, and a line-based scan would read them as
// configuration.
func (s *tomlScanner) stringValue() (string, bool) {
	quote := s.peek()
	triple := s.hasPrefix(strings.Repeat(string(quote), 3))
	if triple {
		s.i += 3
		// A newline immediately after the opening delimiter is not content.
		if !s.eof() && s.peek() == '\r' {
			s.i++
		}
		if !s.eof() && s.peek() == '\n' {
			s.i++
		}
		closing := strings.Repeat(string(quote), 3)
		var b strings.Builder
		for !s.eof() {
			if s.hasPrefix(closing) {
				s.i += 3
				// Up to two extra quotes may precede the delimiter.
				for !s.eof() && s.peek() == quote {
					b.WriteByte(quote)
					s.i++
				}
				return b.String(), true
			}
			if quote == '"' && s.peek() == '\\' {
				s.i++
				// A backslash at end of line escapes the newline and the
				// whitespace after it.
				if j := s.i; j < len(s.src) && isTOMLLineWrap(s.src[j:]) {
					for !s.eof() && (s.peek() == ' ' || s.peek() == '\t' ||
						s.peek() == '\r' || s.peek() == '\n') {
						s.i++
					}
					continue
				}
				e, ok := s.escape()
				if !ok {
					return "", false
				}
				b.WriteByte(e)
				continue
			}
			b.WriteByte(s.peek())
			s.i++
		}
		return "", false
	}
	s.i++
	var b strings.Builder
	for !s.eof() {
		c := s.peek()
		switch {
		case c == quote:
			s.i++
			return b.String(), true
		case c == '\n':
			// A single-quoted string may not span lines.
			return "", false
		case quote == '"' && c == '\\':
			s.i++
			e, ok := s.escape()
			if !ok {
				return "", false
			}
			b.WriteByte(e)
		default:
			b.WriteByte(c)
			s.i++
		}
	}
	return "", false
}

// escape resolves one backslash escape, and REFUSES anything outside
// TOML's set — including the \uXXXX forms, which this scanner does not
// decode.
//
// Failure direction: refusing is the whole point. Passing an unknown escape
// through as its own character invents content that is not in the file
// (`"\0"` became `0`, found by the fuzzer on its first run), and invented
// content is how a command that was never written gets compared against
// agenthub's own and matched.
func (s *tomlScanner) escape() (byte, bool) {
	if s.eof() {
		return 0, false
	}
	c := s.peek()
	s.i++
	switch c {
	case 'b':
		return '\b', true
	case 't':
		return '\t', true
	case 'n':
		return '\n', true
	case 'f':
		return '\f', true
	case 'r':
		return '\r', true
	case '"', '\\':
		return c, true
	default:
		return 0, false
	}
}

// isTOMLLineWrap reports whether what follows a backslash inside a
// multi-line string is a line continuation: optional blanks, then a newline.
func isTOMLLineWrap(rest []byte) bool {
	for _, c := range rest {
		switch c {
		case ' ', '\t', '\r':
			continue
		case '\n':
			return true
		default:
			return false
		}
	}
	return false
}

// arrayValue reads an array, keeping the elements only when every one of
// them is a string. Arrays may span lines and nest.
func (s *tomlScanner) arrayValue() (tomlValue, bool) {
	s.i++ // '['
	out := tomlValue{kind: tomlArray, list: []string{}}
	allStrings := true
	for {
		s.skipIgnorable()
		if s.eof() {
			return tomlValue{}, false
		}
		if s.peek() == ']' {
			s.i++
			if !allStrings {
				out.kind = tomlOther
				out.list = nil
			}
			return out, true
		}
		el, ok := s.value()
		if !ok {
			return tomlValue{}, false
		}
		if el.kind == tomlString {
			out.list = append(out.list, el.str)
			continue
		}
		allStrings = false
	}
}

// inlineTable consumes `{...}` intact. Its contents are never interpreted:
// the only inline table that could matter is a server definition, and a
// caller that meets one where servers live refuses the file instead.
func (s *tomlScanner) inlineTable() (tomlValue, bool) {
	s.i++ // '{'
	for {
		s.skipIgnorable()
		if s.eof() {
			return tomlValue{}, false
		}
		if s.peek() == '}' {
			s.i++
			return tomlValue{kind: tomlOther}, true
		}
		if _, _, ok := s.keyValue(); !ok {
			return tomlValue{}, false
		}
	}
}
