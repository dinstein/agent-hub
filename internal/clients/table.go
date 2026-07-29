package clients

import (
	"fmt"
	"path/filepath"
	"strings"
)

// clientSpec is one row of the adapter table: an identity plus an ordered
// list of candidate configuration files. Behaviour is derived from the
// rows, not written per client — see the package doc.
type clientSpec struct {
	id   string
	name string
	// locs are candidate files, PROJECT PLACEMENTS FIRST. That order is
	// read precedence (a duplicated name is reported project-level first);
	// the default WRITE target is chosen by placement,
	// see defaultTarget.
	locs []locSpec
	// nonJSON marks a probe-only shape (ShapeTOML / ShapeYAML) or the
	// fileless ShapeRemote. Empty means the shape is derived from the
	// first location's section.
	nonJSON Shape
	// readTable names the top-level TOML table holding this client's
	// servers, for the probe-only shapes agenthub can READ without being
	// able to rewrite them. Empty means detection only: Inspect reports
	// that the file exists and nothing about its contents.
	//
	// Reading is not a step towards writing. Connect still refuses — the
	// rule it enforces is about re-encoding a document, and answering
	// "is our entry in here?" re-encodes nothing.
	readTable string
	// note is user-facing text explaining a probe-only or remote client.
	note string
	// manual renders the fragment a user pastes by hand. nil means "use
	// the generic JSON snippet".
	manual func(spec *clientSpec, e Entry) string
	// removal renders how to take the entry back OUT, named for the entry
	// actually found in the file. A disconnect that answered with the add
	// snippet would read like an instruction and be the wrong one.
	removal func(spec *clientSpec, name string) string
}

// locSpec describes where one configuration file lives and where inside it
// the name→entry map sits.
type locSpec struct {
	placement Placement
	// section is the JSON key path from the document root to the
	// name→entry map. nil for non-JSON shapes.
	section []string
	// rel is the path relative to the project base directory
	// (placement == Project).
	rel []string
	// home maps GOOS to a path relative to $HOME (placement == User).
	// A GOOS absent from the map makes the location unavailable there —
	// that is how Windows stays deferred to M2 without a build tag.
	home map[string][]string
}

// mcpServers is the de-facto standard section: {"mcpServers": {...}}.
var mcpServers = []string{"mcpServers"}

// sameOnAll returns a home path used identically on macOS and Linux.
func sameOnAll(seg ...string) map[string][]string {
	return map[string][]string{"darwin": seg, "linux": seg}
}

// perOS returns per-platform home paths.
func perOS(darwin, linux []string) map[string][]string {
	return map[string][]string{"darwin": darwin, "linux": linux}
}

// vscodeUserDir is the VS Code user-settings directory, which several
// extension-hosted clients also nest their settings under.
func vscodeUserDir(tail ...string) map[string][]string {
	darwin := append([]string{"Library", "Application Support", "Code", "User"}, tail...)
	linux := append([]string{".config", "Code", "User"}, tail...)
	return perOS(darwin, linux)
}

// specs is THE adapter table. Every supported client is one row; the code
// paths below are shape-generic.
//
// Placement order inside a row matters: project files come first because a
// project-scoped connection is the least surprising default (it binds the
// gateway to the tree the user is standing in, and it is reviewable in a
// diff).
var specs = []clientSpec{
	{
		id:   "claude-code",
		name: "Claude Code",
		locs: []locSpec{
			{placement: Project, rel: []string{".mcp.json"}, section: mcpServers},
			{placement: User, home: sameOnAll(".claude.json"), section: mcpServers},
		},
	},
	{
		id:   "claude-desktop",
		name: "Claude Desktop",
		locs: []locSpec{
			{placement: User, section: mcpServers, home: perOS(
				[]string{"Library", "Application Support", "Claude", "claude_desktop_config.json"},
				[]string{".config", "Claude", "claude_desktop_config.json"},
			)},
		},
	},
	{
		id:   "cursor",
		name: "Cursor",
		locs: []locSpec{
			{placement: Project, rel: []string{".cursor", "mcp.json"}, section: mcpServers},
			{placement: User, home: sameOnAll(".cursor", "mcp.json"), section: mcpServers},
		},
	},
	{
		id:   "windsurf",
		name: "Windsurf",
		locs: []locSpec{
			{placement: User, home: sameOnAll(".codeium", "windsurf", "mcp_config.json"), section: mcpServers},
		},
	},
	{
		id:   "vscode",
		name: "Visual Studio Code",
		locs: []locSpec{
			// Workspace file: plain JSON, top-level "servers".
			{placement: Project, rel: []string{".vscode", "mcp.json"}, section: []string{"servers"}},
			// User settings: the "mcp" section of a much larger document.
			// Often JSONC in practice — see jsonfile.go, which refuses
			// rather than destroys comments.
			{placement: User, section: []string{"mcp", "servers"}, home: vscodeUserDir("settings.json")},
		},
	},
	{
		id:   "cline",
		name: "Cline",
		locs: []locSpec{
			{placement: User, section: mcpServers, home: vscodeUserDir(
				"globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")},
		},
	},
	{
		id:   "roo-code",
		name: "Roo Code",
		locs: []locSpec{
			{placement: Project, rel: []string{".roo", "mcp.json"}, section: mcpServers},
			{placement: User, section: mcpServers, home: vscodeUserDir(
				"globalStorage", "rooveterinaryinc.roo-cline", "settings", "mcp_settings.json")},
		},
	},
	{
		id:   "zed",
		name: "Zed",
		locs: []locSpec{
			// Zed keeps MCP servers under "context_servers" in its own
			// settings document — same map, different key path.
			{placement: User, section: []string{"context_servers"}, home: sameOnAll(".config", "zed", "settings.json")},
		},
	},
	{
		id:   "gemini-cli",
		name: "Gemini CLI",
		locs: []locSpec{
			{placement: Project, rel: []string{".gemini", "settings.json"}, section: mcpServers},
			{placement: User, home: sameOnAll(".gemini", "settings.json"), section: mcpServers},
		},
	},
	{
		id:      "codex",
		name:    "OpenAI Codex CLI",
		nonJSON: ShapeTOML,
		locs: []locSpec{
			{placement: User, home: sameOnAll(".codex", "config.toml")},
		},
		readTable: "mcp_servers",
		note: "Codex stores MCP servers in TOML. agenthub reads that file but will not " +
			"rewrite it: re-encoding would cost you its comments and layout. Codex's own " +
			"CLI edits it correctly — or edit it by hand.",
		manual:  tomlSnippet,
		removal: tomlRemoval,
	},
	{
		id:      "continue",
		name:    "Continue",
		nonJSON: ShapeYAML,
		locs: []locSpec{
			{placement: Project, rel: []string{".continue", "config.yaml"}},
			{placement: User, home: sameOnAll(".continue", "config.yaml")},
		},
		note: "Continue stores MCP servers in YAML. agenthub detects the file and reads nothing else; " +
			"edit it by hand.",
		manual:  yamlSnippet,
		removal: yamlRemoval,
	},
	{
		id:      "open-webui",
		name:    "Open WebUI",
		nonJSON: ShapeRemote,
		note: "Open WebUI consumes MCP over HTTP and has no local configuration file on this machine. " +
			"Run the agenthub daemon and register its streamable-http endpoint in Open WebUI.",
		manual: remoteSnippet,
	},
}

// specByID indexes specs. Built once; a duplicate ID is a programming
// error and panics at init rather than silently shadowing a row.
var specByID = func() map[string]*clientSpec {
	m := make(map[string]*clientSpec, len(specs))
	for i := range specs {
		if _, dup := m[specs[i].id]; dup {
			panic("clients: duplicate client id " + specs[i].id)
		}
		m[specs[i].id] = &specs[i]
	}
	return m
}()

// shape derives the shape from the row: an explicit non-JSON marker wins,
// otherwise the section key path decides map vs nested.
func (s *clientSpec) shape() Shape {
	if s.nonJSON != "" {
		return s.nonJSON
	}
	if len(s.locs) > 0 && len(s.locs[0].section) == 1 && s.locs[0].section[0] == "mcpServers" {
		return ShapeServerMap
	}
	return ShapeNested
}

// resolve turns the spec's locations into concrete paths. User placements
// whose GOOS is absent from the table (Windows, M2) and, when $HOME is not
// resolvable, all user placements, are dropped — the caller still gets the
// project ones.
func (s *clientSpec) resolve(t *Table, baseDir string) []Location {
	shape := s.shape()
	out := make([]Location, 0, len(s.locs))
	for _, l := range s.locs {
		var path string
		switch l.placement {
		case Project:
			path = filepath.Join(append([]string{t.baseDir(baseDir)}, l.rel...)...)
		case User:
			seg, ok := l.home[t.goos()]
			if !ok {
				continue
			}
			home, ok := t.home()
			if !ok {
				continue
			}
			path = filepath.Join(append([]string{home}, seg...)...)
		default:
			continue
		}
		out = append(out, Location{
			Client:    s.id,
			Placement: l.placement,
			Shape:     shape,
			Path:      path,
			Section:   l.section,
			Writable:  shape.Writable(),
		})
	}
	return out
}

// defaultTarget is the file a write goes to when the caller named neither a
// path nor a placement: this row's DefaultPlacement location.
//
// Failure direction: a row without that placement on this platform (or with
// $HOME unresolvable) falls back to its first resolved location rather than
// returning "". "Nowhere to write" is the wrong answer for a client that
// plainly has a configuration file; the caller who wants a hard refusal asks
// for a placement explicitly, via pathFor.
func (s *clientSpec) defaultTarget(t *Table, baseDir string) string {
	locs := s.resolve(t, baseDir)
	if len(locs) == 0 {
		return ""
	}
	for _, l := range locs {
		if l.Placement == DefaultPlacement {
			return l.Path
		}
	}
	return locs[0].Path
}

// pathFor is the file for one explicit placement, or "" when this row has
// none on this platform. Unlike defaultTarget it never substitutes the other
// placement: the caller named one, so silently writing to the other would put
// the entry in a file nobody asked for.
func (s *clientSpec) pathFor(t *Table, baseDir string, p Placement) string {
	for _, l := range s.resolve(t, baseDir) {
		if l.Placement == p {
			return l.Path
		}
	}
	return ""
}

// locationFor picks the Location a raw --path argument refers to. The
// section a path is written with must be deterministic, so the match is:
//
//  1. exact path equality with a resolved location;
//  2. same file base name as a resolved location (this is what makes
//     `--path /tmp/x/settings.json` behave like the real settings.json
//     instead of silently picking a different section);
//  3. otherwise the primary location.
//
// Failure direction: an unmatched path never guesses a *different* client's
// shape — it falls back to this client's own primary section.
func (s *clientSpec) locationFor(t *Table, baseDir, path string) Location {
	locs := s.resolve(t, baseDir)
	if len(locs) == 0 {
		return Location{Client: s.id, Shape: s.shape(), Path: path, Writable: s.shape().Writable()}
	}
	for _, l := range locs {
		if l.Path == path {
			return l
		}
	}
	base := filepath.Base(path)
	for _, l := range locs {
		if filepath.Base(l.Path) == base {
			l.Path = path
			return l
		}
	}
	primary := locs[0]
	primary.Path = path
	return primary
}

// --- manual snippets -------------------------------------------------

// tomlSnippet renders the codex path: the client's own CLI first, then the
// fragment to paste.
//
// The CLI line leads because it is the one that cannot go wrong — codex
// writes its own TOML, so nobody has to hand-place a table in a file that
// already has 200 lines of them. The fragment stays for the case where
// codex is not on PATH, or the user wants to see exactly what will land.
func tomlSnippet(s *clientSpec, e Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "codex mcp add %s -- %s %s\n", entryName, e.Command, strings.Join(e.Args, " "))
	b.WriteString("\n# or, by hand:\n")
	fmt.Fprintf(&b, "[mcp_servers.%s]\n", entryName)
	fmt.Fprintf(&b, "command = %q\n", e.Command)
	fmt.Fprintf(&b, "args = [%s]\n", quoteJoin(e.Args))
	return b.String()
}

// yamlSnippet renders the continue-style YAML fragment.
func yamlSnippet(s *clientSpec, e Entry) string {
	var b strings.Builder
	b.WriteString("mcpServers:\n")
	fmt.Fprintf(&b, "  - name: %s\n", entryName)
	fmt.Fprintf(&b, "    command: %q\n", e.Command)
	b.WriteString("    args:\n")
	for _, a := range e.Args {
		fmt.Fprintf(&b, "      - %q\n", a)
	}
	return b.String()
}

// tomlRemoval renders how to take the codex entry out again.
func tomlRemoval(s *clientSpec, name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "codex mcp remove %s\n", name)
	fmt.Fprintf(&b, "\n# or, by hand: delete the [mcp_servers.%s] table.\n", name)
	return b.String()
}

// yamlRemoval renders how to take the continue entry out again.
func yamlRemoval(s *clientSpec, name string) string {
	return "# delete the mcpServers entry named \"" + name + "\" by hand.\n"
}

// remoteSnippet renders instructions for a client with no local file.
func remoteSnippet(s *clientSpec, e Entry) string {
	return "# " + s.name + " has no local MCP configuration file.\n" +
		"# 1. start the shared daemon:   " + e.Command + " daemon\n" +
		"# 2. register the daemon's streamable-http MCP endpoint in " + s.name + ".\n" +
		"# `" + e.Command + " " + strings.Join(e.Args, " ") + "` is the stdio entry point and is NOT used here.\n"
}

func quoteJoin(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = fmt.Sprintf("%q", a)
	}
	return strings.Join(parts, ", ")
}
