package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
