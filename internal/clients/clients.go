// Package clients adapts AI client configuration formats (canonical.md §2:
// "20+ client config adapters (Format-driven)"; docs/canonical.md §2 clients.rs row).
//
// # Shape, not vendor
//
// The adapter table is driven by configuration SHAPE, not by a hand-written
// branch per product. Four shapes cover the ecosystem:
//
//   - ShapeServerMap  — a JSON file whose {"mcpServers": {...}} object maps
//     server name to entry (claude-code, claude-desktop, cursor, windsurf,
//     cline, roo-code, gemini-cli).
//   - ShapeNested     — the same name→entry map, reached through a key path
//     inside a larger JSON document (vscode: "mcp"."servers"; zed:
//     "context_servers"; vscode workspace: "servers").
//   - ShapeTOML / ShapeYAML — non-JSON documents (codex, continue). Probed
//     only: agenthub reports that they exist and hands back a manual
//     snippet. It never rewrites a format it cannot round-trip losslessly.
//   - ShapeRemote     — no local file at all (open-webui talks MCP over
//     HTTP). Nothing to detect, nothing to write.
//
// Adding a client is one row in the table (table.go), not a new code path.
//
// # Frozen invariants
//
// Every write path upholds these; they are the whole reason this package
// exists rather than a fmt.Fprintf of a JSON blob:
//
//   - Unknown fields and foreign entries are preserved verbatim (raw
//     passthrough via json.RawMessage at every level of the key path).
//   - An existing file that fails to parse aborts the operation with
//     *ParseError. agenthub never overwrites configuration it cannot read
//     — the whole-app-state rule (docs/canonical.md §2): parse failure must
//     error, not destroy. JSONC (comments) counts as unparseable, and the
//     error carries a manual snippet so the user is not stranded.
//   - Files larger than MaxConfigSize (64 MiB) are refused with
//     *TooLargeError, before any read: a client config that big is a
//     runaway log, not configuration.
//   - Writes are preceded by a CENTRAL backup under
//     <data>/backups/clients/<client>-<ts>Z.json (0600, rotated), never a
//     sidecar next to the original — a project-level .mcp.json is a
//     committed file and must not sprout untracked debris.
//   - Writes are atomic (same-directory temp file + fsync + rename).
//   - Disconnect removes only entries agenthub itself wrote, identified by
//     ownership (args contain "connect" and a matching --client value),
//     never by entry name alone.
//
// # macOS TCC
//
// Reading another application's configuration file can trigger a system
// privacy prompt. Detect therefore only ever calls os.Stat on other
// clients' files — never os.ReadFile. Content reads happen exclusively in
// Inspect and Import, which are single-client user actions where a prompt
// is expected and explainable. A denied stat/read is classified as
// *PermissionError carrying remediation text (ctlapi maps it to 403); it
// is never folded into "not found", because "the file is not there" and
// "you may not look at the file" call for opposite user actions.
package clients

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

// entryName is the server entry name agenthub writes into the server map.
const entryName = "agenthub"

// Entry is the gateway server entry a Format writes into a client
// configuration: it makes the client spawn the agenthub binary as its MCP
// server via `agenthub connect --client <id>`.
type Entry struct {
	Command string
	Args    []string
}

// Shape classifies a client configuration by form. See the package doc.
type Shape string

// The four configuration shapes.
const (
	ShapeServerMap Shape = "mcpServers-map"
	ShapeNested    Shape = "nested-json"
	ShapeTOML      Shape = "toml"
	ShapeYAML      Shape = "yaml"
	ShapeRemote    Shape = "remote"
)

// Writable reports whether agenthub can rewrite this shape. Only the two
// JSON shapes round-trip losslessly; the rest are probe-only.
func (s Shape) Writable() bool { return s == ShapeServerMap || s == ShapeNested }

// Placement is where a configuration file lives. It is about file location
// only and has nothing to do with internal/scope's four-layer chain.
type Placement string

// The two placements.
const (
	// Project files live inside the working tree and are usually committed.
	Project Placement = "project"
	// User files live under the user's home directory.
	User Placement = "user"
)

// DefaultPlacement is where a connect writes when the caller names neither a
// path nor a placement: the user's home directory.
//
// The entry agenthub writes carries the ABSOLUTE path of this machine's
// agenthub binary, and a project-level file (.mcp.json, .cursor/mcp.json) is
// meant to be committed and shared — so a project-level default commits a
// machine-specific path to everyone else on the team. A user-level default
// also matches what agenthub is: one hub every client on this machine shares,
// not something re-wired per repository. Which servers a client may see is
// decided by internal/scope, never by which file the entry sits in.
//
// Rows with no user location on this platform (or with $HOME unresolvable)
// fall back to their first location rather than refusing — see
// clientSpec.defaultTarget.
const DefaultPlacement = User

// Location is a resolved configuration file candidate for one client.
type Location struct {
	Client    string    `json:"client"`
	Placement Placement `json:"placement"`
	Shape     Shape     `json:"shape"`
	// Path is the absolute (or baseDir-relative-resolved) file path.
	Path string `json:"path"`
	// Section is the JSON key path from the document root down to the
	// name→entry map (["mcpServers"], ["mcp","servers"], ...). Empty for
	// non-JSON shapes.
	Section []string `json:"section,omitempty"`
	// Writable mirrors Shape.Writable for convenience in JSON output.
	Writable bool `json:"writable"`
}

// Result reports what a Connect or Disconnect actually did.
type Result struct {
	// Path is the configuration file operated on.
	Path string
	// Backup is the central backup file written before modifying an
	// existing file ("" when the target did not exist or nothing was
	// written).
	Backup string
	// Changed is false when the file already had exactly the desired
	// content and nothing was written (idempotent re-connect).
	Changed bool
	// Removed lists the entry names Disconnect deleted (nil for Connect).
	Removed []string
}

// Format adapts one AI client's configuration format.
type Format interface {
	// ID is the client identifier (`client connect <id>`).
	ID() string
	// DisplayName is the human-facing product name.
	DisplayName() string
	// Shape classifies the configuration form.
	Shape() Shape
	// Writable reports whether Connect/Disconnect can rewrite the file.
	// When false both return *UnsupportedError carrying a manual snippet.
	Writable() bool
	// Locations lists every configuration file this client may use,
	// project placements first, resolved against baseDir and $HOME.
	//
	// This order is READ precedence (Import resolves a duplicate name in
	// favour of the project-level definition), not write preference —
	// DefaultPath decides where a connect goes.
	Locations(baseDir string) []Location
	// DefaultPath is the file targeted when the caller names neither a
	// path nor a placement: the DefaultPlacement location, or the first
	// location when this client has none on this platform.
	DefaultPath(baseDir string) string
	// PathFor is the file for one explicit placement, or "" when this
	// client has no such location on this platform. A caller that asked
	// for a placement must surface that "" as a refusal: writing to the
	// other placement instead would put the entry somewhere nobody named.
	PathFor(baseDir string, p Placement) string
	// Connect merges entry into the configuration at path (creating the
	// file if absent) under the invariants documented on the package.
	Connect(path string, entry Entry) (Result, error)
	// Disconnect removes the entries agenthub owns from path. It returns
	// *NotConnectedError when the file is missing or holds no owned entry.
	Disconnect(path string) (Result, error)
	// ManualSnippet renders the configuration fragment a user must paste
	// by hand. Defined for every shape: it is the only remedy for
	// probe-only shapes and the fallback hint when a JSON file is
	// refused (JSONC, oversized, unparseable).
	ManualSnippet(entry Entry) string
}

// Options configures a Table. The zero value uses the real process
// environment and is what the package-level functions use.
type Options struct {
	// GOOS overrides runtime.GOOS (path table selection).
	GOOS string
	// Home overrides the user home directory.
	Home string
	// BackupDir overrides <data>/backups/clients.
	BackupDir string
	// KeepBackups is the per-client backup retention count
	// (<= 0 means DefaultKeepBackups).
	KeepBackups int
}

// Table is the adapter table bound to one environment. Tables are
// stateless beyond their Options and safe for concurrent use.
type Table struct {
	opts Options
}

// New returns a Table using opts.
func New(opts Options) *Table { return &Table{opts: opts} }

// Default returns a Table bound to the real process environment.
func Default() *Table { return &Table{} }

func (t *Table) goos() string {
	if t.opts.GOOS != "" {
		return t.opts.GOOS
	}
	return runtime.GOOS
}

// home resolves the user home directory. Failure is not fatal: it only
// makes user-placement locations unavailable, so a machine without a
// resolvable HOME still gets project-level adaptation.
func (t *Table) home() (string, bool) {
	if t.opts.Home != "" {
		return t.opts.Home, true
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return "", false
	}
	return h, true
}

func (t *Table) baseDir(baseDir string) string {
	if baseDir != "" {
		return baseDir
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

// Lookup returns the Format for a client ID.
func (t *Table) Lookup(id string) (Format, bool) {
	spec, ok := specByID[id]
	if !ok {
		return nil, false
	}
	if spec.shape().Writable() {
		return &jsonFormat{tbl: t, spec: spec}, true
	}
	return &probeFormat{tbl: t, spec: spec}, true
}

// IDs lists the supported client IDs, sorted.
func (t *Table) IDs() []string {
	ids := make([]string, 0, len(specs))
	for i := range specs {
		ids = append(ids, specs[i].id)
	}
	sort.Strings(ids)
	return ids
}

// Formats returns every registered Format, ordered by ID.
func (t *Table) Formats() []Format {
	out := make([]Format, 0, len(specs))
	for _, id := range t.IDs() {
		if f, ok := t.Lookup(id); ok {
			out = append(out, f)
		}
	}
	return out
}

// Package-level convenience wrappers over Default(). Prefer a *Table in
// code that needs a controlled environment.

// Lookup returns the Format for a client ID using the process environment.
func Lookup(id string) (Format, bool) { return Default().Lookup(id) }

// IDs lists the supported client IDs, sorted.
func IDs() []string { return Default().IDs() }

// Formats returns every registered Format, ordered by ID.
func Formats() []Format { return Default().Formats() }

// Detect enumerates installed clients' existing configuration files using
// the process environment and working directory. It only stats; see the
// package doc on macOS TCC.
func Detect(ctx context.Context) []Detected { return Default().Detect(ctx, "") }

// Inspect reads one client's configuration files. This is a user action:
// unlike Detect it opens the files.
func Inspect(clientID string) (Inspection, error) { return Default().Inspect(clientID, "") }

// Import converts a client's existing server entries into registry
// entries. existing names are reported as conflicts, never overwritten.
func Import(clientID string, existing []string) (ImportResult, error) {
	return Default().Import(clientID, "", existing)
}

// DisconnectDefault removes agenthub's entry from the client's default write
// target and, ONLY when that file holds nothing agenthub owns, from whichever
// other location of the same client does.
//
// The fallback exists because the default write target moved: an entry
// written when connect defaulted to the project level still sits in
// .mcp.json, and a disconnect that reported "not connected" while the entry
// was plainly still there would be the worst possible answer. It is not a
// search — it only ever visits this one client's own locations, and only
// after the default target came back empty.
//
// Callers that named a path or a placement must NOT use this: an explicit
// target is an instruction, not a starting point.
//
// Failure direction: a fallback location that fails for any reason other
// than "nothing of ours here" (unparseable, oversized, denied) returns that
// error rather than being skipped — a file agenthub refused to touch must not
// be reported as a file with nothing in it.
func DisconnectDefault(f Format, baseDir string) (Result, error) {
	target := f.DefaultPath(baseDir)
	res, err := f.Disconnect(target)
	var notConnected *NotConnectedError
	if err == nil || !errors.As(err, &notConnected) {
		return res, err
	}
	for _, loc := range f.Locations(baseDir) {
		if loc.Path == target {
			continue
		}
		alt, altErr := f.Disconnect(loc.Path)
		switch {
		case altErr == nil:
			return alt, nil
		case errors.As(altErr, &notConnected):
			continue
		default:
			return Result{}, altErr
		}
	}
	return res, err
}

// BackupDir returns the central client-config backup directory under
// dataDir. Exported so callers (CLI, doctor, tests) can locate backups
// without duplicating the layout.
func BackupDir(dataDir string) string { return filepath.Join(dataDir, "backups", "clients") }
