package clients

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

// Connect always refuses, carrying the snippet that does the job.
func (f *probeFormat) Connect(path string, entry Entry) (Result, error) {
	return Result{}, f.unsupported("connect", path, entry)
}

// Disconnect always refuses: agenthub never wrote anything here, so there
// is nothing it may safely remove.
func (f *probeFormat) Disconnect(path string) (Result, error) {
	return Result{}, f.unsupported("disconnect",
		path, Entry{Command: "agenthub", Args: []string{"connect", "--client", f.spec.id}})
}

func (f *probeFormat) unsupported(op, path string, entry Entry) *UnsupportedError {
	if path == "" {
		path = f.DefaultPath("")
	}
	return &UnsupportedError{
		Client:  f.spec.id,
		Op:      op,
		Shape:   f.spec.shape(),
		Path:    path,
		Snippet: f.spec.note + "\n\n" + f.ManualSnippet(entry),
	}
}
