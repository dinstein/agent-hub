package scope

import (
	"encoding/hex"
	"testing"
)

// goldenScope builds a fixed, fully-populated merge input. Any change to the
// hash encoding breaks the frozen digest below — determinism is contract
// (docs/conventions.md#engineering-conventions): cursors, approvals and search caches key off this hash
// across processes.
func goldenScope(t *testing.T) *EffectiveScope {
	t.Helper()
	layers := []ScopeLayer{
		{Kind: LayerGlobal, Discovery: discPtr(DiscoveryFull),
			ResultBudget: map[string]*Budget{"*": {Bytes: 4096, Forced: true}}},
		{Kind: LayerProfile, Servers: []string{"fs", "git"},
			Discovery: discPtr(DiscoveryGrouped),
			Tools: map[string]*ToolSelector{"fs": {
				Allow: []string{"read", "write"},
			}}},
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
//
// Bumped again, on purpose: EffectiveApproval is gone entirely — the two
// switches it folded (humanApproval, denyDestructive) were read only by the
// HITL gate, and hash.go now writes two fewer bools. Same cost as before: a
// cold start for cursors and search caches, which recompute rather than
// serve a wrong answer.
//
// And once more, for the domain separator those bools sat behind. Keeping a
// constant "approval" label in the digest would have avoided a second cold
// start at the price of a hash whose shape describes a field that does not
// exist — and the next person to change hash.go would have had to work out
// which of the two it was. The separator is gone; the cost was paid once
// more, deliberately, in the same release as the bump above.
//
// NOT bumped when ToolSelector lost Deny. The digest covers the RESOLVED
// scope, not the layers that produced it, so the fixture was rewritten to
// spell the same effective tool set with an allow list alone
// (allow[read,write,delete] minus deny[delete] == allow[read,write]) and
// every persisted digest stayed valid. A fixture edit that moved the hash
// would have charged real users a cold start for a refactor they cannot see.
const goldenHashHex = "aeb2c0a5e781d5a0854ecae5a441abb37e50bc8ac700a8b03966f6cddfbf354c"

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
	for range 10 {
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
		{"tool set", []ScopeLayer{{Kind: LayerSession, Tools: map[string]*ToolSelector{"fs": {Allow: []string{"read"}}}}}, nil},
		{"discovery", []ScopeLayer{{Kind: LayerSession, Discovery: discPtr(DiscoveryLazy)}}, nil},
		{"budget", []ScopeLayer{{Kind: LayerSession, ResultBudget: map[string]*Budget{"*": {Bytes: 1}}}}, nil},
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
