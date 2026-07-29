package skills

import (
	"bytes"
	"fmt"
	"strings"
)

// SkillFileName is the manifest every skill package must carry at its root.
const SkillFileName = "SKILL.md"

// fmDelim is the frontmatter fence.
const fmDelim = "---"

// Meta is the parsed SKILL.md manifest: a small frontmatter block plus the
// markdown body.
//
// The frontmatter parser is a deliberately RESTRICTED subset of YAML —
// single-line "key: value" pairs, optionally quoted — not a YAML
// implementation. Two reasons: this package may not add a dependency, and a
// half-written YAML parser that silently mis-reads a nested structure is
// worse than one that admits it does not understand a line. Lines it does
// not understand are never dropped: they are preserved verbatim in Extra
// and written back in place, so a package using richer frontmatter
// round-trips unharmed even though agenthub only reads four keys from it.
type Meta struct {
	Name        string
	Description string
	Version     string
	// Kind is the optional "kind:" key; empty means the caller decides
	// (importers default to KindSkillPack).
	Kind SkillKind
	// Extra holds frontmatter lines agenthub does not interpret, verbatim
	// and in order.
	Extra []string
	// Body is everything after the closing fence, verbatim.
	Body string
}

// ParseSkillMD parses a SKILL.md.
//
// A file without frontmatter is valid: the whole file becomes Body and the
// caller falls back to the directory name (docs/modules/config.md). An UNTERMINATED
// frontmatter block is an error — a file that opens a fence and never closes
// it is either truncated or hand-mangled, and guessing where the metadata
// ends is how a whole document ends up in a description field.
func ParseSkillMD(b []byte) (*Meta, error) {
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	m := &Meta{}
	if !strings.HasPrefix(text, fmDelim+"\n") && text != fmDelim {
		m.Body = text
		return m, nil
	}
	rest := strings.TrimPrefix(text, fmDelim)
	rest = strings.TrimPrefix(rest, "\n")
	end := strings.Index(rest, "\n"+fmDelim)
	if end < 0 {
		return nil, fmt.Errorf("skills: %s frontmatter is not terminated by a closing %q fence", SkillFileName, fmDelim)
	}
	block := rest[:end]
	// Blank lines between the closing fence and the body are formatting,
	// not content: dropping them here is what makes Parse(Bytes(m)) == m.
	m.Body = strings.TrimLeft(rest[end+1+len(fmDelim):], "\n")

	for _, line := range strings.Split(block, "\n") {
		key, value, ok := splitFrontmatterLine(line)
		if !ok {
			if strings.TrimSpace(line) != "" {
				m.Extra = append(m.Extra, line)
			}
			continue
		}
		// First occurrence wins: a duplicated key is a mistake, and taking
		// the later one would let an appended line silently override a
		// reviewed value.
		switch key {
		case "name":
			if m.Name == "" {
				m.Name = value
				continue
			}
		case "description":
			if m.Description == "" {
				m.Description = value
				continue
			}
		case "version":
			if m.Version == "" {
				m.Version = value
				continue
			}
		case "kind":
			if m.Kind == "" {
				m.Kind = SkillKind(value)
				continue
			}
		}
		m.Extra = append(m.Extra, line)
	}
	return m, nil
}

// splitFrontmatterLine recognizes a top-level "key: value" pair. Indented
// lines (nested structures), comments, list items and anything without a
// colon are not recognized and are preserved verbatim by the caller.
func splitFrontmatterLine(line string) (key, value string, ok bool) {
	if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' || line[0] == '-' {
		return "", "", false
	}
	i := strings.Index(line, ":")
	if i <= 0 {
		return "", "", false
	}
	key = line[:i]
	if strings.ContainsAny(key, " \t\"'") {
		return "", "", false
	}
	return key, unquote(strings.TrimSpace(line[i+1:])), true
}

// unquote strips one layer of matching quotes and undoes backslash escaping
// inside double quotes.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner := s[1 : len(s)-1]
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		return strings.ReplaceAll(inner, `\\`, `\`)
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1]
	}
	return s
}

// quoteValue renders a frontmatter value, quoting only when the plain form
// would not survive a round trip.
func quoteValue(s string) string {
	needs := s == "" ||
		strings.ContainsAny(s, ":#\n\"'") ||
		s != strings.TrimSpace(s) ||
		strings.HasPrefix(s, "-") ||
		strings.HasPrefix(s, "[") ||
		strings.HasPrefix(s, "{")
	if !needs {
		return s
	}
	esc := strings.ReplaceAll(s, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	esc = strings.ReplaceAll(esc, "\n", " ")
	return `"` + esc + `"`
}

// Bytes renders the manifest canonically: fence, the four known keys in a
// fixed order (omitting empty ones), preserved Extra lines in their original
// order, closing fence, blank line, body.
//
// Determinism is contract: this is what golden tests pin and what a future
// `skill export` writes, so key order must never depend on map iteration.
func (m *Meta) Bytes() []byte {
	var buf bytes.Buffer
	buf.WriteString(fmDelim + "\n")
	if m.Name != "" {
		fmt.Fprintf(&buf, "name: %s\n", quoteValue(m.Name))
	}
	if m.Description != "" {
		fmt.Fprintf(&buf, "description: %s\n", quoteValue(m.Description))
	}
	if m.Version != "" {
		fmt.Fprintf(&buf, "version: %s\n", quoteValue(m.Version))
	}
	if m.Kind != "" {
		fmt.Fprintf(&buf, "kind: %s\n", quoteValue(string(m.Kind)))
	}
	for _, line := range m.Extra {
		buf.WriteString(line + "\n")
	}
	buf.WriteString(fmDelim + "\n\n")
	buf.WriteString(m.Body)
	return buf.Bytes()
}
