package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/dinstein/agent-hub/api"
)

// TestNonRegistryBoundMethods runs the surfaces whose subject is not the
// registry through the same four axes as the registry ones (see
// registry_test.go). None of them is `guarded`: they carry no precondition,
// so the runner also asserts that a 409 from them is NEVER dressed up as an
// optimistic-concurrency conflict — "reload and retry" is advice that could
// not work here.
func TestNonRegistryBoundMethods(t *testing.T) {
	runCtlCases(t, []ctlCase{
		// Credential vault.
		{
			name: "ListSecrets", method: http.MethodGet, path: "/v1/secrets", query: "server=github",
			data: []api.SecretRef{{Server: "github", Scope: "global", Key: "TOKEN", Backend: "keyring", Set: true}},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.ListSecrets(ctx, "github")
				return 0, err
			},
		},
		{
			name: "SetSecret", method: http.MethodPut, path: "/v1/secrets/github/TOKEN",
			data: api.SecretChange{Action: api.SecretStored, Server: "github", Key: "TOKEN", Scope: "global"},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.SetSecret(ctx, "github", "", "TOKEN", "value")
				return 0, err
			},
		},
		{
			name: "DeleteSecret", method: http.MethodDelete, path: "/v1/secrets/github/TOKEN",
			query: "scope=project",
			data:  api.SecretChange{Action: api.SecretRemoved, Server: "github", Key: "TOKEN"},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.DeleteSecret(ctx, "github", "project", "TOKEN")
				return 0, err
			},
		},

		// Skills library.
		{
			name: "ListSkillsForClient", method: http.MethodGet, path: "/v1/skills", query: "client=claude",
			data: []api.Skill{{ID: "review", Name: "review", Enabled: true}},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.ListSkillsForClient(ctx, "claude")
				return 0, err
			},
		},
		{
			name: "SetSkillEnabled", method: http.MethodPatch, path: "/v1/skills/review",
			data: api.Skill{ID: "review", Enabled: false},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.SetSkillEnabled(ctx, "review", false)
				return 0, err
			},
		},
		{
			name: "PatchSkill", method: http.MethodPatch, path: "/v1/skills/review",
			data: api.Skill{ID: "review", Enabled: true},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				on := true
				_, err := h.PatchSkill(ctx, "review", api.SkillPatch{Enabled: &on})
				return 0, err
			},
		},
		{
			name: "InstallSkill", method: http.MethodPost, path: "/v1/skills/review/install",
			data: api.SkillInstall{ClientID: "claude", Scope: "user", State: api.ApplyStateApplied},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.InstallSkill(ctx, "review", api.SkillInstallRequest{ClientID: "claude"})
				return 0, err
			},
		},

		// Agent tokens.
		{
			name: "ListTokens", method: http.MethodGet, path: "/v1/tokens",
			data: []api.Token{{Name: "ci", Prefix: "ah_abc123def", State: api.TokenStateActive}},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.ListTokens(ctx)
				return 0, err
			},
		},
		{
			name: "CreateToken", method: http.MethodPost, path: "/v1/tokens",
			data: api.TokenCreated{Token: api.Token{Name: "ci"}, Value: "ah_secret"},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.CreateToken(ctx, api.TokenSpec{Name: "ci"})
				return 0, err
			},
		},
		{
			name: "RevokeToken", method: http.MethodDelete, path: "/v1/tokens/ci",
			data: api.TokenRevoked{Name: "ci", Prefix: "ah_abc123def"},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.RevokeToken(ctx, "ci")
				return 0, err
			},
		},

		// AI-client configuration files.
		{
			name: "DetectClients", method: http.MethodGet, path: "/v1/clients",
			data: api.ClientDetectResult{Supported: []string{"claude", "cursor"}},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.DetectClients(ctx)
				return 0, err
			},
		},
		{
			name: "InspectClient", method: http.MethodGet, path: "/v1/clients/claude/inspect",
			data: api.ClientInspection{Client: "claude", State: api.ClientConnected, Connected: true},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.InspectClient(ctx, "claude")
				return 0, err
			},
		},
		{
			name: "ConnectClient", method: http.MethodPost, path: "/v1/clients/claude/connect",
			data: api.ClientConnection{Client: "claude", Changed: true},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.ConnectClient(ctx, "claude", api.ClientConnectRequest{Profile: "dev"})
				return 0, err
			},
		},
		{
			name: "DisconnectClient", method: http.MethodDelete, path: "/v1/clients/claude/connect",
			data: api.ClientDisconnected{Client: "claude", Removed: []string{"agenthub"}},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.DisconnectClient(ctx, "claude")
				return 0, err
			},
		},

		// Stored OAuth credentials.
		{
			name: "AuthStatus", method: http.MethodGet, path: "/v1/auth", query: "server=github",
			data: []api.AuthStatus{{Server: "github", State: api.AuthStateAuthorized}},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.AuthStatus(ctx, "github")
				return 0, err
			},
		},
		{
			name: "AuthRefresh", method: http.MethodPost, path: "/v1/auth/github/refresh",
			data: api.AuthRefreshed{Server: "github", ExpiresIn: 3600},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.AuthRefresh(ctx, "github")
				return 0, err
			},
		},
		{
			name: "AuthLogout", method: http.MethodDelete, path: "/v1/auth/github",
			data: api.AuthLoggedOut{Server: "github"},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.AuthLogout(ctx, "github")
				return 0, err
			},
		},
	})
}

// nonRegSentinel is a value that could only come from the credential the
// test wrote. Any appearance of it outside the request body is a leak.
const nonRegSentinel = "s3ntinel-Kx9Qw2-VALUE"

// TestSetSecretNeverEchoesTheValue is THE test of this surface. The written
// value must reach the daemon and nothing else: not the answer, not an
// emitted frontend event, and not the `cause` a rejected call hands the page.
//
// The GUI is the last hop before a screen, a clipboard and a screenshot, so
// this is asserted here as well as in internal/ctlapi — a defence that only
// exists one layer down is a defence that a refactor can delete silently.
func TestSetSecretNeverEchoesTheValue(t *testing.T) {
	rec := &ctlRecorder{
		respond: okMode,
		data:    api.SecretChange{Action: api.SecretStored, Server: "github", Key: "TOKEN", Scope: "global"},
	}
	r := &recorder{}
	h, _ := newHub(t, newFakeDaemon(t, rec.serve(t)), r)

	change, err := h.SetSecret(t.Context(), "github", "", "TOKEN", nonRegSentinel)
	if err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if change.Action != api.SecretStored || change.Key != "TOKEN" {
		t.Errorf("change = %+v", change)
	}

	// The value DID travel: the request body is the one place it belongs,
	// so a passing test cannot be a no-op endpoint.
	if !bytes.Contains(rec.last(t).body, []byte(nonRegSentinel)) {
		t.Fatal("the daemon never received the value")
	}

	answer, merr := json.Marshal(change)
	if merr != nil {
		t.Fatalf("marshalling the answer: %v", merr)
	}
	emitted := r.all()
	events, merr := json.Marshal(emitted)
	if merr != nil {
		// The recorded events are opaque structs; fmt is the fallback and
		// is if anything more revealing than JSON.
		events = []byte(fmt.Sprintf("%+v", emitted))
	}
	for name, blob := range map[string][]byte{
		"the answer":        answer,
		"frontend events":   events,
		"the answer's text": []byte(fmt.Sprintf("%+v", change)),
	} {
		if bytes.Contains(blob, []byte(nonRegSentinel)) {
			t.Errorf("%s leaked the credential: %s", name, blob)
		}
	}
	// No write on this service emits anything at all — the SSE bridge is
	// the only source of frontend events.
	if len(emitted) != 1 || emitted[0].name != EventDaemon {
		t.Errorf("unexpected events: %+v", emitted)
	}
}

// TestSetSecretFailureNeverEchoesTheValue covers the nastier direction: a
// daemon (or a vault backend it forwarded) whose failure text embeds the
// value. The message is replaced wholesale rather than forwarded, and the
// machine-readable code survives so the page still branches correctly.
func TestSetSecretFailureNeverEchoesTheValue(t *testing.T) {
	rec := &ctlRecorder{respond: func(t *testing.T, w http.ResponseWriter, _ any) {
		writeErr(t, w, http.StatusInternalServerError, api.ErrCodeInternal,
			"backend refused to store "+nonRegSentinel)
	}}
	h, _ := newHub(t, newFakeDaemon(t, rec.serve(t)), nil)

	change, err := h.SetSecret(t.Context(), "github", "", "TOKEN", nonRegSentinel)
	if err == nil {
		t.Fatal("want an error")
	}
	if change != (api.SecretChange{}) {
		t.Errorf("a failed write returned %+v", change)
	}
	if !api.IsCode(err, api.ErrCodeInternal) {
		t.Errorf("the code did not survive redaction: %v", err)
	}
	for name, blob := range map[string][]byte{
		"the error text":   []byte(err.Error()),
		"the page's cause": MarshalError(err),
	} {
		if bytes.Contains(blob, []byte(nonRegSentinel)) {
			t.Errorf("%s leaked the credential: %s", name, blob)
		}
	}
	if !bytes.Contains(MarshalError(err), []byte(secretRedacted)) {
		t.Errorf("the redaction must say what happened: %s", MarshalError(err))
	}
}

// TestRedactSecretPreservesWhatCallersBranchOn: redaction must not turn an
// offline failure into an anonymous one, and must leave an error that never
// mentioned the value completely alone.
func TestRedactSecretPreservesWhatCallersBranchOn(t *testing.T) {
	clean := errors.New("dial failed")
	if got := redactSecret(clean, nonRegSentinel); got != clean {
		t.Errorf("an untainted error was rewritten: %v", got)
	}
	if got := redactSecret(nil, nonRegSentinel); got != nil {
		t.Errorf("redactSecret(nil) = %v", got)
	}
	// An empty value would match every message; it must disable the check
	// rather than redact everything.
	if got := redactSecret(clean, ""); got != clean {
		t.Errorf("an empty value triggered redaction: %v", got)
	}

	offline := fmt.Errorf("%w: refused "+nonRegSentinel, ErrOffline)
	got := redactSecret(offline, nonRegSentinel)
	if !errors.Is(got, ErrOffline) {
		t.Errorf("redaction lost the offline signal: %v", got)
	}
	if bytes.Contains([]byte(got.Error()), []byte(nonRegSentinel)) {
		t.Errorf("offline redaction leaked: %v", got)
	}

	tainted := &api.Error{
		ErrorBody: api.ErrorBody{
			Code: api.ErrCodeBadRequest, Message: "bad " + nonRegSentinel,
			Hint: "try " + nonRegSentinel,
		},
		Status: http.StatusBadRequest, RequestID: "req-1",
	}
	got = redactSecret(tainted, nonRegSentinel)
	var apiErr *api.Error
	if !errors.As(got, &apiErr) {
		t.Fatalf("redaction dropped the api error: %v", got)
	}
	if apiErr.Code != api.ErrCodeBadRequest || apiErr.Status != http.StatusBadRequest ||
		apiErr.RequestID != "req-1" {
		t.Errorf("redaction lost transport context: %+v", apiErr)
	}
	if apiErr.Hint != "" {
		t.Errorf("a tainted hint survived: %q", apiErr.Hint)
	}
	// The original must not be mutated: it may still be held elsewhere.
	if tainted.Message != "bad "+nonRegSentinel {
		t.Errorf("redaction mutated the original error: %+v", tainted)
	}
}

// TestSkillDriftConflictIsNotAStalePrecondition: InstallSkill answers 409
// when the target was edited outside agenthub. Re-reading fixes nothing —
// the user has to decide whether to overwrite their own edit — so the page
// must NOT be told its view is stale. Only E_STALE_PRECONDITION earns the
// conflict kind.
func TestSkillDriftConflictIsNotAStalePrecondition(t *testing.T) {
	rec := &ctlRecorder{respond: func(t *testing.T, w http.ResponseWriter, _ any) {
		writeErr(t, w, http.StatusConflict, api.ErrCodeConflict,
			"the installed copy was edited outside agenthub")
	}}
	h, _ := newHub(t, newFakeDaemon(t, rec.serve(t)), nil)

	_, err := h.InstallSkill(t.Context(), "review", api.SkillInstallRequest{ClientID: "claude"})
	if !api.IsCode(err, api.ErrCodeConflict) {
		t.Fatalf("want E_CONFLICT, got %v", err)
	}
	if api.IsConflict(err) {
		t.Fatal("a drift refusal was classified as an optimistic-concurrency conflict")
	}
	var m map[string]any
	if uerr := json.Unmarshal(MarshalError(err), &m); uerr != nil {
		t.Fatalf("MarshalError: %v", uerr)
	}
	if _, ok := m["kind"]; ok {
		t.Errorf("a non-precondition 409 was stamped %v", m["kind"])
	}
	if _, ok := m["currentGeneration"]; ok {
		t.Error("a surface with no generation reported one")
	}
	if m["status"] != float64(http.StatusConflict) {
		t.Errorf("status = %v; a page still needs to see the 409", m["status"])
	}
}

// TestCreateTokenValueReachesOnlyItsCaller: the plaintext exists for exactly
// one response and must not be republished as an event on the way past.
func TestCreateTokenValueReachesOnlyItsCaller(t *testing.T) {
	const value = "ah_live_Kx9Qw2tokenVALUE"
	rec := &ctlRecorder{respond: okMode, data: api.TokenCreated{
		Token: api.Token{Name: "ci", Prefix: "ah_live_Kx9Q", State: api.TokenStateActive},
		Value: value,
	}}
	r := &recorder{}
	h, _ := newHub(t, newFakeDaemon(t, rec.serve(t)), r)

	created, err := h.CreateToken(t.Context(), api.TokenSpec{Name: "ci", Servers: []string{}})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if created.Value != value {
		t.Fatalf("the caller must receive the value once: %+v", created)
	}
	if blob := fmt.Sprintf("%+v", r.all()); bytes.Contains([]byte(blob), []byte(value)) {
		t.Errorf("the token value was emitted as an event: %s", blob)
	}
	// The closed end of the allowlist survives the wire: an empty list
	// allows NOTHING and must not arrive as null ("every server").
	if !bytes.Contains(rec.last(t).body, []byte(`"servers":[]`)) {
		t.Errorf("the empty allowlist collapsed: %s", rec.last(t).body)
	}
}
