package scope

import "strings"

// NormalizePath canonicalizes a client-reported root path as a PURE string
// operation (docs/architecture.md §7, inherited toolport lesson):
//
//   - backslashes become '/'
//   - duplicate slashes collapse (a single leading "//" is preserved for
//     UNC paths)
//   - trailing slashes are stripped (bare "/" survives)
//   - Windows-form paths (drive-letter prefix or any backslash in the input)
//     are lowercased, because Windows paths compare case-insensitively
//
// It NEVER canonicalizes and NEVER touches the disk — no symlink
// resolution, no existence or case probing: the reported path may not exist
// on this machine.
func NormalizePath(p string) string {
	if p == "" {
		return ""
	}
	windows := strings.ContainsRune(p, '\\') ||
		(len(p) >= 2 && p[1] == ':' && isASCIIAlpha(p[0]))

	s := strings.ReplaceAll(p, "\\", "/")
	// A leading "//" followed by a host character is a UNC prefix and is
	// preserved (also on re-normalization of already-normalized output —
	// NormalizePath must be idempotent). A bare run of slashes is noise.
	unc := strings.HasPrefix(s, "//") && len(s) > 2 && s[2] != '/'

	// Collapse runs of '/' into one.
	var b strings.Builder
	b.Grow(len(s))
	prevSlash := false
	for _, r := range s {
		if r == '/' {
			if prevSlash {
				continue
			}
			prevSlash = true
		} else {
			prevSlash = false
		}
		b.WriteRune(r)
	}
	s = b.String()
	if unc {
		s = "/" + s // restore the single extra leading slash of a UNC path
	}

	// Strip trailing slashes; keep a bare root ("/" or UNC "//") intact.
	for len(s) > 1 && s != "//" && strings.HasSuffix(s, "/") {
		s = s[:len(s)-1]
	}

	if windows {
		s = strings.ToLower(s)
	}
	return s
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
