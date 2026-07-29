package clients

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"
)

// Detected is one configuration file found on this machine.
type Detected struct {
	Client      string    `json:"client"`
	Name        string    `json:"name"`
	Placement   Placement `json:"placement"`
	Shape       Shape     `json:"shape"`
	Path        string    `json:"path"`
	Section     []string  `json:"section,omitempty"`
	Writable    bool      `json:"writable"`
	Size        int64     `json:"size"`
	Modified    time.Time `json:"modified"`
	Note        string    `json:"note,omitempty"`
	Denied      bool      `json:"denied,omitempty"`
	Remediation string    `json:"remediation,omitempty"`

	// Err is non-nil only for a denied location and is always a
	// *PermissionError. It is not serialised; Denied and Remediation are
	// its JSON projection.
	Err *PermissionError `json:"-"`
}

// Detect enumerates every registered client's configuration locations and
// reports the ones that exist.
//
// STAT ONLY. This function never opens a file: on macOS,
// reading another application's data directory triggers a TCC privacy
// prompt, and a bulk scan that prompts a dozen times is worse than no scan
// at all. Content reads are reserved for Inspect and Import, which are
// per-client user actions.
//
// A location that does not exist is silently skipped — a machine with no
// AI clients installed yields an empty slice, not a list of errors. A
// location that exists but may not be stat'ed is reported with Denied set
// and a *PermissionError in Err: "you may not look" is a finding, "it is
// not there" is not.
//
// Results are ordered by client ID, then by the row's placement order.
func (t *Table) Detect(ctx context.Context, baseDir string) []Detected {
	base := t.baseDir(baseDir)
	ids := t.IDs()
	out := make([]Detected, 0, len(ids))
	for _, id := range ids {
		if ctx.Err() != nil {
			return out
		}
		spec := specByID[id]
		for _, loc := range spec.resolve(t, base) {
			if ctx.Err() != nil {
				return out
			}
			d, ok := t.statLocation(spec, loc)
			if ok {
				out = append(out, d)
			}
		}
	}
	return out
}

// statLocation stats one candidate. ok is false for "not present" and for
// any ambiguous failure — the failure direction is to stay quiet rather
// than to invent a client the user does not have.
func (t *Table) statLocation(spec *clientSpec, loc Location) (Detected, bool) {
	d := Detected{
		Client:    spec.id,
		Name:      spec.name,
		Placement: loc.Placement,
		Shape:     loc.Shape,
		Path:      loc.Path,
		Section:   loc.Section,
		Writable:  loc.Writable,
		Note:      spec.note,
	}
	info, err := os.Stat(loc.Path)
	switch {
	case err == nil:
		if info.IsDir() {
			return Detected{}, false
		}
		d.Size = info.Size()
		d.Modified = info.ModTime()
		return d, true
	case errors.Is(err, fs.ErrNotExist):
		return Detected{}, false
	}
	if pe := t.classifyAccess(err, loc.Path, spec.id, "stat"); pe != nil {
		d.Err = pe
		d.Denied = true
		d.Remediation = pe.Remediation
		return d, true
	}
	return Detected{}, false
}

// Inspection is the detail view of one client's configuration.
type Inspection struct {
	Client string          `json:"client"`
	Name   string          `json:"name"`
	Shape  Shape           `json:"shape"`
	Note   string          `json:"note,omitempty"`
	Files  []InspectedFile `json:"files"`
	Manual string          `json:"manual,omitempty"`

	// firstE is the first per-file failure; exposed through Err().
	firstE error
}

// InspectedFile is one configuration file's contents.
type InspectedFile struct {
	Path      string            `json:"path"`
	Placement Placement         `json:"placement"`
	Section   []string          `json:"section,omitempty"`
	Exists    bool              `json:"exists"`
	Parsed    bool              `json:"parsed"`
	Connected bool              `json:"connected"`
	Servers   []InspectedServer `json:"servers,omitempty"`
	Error     string            `json:"error,omitempty"`

	// Err carries the typed failure for this file (*PermissionError,
	// *ParseError, *TooLargeError). Callers use errors.As on it.
	Err error `json:"-"`
}

// InspectedServer summarises one entry in the client's server map.
type InspectedServer struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Command   string `json:"command,omitempty"`
	URL       string `json:"url,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	// Owned marks the agenthub gateway entry itself.
	Owned bool `json:"owned"`
}

// Inspect reads one client's configuration files and reports their server
// entries. Unlike Detect this OPENS the files — it is the deliberate user
// action for which a macOS privacy prompt is expected and explainable.
//
// The returned error is the first per-file failure (also available, per
// file, in InspectedFile.Err) so a caller can errors.As it into
// *PermissionError / *ParseError / *TooLargeError while still rendering
// whatever other files were readable.
func (t *Table) Inspect(clientID, baseDir string) (Inspection, error) {
	spec, ok := specByID[clientID]
	if !ok {
		return Inspection{}, &UnknownClientError{Client: clientID}
	}
	manualEntry := Entry{Command: "agenthub", Args: []string{"connect", "--client", clientID}}
	f, _ := t.Lookup(clientID)
	insp := Inspection{
		Client: spec.id,
		Name:   spec.name,
		Shape:  spec.shape(),
		Note:   spec.note,
		Manual: f.ManualSnippet(manualEntry),
	}

	jf, writable := f.(*jsonFormat)
	for _, loc := range spec.resolve(t, baseDir) {
		file := InspectedFile{Path: loc.Path, Placement: loc.Placement, Section: loc.Section}
		if !writable {
			// Probe-only: report existence, read nothing. The file's
			// syntax is not ours to interpret.
			if info, err := os.Stat(loc.Path); err == nil && !info.IsDir() {
				file.Exists = true
			} else if pe := t.classifyAccess(err, loc.Path, spec.id, "stat"); pe != nil {
				file.Exists = true
				file.Err, file.Error = pe, pe.Error()
			}
			insp.Files = append(insp.Files, file)
			insp.noteErr(file.Err)
			continue
		}
		cfg, err := jf.read(loc)
		if err != nil {
			file.Exists = true // read() only fails on a file that is there
			file.Err, file.Error = err, err.Error()
			insp.Files = append(insp.Files, file)
			insp.noteErr(err)
			continue
		}
		if !cfg.exists {
			insp.Files = append(insp.Files, file)
			continue
		}
		file.Exists, file.Parsed = true, true
		file.Servers = summarise(cfg.servers, spec.id)
		for _, s := range file.Servers {
			if s.Owned {
				file.Connected = true
			}
		}
		insp.Files = append(insp.Files, file)
	}
	return insp, insp.firstE
}

func (i *Inspection) noteErr(err error) {
	if err != nil && i.firstE == nil {
		i.firstE = err
	}
}

// Err returns the first per-file failure, or nil.
func (i Inspection) Err() error { return i.firstE }

// ConnectState is the answer to "is agenthub wired into this client?".
//
// The states beyond yes/no are the point of the type. Folding them into
// "not connected" would be a lie in the one direction that costs the user a
// wrong action: "you may not look", "this file is not mine to interpret"
// and "it is not there" call for three different fixes.
type ConnectState string

const (
	// ConnectedYes: some location carries an entry AGENTHUB ITSELF wrote
	// (InspectedServer.Owned — ownership, never a name match).
	ConnectedYes ConnectState = "connected"
	// ConnectedNo: every location was read, none carried our entry.
	ConnectedNo ConnectState = "not_connected"
	// ConnectedDenied: a location exists and may not be read.
	ConnectedDenied ConnectState = "denied"
	// ConnectedUnreadable: a location exists and agenthub refuses to
	// interpret it (unparseable, oversized).
	ConnectedUnreadable ConnectState = "unreadable"
	// ConnectedUnknown: a location exists and nothing was opened — a
	// probe-only shape (the TOML/YAML clients, whose syntax is not ours to
	// read).
	ConnectedUnknown ConnectState = "unknown"
)

// ConnectState reduces an inspection to one answer, plus the placements
// whose file carries the gateway entry.
//
// Precedence is deliberate. A positive finding wins outright: one readable
// location holding our entry settles the question however the other
// locations went. After that the LOUDEST doubt wins — denied over
// unreadable over unopened — so a location agenthub could not see never
// degrades into "not connected".
//
// Failure direction: fail-loud. Only a client whose every location was
// opened and understood can be reported as not connected.
func (i Inspection) ConnectState() (ConnectState, []Placement) {
	var where []Placement
	var denied, unreadable, unopened bool
	for _, f := range i.Files {
		switch {
		case f.Connected:
			where = append(where, f.Placement)
		case f.Err != nil:
			var perm *PermissionError
			if errors.As(f.Err, &perm) {
				denied = true
				continue
			}
			unreadable = true
		case f.Exists && !f.Parsed:
			// There, but read by nobody: a probe-only shape.
			unopened = true
		}
	}
	switch {
	case len(where) > 0:
		return ConnectedYes, where
	case denied:
		return ConnectedDenied, nil
	case unreadable:
		return ConnectedUnreadable, nil
	case unopened:
		return ConnectedUnknown, nil
	default:
		return ConnectedNo, nil
	}
}

// summarise renders a server map into a stable, sorted overview.
func summarise(servers map[string]json.RawMessage, clientID string) []InspectedServer {
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]InspectedServer, 0, len(names))
	for _, n := range names {
		raw := servers[n]
		fields := decodeFields(raw)
		out = append(out, InspectedServer{
			Name:      n,
			Transport: transportOf(fields),
			Command:   fields.str("command"),
			URL:       urlOf(fields),
			Disabled:  fields.boolean("disabled"),
			Owned:     ownedBy(raw, clientID),
		})
	}
	return out
}

// fields is a tolerant view of one server entry: every key stays raw so a
// value of an unexpected TYPE (an object where a string was expected)
// degrades that one field instead of failing the whole entry.
type fields map[string]json.RawMessage

func decodeFields(raw json.RawMessage) fields {
	var f fields
	if json.Unmarshal(raw, &f) != nil {
		return nil
	}
	return f
}

func (f fields) str(key string) string {
	raw, ok := f[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func (f fields) boolean(key string) bool {
	raw, ok := f[key]
	if !ok {
		return false
	}
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return false
	}
	return b
}

func (f fields) strings(key string) []string {
	raw, ok := f[key]
	if !ok {
		return nil
	}
	var s []string
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return s
}

func (f fields) strMap(key string) map[string]string {
	raw, ok := f[key]
	if !ok {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// urlOf accepts the three spellings clients use for a remote endpoint.
func urlOf(f fields) string {
	for _, k := range []string{"url", "serverUrl", "httpUrl"} {
		if v := f.str(k); v != "" {
			return v
		}
	}
	return ""
}

// transportOf normalises the transport marker. Clients spell it "type" or
// "transport", with several aliases for streamable HTTP. When absent it is
// inferred: a command means stdio, a URL means HTTP.
func transportOf(f fields) string {
	kind := strings.ToLower(strings.TrimSpace(f.str("type")))
	if kind == "" {
		kind = strings.ToLower(strings.TrimSpace(f.str("transport")))
	}
	switch kind {
	case "stdio", "local", "command":
		return "stdio"
	case "sse":
		return "sse"
	case "http", "streamable-http", "streamablehttp", "streamable_http", "remote":
		return "http"
	}
	switch {
	case f.str("command") != "":
		return "stdio"
	case urlOf(f) != "":
		return "http"
	default:
		return ""
	}
}
