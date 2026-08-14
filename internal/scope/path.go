package scope

import "strings"

// NormalizePath canonicalizes a client-reported root path as a PURE string
// operation (docs/model.md, inherited toolport lesson):
//
//   - backslashes become '/'
//   - duplicate slashes collapse (a single leading "//" is preserved when a
//     host character follows it, which is the UNC form; a bare run of slashes
//     is noise and becomes "/")
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

	// Strip trailing slashes; the bare root "/" survives.
	//
	// There is no UNC case to exempt here, and the `s != "//"` that used to
	// stand beside the length test could not fire: reaching it needs the
	// collapse above to have produced "/" AND unc to be true, but unc requires
	// a non-slash at index 2 of the pre-collapse string, which is exactly the
	// character that keeps the collapsed form longer than one byte. A bare run
	// of slashes is noise by the rule two blocks up — "//" normalizes to "/",
	// and the guard read as though it did not.
	for len(s) > 1 && strings.HasSuffix(s, "/") {
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
