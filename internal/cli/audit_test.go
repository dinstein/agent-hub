package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/accesslog"
	"github.com/dinstein/agent-hub/internal/secrets"
)

func TestAuditDefaultsAreBoundedAndStrict(t *testing.T) {
	setDataDir(t)
	var got AuditStatus
	decodeInto(t, mustRun(t, "", "audit", "status", "--json"), &got)
	if got.Enabled || got.Arguments != "full" || got.Results != "truncated" || got.ResultBytes <= 0 {
		t.Fatalf("default audit policy = %+v", got)
	}
	if got.Durability != "sync" || got.RetentionDays <= 0 || got.MaxBytes <= 0 || got.MinFreeBytes <= 0 || got.Pressure != "block" {
		t.Fatalf("default bounds = %+v", got)
	}
}

func TestAuditEnableCreatesOneVaultKeyAndDisableKeepsIt(t *testing.T) {
	dir := setDataDir(t)
	t.Setenv(secrets.EnvDevSecrets, "1")

	var enabled AuditStatus
	decodeInto(t, mustRun(t, "", "audit", "enable", "--json"), &enabled)
	if !enabled.Enabled || enabled.KeyID == "" {
		t.Fatalf("enabled = %+v", enabled)
	}
	firstID := enabled.KeyID

	// Re-enabling reuses the key, which is required to keep old payloads
	// readable and makes the operation idempotent.
	decodeInto(t, mustRun(t, "", "audit", "enable", "--json"), &enabled)
	if enabled.KeyID != firstID {
		t.Fatalf("second enable key id = %q, want %q", enabled.KeyID, firstID)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "registry", "governance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	audit, ok := doc["audit"].(map[string]any)
	if !ok || audit["keyId"] != firstID {
		t.Fatalf("governance audit metadata = %#v", doc["audit"])
	}
	if _, exists := audit["key"]; exists {
		t.Fatalf("governance contains key material: %s", raw)
	}

	var disabled AuditStatus
	decodeInto(t, mustRun(t, "", "audit", "disable", "--json"), &disabled)
	if disabled.Enabled || disabled.KeyID != firstID {
		t.Fatalf("disabled = %+v", disabled)
	}
	decodeInto(t, mustRun(t, "", "audit", "enable", "--json"), &enabled)
	if enabled.KeyID != firstID {
		t.Fatalf("enable after disable key id = %q, want %q", enabled.KeyID, firstID)
	}
}

func TestAuditCannotBeEnabledThroughConfigBeforeKeyCreation(t *testing.T) {
	setDataDir(t)
	code, _, _ := runCLI(t, "", "config", "set", "audit.enabled", "true")
	if code != ExitUsage {
		t.Fatalf("config enable without key exit = %d, want %d", code, ExitUsage)
	}
}

func TestAuditPolicyConfig(t *testing.T) {
	setDataDir(t)
	for _, pair := range [][2]string{
		{"audit.durability", "write"},
		{"audit.results", "errors"},
		{"audit.result_bytes", "4096"},
		{"audit.retention_days", "7"},
		{"audit.max_bytes", "1048576"},
		{"audit.min_free_bytes", "524288"},
	} {
		mustRun(t, "", "config", "set", pair[0], pair[1])
	}
	var got AuditStatus
	decodeInto(t, mustRun(t, "", "audit", "status", "--json"), &got)
	if got.Durability != "write" || got.Results != "errors" || got.ResultBytes != 4096 ||
		got.RetentionDays != 7 || got.MaxBytes != 1048576 || got.MinFreeBytes != 524288 {
		t.Fatalf("configured audit policy = %+v", got)
	}
	for _, pair := range [][2]string{
		{"audit.durability", "eventually"}, {"audit.results", "sometimes"},
		{"audit.resultBytes", "0"}, {"audit.retentionDays", "0"},
		{"audit.maxBytes", "-1"}, {"audit.minFreeBytes", "nope"},
	} {
		if code, _, _ := runCLI(t, "", "config", "set", pair[0], pair[1]); code != ExitUsage {
			t.Errorf("config set %s %s exit = %d, want %d", pair[0], pair[1], code, ExitUsage)
		}
	}
}

func seedCLIAuditCall(t *testing.T, dataDir, callID string) string {
	t.Helper()
	chain := secrets.NewChain(secrets.ChainConfig{Dir: filepath.Join(dataDir, "secrets")})
	encoded, ok, err := chain.Get(context.Background(), secrets.AuditEncryptionRef())
	if err != nil || !ok {
		t.Fatalf("load audit key: ok=%v err=%v", ok, err)
	}
	key, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	defer zeroSecret(key)
	keyID, err := accesslog.KeyID(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := accesslog.Open(accesslog.Options{
		Root: filepath.Join(dataDir, accesslog.DirectoryName), Key: key, KeyID: keyID,
		Durability: accesslog.DurabilitySync,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Add(-time.Minute)
	request, err := store.PutPayload(ts, callID, accesslog.PayloadRequest, []byte(`{"name":"srv__tool","arguments":{"secret":"request-value"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(accesslog.Event{TS: ts, Kind: accesslog.EventReceived, CallID: callID, Client: "codex", Face: "stdio", Request: &request}); err != nil {
		t.Fatal(err)
	}
	args, err := store.PutPayload(ts.Add(time.Millisecond), callID, accesslog.PayloadEffectiveArgs, []byte(`{"secret":"request-value"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(accesslog.Event{TS: ts.Add(time.Millisecond), Kind: accesslog.EventRouted, CallID: callID, Client: "codex", Exposed: "srv__tool", Server: "srv", Tool: "tool", EffectiveArgs: &args}); err != nil {
		t.Fatal(err)
	}
	result, err := store.PutPayload(ts.Add(2*time.Millisecond), callID, accesslog.PayloadResult, []byte(`{"content":[{"type":"text","text":"result-value"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(accesslog.Event{TS: ts.Add(2 * time.Millisecond), Kind: accesslog.EventFinished, CallID: callID, Client: "codex", Exposed: "srv__tool", Server: "srv", Tool: "tool", Outcome: "success", Result: &result, ResultMode: "full", ResultCapture: "full"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return ts.Format("2006-01-02")
}

func TestAuditReadCommandsAndIntegrityVerification(t *testing.T) {
	dir := setDataDir(t)
	t.Setenv(secrets.EnvDevSecrets, "1")
	var enabled AuditStatus
	decodeInto(t, mustRun(t, "", "audit", "enable", "--json"), &enabled)
	day := seedCLIAuditCall(t, dir, "call-for-cli")

	var tail AuditTail
	decodeInto(t, mustRun(t, "", "audit", "tail", "--since", "all", "--json"), &tail)
	if len(tail.Events) != 3 || tail.Events[1].Server != "srv" {
		t.Fatalf("tail = %+v", tail)
	}

	var stats AuditStats
	decodeInto(t, mustRun(t, "", "audit", "stats", "--since", "all", "--json"), &stats)
	if stats.Calls != 1 || stats.Events != 3 || stats.Outcomes["success"] != 1 || stats.PayloadRaw == 0 {
		t.Fatalf("stats = %+v", stats)
	}

	var metadata AuditCall
	decodeInto(t, mustRun(t, "", "audit", "show", "call-for-cli", "--json"), &metadata)
	if metadata.Request != "" || len(metadata.Events) != 3 {
		t.Fatalf("metadata-only show = %+v", metadata)
	}
	var full AuditCall
	out := mustRun(t, "", "audit", "show", "call-for-cli", "--payloads", "--json")
	decodeInto(t, out, &full)
	if !strings.Contains(full.Request, "request-value") || !strings.Contains(full.Result, "result-value") {
		t.Fatalf("payload show = %+v", full)
	}
	if len(decodeEnvelope(t, out).Warnings) == 0 {
		t.Fatal("payload show did not warn about sensitive data")
	}

	var verify AuditVerify
	decodeInto(t, mustRun(t, "", "audit", "verify", "--json"), &verify)
	if !verify.OK || verify.Events != 3 || verify.Payloads != 3 {
		t.Fatalf("verify = %+v", verify)
	}
	var status AuditStatus
	decodeInto(t, mustRun(t, "", "audit", "status", "--json"), &status)
	if status.Storage.Bytes <= 0 || status.Storage.PackFiles != 1 {
		t.Fatalf("status storage = %+v", status.Storage)
	}

	path := filepath.Join(dir, accesslog.DirectoryName, day, accesslog.EventFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"outcome":"success"`, `"outcome":"failure"`, 1)
	if tampered == string(raw) {
		t.Fatal("test did not find the outcome to tamper")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "", "audit", "verify", "--json")
	if code != ExitLocked {
		t.Fatalf("tampered verify exit = %d, want %d (stderr %s)", code, ExitLocked, stderr)
	}
	decodeInto(t, stdout, &verify)
	if verify.OK || verify.Failures == 0 {
		t.Fatalf("tampered verify = %+v", verify)
	}
}

func TestAuditExportPruneAndKeyRotation(t *testing.T) {
	dir := setDataDir(t)
	t.Setenv(secrets.EnvDevSecrets, "1")
	var enabled AuditStatus
	decodeInto(t, mustRun(t, "", "audit", "enable", "--json"), &enabled)
	seedCLIAuditCall(t, dir, "before-rotation")

	var rotated AuditKeyRotation
	decodeInto(t, mustRun(t, "", "audit", "rotate-key", "--json"), &rotated)
	if rotated.PreviousKeyID != enabled.KeyID || rotated.KeyID == "" || rotated.KeyID == enabled.KeyID || !rotated.Enabled {
		t.Fatalf("rotation = %+v, enabled = %+v", rotated, enabled)
	}
	seedCLIAuditCall(t, dir, "after-rotation")

	// Historical and new entries must both verify and decrypt after rotation.
	var verify AuditVerify
	decodeInto(t, mustRun(t, "", "audit", "verify", "--json"), &verify)
	if !verify.OK || verify.Events != 6 || verify.Payloads != 6 {
		t.Fatalf("post-rotation verify = %+v", verify)
	}
	var old AuditCall
	decodeInto(t, mustRun(t, "", "audit", "show", "before-rotation", "--payloads", "--json"), &old)
	if !strings.Contains(old.Request, "request-value") {
		t.Fatalf("old payload after rotation = %+v", old)
	}

	metadataPath := filepath.Join(dir, "metadata.jsonl")
	var metadataExport AuditExport
	decodeInto(t, mustRun(t, "", "audit", "export", "--output", metadataPath, "--json"), &metadataExport)
	metadataRaw, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if metadataExport.Events != 6 || strings.Contains(string(metadataRaw), "request-value") {
		t.Fatalf("metadata export = %+v, data=%s", metadataExport, metadataRaw)
	}
	if code, _, _ := runCLI(t, "", "audit", "export", "--output", metadataPath); code != ExitUsage {
		t.Fatalf("export overwrote existing file, exit = %d", code)
	}

	payloadPath := filepath.Join(dir, "payloads.jsonl")
	out := mustRun(t, "", "audit", "export", "--output", payloadPath, "--payloads", "--json")
	payloadRaw, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payloadRaw), "request-value") || len(decodeEnvelope(t, out).Warnings) == 0 {
		t.Fatalf("payload export lacks content or warning: %s / %s", payloadRaw, out)
	}

	oldDay := filepath.Join(dir, accesslog.DirectoryName, "2000-01-01")
	if err := os.MkdirAll(oldDay, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDay, "old.pack"), []byte("expired"), 0o600); err != nil {
		t.Fatal(err)
	}
	var dry AuditPrune
	decodeInto(t, mustRun(t, "", "audit", "prune", "--dry-run", "--json"), &dry)
	if !dry.DryRun || dry.Days != 1 {
		t.Fatalf("dry prune = %+v", dry)
	}
	if _, err := os.Stat(oldDay); err != nil {
		t.Fatalf("dry run removed partition: %v", err)
	}
	var pruned AuditPrune
	decodeInto(t, mustRun(t, "", "audit", "prune", "--json"), &pruned)
	if pruned.DryRun || pruned.Days != 1 || pruned.Bytes == 0 {
		t.Fatalf("prune = %+v", pruned)
	}
	if _, err := os.Stat(oldDay); !os.IsNotExist(err) {
		t.Fatalf("expired partition remains: %v", err)
	}
}
