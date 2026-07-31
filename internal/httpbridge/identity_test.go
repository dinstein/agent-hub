package httpbridge_test

import (
	"testing"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/tier"
)

// TestIdentitySeparatesEveryAllowlistState is the regression for a
// fingerprint collision between a nil allowlist and an empty one.
//
// Caller.Identity is what an established session is pinned to and what the
// HTTP data plane keys its per-credential gateway cache by, so two callers
// sharing a fingerprint share authority. nil means "every server" and []
// means "no server at all": narrowing a live token from the first to the
// second is the single most consequential edit an operator can make to
// tokens.json, and while both rendered as "" it changed nothing until the
// cached gateway went idle.
func TestIdentitySeparatesEveryAllowlistState(t *testing.T) {
	t.Parallel()
	base := func(servers []string) *httpbridge.Caller {
		return &httpbridge.Caller{
			Kind:    httpbridge.CallerAgent,
			Token:   "ci",
			Tier:    tier.Read,
			Servers: servers,
		}
	}
	// Every pair below must be distinct. [""] is in the list because it is
	// the only other rendering that a plain join collapses into [].
	states := map[string]*httpbridge.Caller{
		"nil":        base(nil),
		"empty":      base([]string{}),
		"one-empty":  base([]string{""}),
		"one-server": base([]string{"github"}),
		"wildcard":   base([]string{httpbridge.ServerWildcard}),
		"two":        base([]string{"github", "slack"}),
	}
	seen := map[string]string{}
	for name, c := range states {
		id := c.Identity()
		if prev, dup := seen[id]; dup {
			t.Fatalf("%q and %q share the fingerprint %q; a narrowed token would keep the old authority", prev, name, id)
		}
		seen[id] = name
	}
	// The tri-state must not have cost the other fields their separation.
	narrowed := base([]string{"github"})
	narrowed.Tier = tier.Write
	if narrowed.Identity() == states["one-server"].Identity() {
		t.Fatal("a tier change no longer moves the fingerprint")
	}
}

// TestAllowsServerAgreesWithIdentity pins the meaning Identity encodes: the
// two must not disagree about what nil and [] mean, or the fingerprint would
// separate states the gate then treats as one.
func TestAllowsServerAgreesWithIdentity(t *testing.T) {
	t.Parallel()
	all := &httpbridge.Caller{Kind: httpbridge.CallerAgent, Servers: nil}
	none := &httpbridge.Caller{Kind: httpbridge.CallerAgent, Servers: []string{}}
	if !all.AllowsServer("github") {
		t.Fatal("a nil allowlist must reach every server")
	}
	if none.AllowsServer("github") {
		t.Fatal("an empty allowlist must reach nothing")
	}
	if all.Identity() == none.Identity() {
		t.Fatal("allow-all and allow-nothing share a fingerprint")
	}
}
