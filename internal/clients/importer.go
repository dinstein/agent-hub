package clients

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dinstein/agent-hub/internal/registry"
)

// SourcePrefix is prepended to a client ID to form ServerEntry.Source for
// imported entries (`source=imported:<client>`, docs/flows.md `client
// import`). It is how `server ls` explains where an entry came from and how
// a re-import recognises its own previous work.
const SourcePrefix = "imported:"

// ImportSource records that clientID owns Source strings of this form.
func ImportSource(clientID string) string { return SourcePrefix + clientID }

// ImportResult is the outcome of reading a client's server map. It is a
// PROPOSAL: nothing is written to the registry here — the caller decides,
// which is what keeps "never overwrite" enforceable at one place.
type ImportResult struct {
	Client string `json:"client"`
	// Sources lists the configuration files the entries came from.
	Sources []string `json:"sources"`
	// Entries are the conversions ready to be added, keyed by the
	// registry server name.
	Entries map[string]registry.ServerEntry `json:"entries"`
	// Conflicts names entries whose target name already exists in the
	// registry. They are NOT in Entries: an import must never silently
	// redefine a server the user already governs.
	Conflicts []ImportConflict `json:"conflicts,omitempty"`
	// Skipped names entries that were not convertible, with a reason.
	Skipped []ImportSkip `json:"skipped,omitempty"`
	// Renamed maps registry name -> original client name for entries
	// whose name had to be sanitised.
	Renamed map[string]string `json:"renamed,omitempty"`
	// SecretWarnings names entries carrying what looks like a literal
	// credential. Registry documents must never hold a credential
	// (registry.ServerEntry doc); the caller is expected to surface this
	// and offer `agenthub secret set` instead.
	SecretWarnings []string `json:"secret_warnings,omitempty"`
}

// ImportConflict is one name collision with the existing registry.
type ImportConflict struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// ImportSkip is one entry that could not be imported.
type ImportSkip struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Import converts clientID's existing MCP server entries into registry
// entries. existing is the set of server names already in the registry;
// every collision is reported in Conflicts and omitted from Entries —
// this function never produces an entry that would overwrite one.
//
// Reading the client's file is a user action (see Inspect): Import opens
// files and may therefore trigger a macOS privacy prompt, unlike Detect.
//
// Ordering: locations are processed in table order (project before user),
// so a project-level definition wins over a user-level one of the same
// name, and the loser is reported as a duplicate rather than dropped.
func (t *Table) Import(clientID, baseDir string, existing []string) (ImportResult, error) {
	spec, ok := specByID[clientID]
	if !ok {
		return ImportResult{}, &UnknownClientError{Client: clientID}
	}
	f, _ := t.Lookup(clientID)
	jf, writable := f.(*jsonFormat)
	if !writable {
		// The same reason we do not write TOML/YAML: we do not parse it.
		return ImportResult{}, &UnsupportedError{
			Client: clientID, Op: "import", Shape: spec.shape(),
			Path:    f.DefaultPath(baseDir),
			Snippet: spec.note,
		}
	}

	taken := make(map[string]struct{}, len(existing))
	for _, n := range existing {
		taken[n] = struct{}{}
	}
	res := ImportResult{Client: clientID, Entries: map[string]registry.ServerEntry{}}
	seen := map[string]string{} // registry name -> file that defined it

	for _, loc := range spec.resolve(t, baseDir) {
		cfg, err := jf.read(loc)
		if err != nil {
			// A missing file is already handled inside read(); anything
			// left is a real failure the user must see (denied, oversized,
			// unparseable) — importing "whatever parsed" from a broken
			// file would be worse than refusing.
			return ImportResult{}, err
		}
		if !cfg.exists {
			continue
		}
		res.Sources = append(res.Sources, loc.Path)

		for _, name := range sortedKeys(cfg.servers) {
			raw := cfg.servers[name]
			if ownedBy(raw, clientID) {
				res.Skipped = append(res.Skipped, ImportSkip{
					Name: name, Path: loc.Path,
					Reason: "agenthub gateway entry (importing it would point agenthub at itself)",
				})
				continue
			}
			target := sanitizeName(name)
			if target == "" {
				res.Skipped = append(res.Skipped, ImportSkip{Name: name, Path: loc.Path, Reason: "empty server name"})
				continue
			}
			entry, err := toServerEntry(raw, clientID)
			if err != nil {
				res.Skipped = append(res.Skipped, ImportSkip{Name: name, Path: loc.Path, Reason: err.Error()})
				continue
			}
			if prev, dup := seen[target]; dup {
				res.Skipped = append(res.Skipped, ImportSkip{
					Name: name, Path: loc.Path,
					Reason: fmt.Sprintf("already imported from %s", prev),
				})
				continue
			}
			if _, clash := taken[target]; clash {
				res.Conflicts = append(res.Conflicts, ImportConflict{Name: target, Path: loc.Path})
				seen[target] = loc.Path
				continue
			}
			if target != name {
				if res.Renamed == nil {
					res.Renamed = map[string]string{}
				}
				res.Renamed[target] = name
			}
			if looksSecretBearing(entry) {
				res.SecretWarnings = append(res.SecretWarnings, target)
			}
			res.Entries[target] = entry
			seen[target] = loc.Path
		}
	}
	return res, nil
}

// toServerEntry converts one client server entry into a registry entry.
//
// The transport is taken from an explicit "type"/"transport" marker when
// present and inferred from the entry's own shape otherwise (command =>
// stdio, url => streamable HTTP). Failure direction: an entry that names
// neither a command nor a URL is REFUSED, never defaulted — a half-formed
// server that fails at connect time is harder to diagnose than an import
// that says so up front.
func toServerEntry(raw []byte, clientID string) (registry.ServerEntry, error) {
	f := decodeFields(raw)
	if f == nil {
		return registry.ServerEntry{}, fmt.Errorf("entry is not a JSON object")
	}
	kind := transportOf(f)
	entry := registry.ServerEntry{
		Enabled: !f.boolean("disabled"),
		Source:  ImportSource(clientID),
	}
	switch kind {
	case "stdio":
		entry.Transport = registry.TransportStdio
		entry.Command = f.str("command")
		if entry.Command == "" {
			return registry.ServerEntry{}, fmt.Errorf("stdio entry has no command")
		}
		entry.Args = f.strings("args")
		entry.Env = f.strMap("env")
		entry.Cwd = f.str("cwd")
	case "http", "sse":
		entry.Transport = kind
		entry.URL = urlOf(f)
		if entry.URL == "" {
			return registry.ServerEntry{}, fmt.Errorf("%s entry has no url", kind)
		}
		entry.Headers = f.strMap("headers")
		// Provenance is a trust declaration, and an imported endpoint has
		// made no such declaration: default to the screened value so SSRF
		// checks stay on. Only an explicit operator action may relax it.
		entry.Provenance = registry.ProvenanceRemote
	default:
		return registry.ServerEntry{}, fmt.Errorf("entry names neither a command nor a url")
	}
	return entry, nil
}

// sanitizeName maps a client's server name onto the character set the
// router namespaces with ([A-Za-z0-9_-]), using the same substitution rule
// as router.sanitize so an imported name never changes meaning downstream.
func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// secretishKeys are the substrings that make a config value likely to be a
// credential.
var secretishKeys = []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "PASSWD", "AUTH", "CREDENTIAL"}

// looksSecretBearing reports whether an entry carries a literal value under
// a credential-looking key. A ${...} placeholder does not count: that is
// exactly the form the registry is designed to hold.
func looksSecretBearing(e registry.ServerEntry) bool {
	for _, m := range []map[string]string{e.Env, e.Headers} {
		for k, v := range m {
			if v == "" || (strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}")) {
				continue
			}
			up := strings.ToUpper(k)
			for _, s := range secretishKeys {
				if strings.Contains(up, s) {
					return true
				}
			}
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
