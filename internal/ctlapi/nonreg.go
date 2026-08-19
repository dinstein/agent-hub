package ctlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/clients"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/secrets"
	"github.com/dinstein/agent-hub/internal/skills"
)

// This file and its nonreg* siblings implement the NON-REGISTRY half of the
// control plane (docs/subsystems/controlplane.md): credentials, skills, agent tokens, client
// adaptation, OAuth lifecycle and the live connection self-test. Everything
// here writes state that does NOT live in the registry document, which is
// why none of it carries a generation precondition — there is no shared
// document to lose a compare-and-swap against.
//
// Three rules run through every handler below:
//
//  1. A CREDENTIAL VALUE NEVER TRAVELS OUTWARD. Secret values are write-only
//     (the wire types have no field to put one in), and an agent token's
//     plaintext appears exactly once, in the response that minted it.
//  2. A MISSING COLLABORATOR IS A 404, NOT A 500. A daemon assembled without
//     a vault answers /v1/secrets with the uniform "not found" body, exactly
//     like a daemon too old to know the route — which is precisely what a
//     newer frontend must render as "unavailable on this daemon".
//  3. A CREDENTIAL VALUE IS NEVER RECORDED ANYWHERE, not even as a hash.
//     This item used to promise that every write was recorded, and pointed
//     at a helper that has never existed in the tree. No audit trail is
//     written; what survives from that claim is the rule the value itself
//     must obey.

// CodeForbidden reports an operation refused by the operating system rather
// than by agenthub: today, a client configuration file this process may not
// read (macOS TCC). It is deliberately distinct from CodeNotFound —
// "the file is not there" and "you may not look at the file" call for
// opposite user actions (internal/clients states the same rule on
// *PermissionError, which pins the status to 403).
const CodeForbidden = "E_FORBIDDEN"

// SecretVault is the ctlapi face of the credential vault
// (*secrets.Chain satisfies it). Get is deliberately ABSENT: no
// control-plane path may read a value back, and the way to keep that true
// is to not have the method.
type SecretVault interface {
	List(ctx context.Context) ([]secrets.Ref, error)
	Set(ctx context.Context, ref secrets.Ref, val string) error
	Delete(ctx context.Context, ref secrets.Ref) error
}

// CallsKeyVault is the deliberately separate, inward-reading face used by
// the local call ledger. Unlike SecretVault, it may resolve key material,
// but the ledger handlers never return that material: it is used only to decrypt
// one selected call or authenticate the ledger.
type CallsKeyVault interface {
	Get(ctx context.Context, ref secrets.Ref) (string, bool, error)
	Set(ctx context.Context, ref secrets.Ref, val string) error
}

// SkillLibrary is the ctlapi face of *skills.Manager: read the library,
// flip the coarse enable switch, materialize one install point.
type SkillLibrary interface {
	List(ctx context.Context, opts skills.ListOptions) ([]skills.SkillView, error)
	Enable(ctx context.Context, id string) (*skills.Skill, error)
	Disable(ctx context.Context, id string) (*skills.Skill, error)
	InstallTo(ctx context.Context, req skills.InstallRequest) (*skills.InstallState, error)
}

// TokenStore is the ctlapi face of *httpbridge.Store. Lookup is absent for
// the same reason SecretVault.Get is: nothing on the control plane resolves
// a plaintext.
type TokenStore interface {
	List() ([]httpbridge.Token, error)
	Create(ctx context.Context, spec httpbridge.CreateSpec) (httpbridge.Token, string, error)
	Revoke(ctx context.Context, name string, now time.Time) (httpbridge.Token, error)
}

// ClientAdapters is the ctlapi face of *clients.Table. The signatures are
// the Table's own so the daemon can pass one straight through.
type ClientAdapters interface {
	Detect(ctx context.Context, baseDir string) []clients.Detected
	// Inspect OPENS one client's configuration files. It is deliberately
	// not folded into Detect: the listing stays a stat, and a caller that
	// wants contents asks for one named client, which is what makes the
	// macOS privacy prompt it may raise explainable.
	Inspect(clientID, baseDir string) (clients.Inspection, error)
	Lookup(id string) (clients.Format, bool)
	IDs() []string
}

// OAuthStore is the ctlapi face of *oauthflow.Store: read the stored state
// and drop it. LoadAccessToken returns the token, so the value it produces
// is never put into a response — it is consulted only to distinguish
// "registered but unusable" from "authorized" (auth status).
type OAuthStore interface {
	LoadState(ctx context.Context, serverID string) (*oauthflow.State, error)
	LoadAccessToken(ctx context.Context, serverID string) (string, error)
	Clear(ctx context.Context, serverID string) error
}

// OAuthRefresher is the ctlapi face of *oauthflow.Coordinator. The returned
// access token is discarded by every caller here.
type OAuthRefresher interface {
	Refresh(ctx context.Context, serverID string) (*oauthflow.State, string, error)
}

// TestConn is the narrow face POST /v1/servers/{id}/test needs from a live
// downstream connection (*downstream.Server satisfies it).
type TestConn interface {
	InitializeResult() *mcp.InitializeResult
	Tools() []mcp.ToolDef
	Call(ctx context.Context, tool string, args json.RawMessage) (*mcp.CallResult, error)
	Close()
}

// Connector dials one downstream server for the self-test. nil selects
// downstream.Connect; tests inject an in-process fake so the control-plane
// suite never spawns a child process.
type Connector func(ctx context.Context, spec downstream.Spec, deps downstream.Deps) (TestConn, error)

// NonRegistryDeps carries the collaborators of the non-registry endpoints.
// Every field is optional: a nil collaborator disables its endpoints (they
// answer the uniform 404), which is what lets a partially assembled daemon —
// and every existing test — keep working unchanged.
type NonRegistryDeps struct {
	// CallsRoot is <data>/calls. Empty keeps every /v1/calls endpoint off.
	CallsRoot string
	// LogsDir is <data>/logs, where the process logs live. Empty keeps
	// /v1/logs off: the endpoint would have nothing to read, and an empty
	// page is indistinguishable from a hub that has never logged anything.
	//
	// The route exists for the GUI, which may not import internal/* and so
	// cannot read a file itself. The CLI reads the same files through the
	// same package (internal/proclog) with no daemon at all.
	LogsDir string
	// EventLogPath is <data>/logs/events.jsonl. Empty keeps
	// /v1/events/log off.
	//
	// The route exists for the GUI, which may not import internal/* and so
	// cannot read the file itself; the CLI reads it directly and works with
	// no daemon at all.
	EventLogPath string
	// CallsKeys resolves the immutable encryption keys used by detail and
	// verification operations. Metadata listing does not consult it.
	CallsKeys CallsKeyVault
	// Secrets is the credential vault behind /v1/secrets.
	Secrets SecretVault
	// SecretsDir is <data>/secrets. It is read (never written) to classify
	// which backend holds a listed key WITHOUT touching the OS keychain:
	// drawing a table must not raise a keychain prompt.
	SecretsDir string
	// Skills is the library behind /v1/skills.
	Skills SkillLibrary
	// Tokens is the agent-token store behind /v1/tokens.
	Tokens TokenStore
	// Clients is the client-adapter table behind /v1/clients.
	Clients ClientAdapters
	// ClientBaseDir is the directory project-level client configurations
	// are resolved against ("" = the daemon's working directory).
	ClientBaseDir string
	// Executable is the agenthub binary path written into a client's
	// gateway entry ("" = os.Executable, falling back to "agenthub").
	Executable string
	// OAuth is the credential store behind /v1/auth.
	OAuth OAuthStore
	// Refresher performs /v1/auth/{server}/refresh. Without it the refresh
	// endpoint is unavailable while status and logout still work.
	Refresher OAuthRefresher
	// Logins runs the interactive login behind POST /v1/auth/{server}/login
	// and /v1/logins/{id}. Without it those endpoints are unavailable and
	// the rest of the auth group is unaffected — a daemon that cannot open a
	// browser dance is still perfectly able to report and refresh what is
	// already stored.
	Logins LoginSessions
	// Connect dials a downstream server for /v1/servers/{id}/test
	// (nil = downstream.Connect).
	Connect Connector
	// TestDeps supplies the credential collaborators of one self-test
	// connection (secret resolution and the bearer source). nil yields a
	// zero downstream.Deps, which can still connect a stdio server that
	// needs no secrets.
	TestDeps func(id string, spec downstream.Spec) downstream.Deps
}

// routeNonRegistry dispatches the non-registry endpoints. It returns false
// for anything it does not own — unknown resource, unknown method, or a
// collaborator this daemon was assembled without — and the caller then
// writes the ONE uniform 404. Keeping the "not mine" answer indistinguishable
// from "no such route" is the same anti-probing rule route() follows.
// Path matching goes through admin.go's pathSegments — the package's single
// path-splitting discipline: the match runs on the ESCAPED path so a %2F
// inside an id cannot smuggle an extra segment past the router, and each
// segment is unescaped and re-checked afterwards.
func (s *Server) routeNonRegistry(w http.ResponseWriter, r *http.Request) bool {
	d := &s.opts.NonRegistry
	p := r.URL.Path

	// Collection endpoints (exact paths).
	switch {
	case p == "/v1/logs" && d.LogsDir != "" && r.Method == http.MethodGet:
		s.handleProcLogs(w, r)
		return true
	case p == "/v1/calls/status" && d.CallsRoot != "" && r.Method == http.MethodGet:
		s.handleCallsStatus(w, r)
		return true
	case p == "/v1/calls" && d.CallsRoot != "" && r.Method == http.MethodGet:
		s.handleCallPage(w, r)
		return true
	case p == "/v1/calls/stats" && d.CallsRoot != "" && r.Method == http.MethodGet:
		s.handleCallsStats(w, r)
		return true
	case p == "/v1/calls/enabled" && d.CallsRoot != "" && d.CallsKeys != nil && r.Method == http.MethodPut:
		s.handleAuditEnabled(w, r)
		return true
	case p == "/v1/calls/rotate-key" && d.CallsRoot != "" && d.CallsKeys != nil && r.Method == http.MethodPost:
		s.handleAuditRotateKey(w, r)
		return true
	case p == "/v1/calls/verify" && d.CallsRoot != "" && d.CallsKeys != nil && r.Method == http.MethodPost:
		s.handleCallsVerify(w, r)
		return true
	case p == "/v1/calls/prune" && d.CallsRoot != "" && r.Method == http.MethodPost:
		s.handleCallsPrune(w, r)
		return true
	case p == "/v1/events/log" && d.EventLogPath != "" && r.Method == http.MethodGet:
		s.handleEventLog(w, r)
		return true
	case p == "/v1/secrets" && d.Secrets != nil && r.Method == http.MethodGet:
		s.handleSecretsList(w, r)
		return true
	case p == "/v1/skills" && d.Skills != nil && r.Method == http.MethodGet:
		s.handleSkillsList(w, r)
		return true
	case p == "/v1/tokens" && d.Tokens != nil && r.Method == http.MethodGet:
		s.handleTokensList(w, r)
		return true
	case p == "/v1/tokens" && d.Tokens != nil && r.Method == http.MethodPost:
		s.handleTokenCreate(w, r)
		return true
	case p == "/v1/clients" && d.Clients != nil && r.Method == http.MethodGet:
		s.handleClientsList(w, r)
		return true
	case p == "/v1/auth" && d.OAuth != nil && r.Method == http.MethodGet:
		s.handleAuthStatus(w, r)
		return true
	}
	if d.CallsRoot != "" && d.CallsKeys != nil {
		if seg, ok := pathSegments(r, "/v1/calls/", 1); ok && r.Method == http.MethodGet {
			s.handleCallDetail(w, r, seg[0])
			return true
		}
	}

	// Item endpoints (one or two path parameters).
	if d.Secrets != nil {
		if seg, ok := pathSegments(r, "/v1/secrets/", 2); ok {
			switch r.Method {
			case http.MethodPut:
				s.handleSecretPut(w, r, seg[0], seg[1])
				return true
			case http.MethodDelete:
				s.handleSecretDelete(w, r, seg[0], seg[1])
				return true
			}
		}
	}
	if d.Skills != nil {
		if seg, ok := pathSegments(r, "/v1/skills/", 1); ok && r.Method == http.MethodPatch {
			s.handleSkillPatch(w, r, seg[0])
			return true
		}
		if seg, ok := pathSegments(r, "/v1/skills/", 2); ok && seg[1] == "install" && r.Method == http.MethodPost {
			s.handleSkillInstall(w, r, seg[0])
			return true
		}
	}
	if d.Tokens != nil {
		if seg, ok := pathSegments(r, "/v1/tokens/", 1); ok && r.Method == http.MethodDelete {
			s.handleTokenRevoke(w, r, seg[0])
			return true
		}
	}
	if d.Clients != nil {
		if seg, ok := pathSegments(r, "/v1/clients/", 2); ok && seg[1] == "inspect" &&
			r.Method == http.MethodGet {
			s.handleClientInspect(w, r, seg[0])
			return true
		}
		if seg, ok := pathSegments(r, "/v1/clients/", 2); ok && seg[1] == "connect" {
			switch r.Method {
			case http.MethodPost:
				s.handleClientConnect(w, r, seg[0])
				return true
			case http.MethodDelete:
				s.handleClientDisconnect(w, r, seg[0])
				return true
			}
		}
	}
	if d.OAuth != nil {
		if seg, ok := pathSegments(r, "/v1/auth/", 1); ok && r.Method == http.MethodDelete {
			s.handleAuthLogout(w, r, seg[0])
			return true
		}
		if seg, ok := pathSegments(r, "/v1/auth/", 2); ok && seg[1] == "refresh" &&
			r.Method == http.MethodPost && d.Refresher != nil {
			s.handleAuthRefresh(w, r, seg[0])
			return true
		}
		if seg, ok := pathSegments(r, "/v1/auth/", 2); ok && seg[1] == "login" &&
			r.Method == http.MethodPost {
			// Reached even with d.Logins nil, so the answer is the explicit
			// "this daemon cannot run an interactive login" plus the command
			// that can, rather than a 404 indistinguishable from a typo.
			s.handleAuthLoginStart(w, r, seg[0])
			return true
		}
	}
	// Login sessions are their OWN resource, not a sub-resource of a server:
	// under /v1/auth/{server}/{id} a server called "logins" would decide
	// which route won by the order these cases happen to be written in.
	if d.Logins != nil {
		if seg, ok := pathSegments(r, "/v1/logins/", 1); ok {
			switch r.Method {
			case http.MethodGet:
				s.handleLoginGet(w, r, seg[0])
				return true
			case http.MethodDelete:
				s.handleLoginCancel(w, r, seg[0])
				return true
			}
		}
	}
	// Only the /test verb belongs to this file; /v1/servers itself is the
	// registry surface's (admin.go).
	if seg, ok := pathSegments(r, "/v1/servers/", 2); ok && seg[1] == "test" && r.Method == http.MethodPost {
		s.handleServerTest(w, r, seg[0])
		return true
	}
	return false
}

// decodeBody reads and JSON-decodes a bounded request body into v. An EMPTY
// body decodes to the zero value instead of failing: every request type in
// this file has a usable zero form, and forcing "{}" on a plain POST would
// be a curl trap with no safety value.
func decodeBody(r *http.Request, v any) error {
	body, err := readBody(r)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// SetRefresher installs the OAuth refresh coordinator after construction.
//
// It exists because of an ordering fact rather than a preference: the
// daemon starts its proactive refresher only once its background context
// exists, which is after the server is built — and the control plane must
// share THAT coordinator rather than construct a second one, so a
// user-triggered refresh joins the in-flight singleflight instead of
// racing it. A one-time refresh token spent twice is unrecoverable.
//
// Until it is called, /v1/auth/{server}/refresh reports unavailable while
// status and logout keep working; nil clears it again.
func (s *Server) SetRefresher(r OAuthRefresher) {
	s.nonRegMu.Lock()
	defer s.nonRegMu.Unlock()
	s.opts.NonRegistry.Refresher = r
}

// refresher returns the coordinator under the lock SetRefresher writes.
func (s *Server) refresher() OAuthRefresher {
	s.nonRegMu.Lock()
	defer s.nonRegMu.Unlock()
	return s.opts.NonRegistry.Refresher
}
