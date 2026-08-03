package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/calllog"
	"github.com/dinstein/agent-hub/internal/secrets"
)

func TestAuditDefaultsAreBoundedAndStrict(t *testing.T) {
	setDataDir(t)
	var got CallsStatus
	decodeInto(t, mustRun(t, "", "calls", "status", "--json"), &got)
	if got.Enabled || got.Arguments != "full" || got.Results != "truncated" || got.ResultBytes <= 0 {
		t.Fatalf("default audit policy = %+v", got)
	}
	if got.Durability != "sync" || got.RetentionDays <= 0 || got.MaxBytes <= 0 || got.MinFreeBytes <= 0 || got.Pressure != "block" {
		t.Fatalf("default bounds = %+v", got)
	}
}

func TestCallsEnableCreatesOneVaultKeyAndDisableKeepsIt(t *testing.T) {
	dir := setDataDir(t)
	t.Setenv(secrets.EnvDevSecrets, "1")

	var enabled CallsStatus
	decodeInto(t, mustRun(t, "", "calls", "enable", "--json"), &enabled)
	if !enabled.Enabled || enabled.KeyID == "" {
		t.Fatalf("enabled = %+v", enabled)
	}
	firstID := enabled.KeyID

	// Re-enabling reuses the key, which is required to keep old payloads
	// readable and makes the operation idempotent.
	decodeInto(t, mustRun(t, "", "calls", "enable", "--json"), &enabled)
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
	audit, ok := doc["calls"].(map[string]any)
	if !ok || audit["keyId"] != firstID {
		t.Fatalf("governance audit metadata = %#v", doc["calls"])
	}
	if _, exists := audit["key"]; exists {
		t.Fatalf("governance contains key material: %s", raw)
	}

	var disabled CallsStatus
	decodeInto(t, mustRun(t, "", "calls", "disable", "--json"), &disabled)
	if disabled.Enabled || disabled.KeyID != firstID {
		t.Fatalf("disabled = %+v", disabled)
	}
	decodeInto(t, mustRun(t, "", "calls", "enable", "--json"), &enabled)
	if enabled.KeyID != firstID {
		t.Fatalf("enable after disable key id = %q, want %q", enabled.KeyID, firstID)
	}
}

func TestAuditCannotBeEnabledThroughConfigBeforeKeyCreation(t *testing.T) {
	setDataDir(t)
	code, _, _ := runCLI(t, "", "config", "set", "calls.enabled", "true")
	if code != ExitUsage {
		t.Fatalf("config enable without key exit = %d, want %d", code, ExitUsage)
	}
}

func TestCallsPolicyConfig(t *testing.T) {
	setDataDir(t)
	for _, pair := range [][2]string{
		{"calls.durability", "write"},
		{"calls.results", "errors"},
		{"audit.result_bytes", "4096"},
		{"audit.retention_days", "7"},
		{"audit.max_bytes", "1048576"},
		{"audit.min_free_bytes", "524288"},
	} {
		mustRun(t, "", "config", "set", pair[0], pair[1])
	}
	var got CallsStatus
	decodeInto(t, mustRun(t, "", "calls", "status", "--json"), &got)
	if got.Durability != "write" || got.Results != "errors" || got.ResultBytes != 4096 ||
		got.RetentionDays != 7 || got.MaxBytes != 1048576 || got.MinFreeBytes != 524288 {
		t.Fatalf("configured audit policy = %+v", got)
	}
	for _, pair := range [][2]string{
		{"calls.durability", "eventually"}, {"calls.results", "sometimes"},
		{"calls.resultBytes", "0"}, {"calls.retentionDays", "0"},
		{"calls.maxBytes", "-1"}, {"calls.minFreeBytes", "nope"},
	} {
		if code, _, _ := runCLI(t, "", "config", "set", pair[0], pair[1]); code != ExitUsage {
			t.Errorf("config set %s %s exit = %d, want %d", pair[0], pair[1], code, ExitUsage)
		}
	}
}

func seedCLIAuditCall(t *testing.T, dataDir, callID string) string {
	t.Helper()
	chain := secrets.NewChain(secrets.ChainConfig{Dir: filepath.Join(dataDir, "secrets")})
	encoded, ok, err := chain.Get(context.Background(), secrets.CallsEncryptionRef())
	if err != nil || !ok {
		t.Fatalf("load audit key: ok=%v err=%v", ok, err)
	}
	key, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	defer zeroSecret(key)
	keyID, err := calllog.KeyID(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := calllog.Open(calllog.Options{
		Root: filepath.Join(dataDir, calllog.DirectoryName), Key: key, KeyID: keyID,
		Durability: calllog.DurabilitySync,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Add(-time.Minute)
	request, err := store.PutPayload(ts, callID, calllog.PayloadRequest, []byte(`{"name":"srv__tool","arguments":{"secret":"request-value"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(calllog.Event{TS: ts, Kind: calllog.EventReceived, CallID: callID, Client: "codex", Face: "stdio", Request: &request}); err != nil {
		t.Fatal(err)
	}
	args, err := store.PutPayload(ts.Add(time.Millisecond), callID, calllog.PayloadEffectiveArgs, []byte(`{"secret":"request-value"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(calllog.Event{TS: ts.Add(time.Millisecond), Kind: calllog.EventRouted, CallID: callID, Client: "codex", Exposed: "srv__tool", Server: "srv", Tool: "tool", EffectiveArgs: &args}); err != nil {
		t.Fatal(err)
	}
	result, err := store.PutPayload(ts.Add(2*time.Millisecond), callID, calllog.PayloadResult, []byte(`{"content":[{"type":"text","text":"result-value"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(calllog.Event{TS: ts.Add(2 * time.Millisecond), Kind: calllog.EventFinished, CallID: callID, Client: "codex", Exposed: "srv__tool", Server: "srv", Tool: "tool", Outcome: "success", Result: &result, ResultMode: "full", ResultCapture: "full"}); err != nil {
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
	var enabled CallsStatus
	decodeInto(t, mustRun(t, "", "calls", "enable", "--json"), &enabled)
	day := seedCLIAuditCall(t, dir, "call-for-cli")

	var tail CallTail
	decodeInto(t, mustRun(t, "", "calls", "tail", "--since", "all", "--json"), &tail)
	if len(tail.Events) != 3 || tail.Events[1].Server != "srv" {
		t.Fatalf("tail = %+v", tail)
	}

	var stats CallsStats
	decodeInto(t, mustRun(t, "", "calls", "stats", "--since", "all", "--json"), &stats)
	if stats.Calls != 1 || stats.Events != 3 || stats.Outcomes["success"] != 1 || stats.PayloadRaw == 0 {
		t.Fatalf("stats = %+v", stats)
	}

	var metadata CallDetail
	decodeInto(t, mustRun(t, "", "calls", "show", "call-for-cli", "--json"), &metadata)
	if metadata.Request != "" || len(metadata.Events) != 3 {
		t.Fatalf("metadata-only show = %+v", metadata)
	}
	var full CallDetail
	out := mustRun(t, "", "calls", "show", "call-for-cli", "--payloads", "--json")
	decodeInto(t, out, &full)
	if !strings.Contains(full.Request, "request-value") || !strings.Contains(full.Result, "result-value") {
		t.Fatalf("payload show = %+v", full)
	}
	if len(decodeEnvelope(t, out).Warnings) == 0 {
		t.Fatal("payload show did not warn about sensitive data")
	}

	var verify CallsVerify
	decodeInto(t, mustRun(t, "", "calls", "verify", "--json"), &verify)
	if !verify.OK || verify.Events != 3 || verify.Payloads != 3 {
		t.Fatalf("verify = %+v", verify)
	}
	var status CallsStatus
	decodeInto(t, mustRun(t, "", "calls", "status", "--json"), &status)
	if status.Storage.Bytes <= 0 || status.Storage.PackFiles != 1 {
		t.Fatalf("status storage = %+v", status.Storage)
	}

	path := filepath.Join(dir, calllog.DirectoryName, day, calllog.EventFileName)
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
	code, stdout, stderr := runCLI(t, "", "calls", "verify", "--json")
	if code != ExitLocked {
		t.Fatalf("tampered verify exit = %d, want %d (stderr %s)", code, ExitLocked, stderr)
	}
	decodeInto(t, stdout, &verify)
	if verify.OK || verify.Failures == 0 {
		t.Fatalf("tampered verify = %+v", verify)
	}
}

func TestCallsExportPruneAndKeyRotation(t *testing.T) {
	dir := setDataDir(t)
	t.Setenv(secrets.EnvDevSecrets, "1")
	var enabled CallsStatus
	decodeInto(t, mustRun(t, "", "calls", "enable", "--json"), &enabled)
	seedCLIAuditCall(t, dir, "before-rotation")

	var rotated CallsKeyRotation
	decodeInto(t, mustRun(t, "", "calls", "rotate-key", "--json"), &rotated)
	if rotated.PreviousKeyID != enabled.KeyID || rotated.KeyID == "" || rotated.KeyID == enabled.KeyID || !rotated.Enabled {
		t.Fatalf("rotation = %+v, enabled = %+v", rotated, enabled)
	}
	seedCLIAuditCall(t, dir, "after-rotation")

	// Historical and new entries must both verify and decrypt after rotation.
	var verify CallsVerify
	decodeInto(t, mustRun(t, "", "calls", "verify", "--json"), &verify)
	if !verify.OK || verify.Events != 6 || verify.Payloads != 6 {
		t.Fatalf("post-rotation verify = %+v", verify)
	}
	var old CallDetail
	decodeInto(t, mustRun(t, "", "calls", "show", "before-rotation", "--payloads", "--json"), &old)
	if !strings.Contains(old.Request, "request-value") {
		t.Fatalf("old payload after rotation = %+v", old)
	}

	metadataPath := filepath.Join(dir, "metadata.jsonl")
	var metadataExport CallsExport
	decodeInto(t, mustRun(t, "", "calls", "export", "--output", metadataPath, "--json"), &metadataExport)
	metadataRaw, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if metadataExport.Events != 6 || strings.Contains(string(metadataRaw), "request-value") {
		t.Fatalf("metadata export = %+v, data=%s", metadataExport, metadataRaw)
	}
	if code, _, _ := runCLI(t, "", "calls", "export", "--output", metadataPath); code != ExitUsage {
		t.Fatalf("export overwrote existing file, exit = %d", code)
	}

	payloadPath := filepath.Join(dir, "payloads.jsonl")
	out := mustRun(t, "", "calls", "export", "--output", payloadPath, "--payloads", "--json")
	payloadRaw, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payloadRaw), "request-value") || len(decodeEnvelope(t, out).Warnings) == 0 {
		t.Fatalf("payload export lacks content or warning: %s / %s", payloadRaw, out)
	}

	oldDay := filepath.Join(dir, calllog.DirectoryName, "2000-01-01")
	if err := os.MkdirAll(oldDay, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDay, "old.pack"), []byte("expired"), 0o600); err != nil {
		t.Fatal(err)
	}
	var dry CallsPrune
	decodeInto(t, mustRun(t, "", "calls", "prune", "--dry-run", "--json"), &dry)
	if !dry.DryRun || dry.Days != 1 {
		t.Fatalf("dry prune = %+v", dry)
	}
	if _, err := os.Stat(oldDay); err != nil {
		t.Fatalf("dry run removed partition: %v", err)
	}
	var pruned CallsPrune
	decodeInto(t, mustRun(t, "", "calls", "prune", "--json"), &pruned)
	if pruned.DryRun || pruned.Days != 1 || pruned.Bytes == 0 {
		t.Fatalf("prune = %+v", pruned)
	}
	if _, err := os.Stat(oldDay); !os.IsNotExist(err) {
		t.Fatalf("expired partition remains: %v", err)
	}
}

// -f keeps printing. The three record readers under Observe all follow, and
// this was the one that could not: watching a client misbehave meant
// re-running `calls tail` by hand, which is exactly when a call is missed.
func TestCallsTailFollowPrintsOnlyWhatIsNew(t *testing.T) {
	dir := setDataDir(t)
	root := filepath.Join(dir, "calls")
	base := time.Now().UTC().Add(-time.Minute)
	seedCallEvent(t, root, base, "call-old", "received")

	// The tail the follower is handed, exactly as the command builds it.
	tail, err := readCallTail(root, time.Time{}, 20, callSelector{})
	if err != nil {
		t.Fatalf("readCallTail: %v", err)
	}
	if len(tail.Events) != 1 || tail.Events[0].CallID != "call-old" {
		t.Fatalf("seed tail = %+v", tail.Events)
	}

	// A record that arrives after that tail was taken.
	seedCallEvent(t, root, base.Add(time.Second), "call-new", "finished")

	var out bytes.Buffer
	app := &App{stdout: &out, stderr: io.Discard}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.followCalls(ctx, root, tail, callSelector{}) }()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "call-new") {
		if time.Now().After(deadline) {
			t.Fatalf("the follower never printed the new record:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("followCalls: %v", err)
	}

	// And only what is new: reprinting the row the tail already showed reads
	// as the same call happening twice.
	if strings.Contains(out.String(), "call-old") {
		t.Fatalf("the follower reprinted a record the tail had shown:\n%s", out.String())
	}
}

// seedCallEvent appends one lifecycle record to the day partition its
// timestamp belongs to, which is where the reader looks for it.
func seedCallEvent(t *testing.T, root string, ts time.Time, callID, kind string) {
	t.Helper()
	dir := filepath.Join(root, ts.UTC().Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"v":1,"ts":%q,"event":%q,"callId":%q,"client":"claude-code","pid":7}`,
		ts.UTC().Format(time.RFC3339Nano), kind, callID) + "\n"
	f, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}
