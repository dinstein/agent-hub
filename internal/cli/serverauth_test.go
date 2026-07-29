package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// probeFor builds a probe whose vault holds exactly the given (server, key)
// pairs and whose OAuth states are the given map. Nothing here touches a real
// vault: the ladder is a pure function of these two inputs plus the entry.
func probeFor(stored []secrets.Ref, states map[string]*oauthflow.State, env map[string]string) authProbe {
	index := map[string]bool{}
	for _, ref := range stored {
		index[ref.StorageKey()] = true
	}
	return authProbe{
		index: index,
		loadState: func(_ context.Context, id string) (*oauthflow.State, error) {
			st, ok := states[id]
			if !ok {
				return nil, fmt.Errorf("%w: server %q", oauthflow.ErrNoState, id)
			}
			return st, nil
		},
		lookupEnv: func(name string) (string, bool) {
			v, ok := env[name]
			return v, ok
		},
	}
}

// freshState is an ordinary hour-long token: long enough that the refresh
// grace applies, so the expiring case below is reachable.
func freshState(now time.Time) *oauthflow.State {
	return &oauthflow.State{
		Issuer:       "https://idp.example.com",
		RefreshToken: "r",
		IssuedAt:     now.Add(-30 * time.Minute).Unix(),
		ExpiresAt:    now.Add(30 * time.Minute).Unix(),
	}
}

func TestClassifyServerAuthLadder(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	httpEntry := func(headers map[string]string) registry.ServerEntry {
		return registry.ServerEntry{Transport: "http", URL: "https://mcp.example.com/mcp", Headers: headers}
	}
	secretEntry := registry.ServerEntry{Command: "srv", Env: map[string]string{"TOKEN": "${SECRET_API_KEY}"}}

	expiring := freshState(now)
	expiring.ExpiresAt = now.Add(30 * time.Second).Unix() // inside the 60s grace
	expired := freshState(now)
	expired.ExpiresAt = now.Add(-time.Second).Unix()
	never := freshState(now)
	never.ExpiresAt, never.IssuedAt = 0, 0

	for _, tc := range []struct {
		name   string
		entry  registry.ServerEntry
		probe  authProbe
		want   string // the AUTH cell
		kind   string
		state  string
		action string
	}{
		{
			name:  "nothing configured and nothing stored",
			entry: registry.ServerEntry{Command: "srv"},
			probe: probeFor(nil, nil, nil),
			want:  "-", kind: authKindNone, state: api.AuthStateNone,
		},
		{
			name:  "a secret the vault does not have",
			entry: secretEntry,
			probe: probeFor(nil, nil, nil),
			want:  "secret:missing", kind: authKindSecret, state: authStateMissing,
			action: api.ActionSetSecret,
		},
		{
			name:  "a secret stored in the vault",
			entry: secretEntry,
			probe: probeFor([]secrets.Ref{secrets.UserRef("srv", "API_KEY")}, nil, nil),
			want:  "secret", kind: authKindSecret, state: authStateStored,
		},
		{
			name:  "a secret satisfied by the environment level",
			entry: secretEntry,
			probe: probeFor(nil, nil, map[string]string{"AGENTHUB_SECRET_API_KEY": "v"}),
			want:  "secret", kind: authKindSecret, state: authStateStored,
		},
		{
			name:  "a literal Authorization header",
			entry: httpEntry(map[string]string{"Authorization": "Bearer abc"}),
			probe: probeFor(nil, nil, nil),
			want:  "header", kind: authKindHeader, state: authStateConfigured,
		},
		{
			name:  "an Authorization header built from a stored secret is a secret",
			entry: httpEntry(map[string]string{"Authorization": "Bearer ${SECRET_API_KEY}"}),
			probe: probeFor([]secrets.Ref{secrets.UserRef("srv", "API_KEY")}, nil, nil),
			want:  "secret", kind: authKindSecret, state: authStateStored,
		},
		{
			name:  "an authorized OAuth login",
			entry: httpEntry(nil),
			probe: probeFor(
				[]secrets.Ref{secrets.OAuthStateRef("srv"), secrets.HTTPAuthRef("srv")},
				map[string]*oauthflow.State{"srv": freshState(now)}, nil),
			want: "oauth", kind: authKindOAuth, state: api.AuthStateAuthorized,
		},
		{
			name:  "a token with no advertised expiry is authorized, not expired",
			entry: httpEntry(nil),
			probe: probeFor(
				[]secrets.Ref{secrets.OAuthStateRef("srv"), secrets.HTTPAuthRef("srv")},
				map[string]*oauthflow.State{"srv": never}, nil),
			want: "oauth", kind: authKindOAuth, state: api.AuthStateAuthorized,
		},
		{
			name:  "an OAuth login inside the refresh grace",
			entry: httpEntry(nil),
			probe: probeFor(
				[]secrets.Ref{secrets.OAuthStateRef("srv"), secrets.HTTPAuthRef("srv")},
				map[string]*oauthflow.State{"srv": expiring}, nil),
			want: "oauth:expiring", kind: authKindOAuth, state: api.AuthStateExpiring,
			action: api.ActionLogin,
		},
		{
			name:  "an expired OAuth login",
			entry: httpEntry(nil),
			probe: probeFor(
				[]secrets.Ref{secrets.OAuthStateRef("srv"), secrets.HTTPAuthRef("srv")},
				map[string]*oauthflow.State{"srv": expired}, nil),
			want: "oauth:expired", kind: authKindOAuth, state: api.AuthStateExpired,
			action: api.ActionLogin,
		},
		{
			name:  "client registration stored, no access token",
			entry: httpEntry(nil),
			probe: probeFor([]secrets.Ref{secrets.OAuthStateRef("srv")},
				map[string]*oauthflow.State{"srv": freshState(now)}, nil),
			want: "oauth:login", kind: authKindOAuth, state: api.AuthStateNone,
			action: api.ActionLogin,
		},
		{
			name:  "login hints with nothing stored",
			entry: registry.ServerEntry{Transport: "http", URL: "https://mcp.example.com/mcp", OAuth: &registry.OAuthHint{Issuer: "https://idp.example.com"}},
			probe: probeFor(nil, nil, nil),
			want:  "oauth:login", kind: authKindOAuth, state: api.AuthStateNone,
			action: api.ActionLogin,
		},
		{
			name:  "a hand-pasted token",
			entry: httpEntry(nil),
			probe: probeFor([]secrets.Ref{secrets.HTTPAuthRef("srv")}, nil, nil),
			want:  "token", kind: authKindToken, state: authStateStored,
		},
		{
			// The index said there was OAuth state; the store says otherwise
			// (cleared between the two reads). The ladder continues rather
			// than reporting an error that is really a race.
			name:  "state that vanished between the index and the read",
			entry: httpEntry(nil),
			probe: probeFor([]secrets.Ref{secrets.OAuthStateRef("srv"), secrets.HTTPAuthRef("srv")}, nil, nil),
			want:  "token", kind: authKindToken, state: authStateStored,
		},
		{
			name:  "an HTTP endpoint with no credential does not guess",
			entry: httpEntry(nil),
			probe: probeFor(nil, nil, nil),
			want:  "-", kind: authKindNone, state: api.AuthStateNone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.probe.classify(context.Background(), "srv", tc.entry, now)
			if got.cell() != tc.want {
				t.Errorf("cell = %q, want %q (%+v)", got.cell(), tc.want, got)
			}
			if got.Kind != tc.kind || got.State != tc.state || got.Action != tc.action {
				t.Errorf("kind/state/action = %q/%q/%q, want %q/%q/%q",
					got.Kind, got.State, got.Action, tc.kind, tc.state, tc.action)
			}
		})
	}
}

// TestClassifyServerAuthNeverInventsACredential covers the two ways the
// classification can be wrong in the dangerous direction: reporting "-" (no
// credential needed) when the vault could not be read, and reporting a stored
// token as the credential when a literal header will be sent instead.
func TestClassifyServerAuthNeverInventsACredential(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)

	broken := authProbe{indexErr: errors.New("keychain is on fire")}
	got := broken.classify(context.Background(), "srv", registry.ServerEntry{Command: "srv"}, now)
	if got.cell() != "error" || got.State != api.AuthStateError {
		t.Errorf("unreadable vault = %q/%+v, want an error cell and never %q", got.cell(), got, "-")
	}
	if !strings.Contains(got.Detail, "keychain is on fire") {
		t.Errorf("detail = %q, want the underlying failure", got.Detail)
	}

	shadowed := probeFor([]secrets.Ref{secrets.HTTPAuthRef("srv"), secrets.OAuthStateRef("srv")},
		map[string]*oauthflow.State{"srv": freshState(now)}, nil)
	entry := registry.ServerEntry{
		Transport: "http", URL: "https://mcp.example.com/mcp",
		Headers: map[string]string{"AUTHORIZATION": "Bearer abc"},
	}
	got = shadowed.classify(context.Background(), "srv", entry, now)
	if got.Kind != authKindHeader {
		t.Fatalf("kind = %q, want the header that will actually be sent", got.Kind)
	}
	if !strings.Contains(got.Detail, "overrides the stored token") {
		t.Errorf("detail = %q, want the shadowing spelled out", got.Detail)
	}
}

// TestClassifyServerAuthCorruptStateIsARow pins the diagnostic rule: a
// credential that cannot be read becomes an error ROW, not a failed listing.
func TestClassifyServerAuthCorruptStateIsARow(t *testing.T) {
	t.Parallel()
	p := probeFor([]secrets.Ref{secrets.OAuthStateRef("srv"), secrets.HTTPAuthRef("srv")}, nil, nil)
	p.loadState = func(context.Context, string) (*oauthflow.State, error) {
		return nil, errors.New("stored oauth state is corrupt")
	}
	got := p.classify(context.Background(), "srv", registry.ServerEntry{Transport: "http"}, time.Now())
	if got.cell() != "error" || !strings.Contains(got.Detail, "corrupt") {
		t.Errorf("classify = %+v, want an error row naming the failure", got)
	}
}

func TestServerAuthHints(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		auth ServerAuth
		want string
	}{
		{
			name: "an expiry with a refresh token needs no browser",
			auth: ServerAuth{Kind: authKindOAuth, State: api.AuthStateExpired, HasRefreshToken: true},
			want: "notion: sign-in expired — run 'agenthub auth refresh notion'",
		},
		{
			name: "an expiry without one does",
			auth: ServerAuth{Kind: authKindOAuth, State: api.AuthStateExpired},
			want: "notion: sign-in expired — run 'agenthub auth login notion'",
		},
		{
			name: "nothing stored",
			auth: ServerAuth{Kind: authKindOAuth, State: api.AuthStateNone},
			want: "notion: not signed in — run 'agenthub auth login notion'",
		},
		{
			name: "a missing secret names the key to store",
			auth: ServerAuth{Kind: authKindSecret, State: authStateMissing, MissingSecrets: []string{"API_KEY"}},
			want: "notion: secret API_KEY not stored — run 'agenthub secret set notion API_KEY'",
		},
		{
			name: "a healthy credential says nothing",
			auth: ServerAuth{Kind: authKindOAuth, State: api.AuthStateAuthorized},
			want: "",
		},
		{
			name: "a stored secret says nothing",
			auth: ServerAuth{Kind: authKindSecret, State: authStateStored},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.auth.hint("notion"); got != tc.want {
				t.Errorf("hint = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestServerLsReportsStoredCredentials is the end-to-end half: the ladder
// above proves the classification, this proves `server ls` actually asks —
// against a real vault, a real registry, and the real rendering.
func TestServerLsReportsStoredCredentials(t *testing.T) {
	secretEnv(t) // the enc-file vault, so no test can reach the OS keychain

	mustRun(t, "", "server", "add", "local", "--cmd", "srv")

	// A machine with nothing to authorize gets no AUTH column at all: "-" on
	// every row forever is the column nobody reads.
	_, out, _ := runCLI(t, "", "server", "ls")
	if strings.Contains(out, "AUTH") {
		t.Errorf("ls grew an AUTH column with no credentials anywhere:\n%s", out)
	}

	mustRun(t, "", "server", "add", "api", "--url", "https://mcp.example.com/mcp",
		"--header", "X-Api-Key=${SECRET_API_KEY}")

	_, out, _ = runCLI(t, "", "server", "ls")
	if !strings.Contains(out, "AUTH") || !strings.Contains(out, "secret:missing") {
		t.Errorf("ls did not report the unstored secret:\n%s", out)
	}
	if !strings.Contains(out, "agenthub secret set api API_KEY") {
		t.Errorf("ls did not say what to run:\n%s", out)
	}
	// The row that needs nothing still renders as "-", now that the column
	// carries information.
	if !strings.Contains(out, "local") || !strings.Contains(out, "-") {
		t.Errorf("ls dropped the credential-free row:\n%s", out)
	}

	var rows ServerList
	decodeInto(t, mustRun(t, "", "server", "ls", "--json"), &rows)
	byID := map[string]ServerRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if a := byID["api"].Auth; a == nil || a.Kind != authKindSecret || a.State != authStateMissing ||
		len(a.MissingSecrets) != 1 || a.MissingSecrets[0] != "API_KEY" || a.Action != api.ActionSetSecret {
		t.Errorf("api auth = %+v", byID["api"].Auth)
	}
	if a := byID["local"].Auth; a == nil || a.Kind != authKindNone {
		t.Errorf("local auth = %+v, want none", byID["local"].Auth)
	}

	mustRun(t, "sk-live-not-echoed\n", "secret", "set", "api", "API_KEY", "--stdin")

	_, out, _ = runCLI(t, "", "server", "ls")
	if strings.Contains(out, "secret:missing") || strings.Contains(out, "agenthub secret set") {
		t.Errorf("ls still calls the stored secret missing:\n%s", out)
	}
	if strings.Contains(out, "sk-live-not-echoed") {
		t.Fatalf("ls printed the secret VALUE:\n%s", out)
	}
	decodeInto(t, mustRun(t, "", "server", "ls", "--json"), &rows)
	for _, r := range rows {
		if r.ID == "api" && (r.Auth == nil || r.Auth.State != authStateStored) {
			t.Errorf("api auth after storing = %+v", r.Auth)
		}
	}
}

// TestServerAddEmitsNoCredentialState pins the compatibility half: `add`
// writes configuration and knows nothing about credentials, so its --json
// output must not grow an auth object (nor pay for a vault read to produce
// one).
func TestServerAddEmitsNoCredentialState(t *testing.T) {
	setDataDir(t)
	var added AddedServers
	decodeInto(t, mustRun(t, "", "server", "add", "local", "--cmd", "srv", "--json"), &added)
	if len(added.Added) != 1 {
		t.Fatalf("added = %+v", added.Added)
	}
	if added.Added[0].Auth != nil {
		t.Errorf("add reported credential state: %+v", added.Added[0].Auth)
	}
}
