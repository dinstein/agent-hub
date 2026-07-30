package api

import (
	"context"
	"encoding/json"
	"testing"
)

// Every configuration method is covered here twice: the REQUEST it puts on
// the wire (method, path, query, body) and the RESPONSE it decodes. Those
// two are the whole contract of a client — a method that builds the right
// URL but decodes the wrong shape is exactly as broken as one that does not.

// wireCase describes one method call and what it must produce on the wire.
type wireCase struct {
	name   string
	call   func(c *Client) error
	method string
	path   string
	query  string
	// body is the exact JSON expected, or "" for no body at all.
	body string
	// data is the canned response payload; nil means an empty object, which
	// decodes into every non-list result type.
	data any
}

func runWireCases(t *testing.T, cases []wireCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.data
			if data == nil {
				data = json.RawMessage(`{}`)
			}
			got, c := newCapturingDaemon(t, data)
			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got.method != tc.method {
				t.Errorf("method = %s, want %s", got.method, tc.method)
			}
			if got.path != tc.path {
				t.Errorf("path = %s, want %s", got.path, tc.path)
			}
			if got.query != tc.query {
				t.Errorf("query = %q, want %q", got.query, tc.query)
			}
			if string(got.body) != tc.body {
				t.Errorf("body\n got: %s\nwant: %s", got.body, tc.body)
			}
		})
	}
}

func TestRegistryWireShapes(t *testing.T) {
	entry := ServerEntry{Command: "npx", Enabled: true}
	fullEntry := `{"transport":"","command":"npx","args":null,"env":null,"cwd":"","url":"",` +
		`"headers":null,"oauth":null,"provenance":"","derive":"","runtime":"",` +
		`"docker":null,"enabled":true,"source":""}`
	subset := []string{"github"}

	runWireCases(t, []wireCase{
		{
			name:   "servers_get",
			call:   func(c *Client) error { _, err := c.Servers.Get(context.Background(), "gi/hub"); return err },
			method: "GET", path: "/v1/servers/gi%2Fhub",
		},
		{
			name: "servers_create",
			call: func(c *Client) error {
				_, err := c.Servers.Create(context.Background(), ServerSpec{ID: "github", Entry: entry}, 4)
				return err
			},
			method: "POST", path: "/v1/servers", query: "expected_generation=4",
			body: `{"id":"github","entry":` + fullEntry + `}`,
		},
		{
			name: "servers_update_is_wholesale",
			call: func(c *Client) error {
				_, err := c.Servers.Update(context.Background(), ServerSpec{ID: "github", Entry: entry}, 4)
				return err
			},
			method: "PATCH", path: "/v1/servers/github", query: "expected_generation=4",
			body: `{"entry":` + fullEntry + `}`,
		},
		{
			name: "servers_set_enabled_touches_one_key",
			call: func(c *Client) error {
				_, err := c.Servers.SetEnabled(context.Background(), "github", false, 4)
				return err
			},
			method: "PATCH", path: "/v1/servers/github", query: "expected_generation=4",
			body: `{"entry":{"enabled":false}}`,
		},
		{
			name:   "servers_delete",
			call:   func(c *Client) error { _, err := c.Servers.Delete(context.Background(), "github", 4); return err },
			method: "DELETE", path: "/v1/servers/github", query: "expected_generation=4",
		},
		{
			name: "servers_test_has_no_precondition",
			call: func(c *Client) error {
				_, err := c.Servers.Test(context.Background(), "github",
					ServerTestRequest{Tool: "search", Args: json.RawMessage(`{"q":"x"}`), TimeoutMillis: 5000})
				return err
			},
			method: "POST", path: "/v1/servers/github/test",
			body: `{"tool":"search","args":{"q":"x"},"timeout_ms":5000}`,
		},
		{
			name:   "profiles_list",
			call:   func(c *Client) error { _, err := c.Profiles.List(context.Background()); return err },
			method: "GET", path: "/v1/profiles",
		},
		{
			name: "profiles_create",
			call: func(c *Client) error {
				_, err := c.Profiles.Create(context.Background(), "dev", &subset, 2)
				return err
			},
			method: "POST", path: "/v1/profiles", query: "expected_generation=2",
			body: `{"name":"dev","servers":["github"]}`,
		},
		{
			name:   "profiles_rename",
			call:   func(c *Client) error { _, err := c.Profiles.Rename(context.Background(), "dev", "work", 2); return err },
			method: "PATCH", path: "/v1/profiles/dev", query: "expected_generation=2",
			body: `{"rename":"work"}`,
		},
		{
			name: "profiles_set_servers",
			call: func(c *Client) error {
				_, err := c.Profiles.SetServers(context.Background(), "dev",
					ServerSetEdit{Mode: ServerSetAdd, Servers: subset}, 2)
				return err
			},
			method: "PATCH", path: "/v1/profiles/dev", query: "expected_generation=2",
			body: `{"servers":{"mode":"add","servers":["github"]}}`,
		},
		{
			name: "profiles_set_tools_block_all",
			call: func(c *Client) error {
				_, err := c.Profiles.SetTools(context.Background(), "dev", "jira", NoTools(), 2)
				return err
			},
			method: "PATCH", path: "/v1/profiles/dev", query: "expected_generation=2",
			body: `{"tools":{"server":"jira","mode":"none"}}`,
		},
		{
			name:   "profiles_set_active",
			call:   func(c *Client) error { _, err := c.Profiles.SetActive(context.Background(), "dev", 0); return err },
			method: "PATCH", path: "/v1/profiles/dev",
			body: `{"active":true}`,
		},
		{
			name:   "profiles_clear_active",
			call:   func(c *Client) error { _, err := c.Profiles.ClearActive(context.Background(), "dev", 0); return err },
			method: "PATCH", path: "/v1/profiles/dev",
			body: `{"active":false}`,
		},
		{
			name:   "profiles_delete",
			call:   func(c *Client) error { _, err := c.Profiles.Delete(context.Background(), "dev", 2); return err },
			method: "DELETE", path: "/v1/profiles/dev", query: "expected_generation=2",
		},
		{
			name:   "scope_get",
			call:   func(c *Client) error { _, err := c.Scope.Get(context.Background(), "claude"); return err },
			method: "GET", path: "/v1/scope/claude",
		},
		{
			name: "scope_set",
			call: func(c *Client) error {
				discovery := "grouped"
				_, err := c.Scope.Set(context.Background(), "claude", ClientBinding{
					Profile:   &ProfileBinding{Kind: BindingNamed, Name: "dev"},
					Servers:   &subset,
					Tools:     map[string]ProfileTools{"jira": NoTools()},
					Discovery: &discovery,
				}, 5)
				return err
			},
			method: "PUT", path: "/v1/scope/claude", query: "expected_generation=5",
			body: `{"profile":{"kind":"named","name":"dev"},"servers":["github"],` +
				`"tools":{"jira":{"mode":"none"}},"discovery":"grouped"}`,
		},
		{
			name:   "scope_clear",
			call:   func(c *Client) error { _, err := c.Scope.Clear(context.Background(), "claude", 5); return err },
			method: "DELETE", path: "/v1/scope/claude", query: "expected_generation=5",
		},
		{
			name:   "config_keys",
			call:   func(c *Client) error { _, err := c.Config.Keys(context.Background()); return err },
			method: "GET", path: "/v1/config",
		},
		{
			name: "config_set",
			call: func(c *Client) error {
				_, err := c.Config.Set(context.Background(), "denyDestructive", "true", 6)
				return err
			},
			method: "PUT", path: "/v1/config/denyDestructive", query: "expected_generation=6",
			body: `{"value":"true"}`,
		},
	})
}

func TestNonRegistryWireShapes(t *testing.T) {
	runWireCases(t, []wireCase{
		{
			name:   "secrets_list",
			call:   func(c *Client) error { _, err := c.Secrets.List(context.Background(), ""); return err },
			method: "GET", path: "/v1/secrets",
			data: json.RawMessage(`[]`),
		},
		{
			name:   "secrets_list_filtered",
			call:   func(c *Client) error { _, err := c.Secrets.List(context.Background(), "github"); return err },
			method: "GET", path: "/v1/secrets", query: "server=github",
			data: json.RawMessage(`[]`),
		},
		{
			name: "secrets_set_carries_the_value_only_in_the_body",
			call: func(c *Client) error {
				_, err := c.Secrets.Set(context.Background(), "github", "", "TOKEN", "s3cr3t")
				return err
			},
			method: "PUT", path: "/v1/secrets/github/TOKEN",
			body: `{"value":"s3cr3t"}`,
		},
		{
			name: "secrets_set_scoped",
			call: func(c *Client) error {
				_, err := c.Secrets.Set(context.Background(), "github", "work", "TOKEN", "s3cr3t")
				return err
			},
			method: "PUT", path: "/v1/secrets/github/TOKEN",
			body: `{"value":"s3cr3t","scope":"work"}`,
		},
		{
			name: "secrets_delete_scoped",
			call: func(c *Client) error {
				_, err := c.Secrets.Delete(context.Background(), "github", "work", "TOKEN")
				return err
			},
			method: "DELETE", path: "/v1/secrets/github/TOKEN", query: "scope=work",
		},
		{
			name:   "skills_list",
			call:   func(c *Client) error { _, err := c.Skills.List(context.Background()); return err },
			method: "GET", path: "/v1/skills",
			data: json.RawMessage(`[]`),
		},
		{
			name:   "skills_list_for_client",
			call:   func(c *Client) error { _, err := c.Skills.ListForClient(context.Background(), "claude"); return err },
			method: "GET", path: "/v1/skills", query: "client=claude",
			data: json.RawMessage(`[]`),
		},
		{
			name:   "skills_set_enabled",
			call:   func(c *Client) error { _, err := c.Skills.SetEnabled(context.Background(), "review", true); return err },
			method: "PATCH", path: "/v1/skills/review",
			body: `{"enabled":true}`,
		},
		{
			name: "skills_install",
			call: func(c *Client) error {
				_, err := c.Skills.Install(context.Background(), "review", SkillInstallRequest{
					ClientID: "claude", Scope: "project", ProjectRoot: "/work/repo", AllowDrift: true,
				})
				return err
			},
			method: "POST", path: "/v1/skills/review/install",
			body: `{"client_id":"claude","scope":"project","project_root":"/work/repo","allow_drift":true}`,
		},
		{
			name:   "tokens_list",
			call:   func(c *Client) error { _, err := c.Tokens.List(context.Background()); return err },
			method: "GET", path: "/v1/tokens",
			data: json.RawMessage(`[]`),
		},
		{
			name: "tokens_create",
			call: func(c *Client) error {
				_, err := c.Tokens.Create(context.Background(), TokenSpec{
					Name: "ci", Tier: TierRead, Servers: []string{"github"}, ExpiresInSeconds: 3600,
				})
				return err
			},
			method: "POST", path: "/v1/tokens",
			body: `{"name":"ci","tier":"read","servers":["github"],"expires_in_seconds":3600}`,
		},
		{
			name:   "tokens_revoke",
			call:   func(c *Client) error { _, err := c.Tokens.Revoke(context.Background(), "ci"); return err },
			method: "DELETE", path: "/v1/tokens/ci",
		},
		{
			name:   "clients_detect",
			call:   func(c *Client) error { _, err := c.Clients.Detect(context.Background()); return err },
			method: "GET", path: "/v1/clients",
		},
		{
			name: "clients_inspect",
			call: func(c *Client) error {
				_, err := c.Clients.Inspect(context.Background(), "claude")
				return err
			},
			method: "GET", path: "/v1/clients/claude/inspect",
		},
		{
			name: "clients_connect_dry_run",
			call: func(c *Client) error {
				_, err := c.Clients.Connect(context.Background(), "claude",
					ClientConnectRequest{Profile: "dev", DryRun: true})
				return err
			},
			method: "POST", path: "/v1/clients/claude/connect",
			body: `{"profile":"dev","dry_run":true}`,
		},
		{
			name:   "clients_disconnect",
			call:   func(c *Client) error { _, err := c.Clients.Disconnect(context.Background(), "claude"); return err },
			method: "DELETE", path: "/v1/clients/claude/connect",
		},
		{
			name:   "auth_status_all",
			call:   func(c *Client) error { _, err := c.Auth.Status(context.Background(), ""); return err },
			method: "GET", path: "/v1/auth",
			data: json.RawMessage(`[]`),
		},
		{
			name:   "auth_status_one",
			call:   func(c *Client) error { _, err := c.Auth.Status(context.Background(), "github"); return err },
			method: "GET", path: "/v1/auth", query: "server=github",
			data: json.RawMessage(`[]`),
		},
		{
			name:   "auth_refresh",
			call:   func(c *Client) error { _, err := c.Auth.Refresh(context.Background(), "github"); return err },
			method: "POST", path: "/v1/auth/github/refresh",
		},
		{
			name:   "auth_logout",
			call:   func(c *Client) error { _, err := c.Auth.Logout(context.Background(), "github"); return err },
			method: "DELETE", path: "/v1/auth/github",
		},
	})
}

// TestUnavailableEndpointIsNotFound: a daemon that does not serve one of
// these routes yet answers the uniform 404. A frontend must render
// "unavailable on this daemon", which is what IsCode is for.
func TestUnavailableEndpointIsNotFound(t *testing.T) {
	c := newFailingDaemon(t, 404, `{"code":"E_NOT_FOUND","message":"not found"}`)
	_, err := c.Config.Keys(context.Background())
	if !IsCode(err, ErrCodeNotFound) {
		t.Errorf("want E_NOT_FOUND, got %v", err)
	}
	if IsConflict(err) {
		t.Error("a 404 is never a conflict")
	}
}
