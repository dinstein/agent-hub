package clients_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/clients"
)

// env is a hermetic Table plus the fake home and backup directories it
// resolves against, so no test ever touches the real user environment.
type env struct {
	tbl     *clients.Table
	home    string
	backups string
	project string
}

func newEnv(t *testing.T, goos string) env {
	t.Helper()
	root := t.TempDir()
	e := env{
		home:    filepath.Join(root, "home"),
		backups: filepath.Join(root, "backups"),
		project: filepath.Join(root, "project"),
	}
	for _, d := range []string{e.home, e.project} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// NoDelegate by default: delegation RUNS another application's CLI, and
	// a test that does that edits the developer's own configuration with
	// their real codex. The delegation tests opt back in with a fake one on
	// PATH.
	e.tbl = clients.New(clients.Options{
		GOOS: goos, Home: e.home, BackupDir: e.backups, NoDelegate: true,
	})
	return e
}

func (e env) format(t *testing.T, id string) clients.Format {
	t.Helper()
	f, ok := e.tbl.Lookup(id)
	if !ok {
		t.Fatalf("client %q not registered", id)
	}
	return f
}

// write creates a file (and parents) with content.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func entry(clientID string) clients.Entry {
	return clients.Entry{
		Command: "/opt/agenthub/bin/agenthub",
		Args:    []string{"connect", "--client", clientID},
	}
}

func backupsOf(t *testing.T, e env, clientID string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(e.backups, clientID+"-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestTableCoversTheEcosystem pins the adapter table: every shape has at
// least one client, and the IDs the M1 task enumerates are all present.
func TestTableCoversTheEcosystem(t *testing.T) {
	e := newEnv(t, "darwin")
	want := []string{
		"claude-code", "claude-desktop", "cursor", "windsurf", "vscode",
		"cline", "roo-code", "zed", "gemini-cli", "continue", "codex", "open-webui",
	}
	ids := e.tbl.IDs()
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("client %q missing from the table (have %v)", id, ids)
		}
	}
	if len(ids) < 10 {
		t.Errorf("table has %d clients, want at least 10", len(ids))
	}

	shapes := map[clients.Shape]int{}
	for _, f := range e.tbl.Formats() {
		shapes[f.Shape()]++
		if f.Writable() != f.Shape().Writable() {
			t.Errorf("%s: Writable()=%t but Shape().Writable()=%t", f.ID(), f.Writable(), f.Shape().Writable())
		}
	}
	for _, s := range []clients.Shape{clients.ShapeServerMap, clients.ShapeNested, clients.ShapeTOML, clients.ShapeYAML, clients.ShapeRemote} {
		if shapes[s] == 0 {
			t.Errorf("no client uses shape %q", s)
		}
	}
}

// TestLocationsResolvePerPlatform: the same row yields different user
// paths per OS, project paths come first, and an OS absent from the row's
// table (Windows, unfilled) drops the user location instead of inventing
// one.
func TestLocationsResolvePerPlatform(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want string
	}{
		{"darwin", filepath.Join("Library", "Application Support", "Claude", "claude_desktop_config.json")},
		{"linux", filepath.Join(".config", "Claude", "claude_desktop_config.json")},
	} {
		e := newEnv(t, tc.goos)
		locs := e.format(t, "claude-desktop").Locations(e.project)
		if len(locs) != 1 || locs[0].Path != filepath.Join(e.home, tc.want) {
			t.Errorf("%s: locations = %+v", tc.goos, locs)
		}
	}

	e := newEnv(t, "darwin")
	locs := e.format(t, "claude-code").Locations(e.project)
	if len(locs) != 2 || locs[0].Placement != clients.Project || locs[1].Placement != clients.User {
		t.Fatalf("claude-code locations = %+v, want project then user", locs)
	}
	if locs[0].Path != filepath.Join(e.project, ".mcp.json") {
		t.Errorf("project path = %q", locs[0].Path)
	}
	// Locations stay project-first (that ordering is Import's read
	// precedence), but the default WRITE target is the user-level file.
	if got := e.format(t, "claude-code").DefaultPath(e.project); got != locs[1].Path {
		t.Errorf("DefaultPath = %q, want the user location %q", got, locs[1].Path)
	}

	// Windows is not in any row's home table yet: user locations vanish,
	// project ones survive.
	w := clients.New(clients.Options{GOOS: "windows", Home: e.home, BackupDir: e.backups})
	wf, _ := w.Lookup("claude-code")
	if locs := wf.Locations(e.project); len(locs) != 1 || locs[0].Placement != clients.Project {
		t.Errorf("windows locations = %+v, want project only", locs)
	}
	wd, _ := w.Lookup("claude-desktop")
	if locs := wd.Locations(e.project); len(locs) != 0 {
		t.Errorf("windows claude-desktop locations = %+v, want none", locs)
	}
}

// TestDefaultTargetIsUserLevel pins where a connect goes when the caller
// names neither a path nor a placement: the user's home directory, never the
// working tree. The entry carries this machine's absolute agenthub path, and
// a project-level file is a committed one.
func TestDefaultTargetIsUserLevel(t *testing.T) {
	e := newEnv(t, "darwin")
	f := e.format(t, "claude-code")

	if _, err := f.Connect(f.DefaultPath(e.project), entry("claude-code")); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".claude.json")); err != nil {
		t.Errorf("user-level file not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.project, ".mcp.json")); !os.IsNotExist(err) {
		t.Errorf("the working tree was touched: stat .mcp.json = %v", err)
	}
}

// TestPathForIsExactOrNothing: an explicitly named placement is an
// instruction. A client with no such location answers "" rather than the
// other placement's file, so the caller can refuse instead of writing the
// entry somewhere nobody named.
func TestPathForIsExactOrNothing(t *testing.T) {
	e := newEnv(t, "darwin")
	code := e.format(t, "claude-code")
	if got, want := code.PathFor(e.project, clients.Project), filepath.Join(e.project, ".mcp.json"); got != want {
		t.Errorf("claude-code project path = %q, want %q", got, want)
	}
	if got, want := code.PathFor(e.project, clients.User), filepath.Join(e.home, ".claude.json"); got != want {
		t.Errorf("claude-code user path = %q, want %q", got, want)
	}
	// claude-desktop is user-only: asking for its project file gets nothing,
	// not the user file wearing a project label.
	if got := e.format(t, "claude-desktop").PathFor(e.project, clients.Project); got != "" {
		t.Errorf("claude-desktop project path = %q, want \"\"", got)
	}
	// A row with no user location on this platform still has a default: the
	// fallback keeps Windows (absent from every home table) writable.
	w := clients.New(clients.Options{GOOS: "windows", Home: e.home, BackupDir: e.backups})
	wf, _ := w.Lookup("claude-code")
	if got, want := wf.DefaultPath(e.project), filepath.Join(e.project, ".mcp.json"); got != want {
		t.Errorf("windows DefaultPath = %q, want the project fallback %q", got, want)
	}
	if got := wf.PathFor(e.project, clients.User); got != "" {
		t.Errorf("windows user path = %q, want \"\"", got)
	}
}

// TestDisconnectDefaultFindsTheOtherPlacement covers the migration the moved
// default creates: an entry written back when connect defaulted to the
// project level must still be removable by a bare disconnect. Reporting "not
// connected" while the entry sits in .mcp.json is the one unacceptable
// answer.
func TestDisconnectDefaultFindsTheOtherPlacement(t *testing.T) {
	e := newEnv(t, "darwin")
	f := e.format(t, "claude-code")
	project := filepath.Join(e.project, ".mcp.json")
	if _, err := f.Connect(project, entry("claude-code")); err != nil {
		t.Fatalf("connect: %v", err)
	}

	res, err := clients.DisconnectDefault(f, e.project)
	if err != nil {
		t.Fatalf("DisconnectDefault: %v", err)
	}
	if res.Path != project || len(res.Removed) != 1 {
		t.Errorf("result = %+v, want the project file emptied", res)
	}
	if got := read(t, project); strings.Contains(got, "agenthub") {
		t.Errorf("entry survived: %s", got)
	}

	// The default target wins whenever it does hold our entry: the fallback
	// is a last resort, not a sweep.
	user := filepath.Join(e.home, ".claude.json")
	for _, p := range []string{project, user} {
		if _, err := f.Connect(p, entry("claude-code")); err != nil {
			t.Fatalf("connect %s: %v", p, err)
		}
	}
	res, err = clients.DisconnectDefault(f, e.project)
	if err != nil {
		t.Fatalf("DisconnectDefault: %v", err)
	}
	if res.Path != user {
		t.Errorf("removed from %q, want the default target %q", res.Path, user)
	}
	if got := read(t, project); !strings.Contains(got, "agenthub") {
		t.Errorf("the project entry was swept up too: %s", got)
	}
}

// TestDisconnectDefaultSurfacesRefusals: a fallback location agenthub refused
// to touch is reported as the refusal it is. Folding it into "not connected"
// would tell the user their config is clean when agenthub could not read it.
func TestDisconnectDefaultSurfacesRefusals(t *testing.T) {
	e := newEnv(t, "darwin")
	f := e.format(t, "claude-code")
	write(t, filepath.Join(e.project, ".mcp.json"), "{ \"mcpServers\": [] }\n")

	_, err := clients.DisconnectDefault(f, e.project)
	var pe *clients.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *ParseError from the unparseable fallback", err)
	}
}

// TestConnectRoundTrip covers create -> idempotent re-connect -> disconnect
// on the mcpServers-map shape.
func TestConnectRoundTrip(t *testing.T) {
	e := newEnv(t, "darwin")
	f := e.format(t, "claude-code")
	path := filepath.Join(e.project, ".mcp.json")

	res, err := f.Connect(path, entry("claude-code"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !res.Changed || res.Backup != "" || res.Path != path {
		t.Errorf("result = %+v (a fresh file has nothing to back up)", res)
	}
	if n := len(backupsOf(t, e, "claude-code")); n != 0 {
		t.Errorf("creating a new file wrote %d backups", n)
	}

	// Second connect with identical content is a no-op.
	res, err = f.Connect(path, entry("claude-code"))
	if err != nil {
		t.Fatalf("re-connect: %v", err)
	}
	if res.Changed || res.Backup != "" {
		t.Errorf("re-connect must be a no-op, got %+v", res)
	}
	if n := len(backupsOf(t, e, "claude-code")); n != 0 {
		t.Errorf("no-op re-connect wrote %d backups", n)
	}

	// Disconnect removes the entry and reports it (and does back up).
	dres, err := f.Disconnect(path)
	if err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if len(dres.Removed) != 1 || dres.Removed[0] != "agenthub" || !dres.Changed {
		t.Errorf("disconnect result = %+v", dres)
	}
	if dres.Backup == "" {
		t.Error("disconnect modified an existing file without a backup")
	}

	// Nothing owned left.
	var nc *clients.NotConnectedError
	if _, err := f.Disconnect(path); !errors.As(err, &nc) {
		t.Fatalf("second disconnect: err = %v, want *NotConnectedError", err)
	}
}

// TestDisconnectRemovesOnlyOwnedEntries: ownership is by args shape, not
// by entry name. A renamed-but-ours entry goes; a foreign entry that
// happens to be named "agenthub" stays; another client's entry stays.
func TestDisconnectRemovesOnlyOwnedEntries(t *testing.T) {
	e := newEnv(t, "darwin")
	path := filepath.Join(e.project, ".mcp.json")
	write(t, path, `{
  "mcpServers": {
    "agenthub": {"command": "/other/tool", "args": ["serve"]},
    "renamed-hub": {"command": "/usr/local/bin/agenthub", "args": ["connect", "--client", "claude-code"]},
    "hub-eq": {"command": "/usr/local/bin/agenthub", "args": ["connect", "--client=claude-code"]},
    "other-client": {"command": "/usr/local/bin/agenthub", "args": ["connect", "--client", "cursor"]},
    "plain": {"command": "npx", "args": ["-y", "x"]}
  }
}`)
	res, err := e.format(t, "claude-code").Disconnect(path)
	if err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	want := []string{"hub-eq", "renamed-hub"}
	if len(res.Removed) != len(want) {
		t.Fatalf("removed = %v, want %v", res.Removed, want)
	}
	for i := range want {
		if res.Removed[i] != want[i] {
			t.Fatalf("removed = %v, want %v", res.Removed, want)
		}
	}
	got := read(t, path)
	for _, keep := range []string{`"agenthub"`, `"other-client"`, `"plain"`} {
		if !contains(got, keep) {
			t.Errorf("foreign entry %s was removed:\n%s", keep, got)
		}
	}
	for _, gone := range []string{`"hub-eq"`, `"renamed-hub"`} {
		if contains(got, gone) {
			t.Errorf("owned entry %s survived:\n%s", gone, got)
		}
	}
}

func TestDisconnectNotConnected(t *testing.T) {
	e := newEnv(t, "darwin")
	f := e.format(t, "claude-code")
	var nc *clients.NotConnectedError

	if _, err := f.Disconnect(filepath.Join(e.project, ".mcp.json")); !errors.As(err, &nc) {
		t.Fatalf("missing file: err = %v, want *NotConnectedError", err)
	}

	path := filepath.Join(e.project, "present", ".mcp.json")
	content := `{"mcpServers": {"other": {"command": "npx", "args": ["-y", "x"]}}}`
	write(t, path, content)
	if _, err := f.Disconnect(path); !errors.As(err, &nc) {
		t.Fatalf("no owned entry: err = %v, want *NotConnectedError", err)
	}
	if got := read(t, path); got != content {
		t.Errorf("file modified by failed disconnect: %s", got)
	}
}

// TestConnectRejectsUnparseableFile: an existing file that fails to parse
// must abort with *ParseError, stay byte-identical, and produce no backup
// — refusing is the point.
func TestConnectRejectsUnparseableFile(t *testing.T) {
	cases := map[string]struct {
		client, file, content string
	}{
		"truncated":         {"claude-code", ".mcp.json", `{"mcpServers": {`},
		"not an object":     {"claude-code", ".mcp.json", `[1, 2, 3]`},
		"empty file":        {"claude-code", ".mcp.json", ``},
		"section scalar":    {"claude-code", ".mcp.json", `{"mcpServers": "nope"}`},
		"section array":     {"claude-code", ".mcp.json", `{"mcpServers": []}`},
		"trailing junk":     {"claude-code", ".mcp.json", "{}\ngarbage"},
		"nested mid scalar": {"vscode", "settings.json", `{"mcp": 7}`},
		"nested leaf array": {"vscode", "settings.json", `{"mcp": {"servers": []}}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, "darwin")
			path := filepath.Join(e.project, tc.file)
			write(t, path, tc.content)
			_, err := e.format(t, tc.client).Connect(path, entry(tc.client))
			var pe *clients.ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v, want *ParseError", err)
			}
			if pe.Snippet == "" || pe.Hint == "" {
				t.Errorf("ParseError must carry a hint and a manual snippet: %+v", pe)
			}
			if got := read(t, path); got != tc.content {
				t.Errorf("file was modified despite parse failure: %q", got)
			}
			if n := len(backupsOf(t, e, tc.client)); n != 0 {
				t.Errorf("parse failure wrote %d backups", n)
			}
		})
	}
}

// TestConnectSplicesJSONCInsteadOfRewritingIt: VS Code / Zed settings are
// routinely JSONC. agenthub reads them and edits ONLY the bytes of its own
// entry — the comments, the key order and the user's formatting all come
// out the other side untouched, because nothing else was re-encoded.
func TestConnectSplicesJSONCInsteadOfRewritingIt(t *testing.T) {
	e := newEnv(t, "darwin")
	path := filepath.Join(e.project, ".vscode", "mcp.json")
	content := "{\n  // my servers\n  \"servers\": {\n    // linear does the tickets\n" +
		"    \"linear\": {\"command\": \"npx\"},\n  }\n}\n"
	write(t, path, content)

	res, err := e.format(t, "vscode").Connect(path, entry("vscode"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !res.Changed || res.Backup == "" {
		t.Errorf("result = %+v, want a change with a backup", res)
	}
	got := read(t, path)
	for _, keep := range []string{"// my servers", "// linear does the tickets", "\"linear\""} {
		if !contains(got, keep) {
			t.Errorf("the splice lost %q:\n%s", keep, got)
		}
	}
	// A trailing comma is legal here and is the user's, not ours to fix.
	if !contains(got, "},\n  }") {
		t.Errorf("the splice reformatted the document:\n%s", got)
	}
	if !contains(got, "agenthub") {
		t.Errorf("the entry did not land:\n%s", got)
	}

	// Idempotent: connecting again changes nothing at all.
	again, err := e.format(t, "vscode").Connect(path, entry("vscode"))
	if err != nil || again.Changed {
		t.Errorf("second connect = (%+v, %v), want no change", again, err)
	}

	// And removing it takes out only our own bytes.
	if _, err := e.format(t, "vscode").Disconnect(path); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	after := read(t, path)
	if contains(after, "agenthub") {
		t.Errorf("the entry survived a disconnect:\n%s", after)
	}
	for _, keep := range []string{"// my servers", "// linear does the tickets", "\"linear\""} {
		if !contains(after, keep) {
			t.Errorf("disconnect lost %q:\n%s", keep, after)
		}
	}
}

// TestProbeOnlyShapesRefuseWrites: TOML/YAML/remote clients are detected
// and explained, never rewritten.
func TestProbeOnlyShapesRefuseWrites(t *testing.T) {
	e := newEnv(t, "darwin")
	for _, id := range []string{"codex", "continue", "open-webui"} {
		f := e.format(t, id)
		if f.Writable() {
			t.Errorf("%s must not be writable", id)
			continue
		}
		var ue *clients.UnsupportedError
		_, err := f.Connect(f.DefaultPath(e.project), entry(id))
		if !errors.As(err, &ue) {
			t.Fatalf("%s connect: err = %v, want *UnsupportedError", id, err)
		}
		if ue.Op != "connect" || ue.Snippet == "" {
			t.Errorf("%s: %+v", id, ue)
		}
		if _, err := f.Disconnect(f.DefaultPath(e.project)); !errors.As(err, &ue) {
			t.Fatalf("%s disconnect: err = %v, want *UnsupportedError", id, err)
		}
	}

	// open-webui has no file at all.
	if locs := e.format(t, "open-webui").Locations(e.project); len(locs) != 0 {
		t.Errorf("open-webui locations = %+v, want none", locs)
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
