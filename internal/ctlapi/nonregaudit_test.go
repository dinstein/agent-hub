package ctlapi

import (
	"encoding/base64"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/accesslog"
	"github.com/dinstein/agent-hub/internal/secrets"
)

func seedControlAuditCall(t *testing.T, root, callID string, key []byte) string {
	t.Helper()
	keyID, err := accesslog.KeyID(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := accesslog.Open(accesslog.Options{
		Root: root, Key: key, KeyID: keyID, Durability: accesslog.DurabilitySync,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Add(-time.Minute)
	request, err := store.PutPayload(ts, callID, accesslog.PayloadRequest,
		[]byte(`{"name":"srv__tool","arguments":{"secret":"request-value"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(accesslog.Event{
		TS: ts, Kind: accesslog.EventReceived, CallID: callID,
		Client: "codex", Face: "stdio", Exposed: "srv__tool", Request: &request,
	}); err != nil {
		t.Fatal(err)
	}
	args, err := store.PutPayload(ts.Add(time.Millisecond), callID,
		accesslog.PayloadEffectiveArgs, []byte(`{"secret":"request-value"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(accesslog.Event{
		TS: ts.Add(time.Millisecond), Kind: accesslog.EventRouted, CallID: callID,
		Client: "codex", Exposed: "srv__tool", Server: "srv", Tool: "tool",
		EffectiveArgs: &args,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := store.PutPayload(ts.Add(2*time.Millisecond), callID,
		accesslog.PayloadResult, []byte(`{"content":[{"type":"text","text":"result-value"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(accesslog.Event{
		TS: ts.Add(2 * time.Millisecond), Kind: accesslog.EventFinished, CallID: callID,
		Client: "codex", Exposed: "srv__tool", Server: "srv", Tool: "tool",
		Outcome: "success", DurationMs: 12, Result: &result, ResultCapture: "full",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return keyID
}

func TestAuditListIsMetadataOnlyAndDetailDecryptsImmediately(t *testing.T) {
	root := filepath.Join(t.TempDir(), "audit")
	key := []byte("0123456789abcdef0123456789abcdef")
	keyID := seedControlAuditCall(t, root, "call-for-gui", key)
	vault := newNRVault()
	vault.stored[secrets.AuditEncryptionKeyRef(keyID).StorageKey()] =
		base64.RawStdEncoding.EncodeToString(key)
	client, env := startServer(t, func(o *Options) {
		o.NonRegistry.AuditRoot = root
		o.NonRegistry.AuditKeys = vault
	})

	status, err := client.Audit.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.Storage.PackFiles == 0 || status.Storage.Bytes == 0 {
		t.Fatalf("status storage = %+v", status.Storage)
	}

	code, raw := nrDo(t, env.sock, http.MethodGet, "/v1/audit/calls", nil)
	if code != http.StatusOK {
		t.Fatalf("calls status = %d: %s", code, raw)
	}
	if strings.Contains(string(raw), "request-value") || strings.Contains(string(raw), "result-value") {
		t.Fatalf("metadata listing leaked decrypted payload: %s", raw)
	}
	var calls api.AuditCalls
	nrData(t, raw, &calls)
	if len(calls.Calls) != 1 || calls.Calls[0].Server != "srv" || !calls.Calls[0].Complete {
		t.Fatalf("calls = %+v", calls)
	}

	detail, err := client.Audit.Call(t.Context(), "call-for-gui")
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

	stats, err := client.Audit.Stats(t.Context(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Calls != 1 || stats.Outcomes["success"] != 1 || stats.PayloadRaw == 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestAuditEnableCreatesKeyAndCanDisable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "audit")
	vault := newNRVault()
	client, _ := startServer(t, func(o *Options) {
		o.NonRegistry.AuditRoot = root
		o.NonRegistry.AuditKeys = vault
	})
	status, err := client.Audit.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := client.Audit.SetEnabled(t.Context(), true, status.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.Enabled || enabled.KeyID == "" || len(vault.stored) != 2 {
		t.Fatalf("enabled = %+v, stored keys = %d", enabled, len(vault.stored))
	}
	disabled, err := client.Audit.SetEnabled(t.Context(), false, enabled.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled || len(vault.stored) != 2 {
		t.Fatalf("disabled = %+v, stored keys = %d", disabled, len(vault.stored))
	}
}
