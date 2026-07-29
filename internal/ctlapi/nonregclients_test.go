package ctlapi

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/clients"
)

func nrClientDeps(f *nrFormat, found ...clients.Detected) func(*NonRegistryDeps) {
	return func(d *NonRegistryDeps) {
		d.Clients = &nrClients{found: found, ids: []string{"claude-code", "cursor"}, format: f}
		d.Executable = "/usr/local/bin/agenthub"
	}
}

func TestClientsList(t *testing.T) {
	det := clients.Detected{
		Client: "cursor", Name: "Cursor", Placement: clients.Project, Shape: clients.ShapeServerMap,
		Path: "/tmp/p/.cursor/mcp.json", Writable: true, Size: 42, Modified: time.Unix(1700000000, 0).UTC(),
	}
	denied := clients.Detected{
		Client: "claude-desktop", Name: "Claude Desktop", Placement: clients.User,
		Path: "/Users/u/Library/x.json", Denied: true, Remediation: "grant Full Disk Access",
	}
	env := nrStart(t, nrClientDeps(nil, det, denied))

	status, body := nrDo(t, env.sock, http.MethodGet, "/v1/clients", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out ClientsWire
	nrData(t, body, &out)
	if len(out.Found) != 2 {
		t.Fatalf("found = %+v", out.Found)
	}
	if out.Found[0].Client != "cursor" || out.Found[0].Size != 42 || !out.Found[0].Writable {
		t.Errorf("row 0 = %+v", out.Found[0])
	}
	// "You may not look" is a finding, never folded into "not there".
	if !out.Found[1].Denied || out.Found[1].Remediation == "" {
		t.Errorf("denied row = %+v", out.Found[1])
	}
	if !slices.Contains(out.Supported, "cursor") {
		t.Errorf("supported = %+v", out.Supported)
	}
}

// TestClientsDetectOnlyStats is the TCC assertion (docs/modules/controlplane.md, internal/clients
// package doc): detection must classify a client's configuration file from
// its metadata alone. It runs against the REAL adapter table with a real file
// on disk whose content is a sentinel — and asserts the sentinel is not in
// the response, while the size and path from the stat are.
func TestClientsDetectOnlyStats(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	const secretInsideConfig = "S3NT1NEL-config-body-4c19"
	cfg := filepath.Join(project, ".mcp.json")
	content := `{"mcpServers":{"x":{"command":"` + secretInsideConfig + `"}}}`
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	table := clients.New(clients.Options{Home: home, BackupDir: filepath.Join(t.TempDir(), "b")})
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.Clients = table
		d.ClientBaseDir = project
	})

	status, body := nrDo(t, env.sock, http.MethodGet, "/v1/clients", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	if nrContains(body, secretInsideConfig) {
		t.Fatalf("detection returned file CONTENT: %s", body)
	}
	var out ClientsWire
	nrData(t, body, &out)
	var row *ClientWire
	for i := range out.Found {
		if out.Found[i].Path == cfg {
			row = &out.Found[i]
		}
	}
	if row == nil {
		t.Fatalf("the project config was not detected: %+v", out.Found)
	}
	if row.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d (stat metadata)", row.Size, len(content))
	}
	if row.Modified.IsZero() {
		t.Errorf("modified time missing; the stat result is not being reported")
	}
}

// TestClientsDetectUnreadableFileStillListed is the stronger form of the same
// rule: a file this process may STAT but not READ must still be reported
// normally. If detection opened it, this would come back denied or missing.
func TestClientsDetectUnreadableFileStillListed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions; the read/stat distinction is unobservable")
	}
	project := t.TempDir()
	cfg := filepath.Join(project, ".mcp.json")
	if err := os.WriteFile(cfg, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg, 0o600) })

	table := clients.New(clients.Options{Home: t.TempDir(), BackupDir: filepath.Join(t.TempDir(), "b")})
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.Clients = table
		d.ClientBaseDir = project
	})

	_, body := nrDo(t, env.sock, http.MethodGet, "/v1/clients", nil)
	var out ClientsWire
	nrData(t, body, &out)
	for _, row := range out.Found {
		if row.Path != cfg {
			continue
		}
		if row.Denied {
			t.Fatalf("an unreadable-but-stattable file was reported denied: %+v", row)
		}
		return
	}
	t.Fatalf("the unreadable config was not listed at all: %+v", out.Found)
}

func TestClientConnect(t *testing.T) {
	f := &nrFormat{
		id: "cursor", defaultPath: "/tmp/p/.cursor/mcp.json",
		connectResult: clients.Result{Path: "/tmp/p/.cursor/mcp.json", Backup: "/tmp/b/cursor.json", Changed: true},
	}
	env := nrStart(t, nrClientDeps(f))

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/clients/cursor/connect",
		ClientConnectRequest{Profile: "dev"})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out ClientConnectWire
	nrData(t, body, &out)
	if out.DryRun || !out.Changed || out.Path != f.defaultPath || out.Backup == "" {
		t.Errorf("result = %+v", out)
	}
	want := []string{"connect", "--client", "cursor", "--profile", "dev"}
	if !slices.Equal(out.Entry.Args, want) {
		t.Errorf("args = %+v, want %+v", out.Entry.Args, want)
	}
	if out.Entry.Command != "/usr/local/bin/agenthub" {
		t.Errorf("command = %q", out.Entry.Command)
	}
	if f.gotPath != f.defaultPath {
		t.Errorf("wrote to %q", f.gotPath)
	}
	if recs := nrFindAudit(env, "clients/connect"); len(recs) != 1 || recs[0].Decision != "allowed" {
		t.Errorf("audit = %+v", recs)
	}
}

// TestClientConnectDryRunWritesNothing: a preview is a read, so it neither
// touches the format nor produces an audit line.
func TestClientConnectDryRunWritesNothing(t *testing.T) {
	f := &nrFormat{id: "cursor", defaultPath: "/tmp/x"}
	env := nrStart(t, nrClientDeps(f))

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/clients/cursor/connect",
		ClientConnectRequest{DryRun: true})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out ClientConnectWire
	nrData(t, body, &out)
	if !out.DryRun || out.Path != "" {
		t.Errorf("result = %+v", out)
	}
	if f.gotPath != "" {
		t.Errorf("a dry run reached the writer (%q)", f.gotPath)
	}
	if recs := nrFindAudit(env, "clients/connect"); len(recs) != 0 {
		t.Errorf("a dry run was audited as a write: %+v", recs)
	}
}

func TestClientConnectPathOverride(t *testing.T) {
	f := &nrFormat{id: "cursor", defaultPath: "/tmp/default"}
	env := nrStart(t, nrClientDeps(f))

	status, _ := nrDo(t, env.sock, http.MethodPost, "/v1/clients/cursor/connect",
		ClientConnectRequest{Path: "/tmp/explicit", Bin: "/opt/agenthub"})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if f.gotPath != "/tmp/explicit" {
		t.Errorf("path = %q", f.gotPath)
	}
	if f.gotEntry.Command != "/opt/agenthub" {
		t.Errorf("command = %q", f.gotEntry.Command)
	}
}

// TestClientConnectPlacement: with no target named a connect takes the
// default (user-level) file; a named placement is honoured exactly or
// refused with 400. Every refusal must leave the writer untouched — the
// point of refusing is that no file gets written.
func TestClientConnectPlacement(t *testing.T) {
	newFormat := func() *nrFormat {
		return &nrFormat{
			id: "cursor", defaultPath: "/home/u/.cursor/mcp.json",
			placementPaths: map[clients.Placement]string{
				clients.User:    "/home/u/.cursor/mcp.json",
				clients.Project: "/w/.cursor/mcp.json",
			},
		}
	}

	for _, tc := range []struct {
		name       string
		format     *nrFormat
		req        ClientConnectRequest
		wantStatus int
		wantPath   string
	}{
		{
			name: "no target is the default", format: newFormat(),
			req: ClientConnectRequest{}, wantStatus: http.StatusOK,
			wantPath: "/home/u/.cursor/mcp.json",
		},
		{
			name: "project placement", format: newFormat(),
			req: ClientConnectRequest{Placement: "project"}, wantStatus: http.StatusOK,
			wantPath: "/w/.cursor/mcp.json",
		},
		{
			name: "unknown placement", format: newFormat(),
			req: ClientConnectRequest{Placement: "global"}, wantStatus: http.StatusBadRequest,
		},
		{
			name: "path and placement together", format: newFormat(),
			req:        ClientConnectRequest{Path: "/tmp/x", Placement: "user"},
			wantStatus: http.StatusBadRequest,
		},
		{
			// A user-only client asked for its project file: the user file
			// is not an acceptable substitute for a target nobody named.
			name: "placement the client does not have",
			format: &nrFormat{id: "cursor", defaultPath: "/home/u/.cursor/mcp.json",
				placementPaths: map[clients.Placement]string{clients.User: "/home/u/.cursor/mcp.json"}},
			req: ClientConnectRequest{Placement: "project"}, wantStatus: http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := nrStart(t, nrClientDeps(tc.format))
			status, body := nrDo(t, env.sock, http.MethodPost, "/v1/clients/cursor/connect", tc.req)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", status, tc.wantStatus, body)
			}
			if tc.format.gotPath != tc.wantPath {
				t.Errorf("wrote to %q, want %q", tc.format.gotPath, tc.wantPath)
			}
		})
	}
}

func TestClientUnknownIs404(t *testing.T) {
	env := nrStart(t, nrClientDeps(&nrFormat{id: "cursor"}))

	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		status, body := nrDo(t, env.sock, method, "/v1/clients/nope/connect", nil)
		if status != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404: %s", method, status, body)
		}
	}
}

func TestClientDisconnect(t *testing.T) {
	f := &nrFormat{
		id: "cursor", defaultPath: "/tmp/p/.cursor/mcp.json",
		disconnectRes: clients.Result{Path: "/tmp/p/.cursor/mcp.json", Removed: []string{"agenthub"}},
	}
	env := nrStart(t, nrClientDeps(f))

	status, body := nrDo(t, env.sock, http.MethodDelete, "/v1/clients/cursor/connect?path=/tmp/other", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out ClientDisconnectWire
	nrData(t, body, &out)
	if len(out.Removed) != 1 || out.Removed[0] != "agenthub" {
		t.Errorf("result = %+v", out)
	}
	if f.gotPath != "/tmp/other" {
		t.Errorf("path override ignored: %q", f.gotPath)
	}
	if recs := nrFindAudit(env, "clients/disconnect"); len(recs) != 1 {
		t.Errorf("audit = %+v", recs)
	}
}

// TestClientDisconnectPlacement: ?placement= names the file to edit, and an
// unknown value is a 400 rather than a silent fall back to the default.
func TestClientDisconnectPlacement(t *testing.T) {
	f := &nrFormat{
		id: "cursor", defaultPath: "/home/u/.cursor/mcp.json",
		placementPaths: map[clients.Placement]string{
			clients.User:    "/home/u/.cursor/mcp.json",
			clients.Project: "/w/.cursor/mcp.json",
		},
		disconnectRes: clients.Result{Path: "/w/.cursor/mcp.json", Removed: []string{"agenthub"}},
	}
	env := nrStart(t, nrClientDeps(f))

	if status, body := nrDo(t, env.sock, http.MethodDelete,
		"/v1/clients/cursor/connect?placement=project", nil); status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	if f.gotPath != "/w/.cursor/mcp.json" {
		t.Errorf("edited %q, want the project file", f.gotPath)
	}

	f.gotPath = ""
	if status, _ := nrDo(t, env.sock, http.MethodDelete,
		"/v1/clients/cursor/connect?placement=global", nil); status != http.StatusBadRequest {
		t.Errorf("unknown placement status = %d, want 400", status)
	}
	if f.gotPath != "" {
		t.Errorf("a refused placement still reached the writer (%q)", f.gotPath)
	}
}

func TestClientDisconnectNotConnectedIs404(t *testing.T) {
	f := &nrFormat{id: "cursor", defaultPath: "/tmp/x",
		disconnectErr: &clients.NotConnectedError{Path: "/tmp/x"}}
	env := nrStart(t, nrClientDeps(f))

	status, body := nrDo(t, env.sock, http.MethodDelete, "/v1/clients/cursor/connect", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", status, body)
	}
}

// TestClientErrorMapping pins the two load-bearing statuses: a permission
// denial is 403 (never 404 — opposite user actions) and an unparseable file
// is 409 (agenthub REFUSED, it did not fail).
func TestClientErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"permission", &clients.PermissionError{Path: "/x", Op: "write", Remediation: "grant access",
			Err: os.ErrPermission}, http.StatusForbidden, CodeForbidden},
		{"parse", &clients.ParseError{Path: "/x", Err: errors.New("invalid character")},
			http.StatusConflict, CodeConflict},
		{"unsupported", &clients.UnsupportedError{Client: "codex", Op: "connect",
			Shape: clients.ShapeTOML, Snippet: "paste this"}, http.StatusConflict, CodeConflict},
		{"too-large", &clients.TooLargeError{Path: "/x", Size: 1, Limit: 0},
			http.StatusConflict, CodeConflict},
		{"other", errors.New("disk full"), http.StatusInternalServerError, CodeInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &nrFormat{id: "cursor", defaultPath: "/tmp/x", connectErr: c.err}
			env := nrStart(t, nrClientDeps(f))
			status, body := nrDo(t, env.sock, http.MethodPost, "/v1/clients/cursor/connect",
				ClientConnectRequest{})
			if status != c.status {
				t.Fatalf("status = %d, want %d: %s", status, c.status, body)
			}
			if code := nrErrCode(t, body); code != c.code {
				t.Errorf("code = %s, want %s", code, c.code)
			}
		})
	}
}

// TestClientInspectReadsOneNamedClient is the other half of the TCC rule:
// the listing stats, and THIS endpoint is where contents may be read — for
// one client, because the caller named it. It runs against the real adapter
// table so the read is a real read.
func TestClientInspectReadsOneNamedClient(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	cfg := filepath.Join(project, ".mcp.json")
	body := `{"mcpServers":{"agenthub":{"command":"/usr/local/bin/agenthub",` +
		`"args":["connect","--client","claude-code"]},"linear":{"command":"npx"}}}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	table := clients.New(clients.Options{Home: home, BackupDir: filepath.Join(t.TempDir(), "b")})
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.Clients = table
		d.ClientBaseDir = project
	})

	status, raw := nrDo(t, env.sock, http.MethodGet, "/v1/clients/claude-code/inspect", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	var out ClientInspectWire
	nrData(t, raw, &out)
	if out.State != string(clients.ConnectedYes) || !out.Connected {
		t.Fatalf("state = %q connected = %v, want connected", out.State, out.Connected)
	}
	if len(out.Placements) != 1 || out.Placements[0] != string(clients.Project) {
		t.Errorf("placements = %v, want the project file", out.Placements)
	}
	var file *ClientInspectFileWire
	for i := range out.Files {
		if out.Files[i].Path == cfg {
			file = &out.Files[i]
		}
	}
	if file == nil || !file.Parsed || !file.Connected {
		t.Fatalf("project file = %+v, want it read and connected", file)
	}
	owned, other := 0, 0
	for _, s := range file.Servers {
		if s.Owned {
			owned++
			continue
		}
		other++
	}
	// Ownership, never the name: exactly one entry is agenthub's, and the
	// user's own server is reported without being claimed.
	if owned != 1 || other != 1 {
		t.Errorf("servers = %+v, want one owned and one foreign", file.Servers)
	}
	// Locations that do not exist are still reported: "we looked here too".
	if len(out.Files) < 2 {
		t.Errorf("files = %+v, want every location of the client", out.Files)
	}

	// An unknown client is the uniform 404, same as an unknown route.
	if status, _ := nrDo(t, env.sock, http.MethodGet, "/v1/clients/nope/inspect", nil); status != http.StatusNotFound {
		t.Errorf("unknown client status = %d, want 404", status)
	}
}

// TestClientInspectReportsUnreadableFileWithoutLosingTheRest: one denied
// location must not sink the request. The state goes to "denied" — never
// "not connected" — and the readable locations are still reported.
func TestClientInspectReportsUnreadableFileWithoutLosingTheRest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	project := t.TempDir()
	home := t.TempDir()
	cfg := filepath.Join(project, ".mcp.json")
	if err := os.WriteFile(cfg, []byte(`{"mcpServers":{}}`), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg, 0o600) })
	table := clients.New(clients.Options{Home: home, BackupDir: filepath.Join(t.TempDir(), "b")})
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.Clients = table
		d.ClientBaseDir = project
	})

	status, raw := nrDo(t, env.sock, http.MethodGet, "/v1/clients/claude-code/inspect", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	var out ClientInspectWire
	nrData(t, raw, &out)
	if out.State != string(clients.ConnectedDenied) || out.Connected {
		t.Errorf("state = %q, want denied (a file we may not read is not an absent entry)", out.State)
	}
	found := false
	for _, f := range out.Files {
		if f.Path == cfg && f.Error != "" && !f.Parsed {
			found = true
		}
	}
	if !found {
		t.Errorf("files = %+v, want the denied location reported with its error", out.Files)
	}
}

// TestClientInspectPassesTheBaseDir: the endpoint must resolve project
// placements against the daemon's configured base directory, not the
// process working directory.
func TestClientInspectPassesTheBaseDir(t *testing.T) {
	fake := &nrClients{ids: []string{"cursor"}, inspection: clients.Inspection{Client: "cursor"}}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.Clients = fake
		d.ClientBaseDir = "/tmp/some/project"
	})
	if status, raw := nrDo(t, env.sock, http.MethodGet, "/v1/clients/cursor/inspect", nil); status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	if fake.inspected != "cursor" || fake.inspectBase != "/tmp/some/project" {
		t.Errorf("Inspect(%q, %q), want (cursor, /tmp/some/project)", fake.inspected, fake.inspectBase)
	}
}
