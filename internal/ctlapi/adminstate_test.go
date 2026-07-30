package ctlapi

import (
	"net/http"
	"testing"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/integrity"
)

// observeTool seeds one approval record the way a live connection would.
func observeTool(t *testing.T, stateDir, server, tool string) {
	t.Helper()
	store, err := integrity.OpenApprovalStore(stateDir, integrity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Observe(t.Context(), server,
		integrity.ToolSnapshot{Name: tool, Description: "d"}, integrity.ModeManual); err != nil {
		t.Fatal(err)
	}
}

func toolRecord(t *testing.T, stateDir, server, tool string) integrity.ApprovalRecord {
	t.Helper()
	store, err := integrity.OpenApprovalStore(stateDir, integrity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := store.Get(t.Context(), server, tool)
	if err != nil {
		t.Fatalf("get %s/%s: %v", server, tool, err)
	}
	return rec
}

func TestToolKillSwitchAndOverride(t *testing.T) {
	env, stateDir, _ := adminServer(t, nil)
	seedServer(t, env.reg, "github", true)
	observeTool(t, stateDir, "github", "read_file")

	res := doAdmin(t, env.sock, http.MethodPut, "/v1/tools/github/read_file",
		map[string]any{"enabled": false})
	if res.status != http.StatusOK {
		t.Fatalf("disable: %d %s", res.status, res.raw)
	}
	if !toolRecord(t, stateDir, "github", "read_file").Disabled {
		t.Fatal("kill switch did not land in the approval store")
	}

	// The override is the neutralization path for a poisoned description.
	res = doAdmin(t, env.sock, http.MethodPut, "/v1/tools/github/read_file", map[string]any{
		"override_description": "reads a file",
		"override_name":        "read",
	})
	if res.status != http.StatusOK {
		t.Fatalf("override: %d %s", res.status, res.raw)
	}
	doc, err := confops.LoadToolOverrides(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.Overrides["github"]["read_file"]; got.Description != "reads a file" || got.Name != "read" {
		t.Fatalf("override = %+v", got)
	}

	var list struct {
		Generation uint64 `json:"generation"`
		Tools      []struct {
			Server              string `json:"server"`
			Tool                string `json:"tool"`
			Status              string `json:"status"`
			Disabled            bool   `json:"disabled"`
			OverrideName        string `json:"override_name"`
			OverrideDescription string `json:"override_description"`
		} `json:"tools"`
	}
	doAdmin(t, env.sock, http.MethodGet, "/v1/tools", nil).decode(t, &list)
	if len(list.Tools) != 1 {
		t.Fatalf("tools = %+v", list.Tools)
	}
	got := list.Tools[0]
	if got.Server != "github" || got.Tool != "read_file" || !got.Disabled ||
		got.Status != string(integrity.StatePending) || got.OverrideName != "read" {
		t.Fatalf("listed tool = %+v", got)
	}

	// Re-enabling never grants an approval: the status is untouched.
	if res = doAdmin(t, env.sock, http.MethodPut, "/v1/tools/github/read_file",
		map[string]any{"enabled": true, "clear_override": true}); res.status != http.StatusOK {
		t.Fatalf("enable: %d %s", res.status, res.raw)
	}
	rec := toolRecord(t, stateDir, "github", "read_file")
	if rec.Disabled || rec.Status != integrity.StatePending {
		t.Fatalf("record = %+v", rec)
	}
}

// The kill switch must not need a running server — but with no approval
// record AND no catalog cache there is nothing to name, and that is the
// uniform 404, not a silent success.
func TestToolSetUnknownToolIsUniform404(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	doAdmin(t, env.sock, http.MethodPut, "/v1/tools/github/ghost",
		map[string]any{"enabled": false}).
		wantErr(t, http.StatusNotFound, CodeNotFound)
}

// With a catalog cache the switch works offline from the connection: the
// record is materialized in the FAIL-CLOSED state (pending), never approved
// as a side effect of an operator command.
func TestToolSetUsesCatalogCacheWithoutAConnection(t *testing.T) {
	lookup := func(server, tool string) (integrity.ToolSnapshot, bool, error) {
		if server == "github" && tool == "cached" {
			return integrity.ToolSnapshot{Name: tool, Description: "from cache"}, true, nil
		}
		return integrity.ToolSnapshot{}, false, nil
	}
	env, stateDir, _ := adminServer(t, func(o *Options) { o.ToolLookup = lookup })

	res := doAdmin(t, env.sock, http.MethodPut, "/v1/tools/github/cached",
		map[string]any{"enabled": false})
	if res.status != http.StatusOK {
		t.Fatalf("disable: %d %s", res.status, res.raw)
	}
	rec := toolRecord(t, stateDir, "github", "cached")
	if !rec.Disabled || rec.Status != integrity.StatePending {
		t.Fatalf("record = %+v (creating one must never grant trust)", rec)
	}
}

func TestToolSetRequiresSomethingToSet(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	doAdmin(t, env.sock, http.MethodPut, "/v1/tools/github/read_file", map[string]any{}).
		wantErr(t, http.StatusBadRequest, CodeBadRequest)
	// Clearing an override cannot be combined with editing one.
	doAdmin(t, env.sock, http.MethodPut, "/v1/tools/github/read_file", map[string]any{
		"clear_override": true, "override_name": "x",
	}).wantErr(t, http.StatusBadRequest, confops.CodeUsage)
}

func TestToolAndQuarantineNeedAStateDir(t *testing.T) {
	_, env := startServer(t, func(o *Options) { o.StateDir = "" })
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/tools"},
		{http.MethodPut, "/v1/tools/a/b"},
		{http.MethodGet, "/v1/quarantine"},
		{http.MethodDelete, "/v1/quarantine/x"},
	} {
		doAdmin(t, env.sock, tc.method, tc.path, map[string]any{"enabled": false}).
			wantErr(t, http.StatusNotFound, CodeNotFound)
	}
}

func TestQuarantineListAndRelease(t *testing.T) {
	env, stateDir, _ := adminServer(t, nil)
	store, err := integrity.OpenQuarantineStore(stateDir, integrity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(t.Context(), "github__read_file", integrity.QuarantineEntry{
		Server: "github", Tool: "read_file", Reason: "definition drift",
		PinnedHash: "v1:aaa", CurrentHash: "v1:bbb",
	}); err != nil {
		t.Fatal(err)
	}

	var list struct {
		Generation uint64 `json:"generation"`
		Entries    []struct {
			Exposed string `json:"exposed"`
			Server  string `json:"server"`
			Tool    string `json:"tool"`
			Reason  string `json:"reason"`
			At      string `json:"at"`
		} `json:"entries"`
	}
	doAdmin(t, env.sock, http.MethodGet, "/v1/quarantine", nil).decode(t, &list)
	if len(list.Entries) != 1 || list.Entries[0].Exposed != "github__read_file" ||
		list.Entries[0].Reason != "definition drift" || list.Entries[0].At == "" {
		t.Fatalf("entries = %+v", list.Entries)
	}

	res := doAdmin(t, env.sock, http.MethodDelete, "/v1/quarantine/github__read_file", nil)
	if res.status != http.StatusOK {
		t.Fatalf("release: %d %s", res.status, res.raw)
	}
	snap, err := store.Snapshot(t.Context())
	if err != nil || len(snap) != 0 {
		t.Fatalf("snapshot = %v (%v)", snap, err)
	}

	// Releasing what is not quarantined is a miss, not a cheerful success:
	// Release is idempotent, so without the check a typo would report
	// "released" and leave the real isolation in place.
	doAdmin(t, env.sock, http.MethodDelete, "/v1/quarantine/github__read_file", nil).
		wantErr(t, http.StatusNotFound, CodeNotFound)
}

// The quarantine store is not the registry, so its guard is the WEAK form —
// it still refuses a write from an operator whose view is stale.
func TestQuarantineReleaseHonoursPrecondition(t *testing.T) {
	env, stateDir, _ := adminServer(t, nil)
	store, err := integrity.OpenQuarantineStore(stateDir, integrity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(t.Context(), "x", integrity.QuarantineEntry{Server: "s", Tool: "t"}); err != nil {
		t.Fatal(err)
	}
	res := doAdmin(t, env.sock, http.MethodDelete, "/v1/quarantine/x?expected_generation=42", nil)
	res.wantErr(t, http.StatusConflict, CodeStalePrecondition)
	if snap, _ := store.Snapshot(t.Context()); len(snap) != 1 {
		t.Error("a refused release must not have removed the entry")
	}
}
