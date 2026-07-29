package api

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// Both halves of the contract, same discipline as controlplane_test.go: the
// REQUEST each method puts on the wire, and the RESPONSE it decodes.

func TestCatalogAndParseWireShapes(t *testing.T) {
	runWireCases(t, []wireCase{
		{
			name:   "catalog_list_sends_no_query",
			call:   func(c *Client) error { _, err := c.Catalog.List(context.Background()); return err },
			method: "GET", path: "/v1/catalog",
		},
		{
			name:   "catalog_search",
			call:   func(c *Client) error { _, err := c.Catalog.Search(context.Background(), "git hub"); return err },
			method: "GET", path: "/v1/catalog", query: "q=git+hub",
		},
		{
			name:   "catalog_get_escapes_the_id",
			call:   func(c *Client) error { _, err := c.Catalog.Get(context.Background(), "a/b"); return err },
			method: "GET", path: "/v1/catalog/a%2Fb",
		},
		{
			name: "catalog_add_carries_the_precondition",
			call: func(c *Client) error {
				_, err := c.Catalog.Add(context.Background(), "filesystem",
					CatalogAddRequest{Name: "fs", Params: map[string]string{"directory": "/tmp"}}, 7)
				return err
			},
			method: "POST", path: "/v1/catalog/filesystem/add", query: "expected_generation=7",
			body: `{"name":"fs","params":{"directory":"/tmp"}}`,
		},
		{
			name: "catalog_add_without_options",
			call: func(c *Client) error {
				_, err := c.Catalog.Add(context.Background(), "fetch", CatalogAddRequest{}, 0)
				return err
			},
			method: "POST", path: "/v1/catalog/fetch/add", body: `{}`,
		},
		{
			name: "parse_client_config_has_no_precondition",
			call: func(c *Client) error {
				_, err := c.Parse.ClientConfig(context.Background(), `{"mcpServers":{}}`)
				return err
			},
			method: "POST", path: "/v1/parse/client-config",
			body: `{"text":"{\"mcpServers\":{}}"}`,
		},
	})
}

func TestCatalogResponseDecoding(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{"query":"git","entries":[
			{"id":"git","name":"Git","description":"d","publisher":"p","homepage":"h",
			 "provenance":"curated","transport":"stdio","command":"uvx",
			 "args":["mcp-server-git","--repository","{{repository}}"],
			 "params":[{"name":"repository","example":"/tmp"}],
			 "tags":["git"],"needs_config":true},
			{"id":"fetch","name":"Fetch","description":"d","provenance":"curated",
			 "transport":"stdio","command":"uvx","args":["mcp-server-fetch"],"needs_config":false}]}`))
		got, err := c.Catalog.Search(context.Background(), "git")
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if got.Query != "git" || len(got.Entries) != 2 {
			t.Fatalf("list not decoded: %+v", got)
		}
		first := got.Entries[0]
		if !first.NeedsConfig || len(first.Params) != 1 || first.Params[0].Name != "repository" {
			t.Errorf("parameters not decoded: %+v", first)
		}
		if first.Provenance != ProvenanceCurated {
			t.Errorf("provenance = %q", first.Provenance)
		}
		if got.Entries[1].NeedsConfig {
			t.Error("a one-click entry decoded as needing configuration")
		}
	})

	t.Run("entry_with_credentials", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{
			"id":"slack","name":"Slack","description":"d","provenance":"curated",
			"transport":"stdio","command":"npx","env":{"SLACK_BOT_TOKEN":"${SECRET_SLACK_BOT_TOKEN}"},
			"keys":[{"key":"SLACK_BOT_TOKEN","description":"bot token"},
			        {"key":"OPTIONAL_KEY","optional":true}],
			"auth":"oauth","needs_config":true,"required_keys":["SLACK_BOT_TOKEN"]}`))
		got, err := c.Catalog.Get(context.Background(), "slack")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(got.Keys) != 2 || got.Keys[0].Key != "SLACK_BOT_TOKEN" || !got.Keys[1].Optional {
			t.Errorf("credentials not decoded: %+v", got.Keys)
		}
		if !reflect.DeepEqual(got.RequiredKeys, []string{"SLACK_BOT_TOKEN"}) {
			t.Errorf("required keys = %v", got.RequiredKeys)
		}
		if got.Auth != CatalogAuthOAuth {
			t.Errorf("auth = %q", got.Auth)
		}
		// The placeholder must survive verbatim: no frontend resolves it.
		if got.Env["SLACK_BOT_TOKEN"] != "${SECRET_SLACK_BOT_TOKEN}" {
			t.Errorf("secret placeholder mangled: %q", got.Env["SLACK_BOT_TOKEN"])
		}
	})

	t.Run("add", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{
			"generation":9,"changed":true,"id":"fs","catalog_id":"filesystem",
			"entry":{"transport":"stdio","command":"npx","args":["-y","pkg","/tmp"],
			         "enabled":true,"source":"catalog:filesystem"},
			"next_steps":["agenthub secret set fs TOKEN"]}`))
		got, err := c.Catalog.Add(context.Background(), "filesystem", CatalogAddRequest{Name: "fs"}, 0)
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if got.Generation != 9 || !got.Changed || got.ID != "fs" || got.CatalogID != "filesystem" {
			t.Fatalf("add not decoded: %+v", got)
		}
		if got.Entry == nil || got.Entry.Source != "catalog:filesystem" {
			t.Fatalf("entry not decoded: %+v", got.Entry)
		}
		if !reflect.DeepEqual(got.NextSteps, []string{"agenthub secret set fs TOKEN"}) {
			t.Errorf("next steps = %v", got.NextSteps)
		}
	})

	t.Run("parsed_client_config", func(t *testing.T) {
		_, c := newCapturingDaemon(t, json.RawMessage(`{
			"shape":"wrapped","section":["mcp","servers"],
			"servers":[{"name":"fs","entry":{"transport":"stdio","command":"npx","enabled":true,
			            "source":"pasted"},"warnings":["ignored fields agenthub does not model: timeout"]}],
			"skipped":[{"name":"agenthub","reason":"agenthub gateway entry"}]}`))
		got, err := c.Parse.ClientConfig(context.Background(), "{}")
		if err != nil {
			t.Fatalf("ClientConfig: %v", err)
		}
		if got.Shape != "wrapped" || !reflect.DeepEqual(got.Section, []string{"mcp", "servers"}) {
			t.Fatalf("shape not decoded: %+v", got)
		}
		if len(got.Servers) != 1 || got.Servers[0].Entry.Command != "npx" {
			t.Fatalf("servers not decoded: %+v", got.Servers)
		}
		if len(got.Servers[0].Warnings) != 1 {
			t.Errorf("warnings not decoded: %v", got.Servers[0].Warnings)
		}
		if len(got.Skipped) != 1 || got.Skipped[0].Name != "agenthub" {
			t.Errorf("skipped not decoded: %+v", got.Skipped)
		}
	})
}

// A catalog entry describes which credential is needed; it must have no
// field a VALUE could be assigned to (api doc.go's red line).
func TestCatalogTypesCarryNoCredentialValue(t *testing.T) {
	for _, field := range []string{"Value", "Secret", "Token", "Password"} {
		if _, ok := reflect.TypeOf(CatalogCredential{}).FieldByName(field); ok {
			t.Errorf("CatalogCredential must not have a %s field", field)
		}
		if _, ok := reflect.TypeOf(CatalogEntry{}).FieldByName(field); ok {
			t.Errorf("CatalogEntry must not have a %s field", field)
		}
	}
}
