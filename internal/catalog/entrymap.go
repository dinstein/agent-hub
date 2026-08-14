package catalog

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/dinstein/agent-hub/internal/clients"
	"github.com/dinstein/agent-hub/internal/registry"
)

// THE ONE READER OF A PASTED CLIENT CONFIGURATION.
//
// Two commands take the same snippet — the GUI's paste preview
// (ParseClientConfig, paste.go) and `agenthub server add --stdin`
// (internal/cli) — and everything about reading it lives here: which wrapper
// keys name a servers section, which shapes count as a document, which keys
// an entry may carry, and what each one means. The routes differ only in
// what they DO with the result.
//
// They used to differ in more than that. The transport spelling table was
// duplicated first and drifted (see TransportFromSpelling below); with that
// shared, the rest of the mapping drifted the same way — the CLI's struct
// decoder modelled no "disabled"/"enabled" key, so a Cline or Zed snippet was
// a hard error there and fine in the GUI, and it knew only the "mcpServers"
// wrapper, so a VS Code {"mcp":{"servers":…}} fragment failed with a
// per-entry complaint about a server named "mcp". A second implementation of
// a rule answers the same question differently the moment either side moves;
// the fix is to have one, not to keep two in step.
//
// The one difference that is DELIBERATE is the direction taken on a key
// agenthub does not model, and it is a parameter here rather than a fork:
// the preview WARNS (it shows the user exactly what would be stored, so
// "these keys were ignored" is actionable) and the write path REFUSES (a
// write with no preview would otherwise report success while the pasted
// "oauth" block is simply absent). See docs/subsystems/docs/subsystems/controlplane.md.
//
// TestPasteRoutesAgreeOnWholeSnippets (internal/cli) drives whole documents
// through both routes and compares the resulting entries, so a private copy
// of any of this fails there rather than in somebody's paste.

// UnknownFields is the direction a route takes when a pasted entry carries a
// key agenthub does not model, or a key whose value has the wrong JSON type.
//
// Both are the same question — "input we cannot store" — and both must
// travel: dropped in silence is the one behaviour neither route may have.
type UnknownFields int

const (
	// WarnOnUnknownFields collects them as notes for a preview to render.
	WarnOnUnknownFields UnknownFields = iota
	// RejectUnknownFields fails the entry, for a route that writes with no
	// preview in front of it.
	RejectUnknownFields
)

// EntryErrorKind classifies why an entry could not be mapped, so a caller can
// translate the failure into its own vocabulary (the CLI's error codes)
// without matching on message text.
type EntryErrorKind string

const (
	// EntryNotObject: the entry is not a JSON object at all.
	EntryNotObject EntryErrorKind = "not-object"
	// EntryUnmodeledFields: keys agenthub does not model, under
	// RejectUnknownFields.
	EntryUnmodeledFields EntryErrorKind = "unmodeled-fields"
	// EntryUnknownTransport: a transport marker was stated and is not one we
	// know. Distinct from EntryNoTransport on purpose — "stated as nonsense"
	// must not be inferred from the entry's shape.
	EntryUnknownTransport EntryErrorKind = "unknown-transport"
	// EntryNoTransport: neither a marker, nor a command, nor a url.
	EntryNoTransport EntryErrorKind = "no-transport"
	// EntryMissingCommand: an stdio entry with no command.
	EntryMissingCommand EntryErrorKind = "missing-command"
	// EntryMissingURL: a remote entry with no url.
	EntryMissingURL EntryErrorKind = "missing-url"
)

// EntryError is a refusal to map one entry.
type EntryError struct {
	Kind    EntryErrorKind
	Message string
}

func (e *EntryError) Error() string { return e.Message }

func entryErrf(kind EntryErrorKind, format string, a ...any) *EntryError {
	return &EntryError{Kind: kind, Message: fmt.Sprintf(format, a...)}
}

// entryKeys are the keys the mapping consumes. Anything else is reported —
// warned about or refused, per the caller's UnknownFields.
var entryKeys = map[string]struct{}{
	"type": {}, "transport": {}, "command": {}, "args": {}, "env": {}, "cwd": {},
	"url": {}, "serverUrl": {}, "httpUrl": {}, "headers": {}, "oauth": {},
	"disabled": {}, "enabled": {},
}

// MapEntry converts one entry object out of a client configuration into a
// registry definition, and reports the input it could not use.
//
// What it does NOT decide, because these are the caller's policy and not the
// snippet's meaning: Source (who is adding this), Provenance, and whether the
// server goes into service — Enabled is filled in with what the SNIPPET says
// (see the switch below), which a caller is free to override. `server add`
// does exactly that: it lands every server switched off whatever the config
// it came from claims, and says so in its output.
//
// Failure direction: an entry naming neither a command nor a url is REFUSED,
// never defaulted into a half-formed server that fails much later at connect
// time with an unrelated-looking message.
func MapEntry(raw json.RawMessage, unknown UnknownFields) (registry.ServerEntry, []string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return registry.ServerEntry{}, nil, entryErrf(EntryNotObject, "entry is not a JSON object")
	}
	f := newFields(obj)
	var notes []string

	entry := registry.ServerEntry{Enabled: !f.boolean("disabled")}
	if _, ok := obj["enabled"]; ok {
		// Zed spells the switch positively. Both spellings present and
		// disagreeing lands on DISABLED: the entry arrives switched off,
		// which is the reversible direction.
		entry.Enabled = entry.Enabled && f.boolean("enabled")
	}
	switch kind := f.transport(); kind {
	case registry.TransportStdio:
		entry.Transport = registry.TransportStdio
		entry.Command = f.str("command")
		if entry.Command == "" {
			return registry.ServerEntry{}, nil, entryErrf(EntryMissingCommand, "stdio entry has no command")
		}
		entry.Args = f.strings("args")
		entry.Env = f.strMap("env")
		entry.Cwd = f.str("cwd")
	case registry.TransportHTTP, registry.TransportSSE:
		entry.Transport = kind
		entry.URL = f.url()
		if entry.URL == "" {
			return registry.ServerEntry{}, nil, entryErrf(EntryMissingURL, "%s entry has no url", kind)
		}
		entry.Headers = f.strMap("headers")
	default:
		if marker := f.marker(); marker != "" {
			return registry.ServerEntry{}, nil,
				entryErrf(EntryUnknownTransport, "entry names an unknown transport %q", marker)
		}
		return registry.ServerEntry{}, nil,
			entryErrf(EntryNoTransport, "entry names neither a command nor a url")
	}
	if hint := f.oauthHint(); hint != nil {
		entry.OAuth = hint
	}

	// Reported last, and after the shape checks, so an entry that is not a
	// server at all is refused as that rather than as a list of odd keys.
	if unusable := f.unusable(); len(unusable) > 0 {
		joined := strings.Join(unusable, ", ")
		if unknown == RejectUnknownFields {
			return registry.ServerEntry{}, nil, entryErrf(EntryUnmodeledFields,
				"fields agenthub does not model or could not read: %s", joined)
		}
		notes = append(notes, "ignored fields agenthub does not model or could not read: "+joined)
	}
	return entry, notes, nil
}

// Recognize decides which document shape a parsed top-level object is, and
// returns the entries it holds keyed by name.
//
// A single unnamed entry comes back under the empty name: it is the caller
// that decides what that means — the preview warns that a name is still
// needed, `server add --stdin` requires the name argument.
func Recognize(top map[string]json.RawMessage) (Shape, []string, map[string]json.RawMessage, bool) {
	if section, servers, ok := findSection(top); ok {
		return ShapeWrapped, section, servers, true
	}
	if isEntryObject(top) {
		raw, err := json.Marshal(top)
		if err != nil { // unreachable: top came from a decode
			return "", nil, nil, false
		}
		return ShapeSingleEntry, nil, map[string]json.RawMessage{"": raw}, true
	}
	if m, ok := asEntryMap(top); ok {
		return ShapeEntryMap, nil, m, true
	}
	return "", nil, nil, false
}

// sectionCandidates returns the wrapper key paths to look for, longest
// first so a nested path ("mcp.servers") is tried before a shallow one
// ("servers") that could also match a different document.
//
// The list comes from the internal/clients table rather than a literal here:
// the key paths are that table's knowledge, and duplicating them would make
// a new client row silently unparseable.
func sectionCandidates() [][]string {
	seen := map[string]struct{}{}
	var out [][]string
	for _, f := range clients.Formats() {
		for _, loc := range f.Locations("") {
			if len(loc.Section) == 0 {
				continue // TOML / YAML / remote: no JSON key path
			}
			key := strings.Join(loc.Section, ".")
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, loc.Section)
		}
	}
	slices.SortStableFunc(out, func(a, b []string) int {
		if c := cmp.Compare(len(b), len(a)); c != 0 { // longer first (descending)
			return c
		}
		return strings.Compare(strings.Join(a, "."), strings.Join(b, "."))
	})
	return out
}

// SectionNames renders the candidate wrapper paths, for an error hint that
// tells the user what this parser would have recognized.
func SectionNames() []string {
	cands := sectionCandidates()
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, strings.Join(c, "."))
	}
	return out
}

// findSection walks each candidate key path and returns the first one that
// resolves to a JSON object of entries.
func findSection(top map[string]json.RawMessage) ([]string, map[string]json.RawMessage, bool) {
	for _, path := range sectionCandidates() {
		cur := top
		var servers map[string]json.RawMessage
		ok := true
		for i, key := range path {
			raw, present := cur[key]
			if !present {
				ok = false
				break
			}
			var next map[string]json.RawMessage
			if json.Unmarshal(raw, &next) != nil {
				ok = false
				break
			}
			if i == len(path)-1 {
				servers = next
			} else {
				cur = next
			}
		}
		// A section that is ITSELF one entry is not a section: a bare map
		// whose single server happens to be called "servers" would otherwise
		// be read as a VS Code wrapper and lose its name.
		if ok && servers != nil && !isEntryObject(servers) {
			return path, servers, true
		}
	}
	return nil, nil, false
}

// isEntryObject reports whether an object is ITSELF one server entry rather
// than a map of them: it names a command or a url directly.
func isEntryObject(obj map[string]json.RawMessage) bool {
	f := newFields(obj)
	return f.str("command") != "" || f.url() != ""
}

// asEntryMap accepts a bare name -> entry map. Every value must be a JSON
// object; one that is not means the document is something else entirely,
// and guessing would produce entries out of a settings file.
func asEntryMap(top map[string]json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(top) == 0 {
		return nil, false
	}
	for _, raw := range top {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return nil, false
		}
		if !isEntryObject(obj) {
			return nil, false
		}
	}
	return top, true
}

// fields is a tolerant view of one pasted entry: every key stays raw, so a
// value of an unexpected TYPE degrades that one field instead of failing the
// whole entry — but the degradation is RECORDED, in malformed, because a
// dropped field must reach the user either as a warning or as a refusal.
// Same discipline (and same accessor names) as internal/clients, which reads
// the identical shapes off disk.
type fields struct {
	raw       map[string]json.RawMessage
	malformed map[string]struct{}
}

func newFields(obj map[string]json.RawMessage) *fields {
	return &fields{raw: obj, malformed: map[string]struct{}{}}
}

// decode reads one key into v, recording the key when its value has a type
// this parser cannot use. A missing key is not a malformed one.
func (f *fields) decode(key string, v any) bool {
	raw, ok := f.raw[key]
	if !ok {
		return false
	}
	if json.Unmarshal(raw, v) != nil {
		f.malformed[key] = struct{}{}
		return false
	}
	return true
}

func (f *fields) str(key string) string {
	var s string
	f.decode(key, &s)
	return s
}

func (f *fields) boolean(key string) bool {
	var b bool
	f.decode(key, &b)
	return b
}

func (f *fields) strings(key string) []string {
	var s []string
	if !f.decode(key, &s) {
		return nil
	}
	return s
}

func (f *fields) strMap(key string) map[string]string {
	var m map[string]string
	if !f.decode(key, &m) || len(m) == 0 {
		return nil
	}
	return m
}

// url accepts the three spellings clients use for a remote endpoint.
func (f *fields) url() string {
	for _, k := range []string{"url", "serverUrl", "httpUrl"} {
		if v := f.str(k); v != "" {
			return v
		}
	}
	return ""
}

// TransportFromSpelling maps one written transport marker onto a registry
// transport, folding the spellings client configs use in the wild. The marker
// is trimmed and case-folded.
//
// An unrecognized marker yields "", and so does the empty string. The caller
// decides whether "absent" may be inferred from the entry's shape, because
// "not stated" and "stated as nonsense" must not lead to the same place: a
// typo'd "htpp" falling back to stdio would silently downgrade a remote server
// into launching a local process.
//
// It exists so that "how is a transport spelled" has ONE answer. Two paste
// paths read the same kind of snippet — `agenthub catalog` and
// `server add --stdin` — and they disagreed: this table was here, while the CLI
// carried a narrower, case-SENSITIVE copy recognizing only streamable-http,
// streamableHttp and http-stream. Configs the catalog accepted were refused by
// `server add --stdin` for no reason a user could infer, which is what a second
// implementation of a rule eventually always does (compare
// downstream.SecretKeysIn, written against the same failure). The rest of this
// file is the same lesson applied to the rest of the mapping.
func TransportFromSpelling(marker string) string {
	switch strings.ToLower(strings.TrimSpace(marker)) {
	case "stdio", "local", "command":
		return registry.TransportStdio
	case "sse":
		return registry.TransportSSE
	case "http", "streamable-http", "streamablehttp", "streamable_http", "http-stream", "remote":
		return registry.TransportHTTP
	}
	return ""
}

// marker is the transport as the entry spells it, "" when it states none.
func (f *fields) marker() string {
	if m := strings.TrimSpace(f.str("type")); m != "" {
		return m
	}
	return strings.TrimSpace(f.str("transport"))
}

// transport normalises the transport marker, inferring it from the entry's
// own shape when absent. An unrecognized explicit marker yields "" (which
// the caller refuses) rather than falling back to inference: a typo'd
// "htpp" must not silently become stdio.
func (f *fields) transport() string {
	if marker := f.marker(); marker != "" {
		return TransportFromSpelling(marker)
	}
	switch {
	case f.str("command") != "":
		return registry.TransportStdio
	case f.url() != "":
		return registry.TransportHTTP
	}
	return ""
}

// oauthHint decodes the optional login hints. A malformed block yields nil
// rather than an error: the hints are an optimization over RFC 9728
// discovery, and the block is reported as unusable either way.
func (f *fields) oauthHint() *registry.OAuthHint {
	var hint registry.OAuthHint
	if !f.decode("oauth", &hint) {
		return nil
	}
	if hint.Issuer == "" && hint.ResourceMetadataURL == "" && len(hint.Scopes) == 0 {
		return nil
	}
	return &hint
}

// unusable lists the entry's keys this parser could not consume, sorted:
// keys it does not model at all, and keys whose value had a type it cannot
// read. The two are one list because they have one consequence — the value
// the user pasted is not in the entry.
func (f *fields) unusable() []string {
	var out []string
	for k := range f.raw {
		if _, known := entryKeys[k]; !known {
			out = append(out, k)
		}
	}
	for k := range f.malformed {
		if _, dup := entryKeys[k]; dup {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
