package catalog

import "strings"

// Two placeholder syntaxes live in a catalog entry, and keeping them apart
// is the point:
//
//	{{name}}        an ADD-TIME parameter. Substituted here, before the entry
//	                is stored; it must never survive into the registry.
//	${SECRET_KEY}   a CONNECT-TIME vault reference. Left verbatim; resolving
//	                it here would put a credential into a registry document,
//	                which is the one thing a registry document must never
//	                hold.
//
// The scanners below are deliberately literal — no regexp, no nesting, no
// escaping rules. A catalog entry is a short command line written by a
// maintainer, not a template language, and every rule this file does not
// have is a rule nobody has to reason about at a review.

const (
	paramOpen  = "{{"
	paramClose = "}}"
	// secretOpen is the vault-reference prefix; the key is what follows it
	// up to the closing brace.
	secretOpen = "${SECRET_"
)

// placeholdersIn returns the {{name}} placeholders in s, in order of
// appearance and with duplicates kept (callers de-duplicate).
//
// An unterminated or empty marker yields nothing: it is literal text, not a
// placeholder, and treating it as one would make a stray "{{" impossible to
// write.
func placeholdersIn(s string) []string {
	var out []string
	rest := s
	for {
		i := strings.Index(rest, paramOpen)
		if i < 0 {
			return out
		}
		rest = rest[i+len(paramOpen):]
		end := strings.Index(rest, paramClose)
		if end < 0 {
			return out
		}
		if name := rest[:end]; validParamName(name) {
			out = append(out, name)
		}
		rest = rest[end+len(paramClose):]
	}
}

// validParamName restricts a placeholder name to [A-Za-z0-9_-]. Anything
// else (a space, a brace, a dot) means the text was not meant as a
// placeholder.
func validParamName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// secretRefsIn returns the ${SECRET_KEY} keys referenced in s, in order of
// appearance and with duplicates kept.
func secretRefsIn(s string) []string {
	var out []string
	rest := s
	for {
		i := strings.Index(rest, secretOpen)
		if i < 0 {
			return out
		}
		rest = rest[i+len(secretOpen):]
		end := strings.Index(rest, "}")
		if end < 0 {
			return out
		}
		if key := rest[:end]; key != "" {
			out = append(out, key)
		}
		rest = rest[end+1:]
	}
}

// substitute replaces every declared {{name}} in s with its value.
//
// A placeholder with no value is left ALONE rather than replaced by the
// empty string. Callers treat a leftover placeholder as a hard failure, so
// leaving it is what makes the failure visible; blanking it would produce a
// plausible-looking command line with an argument silently missing.
func substitute(s string, values map[string]string) string {
	if !strings.Contains(s, paramOpen) {
		return s
	}
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, paramOpen)
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:i])
		after := rest[i+len(paramOpen):]
		end := strings.Index(after, paramClose)
		if end < 0 {
			b.WriteString(rest[i:])
			return b.String()
		}
		name := after[:end]
		if v, ok := values[name]; ok && validParamName(name) {
			b.WriteString(v)
		} else {
			b.WriteString(paramOpen + name + paramClose)
		}
		rest = after[end+len(paramClose):]
	}
}
