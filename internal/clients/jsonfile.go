package clients

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// MaxConfigSize is the largest client configuration agenthub will read
// (docs/canonical.md §2, inherited from toolport clients.rs). Anything larger is a
// runaway file, not configuration; parsing it would burn memory on a path
// the user cannot even see.
const MaxConfigSize int64 = 64 << 20 // 64 MiB

// defaultMode is the file mode for a freshly created configuration file. A
// project-level .mcp.json is meant to be committed and shared, so it is
// world-readable, unlike the 0600 registry documents. An existing file
// keeps its own mode.
const defaultMode fs.FileMode = 0o644

// jsonFormat implements Format for both JSON shapes. There is exactly one
// implementation: ShapeServerMap is ShapeNested with the section
// ["mcpServers"], and pretending otherwise would duplicate every
// invariant.
type jsonFormat struct {
	tbl  *Table
	spec *clientSpec
}

var _ Format = (*jsonFormat)(nil)

func (f *jsonFormat) ID() string          { return f.spec.id }
func (f *jsonFormat) DisplayName() string { return f.spec.name }
func (f *jsonFormat) Shape() Shape        { return f.spec.shape() }
func (f *jsonFormat) Writable() bool      { return true }

func (f *jsonFormat) Locations(baseDir string) []Location {
	return f.spec.resolve(f.tbl, baseDir)
}

// DefaultPath implements Format: the DefaultPlacement (user-level) location.
func (f *jsonFormat) DefaultPath(baseDir string) string {
	return f.spec.defaultTarget(f.tbl, baseDir)
}

// PathFor implements Format: the location with that placement, or "".
func (f *jsonFormat) PathFor(baseDir string, p Placement) string {
	return f.spec.pathFor(f.tbl, baseDir, p)
}

// ManualSnippet renders the JSON fragment for the client's section, used
// as the remedy hint when a file is refused (JSONC, oversized, corrupt).
func (f *jsonFormat) ManualSnippet(entry Entry) string {
	if f.spec.manual != nil {
		return f.spec.manual(f.spec, entry)
	}
	section := mcpServers
	if len(f.spec.locs) > 0 && len(f.spec.locs[0].section) > 0 {
		section = f.spec.locs[0].section
	}
	leaf := map[string]any{entryName: map[string]any{"command": entry.Command, "args": entry.Args}}
	var doc any = leaf
	for i := len(section) - 1; i >= 0; i-- {
		doc = map[string]any{section[i]: doc}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil { // unreachable: only maps/strings/slices above
		return ""
	}
	return string(b) + "\n"
}

// Connect implements Format: read-modify-write of the agenthub entry with
// everything else — sibling keys at every level of the section path,
// foreign server entries, unknown fields — passed through raw.
func (f *jsonFormat) Connect(path string, entry Entry) (Result, error) {
	loc := f.spec.locationFor(f.tbl, "", path)
	cfg, err := f.read(loc)
	if err != nil {
		return Result{}, err
	}
	raw, err := json.Marshal(struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}{Command: entry.Command, Args: entry.Args})
	if err != nil {
		return Result{}, fmt.Errorf("clients: encode gateway entry: %w", err)
	}
	cfg.servers[entryName] = raw
	return f.write(cfg)
}

// Disconnect implements Format: remove exactly the entries agenthub wrote,
// identified by ownership, never by name.
func (f *jsonFormat) Disconnect(path string) (Result, error) {
	loc := f.spec.locationFor(f.tbl, "", path)
	cfg, err := f.read(loc)
	if err != nil {
		return Result{}, err
	}
	if !cfg.exists {
		return Result{}, &NotConnectedError{Path: path}
	}
	var removed []string
	for name, raw := range cfg.servers {
		if ownedBy(raw, f.spec.id) {
			removed = append(removed, name)
		}
	}
	if len(removed) == 0 {
		return Result{}, &NotConnectedError{Path: path}
	}
	slices.Sort(removed)
	for _, name := range removed {
		delete(cfg.servers, name)
	}
	res, err := f.write(cfg)
	if err != nil {
		return Result{}, err
	}
	res.Removed = removed
	return res, nil
}

// ownedBy reports whether a server entry was written by agenthub for
// clientID: its args contain the "connect" subcommand and a --client value
// equal to clientID. Entries that merely happen to be named "agenthub" are
// NOT owned; entries the user renamed but that still point at our gateway
// are. This is the "identify by shape, not by name" rule inherited from
// toolport's repoint.
func ownedBy(raw json.RawMessage, clientID string) bool {
	var e struct {
		Args []string `json:"args"`
	}
	if json.Unmarshal(raw, &e) != nil || !slices.Contains(e.Args, "connect") {
		return false
	}
	for i, a := range e.Args {
		if a == "--client" && i+1 < len(e.Args) && e.Args[i+1] == clientID {
			return true
		}
		if a == "--client="+clientID {
			return true
		}
	}
	return false
}

// jsonConfig is one parsed configuration file. Every level from the
// document root down to the server map is kept as a raw-message map, so
// each level's siblings and every unknown field survive the round trip.
type jsonConfig struct {
	loc     Location
	levels  []map[string]json.RawMessage // levels[0] = document root
	servers map[string]json.RawMessage   // the name -> entry map at the leaf
	orig    []byte                       // original file bytes (nil when !exists)
	exists  bool
	mode    fs.FileMode
}

// read loads loc.Path.
//
// Failure directions, in order of checking:
//   - not-exist        -> empty writable config (the client is not set up)
//   - permission       -> *PermissionError (never confused with not-exist)
//   - larger than 64MB -> *TooLargeError, before any read
//   - unparseable      -> *ParseError, file untouched
func (f *jsonFormat) read(loc Location) (*jsonConfig, error) {
	c := &jsonConfig{
		loc:     loc,
		levels:  []map[string]json.RawMessage{{}},
		servers: map[string]json.RawMessage{},
		mode:    defaultMode,
	}
	for range loc.Section {
		c.levels = append(c.levels, map[string]json.RawMessage{})
	}

	info, err := os.Stat(loc.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return c, nil
	case err != nil:
		if pe := f.tbl.classifyAccess(err, loc.Path, f.spec.id, "stat"); pe != nil {
			return nil, pe
		}
		return nil, fmt.Errorf("clients: stat %s: %w", loc.Path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("clients: %s is a directory, not a configuration file", loc.Path)
	}
	if info.Size() > MaxConfigSize {
		return nil, &TooLargeError{Path: loc.Path, Size: info.Size(), Limit: MaxConfigSize}
	}

	data, err := readLimited(loc.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Raced with a deletion between stat and open: treat as absent.
		return c, nil
	case errors.Is(err, errTooLarge):
		// Grew past the limit between stat and read.
		return nil, &TooLargeError{Path: loc.Path, Size: MaxConfigSize + 1, Limit: MaxConfigSize}
	case err != nil:
		if pe := f.tbl.classifyAccess(err, loc.Path, f.spec.id, "read"); pe != nil {
			return nil, pe
		}
		return nil, fmt.Errorf("clients: read %s: %w", loc.Path, err)
	}
	c.exists = true
	c.orig = data
	c.mode = info.Mode().Perm()

	if err := json.Unmarshal(data, &c.levels[0]); err != nil {
		return nil, f.parseError(loc, data, err)
	}
	for i, key := range loc.Section {
		raw, ok := c.levels[i][key]
		if !ok {
			continue
		}
		var next map[string]json.RawMessage
		if err := json.Unmarshal(raw, &next); err != nil {
			return nil, f.parseError(loc, data,
				fmt.Errorf("%q must be a JSON object: %w", pathOf(loc.Section[:i+1]), err))
		}
		c.levels[i+1] = next
	}
	c.servers = c.levels[len(c.levels)-1]
	return c, nil
}

// parseError builds the typed refusal, adding a JSONC diagnosis when the
// bytes carry comments — the single most common reason a real settings.json
// fails to parse, and one where "invalid JSON" alone reads as a bug.
func (f *jsonFormat) parseError(loc Location, data []byte, err error) *ParseError {
	pe := &ParseError{Path: loc.Path, Client: f.spec.id, Err: err}
	if looksLikeJSONC(data) {
		pe.Hint = "this file uses JSONC (comments); agenthub will not rewrite it because " +
			"re-encoding would delete the comments. Add the entry below by hand."
	} else {
		pe.Hint = "fix or remove the file; agenthub never overwrites configuration it cannot parse"
	}
	pe.Snippet = f.ManualSnippet(Entry{Command: "agenthub", Args: []string{"connect", "--client", f.spec.id}})
	return pe
}

// write renders the config and persists it: no-op when the rendered bytes
// equal the current file content, otherwise a CENTRAL backup (only when a
// previous version existed) followed by an atomic write that preserves the
// original permissions.
//
// Failure direction: if the backup cannot be written the whole operation
// fails and the target is left alone. Modifying a user's config without a
// recoverable copy is worse than not connecting.
func (f *jsonFormat) write(c *jsonConfig) (Result, error) {
	path := c.loc.Path
	c.levels[len(c.levels)-1] = c.servers
	for i := len(c.loc.Section) - 1; i >= 0; i-- {
		raw, err := json.Marshal(c.levels[i+1])
		if err != nil {
			return Result{}, fmt.Errorf("clients: encode %q: %w", pathOf(c.loc.Section[:i+1]), err)
		}
		c.levels[i][c.loc.Section[i]] = raw
	}
	out, err := json.MarshalIndent(c.levels[0], "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("clients: encode %s: %w", path, err)
	}
	out = append(out, '\n')

	if c.exists && bytes.Equal(c.orig, out) {
		return Result{Path: path, Changed: false}, nil
	}

	backup := ""
	if c.exists {
		if backup, err = f.tbl.backup(f.spec.id, path, c.orig); err != nil {
			return Result{}, err
		}
	}
	if err := atomicWrite(path, out, c.mode); err != nil {
		if pe := f.tbl.classifyAccess(err, path, f.spec.id, "write"); pe != nil {
			return Result{}, pe
		}
		return Result{}, fmt.Errorf("clients: write %s: %w", path, err)
	}
	return Result{Path: path, Backup: backup, Changed: true}, nil
}

// errTooLarge signals that readLimited hit the cap.
var errTooLarge = errors.New("clients: file exceeds size limit")

// readLimited reads path with a hard cap of MaxConfigSize bytes, so a file
// that grows between stat and read still cannot exhaust memory.
func readLimited(path string) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	data, err := io.ReadAll(io.LimitReader(fh, MaxConfigSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxConfigSize {
		return nil, errTooLarge
	}
	return data, nil
}

// looksLikeJSONC reports whether data contains a // or /* comment outside
// of a string literal. It is a diagnosis aid, not a parser: a false
// negative only costs a less specific hint, and a false positive is
// impossible for well-formed JSON (which never contains a bare '/').
func looksLikeJSONC(data []byte) bool {
	inString, escaped := false, false
	for i := 0; i < len(data); i++ {
		ch := data[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			continue
		}
		if ch == '/' && i+1 < len(data) && (data[i+1] == '/' || data[i+1] == '*') {
			return true
		}
	}
	return false
}

// pathOf renders a section key path for error messages.
func pathOf(section []string) string {
	out := ""
	for i, k := range section {
		if i > 0 {
			out += "."
		}
		out += k
	}
	return out
}

// atomicWrite persists data to path via a same-directory temp file, chmod,
// fsync and rename, so the target is never observed half-written. Missing
// parent directories are created (0755) because several clients keep their
// config in a dot-directory that only exists once configured. Directory
// fsync is best-effort: project directories may live on filesystems that
// reject it, and the rename itself is still atomic there.
func atomicWrite(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
