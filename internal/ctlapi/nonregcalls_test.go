package ctlapi

import (
	"encoding/base64"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/calllog"
	"github.com/dinstein/agent-hub/internal/secrets"
)

func seedControlAuditCall(t *testing.T, root, callID string, key []byte) string {
	return seedControlAuditCallAt(t, root, callID, key, time.Now().UTC().Add(-time.Minute), "codex", "srv", "tool")
}

func seedControlAuditCallAt(
	t *testing.T, root, callID string, key []byte, ts time.Time, client, server, tool string,
) string {
	t.Helper()
	keyID, err := calllog.KeyID(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := calllog.Open(calllog.Options{
		Root: root, Key: key, KeyID: keyID, Durability: calllog.DurabilitySync,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := store.PutPayload(ts, callID, calllog.PayloadRequest,
		[]byte(`{"name":"srv__tool","arguments":{"secret":"request-value"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(calllog.Event{
		TS: ts, Kind: calllog.EventReceived, CallID: callID,
		Client: client, Face: "stdio", Exposed: server + "__" + tool, Request: &request,
	}); err != nil {
		t.Fatal(err)
	}
	args, err := store.PutPayload(ts.Add(time.Millisecond), callID,
		calllog.PayloadEffectiveArgs, []byte(`{"secret":"request-value"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(calllog.Event{
		TS: ts.Add(time.Millisecond), Kind: calllog.EventRouted, CallID: callID,
		Client: client, Exposed: server + "__" + tool, Server: server, Tool: tool,
		EffectiveArgs: &args,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := store.PutPayload(ts.Add(2*time.Millisecond), callID,
		calllog.PayloadResult, []byte(`{"content":[{"type":"text","text":"result-value"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(calllog.Event{
		TS: ts.Add(2 * time.Millisecond), Kind: calllog.EventFinished, CallID: callID,
		Client: client, Exposed: server + "__" + tool, Server: server, Tool: tool,
		Outcome: "success", DurationMs: 12, Result: &result, ResultCapture: "full",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return keyID
}

func TestCallPageUseStableCursorAndServerSideFilters(t *testing.T) {
	root := filepath.Join(t.TempDir(), "calls")
	key := []byte("0123456789abcdef0123456789abcdef")
	base := time.Now().UTC().Add(-time.Minute)
	seedControlAuditCallAt(t, root, "call-alpha", key, base.Add(3*time.Second), "claude", "linear", "list_projects")
	seedControlAuditCallAt(t, root, "call-beta", key, base.Add(2*time.Second), "codex", "github", "get_issue")
	seedControlAuditCallAt(t, root, "call-gamma", key, base.Add(time.Second), "codex", "linear", "get_project")
	client, env := startServer(t, func(o *Options) { o.NonRegistry.CallsRoot = root })

	first, err := client.Calls.List(t.Context(), api.CallFilter{Since: base, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 3 || len(first.Calls) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %+v", first)
	}
	second, err := client.Calls.List(t.Context(), api.CallFilter{
		Since: base, Limit: 2, Cursor: first.NextCursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Total != 3 || len(second.Calls) != 1 || second.NextCursor != "" {
		t.Fatalf("second page = %+v", second)
	}
	seen := map[string]bool{}
	for _, row := range append(first.Calls, second.Calls...) {
		if seen[row.CallID] {
			t.Fatalf("call %q appeared on more than one page", row.CallID)
		}
		seen[row.CallID] = true
	}

	filtered, err := client.Calls.List(t.Context(), api.CallFilter{
		Since: base, Limit: 10, Query: "PROJECTS", Client: "claude", Server: "linear",
	})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Total != 1 || len(filtered.Calls) != 1 || filtered.Calls[0].CallID != "call-alpha" {
		t.Fatalf("filtered calls = %+v", filtered)
	}
	toolFiltered, err := client.Calls.List(t.Context(), api.CallFilter{
		Since: base, Limit: 10, Tool: "get_project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if toolFiltered.Total != 1 || len(toolFiltered.Calls) != 1 || toolFiltered.Calls[0].CallID != "call-gamma" {
		t.Fatalf("tool-filtered calls = %+v", toolFiltered)
	}

	if status, _ := nrDo(t, env.sock, http.MethodGet, "/v1/calls?cursor=not-a-cursor", nil); status != http.StatusBadRequest {
		t.Fatalf("invalid cursor status = %d, want %d", status, http.StatusBadRequest)
	}
}

func TestCallsListIsMetadataOnlyAndDetailDecryptsImmediately(t *testing.T) {
	root := filepath.Join(t.TempDir(), "calls")
	key := []byte("0123456789abcdef0123456789abcdef")
	keyID := seedControlAuditCall(t, root, "call-for-gui", key)
	vault := newNRVault()
	vault.stored[secrets.AuditEncryptionKeyRef(keyID).StorageKey()] =
		base64.RawStdEncoding.EncodeToString(key)
	client, env := startServer(t, func(o *Options) {
		o.NonRegistry.CallsRoot = root
		o.NonRegistry.CallsKeys = vault
	})

	status, err := client.Calls.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.Storage.PackFiles == 0 || status.Storage.Bytes == 0 {
		t.Fatalf("status storage = %+v", status.Storage)
	}

	code, raw := nrDo(t, env.sock, http.MethodGet, "/v1/calls", nil)
	if code != http.StatusOK {
		t.Fatalf("calls status = %d: %s", code, raw)
	}
	if strings.Contains(string(raw), "request-value") || strings.Contains(string(raw), "result-value") {
		t.Fatalf("metadata listing leaked decrypted payload: %s", raw)
	}
	var calls api.CallPage
	nrData(t, raw, &calls)
	if len(calls.Calls) != 1 || calls.Calls[0].Server != "srv" || !calls.Calls[0].Complete {
		t.Fatalf("calls = %+v", calls)
	}

	detail, err := client.Calls.Get(t.Context(), "call-for-gui")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail.Request.Text, "request-value") ||
		!strings.Contains(detail.EffectiveArguments.Text, "request-value") ||
		!strings.Contains(detail.Result.Text, "result-value") {
		t.Fatalf("detail did not decrypt all payloads: %+v", detail)
	}
	if len(detail.Events) != 3 || detail.Outcome != "success" || detail.DurationMs != 12 {
		t.Fatalf("detail lifecycle = %+v", detail)
	}

	stats, err := client.Calls.Stats(t.Context(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Calls != 1 || stats.Outcomes["success"] != 1 || stats.PayloadRaw == 0 ||
		stats.ServerTools["srv"]["tool"] != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	verified, err := client.Calls.Verify(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !verified.OK || verified.Events != 3 || verified.Payloads != 3 {
		t.Fatalf("verify = %+v", verified)
	}
}

func TestCallsEnableCreatesKeyAndCanDisable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "calls")
	vault := newNRVault()
	client, _ := startServer(t, func(o *Options) {
		o.NonRegistry.CallsRoot = root
		o.NonRegistry.CallsKeys = vault
	})
	status, err := client.Calls.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := client.Calls.SetEnabled(t.Context(), true, status.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.Enabled || enabled.KeyID == "" || len(vault.stored) != 2 {
		t.Fatalf("enabled = %+v, stored keys = %d", enabled, len(vault.stored))
	}
	rotated, err := client.Calls.RotateKey(t.Context(), enabled.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.PreviousKeyID != enabled.KeyID || rotated.KeyID == enabled.KeyID || len(vault.stored) != 3 {
		t.Fatalf("rotated = %+v, stored keys = %d", rotated, len(vault.stored))
	}
	status, err = client.Calls.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := client.Calls.SetEnabled(t.Context(), false, status.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled || len(vault.stored) != 3 {
		t.Fatalf("disabled = %+v, stored keys = %d", disabled, len(vault.stored))
	}
}
