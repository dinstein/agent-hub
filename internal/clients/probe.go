package clients

import (
	"encoding/json"
	"errors"
	"slices"
	"sort"
)

// probeFormat implements Format for the shapes agenthub deliberately does
// not rewrite: TOML (codex), YAML (continue) and fileless remote clients
// (open-webui).
//
// The rule behind this class: agenthub only rewrites documents it can
// round-trip WITHOUT loss. A TOML/YAML re-encoder that drops comments,
// key order and anchors is a config-destroying machine wearing a
// convenience hat — exactly the failure the whole-app-state rule exists to
// prevent. So these clients get detection plus an exact manual snippet,
// and Connect fails loudly with that snippet attached rather than
// half-working.
type probeFormat struct {
	tbl  *Table
	spec *clientSpec
}

var _ Format = (*probeFormat)(nil)

func (f *probeFormat) ID() string          { return f.spec.id }
func (f *probeFormat) DisplayName() string { return f.spec.name }
func (f *probeFormat) Shape() Shape        { return f.spec.shape() }
func (f *probeFormat) Writable() bool      { return false }

func (f *probeFormat) Locations(baseDir string) []Location {
	return f.spec.resolve(f.tbl, baseDir)
}

// DefaultPath returns the DefaultPlacement probe location ("" for remote
// clients, which have no file at all).
func (f *probeFormat) DefaultPath(baseDir string) string {
	return f.spec.defaultTarget(f.tbl, baseDir)
}

// PathFor returns the probe location with that placement, or "".
func (f *probeFormat) PathFor(baseDir string, p Placement) string {
	return f.spec.pathFor(f.tbl, baseDir, p)
}

// ManualSnippet renders the fragment the user must paste by hand.
func (f *probeFormat) ManualSnippet(entry Entry) string {
	if f.spec.manual == nil {
		return ""
	}
	return f.spec.manual(f.spec, entry)
}

// Connect refuses to write — but a client whose file agenthub can READ is
// answered from it first. An entry that is already there and already points
// at this binary means the requested state holds, and reporting that as a
// failure would send the user to hand-edit a file that needs no editing.
// This is the same "already up to date" the writable clients report.
func (f *probeFormat) Connect(path string, entry Entry) (Result, error) {
	path = f.resolvePath(path)
	name, found, ok := f.ownedEntry(path)
	if ok && found != nil && found.command == entry.Command && slices.Equal(found.args, entry.Args) {
		return Result{Path: path, Changed: false}, nil
	}
	if f.spec.delegate == nil || !f.tbl.delegationEnabled() {
		return Result{}, f.refuse("connect", path, entry, name)
	}

	// A stale entry is replaced, not duplicated: `mcp add` on a name that
	// already exists is an error in every CLI worth delegating to.
	var args []string
	if ok && found != nil {
		args = f.spec.delegate.remove(name)
		if _, _, err := f.runDelegate("connect", path, args); err != nil {
			return Result{}, f.delegationFailed(err, "connect", entry)
		}
	}
	args = f.spec.delegate.add(entryName, entry)
	backup, cmd, err := f.runDelegate("connect", path, args)
	if err != nil {
		return Result{}, f.delegationFailed(err, "connect", entry)
	}
	// Verify rather than assume. An exit status is the delegate's opinion
	// about its own success; the file is the fact.
	if _, after, readOK := f.ownedEntry(path); !readOK || after == nil ||
		after.command != entry.Command || !slices.Equal(after.args, entry.Args) {
		return Result{}, f.delegationFailed(&DelegateError{
			Client: f.spec.id, Op: "connect", Command: cmd,
		}, "connect", entry)
	}
	return Result{Path: path, Backup: backup, Changed: true}, nil
}

// Disconnect refuses to write, and says how to take the entry out — which
// is a different instruction from how to put one in.
//
// When the file is readable it also answers the question first: nothing of
// agenthub's in there is *NotConnectedError, exactly as it is for a client
// agenthub can rewrite. "There is nothing to remove" and "I am not allowed
// to remove it" are different answers and the caller acts on them
// differently.
func (f *probeFormat) Disconnect(path string) (Result, error) {
	path = f.resolvePath(path)
	name, found, ok := f.ownedEntry(path)
	if ok && found == nil {
		return Result{}, &NotConnectedError{Path: path}
	}
	if f.spec.delegate == nil || !f.tbl.delegationEnabled() || !ok {
		return Result{}, f.refuse("disconnect", path, Entry{}, name)
	}
	backup, cmd, err := f.runDelegate("disconnect", path, f.spec.delegate.remove(name))
	if err != nil {
		return Result{}, f.delegationFailed(err, "disconnect", Entry{})
	}
	if _, after, readOK := f.ownedEntry(path); !readOK || after != nil {
		return Result{}, f.delegationFailed(&DelegateError{
			Client: f.spec.id, Op: "disconnect", Command: cmd,
		}, "disconnect", Entry{})
	}
	return Result{Path: path, Backup: backup, Removed: []string{name}}, nil
}

// delegationFailed turns any delegation failure into an error that still
// tells the user how to do it themselves. A delegate that is not installed
// is not a failure at all — it is the manual path, which is what agenthub
// did before it could delegate.
func (f *probeFormat) delegationFailed(err error, op string, entry Entry) error {
	var de *DelegateError
	if errors.As(err, &de) {
		de.Snippet = f.refuse(op, f.DefaultPath(""), entry, "").Snippet
		return de
	}
	// LookPath failed, or the backup did: fall back to instructions.
	return f.refuse(op, f.DefaultPath(""), entry, "")
}

// orEntryName falls back to the name agenthub would have used.
func orEntryName(name string) string {
	if name == "" {
		return entryName
	}
	return name
}

func (f *probeFormat) resolvePath(path string) string {
	if path == "" {
		return f.DefaultPath("")
	}
	return path
}

// ownedEntry looks for agenthub's own entry in a probe-only file.
//
// ok is false when nothing was read — a shape with no reader, an
// unreadable or unmodelled file. Callers MUST NOT treat that as absence:
// it is why the answer is three-valued and not a bool.
func (f *probeFormat) ownedEntry(path string) (name string, entry *tomlEntry, ok bool) {
	if f.spec.readTable == "" || path == "" {
		return "", nil, false
	}
	data, err := readLimited(path)
	if err != nil {
		return "", nil, false
	}
	entries, scanned := scanTOMLServers(data, f.spec.readTable)
	if !scanned {
		return "", nil, false
	}
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		e := entries[n]
		raw, err := json.Marshal(struct {
			Args []string `json:"args"`
		}{Args: e.args})
		if err != nil || !ownedBy(raw, f.spec.id) {
			continue
		}
		return n, &e, true
	}
	return "", nil, true
}

// refuse builds the refusal for one operation. The snippet is the whole
// value of it, and it is op-specific: telling someone how to ADD the entry
// when they asked to remove it is worse than saying nothing, because it
// reads like an answer.
//
// existing names the entry already in the file, when one was found.
func (f *probeFormat) refuse(op, path string, entry Entry, existing string) *UnsupportedError {
	snippet := f.spec.note + "\n\n"
	if op == "disconnect" {
		if f.spec.removal != nil {
			snippet += f.spec.removal(f.spec, orEntryName(existing))
		} else {
			snippet += "Remove agenthub's entry from " + path + " by hand.\n"
		}
	} else {
		if existing != "" {
			snippet += "# an agenthub entry is already there as \"" + existing +
				"\" and points somewhere else; replace it with:\n"
		}
		snippet += f.ManualSnippet(entry)
	}
	return &UnsupportedError{
		Client:  f.spec.id,
		Op:      op,
		Shape:   f.spec.shape(),
		Path:    path,
		Snippet: snippet,
	}
}
