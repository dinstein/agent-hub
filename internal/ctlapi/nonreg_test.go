package ctlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/clients"
	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/secrets"
	"github.com/dinstein/agent-hub/internal/skills"
)

// Shared scaffolding for the non-registry endpoint tests (nonreg*.go).
// Every fake here is deliberately dumb: these tests assert the HTTP contract
// — statuses, wire shapes, audit lines and, above all, that no credential
// value comes back out — not the behaviour of the collaborators, which have
// their own suites.

// nrStart boots a control-plane server with the given non-registry deps.
func nrStart(t *testing.T, mutate func(*NonRegistryDeps)) *testEnv {
	t.Helper()
	_, env := startServer(t, func(o *Options) {
		var d NonRegistryDeps
		if mutate != nil {
			mutate(&d)
		}
		o.NonRegistry = d
	})
	return env
}

// nrDo issues one request over the raw UDS client and returns the status and
// the raw response body. body == nil sends no body at all.
func nrDo(t *testing.T, sock, method, path string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		// A string is sent VERBATIM, so a test can post bytes that are not
		// valid JSON at all.
		rdr = strings.NewReader(b)
	default:
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, "http://d"+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := rawClient(sock).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

// nrData decodes the success envelope's data into out.
func nrData(t *testing.T, raw []byte, out any) {
	t.Helper()
	var env struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope %s: %v", raw, err)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got %s", raw)
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			t.Fatalf("decode data %s: %v", env.Data, err)
		}
	}
}

// nrErrCode returns the error code of a failure envelope.
func nrErrCode(t *testing.T, raw []byte) string {
	t.Helper()
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope %s: %v", raw, err)
	}
	if env.OK {
		t.Fatalf("expected failure envelope, got %s", raw)
	}
	return env.Error.Code
}

// ---------------------------------------------------------------- fakes

// nrVault is a SecretVault that records writes without a real backend.
type nrVault struct {
	refs    []secrets.Ref
	stored  map[string]string
	deleted []secrets.Ref
	listErr error
	setErr  error
	delErr  error
}

func newNRVault(refs ...secrets.Ref) *nrVault {
	return &nrVault{refs: refs, stored: map[string]string{}}
}

func (v *nrVault) List(context.Context) ([]secrets.Ref, error) {
	return v.refs, v.listErr
}

func (v *nrVault) Set(_ context.Context, ref secrets.Ref, val string) error {
	if v.setErr != nil {
		return v.setErr
	}
	v.stored[ref.StorageKey()] = val
	v.refs = append(v.refs, ref)
	return nil
}

func (v *nrVault) Get(_ context.Context, ref secrets.Ref) (string, bool, error) {
	val, ok := v.stored[ref.StorageKey()]
	return val, ok, nil
}

func (v *nrVault) Delete(_ context.Context, ref secrets.Ref) error {
	if v.delErr != nil {
		return v.delErr
	}
	v.deleted = append(v.deleted, ref)
	return nil
}

// nrSkills is a SkillLibrary over an in-memory library.
type nrSkills struct {
	views     []skills.SkillView
	installed *skills.InstallState
	lastReq   skills.InstallRequest
	listErr   error
	opErr     error
	enabled   map[string]bool
}

func (s *nrSkills) List(_ context.Context, _ skills.ListOptions) ([]skills.SkillView, error) {
	return s.views, s.listErr
}

func (s *nrSkills) Enable(_ context.Context, id string) (*skills.Skill, error) {
	return s.flip(id, true)
}

func (s *nrSkills) Disable(_ context.Context, id string) (*skills.Skill, error) {
	return s.flip(id, false)
}

func (s *nrSkills) flip(id string, on bool) (*skills.Skill, error) {
	if s.opErr != nil {
		return nil, s.opErr
	}
	if s.enabled == nil {
		s.enabled = map[string]bool{}
	}
	s.enabled[id] = on
	return &skills.Skill{ID: id, Name: id, Kind: skills.KindSkillPack, Enabled: on}, nil
}

func (s *nrSkills) InstallTo(_ context.Context, req skills.InstallRequest) (*skills.InstallState, error) {
	if s.opErr != nil {
		return nil, s.opErr
	}
	s.lastReq = req
	st := &skills.InstallState{
		SkillID: req.SkillID, ClientID: req.ClientID, Scope: req.Scope,
		Path: "/tmp/skills/" + req.SkillID, State: skills.StateApplied,
		Granularity: skills.GranularityClient,
	}
	s.installed = st
	return st, nil
}

// nrTokens is a TokenStore that mints predictable plaintexts.
type nrTokens struct {
	toks      []httpbridge.Token
	value     string
	listErr   error
	createErr error
	revokeErr error
}

func (s *nrTokens) List() ([]httpbridge.Token, error) { return s.toks, s.listErr }

func (s *nrTokens) Create(_ context.Context, spec httpbridge.CreateSpec) (httpbridge.Token, string, error) {
	if s.createErr != nil {
		return httpbridge.Token{}, "", s.createErr
	}
	tok := httpbridge.Token{
		Name: spec.Name, Prefix: s.value[:min(len(s.value), 12)], Tier: spec.Tier,
		Servers: spec.Servers, Profile: spec.Profile,
		CreatedAt: time.Now(), ExpiresAt: spec.ExpiresAt,
	}
	s.toks = append(s.toks, tok)
	return tok, s.value, nil
}

func (s *nrTokens) Revoke(_ context.Context, name string, now time.Time) (httpbridge.Token, error) {
	if s.revokeErr != nil {
		return httpbridge.Token{}, s.revokeErr
	}
	for i := range s.toks {
		if s.toks[i].Name == name {
			s.toks[i].RevokedAt = now
			return s.toks[i], nil
		}
	}
	return httpbridge.Token{}, httpbridge.ErrTokenNotFound
}

// nrOAuth is an OAuthStore over an in-memory map.
type nrOAuth struct {
	states   map[string]*oauthflow.State
	tokens   map[string]string
	cleared  []string
	clearErr error
	loadErr  error
}

func (s *nrOAuth) LoadState(_ context.Context, id string) (*oauthflow.State, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	st, ok := s.states[id]
	if !ok {
		return nil, oauthflow.ErrNoState
	}
	return st, nil
}

func (s *nrOAuth) LoadAccessToken(_ context.Context, id string) (string, error) {
	v, ok := s.tokens[id]
	if !ok {
		return "", oauthflow.ErrNoState
	}
	return v, nil
}

func (s *nrOAuth) Clear(_ context.Context, id string) error {
	if s.clearErr != nil {
		return s.clearErr
	}
	s.cleared = append(s.cleared, id)
	return nil
}

// nrRefresher is an OAuthRefresher returning a canned outcome.
type nrRefresher struct {
	state *oauthflow.State
	token string
	err   error
	calls []string
}

func (r *nrRefresher) Refresh(_ context.Context, id string) (*oauthflow.State, string, error) {
	r.calls = append(r.calls, id)
	return r.state, r.token, r.err
}

// nrClients is a ClientAdapters over a canned detection list and one format.
type nrClients struct {
	found  []clients.Detected
	ids    []string
	format *nrFormat
	base   string // records the baseDir Detect was called with

	inspection  clients.Inspection
	inspectErr  error
	inspected   string // records the client Inspect was called with
	inspectBase string
}

func (c *nrClients) Detect(_ context.Context, baseDir string) []clients.Detected {
	c.base = baseDir
	return c.found
}

// inspection is the canned answer to Inspect; inspectErr its failure.
func (c *nrClients) Inspect(clientID, baseDir string) (clients.Inspection, error) {
	c.inspected, c.inspectBase = clientID, baseDir
	return c.inspection, c.inspectErr
}

func (c *nrClients) Lookup(id string) (clients.Format, bool) {
	if c.format == nil || c.format.id != id {
		return nil, false
	}
	return c.format, true
}

func (c *nrClients) IDs() []string { return c.ids }

// nrFormat is a clients.Format that records what it was asked to do.
type nrFormat struct {
	id          string
	defaultPath string
	// placementPaths answers PathFor; an absent placement yields "", the
	// same "this client has no such file" the real table reports.
	placementPaths map[clients.Placement]string
	connectResult  clients.Result
	connectErr     error
	disconnectRes  clients.Result
	disconnectErr  error

	gotPath  string
	gotEntry clients.Entry
}

func (f *nrFormat) ID() string                          { return f.id }
func (f *nrFormat) DisplayName() string                 { return f.id }
func (f *nrFormat) Shape() clients.Shape                { return clients.ShapeServerMap }
func (f *nrFormat) Writable() bool                      { return true }
func (f *nrFormat) Locations(string) []clients.Location { return nil }
func (f *nrFormat) DefaultPath(string) string           { return f.defaultPath }

func (f *nrFormat) PathFor(_ string, p clients.Placement) string {
	return f.placementPaths[p]
}
func (f *nrFormat) ManualSnippet(clients.Entry) string { return "snippet" }

func (f *nrFormat) Connect(path string, e clients.Entry) (clients.Result, error) {
	f.gotPath, f.gotEntry = path, e
	return f.connectResult, f.connectErr
}

func (f *nrFormat) Disconnect(path string) (clients.Result, error) {
	f.gotPath = path
	return f.disconnectRes, f.disconnectErr
}

// ---------------------------------------------------------------- routing

// TestNonRegistryUnwiredIsUniform404 pins rule 2 of nonreg.go: a daemon
// assembled WITHOUT a collaborator answers its routes with the byte-identical
// uniform 404, so "this daemon does not have that feature" and "no such
// route" are the same observation — which is exactly what a newer frontend
// must render as "unavailable" rather than "broken".
func TestNonRegistryUnwiredIsUniform404(t *testing.T) {
	env := nrStart(t, nil) // every collaborator nil

	paths := []struct{ method, path string }{
		{http.MethodGet, "/v1/secrets"},
		{http.MethodPut, "/v1/secrets/github/TOKEN"},
		{http.MethodDelete, "/v1/secrets/github/TOKEN"},
		{http.MethodGet, "/v1/skills"},
		{http.MethodPatch, "/v1/skills/writer"},
		{http.MethodPost, "/v1/skills/writer/install"},
		{http.MethodGet, "/v1/tokens"},
		{http.MethodPost, "/v1/tokens"},
		{http.MethodDelete, "/v1/tokens/ci"},
		{http.MethodGet, "/v1/clients"},
		{http.MethodPost, "/v1/clients/cursor/connect"},
		{http.MethodDelete, "/v1/clients/cursor/connect"},
		{http.MethodGet, "/v1/auth"},
		{http.MethodPost, "/v1/auth/github/refresh"},
		{http.MethodDelete, "/v1/auth/github"},
	}
	// The reference body: a route that certainly does not exist.
	_, want := nrDo(t, env.sock, http.MethodGet, "/v1/definitely-not-a-route", nil)
	want = nrStripRequestID(t, want)

	for _, p := range paths {
		status, body := nrDo(t, env.sock, p.method, p.path, nil)
		if status != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404", p.method, p.path, status)
		}
		if got := nrStripRequestID(t, body); !bytes.Equal(got, want) {
			t.Errorf("%s %s: body = %s, want %s", p.method, p.path, got, want)
		}
	}
}

// TestNonRegistryWrongMethodIs404 pins the same anti-probing rule for a
// method the route does not serve: a wired collaborator must not make the
// route's existence observable through a different status.
func TestNonRegistryWrongMethodIs404(t *testing.T) {
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.Secrets = newNRVault()
		d.Tokens = &nrTokens{value: "agh_secretvalue"}
	})
	for _, p := range []struct{ method, path string }{
		{http.MethodPost, "/v1/secrets"},
		{http.MethodGet, "/v1/secrets/github/TOKEN"},
		{http.MethodPatch, "/v1/tokens/ci"},
		{http.MethodGet, "/v1/secrets/github"},     // wrong arity
		{http.MethodPut, "/v1/secrets/github/A/B"}, // too many segments
		{http.MethodGet, "/v1/secrets/"},           // trailing slash
	} {
		status, body := nrDo(t, env.sock, p.method, p.path, nil)
		if status != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404 (body %s)", p.method, p.path, status, body)
		}
	}
}

// nrStripRequestID removes the only field a uniform 404 is allowed to vary.
func nrStripRequestID(t *testing.T, raw []byte) []byte {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if e, ok := env["error"].(map[string]any); ok {
		delete(e, "requestId")
	}
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// nrContains reports whether haystack holds needle, case-insensitively —
// the leak assertions must not be defeated by a re-cased echo.
func nrContains(haystack []byte, needle string) bool {
	return bytes.Contains(bytes.ToLower(haystack), []byte(strings.ToLower(needle)))
}
