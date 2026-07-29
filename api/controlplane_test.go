package api

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
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
		{
			name:   "tools_list_filtered",
			call:   func(c *Client) error { _, err := c.Tools.List(context.Background(), "github"); return err },
			method: "GET", path: "/v1/tools", query: "server=github",
		},
		{
			name: "tools_set_enabled",
			call: func(c *Client) error {
				_, err := c.Tools.SetEnabled(context.Background(), "github", "delete_repo", false, 7)
				return err
			},
			method: "PUT", path: "/v1/tools/github/delete_repo", query: "expected_generation=7",
			body: `{"enabled":false}`,
		},
		{
			name: "tools_set_override_neutralizes_description",
			call: func(c *Client) error {
				desc := "redacted by the operator"
				_, err := c.Tools.SetOverride(context.Background(), "github", "search",
					ToolOverride{Description: &desc}, 7)
				return err
			},
			method: "PUT", path: "/v1/tools/github/search", query: "expected_generation=7",
			body: `{"override_description":"redacted by the operator"}`,
		},
		{
			name: "tools_clear_override",
			call: func(c *Client) error {
				_, err := c.Tools.SetOverride(context.Background(), "github", "search",
					ToolOverride{Clear: true}, 0)
				return err
			},
			method: "PUT", path: "/v1/tools/github/search",
			body: `{"clear_override":true}`,
		},
		{
			name:   "quarantine_list",
			call:   func(c *Client) error { _, err := c.Quarantine.List(context.Background()); return err },
			method: "GET", path: "/v1/quarantine",
		},
		{
			name: "quarantine_release",
			call: func(c *Client) error {
				_, err := c.Quarantine.Release(context.Background(), "github__search", 8)
				return err
			},
			method: "DELETE", path: "/v1/quarantine/github__search",
			query: "expected_generation=8",
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

// TestAuditStreamsAreSeparateRoutes: the two ledgers are two routes, not one
// route with a selector. It also pins the client-side clamp of the limit.
func TestAuditStreamsAreSeparateRoutes(t *testing.T) {
	runWireCases(t, []wireCase{
		{
			name:   "audit_tail",
			call:   func(c *Client) error { _, err := c.Audit.Tail(context.Background(), 20); return err },
			method: "GET", path: "/v1/audit", query: "limit=20",
			data: json.RawMessage(`[]`),
		},
		{
			name:   "audit_tail_default_limit",
			call:   func(c *Client) error { _, err := c.Audit.Tail(context.Background(), 0); return err },
			method: "GET", path: "/v1/audit",
			data: json.RawMessage(`[]`),
		},
		{
			name:   "audit_tail_is_clamped",
			call:   func(c *Client) error { _, err := c.Audit.Tail(context.Background(), 1_000_000); return err },
			method: "GET", path: "/v1/audit", query: "limit=1000",
			data: json.RawMessage(`[]`),
		},
		{
			name:   "security_tail",
			call:   func(c *Client) error { _, err := c.Audit.TailSecurity(context.Background(), 5); return err },
			method: "GET", path: "/v1/security", query: "limit=5",
			data: json.RawMessage(`[]`),
		},
	})
}

// TestResponseDecoding pins the decode half: every listing and every write
// answer lands in the right fields, with the daemon's own JSON as input.
func TestResponseDecoding(t *testing.T) {
	t.Run("server_detail", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{
			"generation":12,"id":"github",
			"entry":{"transport":"stdio","command":"npx","args":["-y","srv"],
				"env":{"TOKEN":"${SECRET_TOKEN}"},"enabled":true,
				"runtime":"docker","docker":{"image":"ghcr.io/x:1","mounts":[{"source":"/w","write":true}]}}}`))
		got, err := c.Servers.Get(context.Background(), "github")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Generation != 12 || got.ID != "github" {
			t.Errorf("detail header not decoded: %+v", got)
		}
		if got.Entry.Command != "npx" || !reflect.DeepEqual(got.Entry.Args, []string{"-y", "srv"}) {
			t.Errorf("entry not decoded: %+v", got.Entry)
		}
		// The placeholder must survive verbatim: resolution happens at
		// connect time, never in a frontend.
		if got.Entry.Env["TOKEN"] != "${SECRET_TOKEN}" {
			t.Errorf("secret placeholder mangled: %q", got.Entry.Env["TOKEN"])
		}
		if got.Entry.Docker == nil || got.Entry.Docker.Image != "ghcr.io/x:1" ||
			len(got.Entry.Docker.Mounts) != 1 || !got.Entry.Docker.Mounts[0].Write {
			t.Errorf("docker runtime not decoded: %+v", got.Entry.Docker)
		}
	})

	t.Run("profile_list_active_known", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{
			"generation":3,
			"profiles":[{"name":"dev","servers":[],"tools":{"jira":{"allow":[]}}}],
			"active":"","active_known":false}`))
		got, err := c.Profiles.List(context.Background())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if got.Generation != 3 || len(got.Profiles) != 1 {
			t.Fatalf("list not decoded: %+v", got)
		}
		if got.ActiveKnown {
			t.Error(`active_known false means "this daemon cannot answer", not "no active profile"`)
		}
		if !got.Profiles[0].Blocked() || !got.Profiles[0].Tools["jira"].Blocked() {
			t.Error("block-all must survive the round trip at both levels")
		}
	})

	t.Run("scope_detail_binding_resolution", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{
			"generation":4,"client":"claude","exists":true,
			"entry":{"profile":"legacy","profileRef":{"kind":"named","name":"dev"},"servers":["github"]}}`))
		got, err := c.Scope.Get(context.Background(), "claude")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !got.Exists {
			t.Error("exists must be decoded: a missing binding is not an empty one")
		}
		// The explicit reference wins over the shorthand; two spellings
		// that disagree must resolve one way, deterministically.
		if b := got.Entry.Binding(); b.Kind != BindingNamed || b.Name != "dev" {
			t.Errorf("Binding() = %+v, want the explicit profileRef", b)
		}
	})

	t.Run("binding_defaults_to_follow_active", func(t *testing.T) {
		if b := (ClientEntry{}).Binding(); b.Kind != BindingFollowActive {
			t.Errorf("an empty binding must follow the active profile, got %+v", b)
		}
	})

	t.Run("governance_list", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{
			"generation":9,
			"entries":[{"key":"denyDestructive","value":"false","kind":"bool",
				"doc":"refuse destructive tool calls outright"}]}`))
		got, err := c.Config.Keys(context.Background())
		if err != nil {
			t.Fatalf("Keys: %v", err)
		}
		if len(got.Entries) != 1 || got.Entries[0].Value != "false" {
			t.Fatalf("entries not decoded: %+v", got)
		}
		if got.Entries[0].Kind != GovernanceKindBool || got.Entries[0].Doc == "" {
			t.Errorf("key metadata not decoded: %+v", got.Entries[0])
		}
		// The safety flag is derived client-side, so it holds even though
		// the daemon sends no such field.
		if !got.Entries[0].Safety() {
			t.Error("denyDestructive must be flagged as a safety gate without help from the wire")
		}
	})

	t.Run("config_get_selects_and_reports_absence", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{
			"generation":9,
			"entries":[{"key":"blockOnInjection","value":"true","kind":"bool"}]}`))
		got, found, err := c.Config.Get(context.Background(), "block_on_injection")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !found || got.Value != "true" {
			t.Errorf("the snake_case alias must resolve to the canonical key: %+v found=%v", got, found)
		}
		// "no such key" is NOT "the key is unset": a typo must never read as
		// a gate that is simply off.
		if _, found, err = c.Config.Get(context.Background(), "blockOnInjecton"); err != nil || found {
			t.Errorf("a typo must report found=false, got found=%v err=%v", found, err)
		}
	})

	t.Run("tool_write", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{
			"generation":5,"changed":true,"server":"github","tool":"delete_repo",
			"enabled":false,"status":"approved"}`))
		got, err := c.Tools.SetEnabled(context.Background(), "github", "delete_repo", false, 4)
		if err != nil {
			t.Fatalf("SetEnabled: %v", err)
		}
		if got.Enabled == nil || *got.Enabled {
			t.Errorf("kill switch not decoded: %+v", got.Enabled)
		}
		// The approval state is untouched by the kill switch: disabling
		// never discards an approval, and enabling never grants one.
		if got.Status != "approved" {
			t.Errorf("status = %q, want the untouched approval state", got.Status)
		}
	})

	t.Run("tool_drift_is_only_reported_when_both_hashes_are_known", func(t *testing.T) {
		if (Tool{ApprovedHash: "a"}).Drifted() {
			t.Error("an unknown current hash is \"cannot tell\", not \"changed\"")
		}
		if !(Tool{ApprovedHash: "a", CurrentHash: "b"}).Drifted() {
			t.Error("two known, different hashes are drift")
		}
	})

	t.Run("quarantine_release", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{
			"generation":6,"changed":true,"exposed":"github__search","released":true,
			"entry":{"exposed":"github__search","server":"github","tool":"search",
				"reason":"schema drift","at":"2026-07-27T10:00:00Z"}}`))
		got, err := c.Quarantine.Release(context.Background(), "github__search", 5)
		if err != nil {
			t.Fatalf("Release: %v", err)
		}
		if !got.Released || got.Entry.Server != "github" {
			t.Errorf("release not decoded: %+v", got)
		}
	})

	t.Run("quarantine_list", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{
			"generation":2,
			"entries":[{"exposed":"github__search","server":"github","tool":"search",
				"reason":"schema drift","at":"2026-07-27T10:00:00Z"}]}`))
		got, err := c.Quarantine.List(context.Background())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got.Entries) != 1 || got.Entries[0].Exposed != "github__search" {
			t.Fatalf("entries not decoded: %+v", got)
		}
		if !got.Entries[0].At.Equal(time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)) {
			t.Errorf("timestamp not decoded: %v", got.Entries[0].At)
		}
	})

	t.Run("token_created_carries_the_value_once", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{
			"token":{"name":"ci","prefix":"agt_abc12345","tier":"read","servers":null,
				"state":"active","created_at":"2026-07-27T10:00:00Z"},
			"value":"agt_abc12345def..."}`))
		got, err := c.Tokens.Create(context.Background(), TokenSpec{Name: "ci"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.Value == "" || got.Token.Prefix != "agt_abc12345" {
			t.Fatalf("created token not decoded: %+v", got)
		}
		// A null allowlist is "every server"; it must not decode as an
		// empty (deny-everything) list or vice versa.
		if got.Token.Servers != nil {
			t.Errorf("null servers must decode to nil (every server), got %#v", got.Token.Servers)
		}
	})

	t.Run("client_detect", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{
			"found":[{"client":"claude","name":"Claude Code","placement":"user","shape":"json",
				"path":"/u/.claude.json","writable":true,"size":120,
				"modified":"2026-07-27T10:00:00Z","denied":true,"remediation":"grant Full Disk Access"}],
			"supported":["claude","cursor"]}`))
		got, err := c.Clients.Detect(context.Background())
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if len(got.Found) != 1 || !got.Found[0].Denied || got.Found[0].Remediation == "" {
			t.Fatalf("denied location must survive as a finding: %+v", got.Found)
		}
		if len(got.Supported) != 2 {
			t.Errorf("supported list not decoded: %v", got.Supported)
		}
	})

	t.Run("auth_status", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`[{"server":"github","state":"expired",
			"issuer":"https://github.com","expires_at":100,"expires_in":-5,"has_refresh_token":true}]`))
		got, err := c.Auth.Status(context.Background(), "github")
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if len(got) != 1 || got[0].State != AuthStateExpired || !got[0].HasRefreshToken {
			t.Fatalf("status not decoded: %+v", got)
		}
		if got[0].ExpiresIn != -5 {
			t.Errorf("negative expiry must survive: %d", got[0].ExpiresIn)
		}
	})

	t.Run("server_test_result", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{
			"server":"github","transport":"stdio","server_info":"gh/1.2","protocol_version":"2025-11-25",
			"connect_ms":420,"tool_count":2,"tools":["search","get"],
			"call":{"tool":"search","is_error":true,"text":"rate limited","millis":12}}`))
		got, err := c.Servers.Test(context.Background(), "github", ServerTestRequest{Tool: "search"})
		if err != nil {
			t.Fatalf("Test: %v", err)
		}
		if got.ConnectMillis != 420 || got.ToolCount != 2 || got.ServerInfo != "gh/1.2" {
			t.Fatalf("report not decoded: %+v", got)
		}
		if got.Call == nil || !got.Call.IsError || got.Call.Millis != 12 {
			t.Errorf("tool-level failure not decoded: %+v", got.Call)
		}
	})

	t.Run("secret_change", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(
			`{"action":"stored","server":"github","key":"TOKEN","scope":"_global"}`))
		got, err := c.Secrets.Set(context.Background(), "github", "", "TOKEN", "v")
		if err != nil {
			t.Fatalf("Set: %v", err)
		}
		if got.Action != SecretStored || got.Scope != SecretScopeGlobal {
			t.Errorf("change not decoded: %+v", got)
		}
	})

	t.Run("skill_install_state", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{"client_id":"claude","scope":"user",
			"path":"/u/.claude/skills/review","state":"drifted","detail":"edited outside agenthub"}`))
		got, err := c.Skills.Install(context.Background(), "review", SkillInstallRequest{ClientID: "claude"})
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if got.State != ApplyStateDrifted || got.Path == "" {
			t.Errorf("install cell not decoded: %+v", got)
		}
	})
}

// TestIsSafetyKey: the "you are about to weaken a gate" warning is derived
// client-side, so it still fires against a daemon that omits the flag.
func TestIsSafetyKey(t *testing.T) {
	for _, k := range []string{
		"denyDestructive", "deny_destructive",
		"blockOnInjection", "block_on_injection",
		"humanApproval", "human_approval",
	} {
		if !IsSafetyKey(k) {
			t.Errorf("%q must be flagged as a safety gate", k)
		}
	}
	for _, k := range []string{"discovery", "resultBudget.*", ""} {
		if IsSafetyKey(k) {
			t.Errorf("%q is not a safety gate", k)
		}
	}
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
