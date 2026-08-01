package registry_test

import (
	"encoding/json"
	"testing"

	"github.com/dinstein/agent-hub/internal/registry"
)

func TestAuditPolicyDefaultsAndRoundTrip(t *testing.T) {
	var g registry.GovernanceDoc
	p := g.ResolvedAudit()
	if p.Enabled || p.Durability != registry.DefaultAuditDurability ||
		p.ResultMode != registry.DefaultAuditResultMode || p.ResultBytes != registry.DefaultAuditResultBytes ||
		p.RetentionDays != registry.DefaultAuditRetentionDays || p.MaxBytes != registry.DefaultAuditMaxBytes ||
		p.MinFreeBytes != registry.DefaultAuditMinFree || p.KeyID != "" {
		t.Fatalf("defaults = %+v", p)
	}

	raw := []byte(`{"audit":{"enabled":true,"durability":"write","results":"none","resultBytes":123,"retentionDays":9,"maxBytes":456,"minFreeBytes":78,"keyId":"kid"}}`)
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	p = g.ResolvedAudit()
	if !p.Enabled || p.Durability != "write" || p.ResultMode != "none" || p.ResultBytes != 123 ||
		p.RetentionDays != 9 || p.MaxBytes != 456 || p.MinFreeBytes != 78 || p.KeyID != "kid" {
		t.Fatalf("resolved = %+v", p)
	}
}
