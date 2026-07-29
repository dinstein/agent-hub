package ctlapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/audit"
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

	recs := env.aud.records()
	for _, want := range []string{
		"tools/disable:github/read_file",
		"tools/override:github/read_file",
		"tools/enable:github/read_file",
		"tools/override-clear:github/read_file",
	} {
		if len(findAudit(recs, want)) != 1 {
			t.Errorf("missing audit record %q in %+v", want, recs)
		}
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

	recs := findAudit(env.aud.records(), "quarantine/release:github__read_file")
	if len(recs) != 2 {
		t.Fatalf("want two audited attempts, got %+v", recs)
	}
	if recs[0].Decision != audit.DecisionAllowed || recs[1].Decision != audit.DecisionDenied {
		t.Errorf("decisions = %q, %q", recs[0].Decision, recs[1].Decision)
	}
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

// writeStream materializes a JSONL governance stream.
func writeStream(t *testing.T, logsDir, name string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var buf []byte
	for _, l := range lines {
		buf = append(buf, l...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(filepath.Join(logsDir, name), buf, 0o600); err != nil {
		t.Fatal(err)
	}
}

func recordLine(t *testing.T, r audit.Record) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAuditTail(t *testing.T) {
	env, _, logsDir := adminServer(t, nil)
	now := time.Now().UTC()
	lines := make([]string, 0, 5)
	for i := range 5 {
		lines = append(lines, recordLine(t, audit.Record{
			TS: now, Actor: "cli", Server: "github", Tool: string(rune('a' + i)),
			Decision: audit.DecisionAllowed, ArgsHash: "h", RequestID: "r",
		}))
	}
	// A torn last line from a crashed writer must not make the log
	// unreadable — it is skipped, not fatal.
	lines = append(lines, `{"ts":"2026-`)
	writeStream(t, logsDir, audit.AuditFileName, lines...)

	var all []audit.Record
	doAdmin(t, env.sock, http.MethodGet, "/v1/audit", nil).decode(t, &all)
	if len(all) != 5 {
		t.Fatalf("got %d records", len(all))
	}
	if all[0].Tool != "a" || all[4].Tool != "e" {
		t.Errorf("order is not oldest-first: %v", all)
	}
	// The args red line: the record type has no field for arguments, so
	// there is nothing here to leak.
	if all[0].ArgsHash != "h" {
		t.Errorf("argsHash lost: %+v", all[0])
	}

	var tail []audit.Record
	doAdmin(t, env.sock, http.MethodGet, "/v1/audit?limit=2", nil).decode(t, &tail)
	if len(tail) != 2 || tail[0].Tool != "d" || tail[1].Tool != "e" {
		t.Fatalf("tail = %+v", tail)
	}

	doAdmin(t, env.sock, http.MethodGet, "/v1/audit?limit=-1", nil).
		wantErr(t, http.StatusBadRequest, CodeBadRequest)
	doAdmin(t, env.sock, http.MethodGet, "/v1/audit?stream=savings", nil).
		wantErr(t, http.StatusBadRequest, CodeBadRequest)
}

func TestSecurityTail(t *testing.T) {
	env, _, logsDir := adminServer(t, nil)
	ev := audit.SecurityEvent{
		TS: time.Now().UTC(), Event: "injection.blocked", Severity: audit.SeverityCritical,
		Server: "github", Tool: "read_file", Detail: "pattern 3",
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	writeStream(t, logsDir, audit.SecurityFileName, string(b))

	// Both spellings the api client uses reach the same implementation.
	for _, path := range []string{"/v1/security", "/v1/audit?stream=security"} {
		var out []audit.SecurityEvent
		doAdmin(t, env.sock, http.MethodGet, path, nil).decode(t, &out)
		if len(out) != 1 || out[0].Event != "injection.blocked" || out[0].Severity != audit.SeverityCritical {
			t.Fatalf("%s: %+v", path, out)
		}
	}
}

// A stream that has never been written is an EMPTY tail, not an error: the
// daemon may simply not have logged anything yet.
func TestAuditTailOnMissingFileIsEmpty(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	var out []audit.Record
	res := doAdmin(t, env.sock, http.MethodGet, "/v1/audit", nil)
	res.decode(t, &out)
	if len(out) != 0 {
		t.Fatalf("out = %+v", out)
	}
	if string(res.Data) != "[]" {
		t.Errorf("data = %s, want an empty array (a frontend must not decode null)", res.Data)
	}
}

// Without a logs directory the two routes answer the uniform 404 — the same
// "unavailable on this daemon" shape a frontend already handles, never an
// empty list that would read as "nothing was logged".
func TestAuditTailNeedsALogsDir(t *testing.T) {
	_, env := startServer(t, func(o *Options) { o.LogsDir = "" })
	doAdmin(t, env.sock, http.MethodGet, "/v1/audit", nil).
		wantErr(t, http.StatusNotFound, CodeNotFound)
	doAdmin(t, env.sock, http.MethodGet, "/v1/security", nil).
		wantErr(t, http.StatusNotFound, CodeNotFound)
}

// The tail is bounded on the server: a client-side clamp is not a trusted
// bound.
func TestAuditTailClampsLimit(t *testing.T) {
	env, _, logsDir := adminServer(t, nil)
	lines := make([]string, 0, maxAuditTail+10)
	for i := range maxAuditTail + 10 {
		lines = append(lines, recordLine(t, audit.Record{Tool: string(rune('a' + i%26))}))
	}
	writeStream(t, logsDir, audit.AuditFileName, lines...)

	var out []audit.Record
	doAdmin(t, env.sock, http.MethodGet, "/v1/audit?limit=99999", nil).decode(t, &out)
	if len(out) != maxAuditTail {
		t.Fatalf("got %d records, want the %d ceiling", len(out), maxAuditTail)
	}
}
