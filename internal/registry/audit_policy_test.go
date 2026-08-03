package registry_test

import (
	"encoding/json"
	"testing"

	"github.com/dinstein/agent-hub/internal/registry"
)

func TestCallsPolicyDefaultsAndRoundTrip(t *testing.T) {
	var g registry.GovernanceDoc
	p := g.ResolvedCalls()
	if p.Enabled || p.Durability != registry.DefaultCallsDurability ||
		p.ResultMode != registry.DefaultCallsResultMode || p.ResultBytes != registry.DefaultCallsResultBytes ||
		p.RetentionDays != registry.DefaultCallsRetentionDays || p.MaxBytes != registry.DefaultCallsMaxBytes ||
		p.MinFreeBytes != registry.DefaultCallsMinFree || p.KeyID != "" {
		t.Fatalf("defaults = %+v", p)
	}

	raw := []byte(`{"calls":{"enabled":true,"durability":"write","results":"none","resultBytes":123,"retentionDays":9,"maxBytes":456,"minFreeBytes":78,"keyId":"kid"}}`)
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	p = g.ResolvedCalls()
	if !p.Enabled || p.Durability != "write" || p.ResultMode != "none" || p.ResultBytes != 123 ||
		p.RetentionDays != 9 || p.MaxBytes != 456 || p.MinFreeBytes != 78 || p.KeyID != "kid" {
		t.Fatalf("resolved = %+v", p)
	}
}
