package ctlapi

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/dinstein/agent-hub/internal/clients"
)

// GET /v1/clients, POST|DELETE /v1/clients/{id}/connect — AI client
// adaptation (docs/modules/controlplane.md).
//
// DETECTION ONLY STATS. internal/clients.Detect never opens another
// application's configuration file, because on macOS reading one triggers a
// TCC privacy prompt and a bulk scan that prompts a dozen times is worse
// than no scan at all. Content is read only by the single-client actions
// (connect/disconnect/import), where a prompt is expected and explainable.
// A denied location is reported as denied, never folded into "not found":
// "the client is not installed" and "you may not look" call for opposite
// user actions.

// ClientWire is one detected client configuration file.
type ClientWire struct {
	Client    string `json:"client"`
	Name      string `json:"name"`
	Placement string `json:"placement"`
	Shape     string `json:"shape"`
	Path      string `json:"path"`
	Writable  bool   `json:"writable"`
	Size      int64  `json:"size"`
	// Modified is the file mtime; it is metadata from the stat, not content.
	Modified time.Time `json:"modified"`
	Note     string    `json:"note,omitempty"`
	// Denied marks a location that exists but may not be inspected.
	Denied      bool   `json:"denied,omitempty"`
	Remediation string `json:"remediation,omitempty"`
}

// ClientsWire is the answer to GET /v1/clients.
type ClientsWire struct {
	Found []ClientWire `json:"found"`
	// Supported lists every client agenthub can write directly, so the
	// answer to "why is my client missing" ships with the listing.
	Supported []string `json:"supported"`
}

// ClientEntryWire is the MCP server entry agenthub writes into a client
// configuration: it makes the client spawn this binary as its single MCP
// server.
type ClientEntryWire struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// ClientConnectRequest is the body of POST /v1/clients/{id}/connect.
type ClientConnectRequest struct {
	// Profile binds this client to a profile ("" = none).
	Profile string `json:"profile,omitempty"`
	// Path overrides the configuration file to write. It names the file
	// outright and therefore may not be combined with Placement.
	Path string `json:"path,omitempty"`
	// Placement picks between the client's own "user" and "project" files.
	// Empty means clients.DefaultPlacement (user).
	Placement string `json:"placement,omitempty"`
	// Bin overrides the agenthub binary written into the entry.
	Bin string `json:"bin,omitempty"`
	// DryRun previews the entry and writes nothing.
	DryRun bool `json:"dry_run,omitempty"`
}

// ClientConnectWire is the answer to a connect (or its dry-run preview).
type ClientConnectWire struct {
	Client  string          `json:"client"`
	Profile string          `json:"profile,omitempty"`
	DryRun  bool            `json:"dry_run"`
	Entry   ClientEntryWire `json:"entry"`
	Path    string          `json:"path,omitempty"`
	Backup  string          `json:"backup,omitempty"`
	// Changed is false on an idempotent re-connect (the file already said
	// exactly this).
	Changed bool `json:"changed,omitempty"`
}

// ClientDisconnectWire is the answer to DELETE /v1/clients/{id}/connect.
type ClientDisconnectWire struct {
	Client string `json:"client"`
	Path   string `json:"path"`
	// Removed names the entries that were deleted. Only entries agenthub
	// itself wrote are ever removed — ownership, never name alone.
	Removed []string `json:"removed"`
	Backup  string   `json:"backup,omitempty"`
}

// handleClientsList implements GET /v1/clients.
func (s *Server) handleClientsList(w http.ResponseWriter, r *http.Request) {
	d := &s.opts.NonRegistry
	found := d.Clients.Detect(r.Context(), d.ClientBaseDir)
	out := ClientsWire{Found: make([]ClientWire, 0, len(found)), Supported: d.Clients.IDs()}
	for _, f := range found {
		out.Found = append(out.Found, ClientWire{
			Client:      f.Client,
			Name:        f.Name,
			Placement:   string(f.Placement),
			Shape:       string(f.Shape),
			Path:        f.Path,
			Writable:    f.Writable,
			Size:        f.Size,
			Modified:    f.Modified,
			Note:        f.Note,
			Denied:      f.Denied,
			Remediation: f.Remediation,
		})
	}
	writeOK(w, http.StatusOK, out)
}

// ClientInspectServerWire is one entry in an inspected client's server map.
type ClientInspectServerWire struct {
	Name      string `json:"name"`
	Transport string `json:"transport,omitempty"`
	Command   string `json:"command,omitempty"`
	URL       string `json:"url,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	// Owned marks agenthub's own gateway entry, decided by what the entry
	// says rather than by its name.
	Owned bool `json:"owned"`
}

// ClientInspectFileWire is one configuration file of the inspected client.
type ClientInspectFileWire struct {
	Path      string `json:"path"`
	Placement string `json:"placement"`
	Exists    bool   `json:"exists"`
	// Parsed false with Exists true means nobody read it: a probe-only
	// shape, or the failure in Error. Servers is then empty because it was
	// not looked at, NOT because the file holds no servers.
	Parsed    bool                      `json:"parsed"`
	Connected bool                      `json:"connected"`
	Servers   []ClientInspectServerWire `json:"servers,omitempty"`
	Error     string                    `json:"error,omitempty"`
}

// ClientInspectWire is the answer to GET /v1/clients/{id}/inspect.
type ClientInspectWire struct {
	Client string `json:"client"`
	Name   string `json:"name,omitempty"`
	Shape  string `json:"shape,omitempty"`
	// State is a clients.ConnectState. Connected is only its boolean
	// projection: "denied", "unreadable" and "unknown" all mean the
	// question is still open, and a consumer that renders them as "not
	// connected" is the failure this field exists to prevent.
	State      string   `json:"state"`
	Connected  bool     `json:"connected"`
	Placements []string `json:"placements,omitempty"`

	Files []ClientInspectFileWire `json:"files"`
	// Note explains a client agenthub does not parse; Manual is the
	// fragment to add to it by hand.
	Note   string `json:"note,omitempty"`
	Manual string `json:"manual,omitempty"`
}

// handleClientInspect implements GET /v1/clients/{id}/inspect: the
// deliberate, one-client content read that GET /v1/clients refuses to do in
// bulk. A GUI showing connect status asks for it per row, so the privacy
// prompt it may raise belongs to a user action rather than to opening a page.
//
// Failure direction: a per-file failure does NOT fail the request. The
// unreadable location is reported as itself — Error, plus the state it
// forces — next to the locations that were fine, because dropping the whole
// answer over one denied file would hide the entry that IS there.
func (s *Server) handleClientInspect(w http.ResponseWriter, r *http.Request, id string) {
	d := &s.opts.NonRegistry
	insp, err := d.Clients.Inspect(id, d.ClientBaseDir)
	if err != nil && len(insp.Files) == 0 {
		var unknown *clients.UnknownClientError
		if errors.As(err, &unknown) {
			// Uniform 404: an unknown client reads like an unknown route.
			writeNotFound(w, r)
			return
		}
		s.writeClientsError(w, r, err)
		return
	}
	state, where := insp.ConnectState()
	out := ClientInspectWire{
		Client: insp.Client, Name: insp.Name, Shape: string(insp.Shape),
		State: string(state), Connected: state == clients.ConnectedYes,
		Files: make([]ClientInspectFileWire, 0, len(insp.Files)),
		Note:  insp.Note, Manual: insp.Manual,
	}
	for _, p := range where {
		out.Placements = append(out.Placements, string(p))
	}
	for _, f := range insp.Files {
		file := ClientInspectFileWire{
			Path: f.Path, Placement: string(f.Placement), Exists: f.Exists,
			Parsed: f.Parsed, Connected: f.Connected, Error: f.Error,
		}
		for _, srv := range f.Servers {
			file.Servers = append(file.Servers, ClientInspectServerWire{
				Name: srv.Name, Transport: srv.Transport, Command: srv.Command,
				URL: srv.URL, Disabled: srv.Disabled, Owned: srv.Owned,
			})
		}
		out.Files = append(out.Files, file)
	}
	writeOK(w, http.StatusOK, out)
}

// handleClientConnect implements POST /v1/clients/{id}/connect.
func (s *Server) handleClientConnect(w http.ResponseWriter, r *http.Request, id string) {
	reqID := requestIDFrom(r.Context())
	var req ClientConnectRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"decoding connect request: "+err.Error(), "", reqID)
		return
	}
	format, ok := s.opts.NonRegistry.Clients.Lookup(id)
	if !ok {
		// Uniform 404: an unknown client reads exactly like an unknown route.
		// The supported set is not a secret — GET /v1/clients lists it.
		writeNotFound(w, r)
		return
	}

	bin := req.Bin
	if bin == "" {
		bin = s.agenthubExecutable()
	}
	entry := clients.Entry{Command: bin, Args: connectArgs(id, req.Profile)}
	out := ClientConnectWire{
		Client:  id,
		Profile: req.Profile,
		DryRun:  true, // flipped below once a write actually happened
		Entry:   ClientEntryWire{Command: entry.Command, Args: entry.Args},
	}
	if req.DryRun {
		// Nothing is written and nothing is audited: a preview is a read.
		writeOK(w, http.StatusOK, out)
		return
	}

	target, terr := clientTarget(format, s.opts.NonRegistry.ClientBaseDir, req.Path, req.Placement)
	if terr != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, terr.msg, terr.hint, reqID)
		return
	}
	start := time.Now()
	res, err := format.Connect(target, entry)
	s.auditNonReg(r, "", "clients/connect", hashBody([]byte(id)), err == nil, time.Since(start))
	if err != nil {
		s.writeClientsError(w, r, err)
		return
	}
	out.DryRun = false
	out.Path = res.Path
	out.Backup = res.Backup
	out.Changed = res.Changed
	writeOK(w, http.StatusOK, out)
}

// handleClientDisconnect implements DELETE /v1/clients/{id}/connect
// (optional ?path=, ?placement=).
func (s *Server) handleClientDisconnect(w http.ResponseWriter, r *http.Request, id string) {
	format, ok := s.opts.NonRegistry.Clients.Lookup(id)
	if !ok {
		writeNotFound(w, r)
		return
	}
	path := r.URL.Query().Get("path")
	placement := r.URL.Query().Get("placement")
	target, terr := clientTarget(format, s.opts.NonRegistry.ClientBaseDir, path, placement)
	if terr != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, terr.msg, terr.hint, requestIDFrom(r.Context()))
		return
	}
	start := time.Now()
	// With no target named, also look at the client's other location: an
	// entry written before the default write target moved to the user level
	// is still on disk, and answering 404 at a file that plainly holds it is
	// the one unacceptable answer. A named path or placement is an
	// instruction and is never widened.
	var res clients.Result
	var err error
	if path == "" && placement == "" {
		res, err = clients.DisconnectDefault(format, s.opts.NonRegistry.ClientBaseDir)
	} else {
		res, err = format.Disconnect(target)
	}
	s.auditNonReg(r, "", "clients/disconnect", hashBody([]byte(id)), err == nil, time.Since(start))
	if err != nil {
		s.writeClientsError(w, r, err)
		return
	}
	writeOK(w, http.StatusOK, ClientDisconnectWire{
		Client: id, Path: res.Path, Removed: res.Removed, Backup: res.Backup,
	})
}

// targetError is a rejected connect/disconnect target: bad input, so it maps
// to 400 rather than travelling as an opaque error.
type targetError struct{ msg, hint string }

// clientTarget resolves the configuration file a connect or disconnect
// operates on. Precedence: an explicit path, then a placement, then the
// client's default target (user-level, see clients.DefaultPlacement).
//
// Failure direction: a placement this client does not have is REFUSED, never
// swapped for the other one. Writing the gateway entry into a file the caller
// did not name is worse than a 400 it can act on.
func clientTarget(format clients.Format, baseDir, path, placement string) (string, *targetError) {
	if path != "" {
		if placement != "" {
			return "", &targetError{
				msg:  "path and placement name two different targets; send one",
				hint: "path is the file itself; placement picks between the client's own user and project files",
			}
		}
		return path, nil
	}
	if placement == "" {
		return format.DefaultPath(baseDir), nil
	}
	p := clients.Placement(placement)
	if p != clients.User && p != clients.Project {
		return "", &targetError{
			msg:  "unknown placement " + placement,
			hint: string(clients.User) + " or " + string(clients.Project),
		}
	}
	target := format.PathFor(baseDir, p)
	if target == "" {
		return "", &targetError{
			msg:  "client " + format.ID() + " has no " + placement + "-level configuration file on this platform",
			hint: "GET /v1/clients lists the locations this client actually uses",
		}
	}
	return target, nil
}

// writeClientsError maps an internal/clients failure onto the wire.
//
// The two load-bearing mappings: a permission denial is 403 (the type itself
// pins that status — "you may not look" is never a 404), and an unparseable
// existing file is 409, because agenthub REFUSED to overwrite configuration
// it cannot read rather than failing to try.
func (s *Server) writeClientsError(w http.ResponseWriter, r *http.Request, err error) {
	reqID := requestIDFrom(r.Context())
	var perm *clients.PermissionError
	var parse *clients.ParseError
	var unsupported *clients.UnsupportedError
	var tooLarge *clients.TooLargeError
	var notConnected *clients.NotConnectedError
	switch {
	case errors.As(err, &perm):
		writeErr(w, perm.HTTPStatus(), CodeForbidden, perm.Error(), perm.Remediation, reqID)
	case errors.As(err, &notConnected):
		// Nothing agenthub owns was there to remove: the uniform 404, same
		// as an unknown resource.
		writeNotFound(w, r)
	case errors.As(err, &parse):
		writeErr(w, http.StatusConflict, CodeConflict, parse.Error(),
			"agenthub refuses to overwrite configuration it cannot parse; fix the file first", reqID)
	case errors.As(err, &tooLarge):
		writeErr(w, http.StatusConflict, CodeConflict, tooLarge.Error(), "", reqID)
	case errors.As(err, &unsupported):
		// A redirection, not a dead end: the snippet is what to paste.
		writeErr(w, http.StatusConflict, CodeConflict, unsupported.Error(), unsupported.Snippet, reqID)
	default:
		writeErr(w, http.StatusInternalServerError, CodeInternal, err.Error(), "", reqID)
	}
}

// connectArgs builds the gateway invocation a client configuration spawns.
// Frozen shape: `agenthub connect --client <id> [--profile <p>]`.
func connectArgs(clientID, profile string) []string {
	args := []string{"connect", "--client", clientID}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	return args
}

// agenthubExecutable resolves the binary path to write into a client entry.
//
// Failure direction: fall back to the bare name rather than erroring. A
// resolvable-by-PATH command still works for most users, while refusing the
// whole request over an unresolvable /proc entry would strand them; the
// caller can always override it with `bin`.
func (s *Server) agenthubExecutable() string {
	if e := s.opts.NonRegistry.Executable; e != "" {
		return e
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "agenthub"
}
