package scope

import (
	"encoding/hex"
	"testing"
)

// goldenScope builds a fixed, fully-populated merge input. Any change to the
// hash encoding breaks the frozen digest below — determinism is contract
// (canonical.md §6): cursors, approvals and search caches key off this hash
// across processes.
func goldenScope(t *testing.T) *EffectiveScope {
	t.Helper()
	layers := []ScopeLayer{
		{Kind: LayerGlobal, Discovery: discPtr(DiscoveryFull),
			ResultBudget: map[string]*Budget{"*": {Bytes: 4096, Forced: true}},
			Approval:     ApprovalPolicy{DenyDestructive: boolPtr(true)}},
		{Kind: LayerProfile, Servers: []string{"fs", "git"},
			Discovery: discPtr(DiscoveryGrouped),
			Tools: map[string]*ToolSelector{"fs": {
				Allow: []string{"read", "write", "delete"}, Deny: []string{"delete"},
			}},
			Approval: ApprovalPolicy{HumanApproval: boolPtr(true)}},
		{Kind: LayerSession, ResultBudget: map[string]*Budget{"git": {Bytes: 512}}},
	}
	diags := []Diagnostic{{Layer: LayerProfile, Origin: "clients.json#c", Message: "example diagnostic"}}
	es, err := MergeWithDiagnostics(layers, testCatalog(), diags)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

// Frozen digest of goldenScope. If this test fails you changed the content
// encoding — that invalidates every persisted consumer of the hash; bump
// deliberately, never casually.
//
// Bumped once, on purpose: EffectiveApproval lost ConfirmDestructive when the
// client layer was retired, so hash.go writes one fewer bool and every digest
// moved. The cost is a cold start for cursors, search caches and approval
// staleness checks — they recompute rather than serve a wrong answer.
const goldenHashHex = "2d5ed27e55b0a510a7d6755631e25f64f1c407d537f1f15f4f762bdf63fff45c"

func TestHashGolden(t *testing.T) {
	es := goldenScope(t)
	got := hex.EncodeToString(es.Hash[:])
	if got != goldenHashHex {
		t.Fatalf("content hash drifted:\n got %s\nwant %s", got, goldenHashHex)
	}
}

// The hash must not depend on Generation, and must be stable across
// repeated merges (map iteration order must not leak in).
func TestHashStability(t *testing.T) {
	a := goldenScope(t)
	for i := 0; i < 10; i++ {
		b := goldenScope(t)
		if a.Hash != b.Hash {
			t.Fatal("hash unstable across identical merges")
		}
	}
	b := goldenScope(t)
	b.Generation = 999
	if a.Hash != b.Hash {
		t.Fatal("Generation must be excluded from the hash")
	}
}

// Distinct content must produce distinct hashes (spot checks on every
// hashed dimension).
func TestHashDistinguishesContent(t *testing.T) {
	base, _ := Merge(nil, testCatalog())
	variants := []struct {
		name   string
		layers []ScopeLayer
		diags  []Diagnostic
	}{
		{"server set", []ScopeLayer{{Kind: LayerSession, Servers: []string{"fs"}}}, nil},
		{"tool set", []ScopeLayer{{Kind: LayerSession, Tools: map[string]*ToolSelector{"fs": {Deny: []string{"read"}}}}}, nil},
		{"discovery", []ScopeLayer{{Kind: LayerSession, Discovery: discPtr(DiscoveryLazy)}}, nil},
		{"budget", []ScopeLayer{{Kind: LayerSession, ResultBudget: map[string]*Budget{"*": {Bytes: 1}}}}, nil},
		{"approval", []ScopeLayer{{Kind: LayerSession, Approval: ApprovalPolicy{HumanApproval: boolPtr(true)}}}, nil},
		{"diags", nil, []Diagnostic{{Layer: LayerProfile, Origin: "o", Message: "m"}}},
	}
	for _, v := range variants {
		es, err := MergeWithDiagnostics(v.layers, testCatalog(), v.diags)
		if err != nil {
			t.Fatal(err)
		}
		if es.Hash == base.Hash {
			t.Errorf("%s change did not change the hash", v.name)
		}
	}
}
