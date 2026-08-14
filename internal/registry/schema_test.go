package registry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDocAndRoundTrip installs input as <kind>.json, opens the registry,
// runs mutate inside an Update, and compares the resulting file against
// testdata/<kind>.golden.json (regenerate with -update).
func writeDocAndRoundTrip(t *testing.T, kind DocKind, input string, mutate func(tx *Tx) error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(docPath(dir, kind), []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	st := mustOpen(t, dir)
	if err := st.Update(context.Background(), mutate); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(docPath(dir, kind))
	if err != nil {
		t.Fatal(err)
	}
	goldenPath := filepath.Join("testdata", string(kind)+".golden.json")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to regenerate): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s.json does not match golden\n got: %s\nwant: %s", kind, got, want)
	}
}

func TestProfilesRoundTripGolden(t *testing.T) {
	input := `{
  "future_top": {"a": 1},
  "profiles": {
    "dev": {
      "servers": ["github", "filesystem"],
      "tools": {
        "github": {"allow": ["create_issue", "get_issue"], "future_sel": true},
        "filesystem": {"allow": [], "deny": ["delete_file"]}
      },
      "future_profile": "keep-me"
    },
    "readonly": {"servers": []}
  }
}
`
	writeDocAndRoundTrip(t, DocProfiles, input, func(tx *Tx) error {
		p := tx.Profiles.V.Profiles["dev"]
		p.V.Servers = append(p.V.Servers, "stripe") // edit a known field
		tx.Profiles.V.Profiles["dev"] = p
		return nil
	})
}

func TestClientsRoundTripGolden(t *testing.T) {
	// The retired client-layer fields (discovery / servers / tools /
	// resultBudget / approval) are deliberately still in the input: they are
	// now unknown to ClientEntry, so this doubles as the proof that a legacy
	// clients.json survives a rewrite instead of being silently truncated.
	// doctor's scope:projects check is what tells the operator such a block
	// stopped applying; losing it here would destroy the evidence.
	input := `{
  "future_top": [1, 2],
  "clients": {
    "claude-code": {
      "profile": "dev",
      "discovery": "lazy",
      "servers": ["github", "filesystem"],
      "tools": {
        "filesystem": {"deny": ["delete_file"]}
      },
      "resultBudget": {"*": {"bytes": 49152, "future_budget": "x"}},
      "approval": {"confirmDestructive": true},
      "future_client": "keep-me"
    },
    "openwebui": {"profileRef": {"kind": "followActive"}, "discovery": "full"}
  }
}
`
	writeDocAndRoundTrip(t, DocClients, input, func(tx *Tx) error {
		c := tx.Clients.V.Clients["claude-code"]
		c.V.Profile = "dev2" // edit a known field
		tx.Clients.V.Clients["claude-code"] = c
		return nil
	})
}

func TestGovernanceRoundTripGolden(t *testing.T) {
	input := `{
  "calls": {"enabled": true, "future_audit": {"mode": "keep"}},
  "blockOnInjection": true,
  "denyDestructive": true,
  "discovery": "lazy",
  "resultBudget": {"*": {"bytes": 65536, "forced": true}},
  "future_governance": {"flag": false}
}
`
	writeDocAndRoundTrip(t, DocGovernance, input, func(tx *Tx) error {
		return nil
	})
}

// TestToolSelectorTriState pins the three-state contract: nil Allow (full
// server set) must be omitted on disk, while empty Allow (block-all) must
// survive as [] — collapsing it to nil would silently fail open.
func TestToolSelectorTriState(t *testing.T) {
	cases := []struct {
		name string
		in   string
		nil_ bool
	}{
		{"absent allow stays nil", `{}`, true},
		{"empty allow stays empty non-nil", `{"allow": []}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var d Doc[ToolSelector]
			if err := json.Unmarshal([]byte(tc.in), &d); err != nil {
				t.Fatal(err)
			}
			if got := d.V.Allow == nil; got != tc.nil_ {
				t.Fatalf("Allow == nil is %v, want %v", got, tc.nil_)
			}
			out, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			var back Doc[ToolSelector]
			if err := json.Unmarshal(out, &back); err != nil {
				t.Fatal(err)
			}
			if got := back.V.Allow == nil; got != tc.nil_ {
				t.Fatalf("after round-trip (%s): Allow == nil is %v, want %v", out, got, tc.nil_)
			}
		})
	}
}

func TestProfileBindingResolution(t *testing.T) {
	named := func(k ProfileBindingKind, n string) ProfileBinding {
		return ProfileBinding{Kind: k, Name: n}
	}
	explicit := &Doc[ProfileBinding]{V: named(BindingFollowActive, "")}

	cases := []struct {
		name string
		got  ProfileBinding
		want ProfileBinding
	}{
		{"client default is followActive", ClientEntry{}.Binding(), named(BindingFollowActive, "")},
		{"profile shorthand means named", ClientEntry{Profile: "dev"}.Binding(), named(BindingNamed, "dev")},
		{
			"explicit ref wins over shorthand",
			ClientEntry{Profile: "dev", ProfileRef: explicit}.Binding(),
			named(BindingFollowActive, ""),
		},
		{
			"empty-kind ref falls back to shorthand",
			ClientEntry{Profile: "dev", ProfileRef: &Doc[ProfileBinding]{}}.Binding(),
			named(BindingNamed, "dev"),
		},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, tc.got, tc.want)
		}
	}
}

// TestServerEntryHTTPFieldsRoundTrip pins the M1 schema extension: the http
// fields persist, unknown members still survive (Doc[T]), and a stdio entry
// gains no empty http members on disk (an old binary must keep reading the
// file the new one writes).
func TestServerEntryHTTPFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	input := `{
  "servers": {
    "remote": {
      "transport": "http",
      "url": "https://mcp.example.com/mcp",
      "headers": {"X-Api-Key": "${SECRET_API}"},
      "oauth": {"issuer": "https://as.example.com", "scopes": ["read"]},
      "provenance": "remote",
      "enabled": true,
      "future_entry": {"keep": true}
    },
    "local": {"transport": "stdio", "command": "npx", "enabled": true}
  }
}
`
	if err := os.WriteFile(docPath(dir, DocServers), []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	st := mustOpen(t, dir)
	snap := st.Snapshot()

	remote := snap.Servers.V.Servers["remote"].V
	if remote.URL != "https://mcp.example.com/mcp" || remote.Headers["X-Api-Key"] != "${SECRET_API}" {
		t.Fatalf("remote entry = %+v", remote)
	}
	if remote.OAuth == nil || remote.OAuth.Issuer != "https://as.example.com" || len(remote.OAuth.Scopes) != 1 {
		t.Fatalf("oauth hint = %+v", remote.OAuth)
	}
	if !remote.IsHTTP() || remote.TransportName() != TransportHTTP {
		t.Fatalf("transport helpers disagree: %+v", remote)
	}
	local := snap.Servers.V.Servers["local"].V
	if local.IsHTTP() || local.TransportName() != TransportStdio {
		t.Fatalf("stdio entry = %+v", local)
	}

	// Rewrite and re-read: unknown members survive and the stdio entry does
	// not acquire empty url/headers members.
	if err := st.Update(context.Background(), func(tx *Tx) error {
		e := tx.Servers.V.Servers["remote"]
		e.V.Enabled = false
		tx.Servers.V.Servers["remote"] = e
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(docPath(dir, DocServers))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"future_entry"`) {
		t.Errorf("unknown member dropped:\n%s", data)
	}
	var doc struct {
		Servers map[string]map[string]json.RawMessage `json:"servers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"url", "headers", "oauth", "provenance"} {
		if _, ok := doc.Servers["local"][k]; ok {
			t.Errorf("stdio entry acquired an empty %q member:\n%s", k, data)
		}
	}
	if _, ok := doc.Servers["remote"]["command"]; ok {
		t.Errorf("http entry acquired an empty command member:\n%s", data)
	}
}

// TestServerEntryDefaultsToStdio: entries written before the transport
// field existed (or with it empty) must keep working as stdio.
func TestServerEntryDefaultsToStdio(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(docPath(dir, DocServers),
		[]byte(`{"servers":{"legacy":{"command":"npx","enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := mustOpen(t, dir).Snapshot().Servers.V.Servers["legacy"].V
	if e.TransportName() != TransportStdio || e.IsHTTP() {
		t.Fatalf("legacy entry = %+v", e)
	}
}

// TestServerRuntimeRoundTrip pins the M2 runtime extension (docs/subsystems/registry.md
// ): the docker block persists intact, and — the backward-compatibility
// half — a host-runtime entry acquires no `runtime`/`docker` members on
// disk, so a pre-M2 binary keeps reading what an M2 binary writes.
func TestServerRuntimeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	input := `{
  "servers": {
    "sandboxed": {
      "transport": "stdio",
      "command": "node",
      "args": ["/app/server.js"],
      "env": {"TOKEN": "${SECRET_T}"},
      "runtime": "docker",
      "docker": {
        "image": "ghcr.io/example/mcp:1",
        "network": "bridge",
        "mounts": [{"source": "/srv/data", "target": "/data"}, {"source": "/w", "write": true}],
        "memory": "512m",
        "cpus": "1.5"
      },
      "enabled": true,
      "future_entry": {"keep": true}
    },
    "plain": {"command": "npx", "enabled": true}
  }
}
`
	if err := os.WriteFile(docPath(dir, DocServers), []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	st := mustOpen(t, dir)
	snap := st.Snapshot()

	box := snap.Servers.V.Servers["sandboxed"].V
	if !box.IsDocker() || box.RuntimeName() != RuntimeDocker {
		t.Fatalf("runtime helpers disagree: %+v", box)
	}
	if box.Docker == nil || box.Docker.Image != "ghcr.io/example/mcp:1" {
		t.Fatalf("docker block = %+v", box.Docker)
	}
	if len(box.Docker.Mounts) != 2 || box.Docker.Mounts[0].Write || !box.Docker.Mounts[1].Write {
		t.Fatalf("mounts = %+v (read-only must be the default)", box.Docker.Mounts)
	}
	if err := box.ValidateRuntime(); err != nil {
		t.Fatalf("ValidateRuntime: %v", err)
	}

	plain := snap.Servers.V.Servers["plain"].V
	if plain.IsDocker() || plain.RuntimeName() != RuntimeHost {
		t.Fatalf("entry without a runtime field must default to host: %+v", plain)
	}

	if err := st.Update(context.Background(), func(tx *Tx) error {
		e := tx.Servers.V.Servers["plain"]
		e.V.Enabled = false
		tx.Servers.V.Servers["plain"] = e
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(docPath(dir, DocServers))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"future_entry"`) {
		t.Errorf("unknown entry member dropped:\n%s", data)
	}
	if !strings.Contains(string(data), `"ghcr.io/example/mcp:1"`) {
		t.Errorf("docker block lost on rewrite:\n%s", data)
	}
	var doc struct {
		Servers map[string]map[string]json.RawMessage `json:"servers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"runtime", "docker"} {
		if _, ok := doc.Servers["plain"][k]; ok {
			t.Errorf("host-runtime entry acquired an empty %q member:\n%s", k, data)
		}
	}
}

// TestValidateRuntimeFailsClosed: an unrecognised runtime name is refused
// rather than silently downgraded to host — a typo must not drop isolation.
func TestValidateRuntimeFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		entry   ServerEntry
		wantErr bool
	}{
		{"host default", ServerEntry{Command: "npx"}, false},
		{"explicit host", ServerEntry{Command: "npx", Runtime: RuntimeHost}, false},
		{"docker ok", ServerEntry{Command: "npx", Runtime: RuntimeDocker, Docker: &DockerRuntime{Image: "a"}}, false},
		{"typo", ServerEntry{Command: "npx", Runtime: "dcoker"}, true},
		{"docker without image", ServerEntry{Command: "npx", Runtime: RuntimeDocker}, true},
		{"docker with blank image", ServerEntry{Command: "npx", Runtime: RuntimeDocker, Docker: &DockerRuntime{Image: "  "}}, true},
		{"docker on http transport", ServerEntry{Transport: TransportHTTP, URL: "https://x", Runtime: RuntimeDocker, Docker: &DockerRuntime{Image: "a"}}, true},
		{"host with docker block", ServerEntry{Command: "npx", Docker: &DockerRuntime{Image: "a"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.entry.ValidateRuntime()
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateRuntime = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
