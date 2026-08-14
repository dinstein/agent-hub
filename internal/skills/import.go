package skills

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/dinstein/agent-hub/internal/platform"
)

// scanned is the result of reading a source tree without writing anything.
type scanned struct {
	files []FileEntry
	meta  *Meta
	// dirName is the source directory's base name, the fallback skill name.
	dirName string
}

// scanTree validates and hashes a source directory.
//
// Every rejection below closes a path where "import a directory" degrades
// into an arbitrary-read or arbitrary-write primitive, so none of them are
// negotiable:
//
//   - Symlinks (of any kind, file or directory) are refused, not followed:
//     following one copies content from outside the package, and preserving
//     one makes the installed copy point at an attacker-chosen path inside
//     the user's home.
//   - Non-regular files (devices, sockets, fifos) are refused: copying them
//     is either meaningless or a blocking read.
//   - The marker file name is refused in a source tree: it is the ownership
//     proof for installed directories, and a package that carries one could
//     forge ownership of a directory agenthub must refuse to touch.
//   - Size and count caps bound a mistyped path (a home directory, a repo
//     with node_modules) into a fast failure rather than a huge copy.
//
// Path escapes ("..", absolute, drive-relative) cannot occur — every path
// is derived from filepath.Rel against the walk root — but the check stays
// as a belt-and-braces assertion because the whole install layer trusts
// FileEntry.Path to be package-relative.
func (m *Manager) scanTree(src string) (*scanned, error) {
	root, err := filepath.Abs(src)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, &ImportError{Path: root, Reason: "source is a symlink"}
	}
	if !info.IsDir() {
		return nil, &ImportError{Path: root, Reason: "source is not a directory"}
	}

	out := &scanned{dirName: filepath.Base(root)}
	var total int64
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		slashed := filepath.ToSlash(rel)
		if slashed == ".." || strings.HasPrefix(slashed, "../") || filepath.IsAbs(slashed) {
			return &ImportError{Path: p, Reason: "path escapes the package root"}
		}
		if d.Type()&os.ModeSymlink != 0 {
			return &ImportError{Path: p, Reason: "symlinks are not imported"}
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return &ImportError{Path: p, Reason: "not a regular file"}
		}
		if filepath.Base(slashed) == MarkerFileName {
			return &ImportError{Path: p, Reason: "reserved file name " + MarkerFileName}
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Size() > m.opts.MaxFileSize {
			return &ImportError{Path: p, Reason: fmt.Sprintf("file is %d bytes, limit is %d", fi.Size(), m.opts.MaxFileSize)}
		}
		total += fi.Size()
		if total > m.opts.MaxTotalSize {
			return &ImportError{Path: root, Reason: fmt.Sprintf("package exceeds %d bytes", m.opts.MaxTotalSize)}
		}
		if len(out.files) >= m.opts.MaxFiles {
			return &ImportError{Path: root, Reason: fmt.Sprintf("package exceeds %d files", m.opts.MaxFiles)}
		}
		sum, err := hashFile(p)
		if err != nil {
			return err
		}
		out.files = append(out.files, FileEntry{Path: slashed, SHA256: sum, Size: fi.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out.files) == 0 {
		return nil, &ImportError{Path: root, Reason: "package is empty"}
	}
	slices.SortFunc(out.files, func(a, b FileEntry) int { return cmp.Compare(a.Path, b.Path) })

	raw, err := os.ReadFile(filepath.Join(root, SkillFileName))
	switch {
	case err == nil:
		if out.meta, err = ParseSkillMD(raw); err != nil {
			return nil, err
		}
	case errors.Is(err, os.ErrNotExist):
		out.meta = &Meta{}
	default:
		return nil, err
	}
	return out, nil
}

// scanContent puts the untrusted text of a package through Options.
// ContentScanner. A hit refuses the import outright rather than importing and
// flagging, because an imported skill is one `sync` away from being
// materialized into a client's directory, and SKILL.md is a first-class
// prompt-injection carrier (docs/subsystems/skills.md).
//
// NOTHING SETS THAT SCANNER TODAY. The injection scanner it was shaped for
// went with the removed governance surface, so the nil check below is the
// path every import actually takes and this function is a no-op. Options.
// ContentScanner says the same beside the field; it is repeated here because
// this is where a reader tracing the import path arrives, and a description
// of what the seam WOULD do reads exactly like a check imports are passing.
func (m *Manager) scanContent(sc *scanned, name string) error {
	if m.opts.ContentScanner == nil {
		return nil
	}
	if err := m.opts.ContentScanner("name", name); err != nil {
		return fmt.Errorf("skills: content refused (name): %w", err)
	}
	if sc.meta != nil {
		if err := m.opts.ContentScanner("description", sc.meta.Description); err != nil {
			return fmt.Errorf("skills: content refused (description): %w", err)
		}
		if err := m.opts.ContentScanner(SkillFileName, sc.meta.Body); err != nil {
			return fmt.Errorf("skills: content refused (%s): %w", SkillFileName, err)
		}
	}
	return nil
}

// copyTree materializes the scanned files into dst.
//
// Every file is re-hashed while copying and compared against the scan: a
// source that changed between scan and copy aborts the import instead of
// producing a library copy whose ContentHash lies about its content
// (time-of-check/time-of-use).
func copyTree(src, dst string, files []FileEntry) error {
	if err := platform.EnsureDir(dst); err != nil {
		return err
	}
	for _, f := range files {
		from := filepath.Join(src, filepath.FromSlash(f.Path))
		to := filepath.Join(dst, filepath.FromSlash(f.Path))
		if err := platform.EnsureDir(filepath.Dir(to)); err != nil {
			return err
		}
		data, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if got := hashBytes(data); got != f.SHA256 {
			return &ImportError{Path: from, Reason: "file changed during import (hash mismatch)"}
		}
		if err := atomicWrite(to, data); err != nil {
			return err
		}
	}
	return nil
}

// hashFile hashes a file without loading it whole.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashDir recomputes the content hash of a materialized directory: every
// regular file under root except the ownership marker.
//
// Fail direction: an unreadable entry is an error, never a skipped file —
// silently skipping would let a permission trick hide drift.
func hashDir(root string) (string, []FileEntry, error) {
	var files []FileEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		slashed := filepath.ToSlash(rel)
		if slashed == MarkerFileName {
			return nil
		}
		if !d.Type().IsRegular() {
			// A symlink or device where a skill file should be is drift by
			// definition; give it a hash that can never match.
			files = append(files, FileEntry{Path: slashed, SHA256: "non-regular", Size: -1})
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		sum, err := hashFile(p)
		if err != nil {
			return err
		}
		files = append(files, FileEntry{Path: slashed, SHA256: sum, Size: fi.Size()})
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	slices.SortFunc(files, func(a, b FileEntry) int { return cmp.Compare(a.Path, b.Path) })
	return ContentHash(files), files, nil
}

// slugify turns a display name into an ID: lowercase ASCII alphanumerics
// and single dashes, no leading or trailing dash. Non-ASCII runes are
// dropped rather than transliterated; a name that slugifies to nothing
// falls back to "skill", and the caller's collision suffix makes it unique.
func slugify(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "skill"
	}
	return s
}

// uniqueID appends "-2", "-3", ... until the ID is free (the ServerEntry
// deduplication rule).
func uniqueID(base string, taken func(string) bool) string {
	if !taken(base) {
		return base
	}
	for n := 2; ; n++ {
		cand := fmt.Sprintf("%s-%d", base, n)
		if !taken(cand) {
			return cand
		}
	}
}

// deriveVersion returns the declared version or a content-derived one, so
// every library entry always has a version to display and compare.
func deriveVersion(declared, contentHash string) string {
	if declared != "" {
		return declared
	}
	h := contentHash
	if len(h) > 8 {
		h = h[:8]
	}
	return "0.0.0+" + h
}
