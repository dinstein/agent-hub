package catalog

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/dinstein/agent-hub/internal/registry"
)

// SourcePrefix is prepended to a catalog id to form ServerEntry.Source, the
// same way a pasted document marks its own (SourcePasted) and a hand-typed
// one is "manual".
//
// No Go code outside this package reads it. `server ls` prints Source whole,
// prefix included, rather than parsing it — the string is meant to be read,
// not matched on. The one consumer that does match is the GUI's catalog
// page, which carries its own `SOURCE_PREFIX = "catalog:"` and a comment
// naming this constant as its source. Nothing checks that the two agree; see
// TestGUIBindingNamesResolve, which covers the other cross-language string
// seam and now names this one.
const SourcePrefix = "catalog:"

// Source records that a stored entry came from catalog id.
func Source(id string) string { return SourcePrefix + id }

// Provenance grades where a definition came from. See the package doc on
// what this is NOT: it is a source signal, never a cryptographic proof.
type Provenance string

// The three provenance grades.
const (
	// ProvenanceCurated is the embedded seed directory: reviewed by the
	// agenthub maintainers at the time it was written.
	ProvenanceCurated Provenance = "curated"
	// ProvenanceRegistry is a remote index. Not implemented — the constant
	// exists so a future index cannot be mistaken for a curated entry.
	ProvenanceRegistry Provenance = "registry"
	// ProvenanceUser is a definition the person at the keyboard typed or
	// pasted. The paste parser produces these.
	ProvenanceUser Provenance = "user"
)

// AuthOAuth marks an entry whose server requires an OAuth login AFTER it is
// added (`agenthub auth login <id>`). It does not make the entry harder to
// add: the login is a separate, later step, so such an entry is still
// one-click addable.
const AuthOAuth = "oauth"

// Credential is one secret an entry needs before it can connect.
//
// Key is the VAULT key (`agenthub secret set <server> <KEY>`), which is the
// same <KEY> the entry references as ${SECRET_<KEY>} in its environment or
// headers. The value itself never appears here and never appears anywhere
// else in this package: the catalog describes which credential is needed, it
// does not carry one.
type Credential struct {
	Key         string `json:"key"`
	Description string `json:"description,omitempty"`
	// Optional marks a credential the server works without (a rate-limit
	// raiser, say). An optional credential does NOT make an entry need
	// configuration.
	Optional bool `json:"optional,omitempty"`
}

// Param is one plain (non-secret) value the user must supply before the
// entry can be added: a directory, a database path, a workspace id. The
// entry references it as {{name}} in its command line, URL, environment or
// headers.
//
// The two placeholder syntaxes are deliberately different. ${SECRET_X} is
// resolved at CONNECT time from the vault and must survive into the stored
// entry; {{name}} is resolved at ADD time and must NOT survive it.
type Param struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Example     string `json:"example,omitempty"`
}

// Entry is one server the catalog offers.
//
// The transport-specific fields follow the registry's own split: stdio uses
// Command/Args/Env, http and sse use URL/Headers. Mixing them is rejected at
// load time here and again by confops at add time.
type Entry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Publisher is who publishes the server ("modelcontextprotocol",
	// "GitHub", …). A signal, not an identity check.
	Publisher string `json:"publisher,omitempty"`
	// Homepage is where the invocation below was documented.
	Homepage   string     `json:"homepage,omitempty"`
	Provenance Provenance `json:"provenance"`

	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`

	// Keys are the credentials this entry references.
	Keys []Credential `json:"keys,omitempty"`
	// Params are the plain values the user must supply.
	Params []Param `json:"params,omitempty"`
	// Auth is AuthOAuth for a server that needs a login after being added,
	// empty otherwise.
	Auth string `json:"auth,omitempty"`
	// Tags are search keywords.
	Tags []string `json:"tags,omitempty"`
}

// clone returns a deep copy. The package hands out copies so a caller that
// edits an entry's slice or map cannot corrupt the shared seed.
func (e Entry) clone() Entry {
	out := e
	out.Args = slices.Clone(e.Args)
	out.Env = maps.Clone(e.Env)
	out.Headers = maps.Clone(e.Headers)
	out.Keys = slices.Clone(e.Keys)
	out.Params = slices.Clone(e.Params)
	out.Tags = slices.Clone(e.Tags)
	return out
}

// RequiredKeys lists the credentials that must be stored before the server
// can connect (optional ones excluded).
func (e Entry) RequiredKeys() []string {
	out := make([]string, 0, len(e.Keys))
	for _, k := range e.Keys {
		if !k.Optional {
			out = append(out, k.Key)
		}
	}
	return out
}

// NeedsConfig reports whether adding this entry needs input from the user.
//
// The judgement (docs/subsystems/docs/subsystems/controlplane.md, "skip whatever can be skipped"): an entry with no required
// credential, no declared parameter and no leftover placeholder anywhere in
// its definition is a ONE-CLICK add; everything else opens a form. An
// optional credential does not count — the server works without it, and
// asking for it up front would put a form in front of the majority who do
// not have one.
//
// The placeholder scan is not redundant with the declarations: it is what
// makes the judgement fail CLOSED. A seed entry that references {{dir}}
// without declaring it is a bug, and the effect of that bug must be "this
// needs configuration", never "add it silently with the literal text in its
// argv". (Load-time validation rejects such an entry outright; this is the
// second line.)
func (e Entry) NeedsConfig() bool {
	return len(e.RequiredKeys()) > 0 || len(e.Params) > 0 || len(e.Placeholders()) > 0
}

// Placeholders lists the distinct {{name}} placeholders the entry still
// carries, sorted. Secret placeholders are deliberately not included: they
// are resolved at connect time and belong in the stored entry.
func (e Entry) Placeholders() []string {
	seen := map[string]struct{}{}
	for _, s := range e.strings() {
		for _, name := range placeholdersIn(s) {
			seen[name] = struct{}{}
		}
	}
	out := slices.Sorted(maps.Keys(seen))
	return out
}

// SecretRefs lists the distinct ${SECRET_X} keys the entry references,
// sorted.
func (e Entry) SecretRefs() []string {
	seen := map[string]struct{}{}
	for _, s := range e.strings() {
		for _, key := range secretRefsIn(s) {
			seen[key] = struct{}{}
		}
	}
	out := slices.Sorted(maps.Keys(seen))
	return out
}

// strings returns every substitutable string in the entry, in a stable
// order (map values sorted by key) so derived output is deterministic.
func (e Entry) strings() []string {
	out := []string{e.Command, e.URL}
	out = append(out, e.Args...)
	for _, m := range []map[string]string{e.Env, e.Headers} {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			out = append(out, m[k])
		}
	}
	return out
}

// ParamError reports what is wrong with the parameter values supplied to
// Render. All three classes are reported at once so a form can mark every
// field in one pass instead of one per round trip.
//
// Unknown is an error rather than a shrug: a caller that misspells
// "directory" would otherwise get an entry with an unsubstituted
// placeholder and no indication of why.
type ParamError struct {
	// ID is the catalog entry the parameters were meant for.
	ID string
	// Missing names declared parameters with no value (or a blank one).
	Missing []string
	// Unknown names supplied values no parameter declares.
	Unknown []string
	// Invalid maps a parameter name to why its value was refused.
	Invalid map[string]string
}

func (e *ParamError) Error() string {
	var parts []string
	if len(e.Missing) > 0 {
		parts = append(parts, "missing "+strings.Join(e.Missing, ", "))
	}
	if len(e.Unknown) > 0 {
		parts = append(parts, "unknown "+strings.Join(e.Unknown, ", "))
	}
	for _, name := range slices.Sorted(maps.Keys(e.Invalid)) {
		parts = append(parts, name+": "+e.Invalid[name])
	}
	return fmt.Sprintf("catalog entry %q: %s", e.ID, strings.Join(parts, "; "))
}

// Render substitutes params into the entry and returns the registry
// definition to store.
//
// Failure direction: every placeholder must be resolved. A missing value is
// an error, not an empty substitution — `server-filesystem ""` would either
// fail with a confusing message or, worse, resolve to the process working
// directory.
func (e Entry) Render(params map[string]string) (registry.ServerEntry, error) {
	declared := make(map[string]struct{}, len(e.Params))
	for _, p := range e.Params {
		declared[p.Name] = struct{}{}
	}
	perr := &ParamError{ID: e.ID, Invalid: map[string]string{}}
	values := make(map[string]string, len(params))
	for name, raw := range params {
		if _, ok := declared[name]; !ok {
			perr.Unknown = append(perr.Unknown, name)
			continue
		}
		v := strings.TrimSpace(raw)
		switch {
		case v == "":
			perr.Missing = append(perr.Missing, name)
		case strings.ContainsAny(v, "\n\r\x00"):
			// A newline in an argv element or a header value is how a
			// pasted blob turns into a second, invisible setting.
			perr.Invalid[name] = "must not contain a newline"
		default:
			values[name] = v
		}
	}
	for _, p := range e.Params {
		if _, ok := values[p.Name]; !ok && !slices.Contains(perr.Missing, p.Name) {
			if _, bad := perr.Invalid[p.Name]; !bad {
				perr.Missing = append(perr.Missing, p.Name)
			}
		}
	}
	slices.Sort(perr.Missing)
	slices.Sort(perr.Unknown)
	if len(perr.Missing) > 0 || len(perr.Unknown) > 0 || len(perr.Invalid) > 0 {
		return registry.ServerEntry{}, perr
	}

	sub := func(s string) string { return substitute(s, values) }
	out := registry.ServerEntry{
		Transport: e.Transport,
		Enabled:   true,
		Source:    Source(e.ID),
	}
	switch e.Transport {
	case registry.TransportStdio:
		out.Command = sub(e.Command)
		if len(e.Args) > 0 {
			args := make([]string, len(e.Args))
			for i, a := range e.Args {
				args[i] = sub(a)
			}
			out.Args = args
		}
		out.Env = subMap(e.Env, sub)
	case registry.TransportHTTP, registry.TransportSSE:
		out.URL = sub(e.URL)
		out.Headers = subMap(e.Headers, sub)
		// Provenance is a trust declaration and the catalog makes none: a
		// curated endpoint is screened like any other remote one. Only an
		// explicit operator action may relax it.
		out.Provenance = registry.ProvenanceRemote
	default:
		// Unreachable: load-time validation refuses any other transport.
		return registry.ServerEntry{}, fmt.Errorf("catalog entry %q: unsupported transport %q", e.ID, e.Transport)
	}
	// Second line, same reason as in NeedsConfig: never store a literal
	// placeholder.
	if left := leftoverPlaceholders(out); len(left) > 0 {
		return registry.ServerEntry{}, &ParamError{ID: e.ID, Missing: left}
	}
	return out, nil
}

// leftoverPlaceholders reports {{name}} markers that survived substitution.
func leftoverPlaceholders(e registry.ServerEntry) []string {
	seen := map[string]struct{}{}
	strs := append([]string{e.Command, e.URL}, e.Args...)
	for _, m := range []map[string]string{e.Env, e.Headers} {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			strs = append(strs, m[k])
		}
	}
	for _, s := range strs {
		for _, name := range placeholdersIn(s) {
			seen[name] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func subMap(m map[string]string, sub func(string) string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = sub(v)
	}
	return out
}

// seedFile is the on-disk shape of seed.json. Version exists so a future
// format change is a version bump rather than a silent reinterpretation.
type seedFile struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

//go:embed seed.json
var seedJSON []byte

// seedVersion is the only seed format this build understands.
const seedVersion = 1

// seed is the parsed, validated directory, sorted by id.
//
// It is parsed at init and PANICS on a malformed or invalid seed. The seed
// is compiled into the binary, so a failure here is a build defect, not a
// runtime condition: there is no user input that can cause it and no
// recovery that would leave the catalog meaningful. TestSeedIsValid is what
// keeps that panic from ever reaching a user.
var seed = mustLoadSeed()

func mustLoadSeed() []Entry {
	entries, err := parseSeed(seedJSON)
	if err != nil {
		panic("catalog: embedded seed.json is invalid: " + err.Error())
	}
	return entries
}

// parseSeed decodes and validates the embedded directory.
func parseSeed(data []byte) ([]Entry, error) {
	var file seedFile
	dec := json.NewDecoder(strings.NewReader(string(data)))
	// A key nobody models is a field the author believed they were setting.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, err
	}
	if file.Version != seedVersion {
		return nil, fmt.Errorf("seed version %d, want %d", file.Version, seedVersion)
	}
	seen := map[string]struct{}{}
	for i := range file.Entries {
		e := file.Entries[i]
		if err := validateEntry(e); err != nil {
			return nil, err
		}
		if _, dup := seen[e.ID]; dup {
			return nil, fmt.Errorf("duplicate catalog id %q", e.ID)
		}
		seen[e.ID] = struct{}{}
	}
	out := slices.Clone(file.Entries)
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// validateEntry enforces the invariants the rest of the package relies on:
// a usable identity, one of the two transport shapes, and — the load-bearing
// pair — declarations that agree exactly with the placeholders used.
//
// The agreement is checked in BOTH directions. An undeclared {{x}} would
// produce an entry nobody can complete; a declared parameter nothing
// references would put a field in the form that changes nothing.
func validateEntry(e Entry) error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("catalog entry %q: "+format, append([]any{e.ID}, a...)...)
	}
	if strings.TrimSpace(e.ID) == "" {
		return fmt.Errorf("catalog entry with an empty id")
	}
	if strings.TrimSpace(e.Name) == "" || strings.TrimSpace(e.Description) == "" {
		return fail("name and description are required")
	}
	if e.Provenance != ProvenanceCurated && e.Provenance != ProvenanceRegistry && e.Provenance != ProvenanceUser {
		return fail("unknown provenance %q", e.Provenance)
	}
	if e.Auth != "" && e.Auth != AuthOAuth {
		return fail("unknown auth %q", e.Auth)
	}
	switch e.Transport {
	case registry.TransportStdio:
		if strings.TrimSpace(e.Command) == "" {
			return fail("the stdio transport needs a command")
		}
		if e.URL != "" || len(e.Headers) > 0 {
			return fail("url and headers apply to the http and sse transports only")
		}
	case registry.TransportHTTP, registry.TransportSSE:
		if strings.TrimSpace(e.URL) == "" {
			return fail("the %s transport needs a url", e.Transport)
		}
		if e.Command != "" || len(e.Args) > 0 || len(e.Env) > 0 {
			return fail("command/args/env apply to the stdio transport only")
		}
	default:
		return fail("unknown transport %q", e.Transport)
	}

	declaredParams := map[string]struct{}{}
	for _, p := range e.Params {
		if strings.TrimSpace(p.Name) == "" {
			return fail("a parameter needs a name")
		}
		if _, dup := declaredParams[p.Name]; dup {
			return fail("parameter %q declared twice", p.Name)
		}
		declaredParams[p.Name] = struct{}{}
	}
	used := e.Placeholders()
	for _, name := range used {
		if _, ok := declaredParams[name]; !ok {
			return fail("uses undeclared parameter {{%s}}", name)
		}
	}
	for name := range declaredParams {
		if !slices.Contains(used, name) {
			return fail("declares parameter %q that nothing references", name)
		}
	}

	declaredKeys := map[string]struct{}{}
	for _, k := range e.Keys {
		if strings.TrimSpace(k.Key) == "" {
			return fail("a credential needs a key")
		}
		if _, dup := declaredKeys[k.Key]; dup {
			return fail("credential %q declared twice", k.Key)
		}
		declaredKeys[k.Key] = struct{}{}
	}
	refs := e.SecretRefs()
	for _, key := range refs {
		if _, ok := declaredKeys[key]; !ok {
			return fail("references undeclared credential ${SECRET_%s}", key)
		}
	}
	for key := range declaredKeys {
		if !slices.Contains(refs, key) {
			return fail("declares credential %q that nothing references", key)
		}
	}
	return nil
}

// List returns the whole directory, sorted by id.
func List() []Entry {
	out := make([]Entry, 0, len(seed))
	for _, e := range seed {
		out = append(out, e.clone())
	}
	return out
}

// Get returns one entry by id.
func Get(id string) (Entry, bool) {
	i, ok := slices.BinarySearchFunc(seed, id, func(e Entry, target string) int {
		return strings.Compare(e.ID, target)
	})
	if !ok {
		return Entry{}, false
	}
	return seed[i].clone(), true
}

// Search returns the entries matching query, best match first.
//
// The query is split on whitespace and EVERY term must match somewhere —
// "git hub" narrows rather than widens. Scoring is coarse on purpose (id and
// name beat description, exact beats prefix beats substring): with a
// directory this size, a subtler ranker would only make the order harder to
// predict. An empty query returns the full list.
//
// Ordering is fully deterministic: equal scores break by id, so the same
// query always renders the same list — a test can pin it and a user's eye
// can learn it.
func Search(query string) []Entry {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return List()
	}
	type scored struct {
		entry Entry
		score int
	}
	var hits []scored
	for _, e := range seed {
		total := 0
		matchedAll := true
		for _, term := range terms {
			s := scoreEntry(e, term)
			if s == 0 {
				matchedAll = false
				break
			}
			total += s
		}
		if matchedAll {
			hits = append(hits, scored{entry: e.clone(), score: total})
		}
	}
	slices.SortStableFunc(hits, func(a, b scored) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c // descending by score
		}
		return strings.Compare(a.entry.ID, b.entry.ID)
	})
	out := make([]Entry, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.entry)
	}
	return out
}

// scoreEntry scores one lower-cased term against one entry; 0 means no match.
func scoreEntry(e Entry, term string) int {
	id := strings.ToLower(e.ID)
	name := strings.ToLower(e.Name)
	switch {
	case id == term || name == term:
		return 8
	case strings.HasPrefix(id, term) || strings.HasPrefix(name, term):
		return 6
	case strings.Contains(id, term) || strings.Contains(name, term):
		return 4
	}
	for _, tag := range e.Tags {
		if strings.ToLower(tag) == term {
			return 3
		}
	}
	if strings.Contains(strings.ToLower(e.Publisher), term) {
		return 2
	}
	if strings.Contains(strings.ToLower(e.Description), term) {
		return 1
	}
	for _, tag := range e.Tags {
		if strings.Contains(strings.ToLower(tag), term) {
			return 1
		}
	}
	return 0
}
