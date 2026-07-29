package httpbridge

import (
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/guard/leakguard"
)

// TestMintedTokenIsDetectedAsALeak pins a cross-package contract that no
// compiler can enforce.
//
// leakguard carries a rule for agenthub's own agent token, and it has to spell
// the "agt_" prefix and the 64-hex body itself: internal/guard/* is a
// zero-business-dependency foundation and cannot import this package. So the
// pattern is a second copy of the token's shape, and a change to mint() would
// leave it behind silently — the guard would keep passing its own tests while
// no longer recognising the thing it exists to recognise.
//
// This test lives here because internal/httpbridge may import leakguard, and
// it mints a REAL token rather than writing one out, so the two definitions
// are compared rather than restated. It is the same arrangement as the
// api.DefaultSocketPath / platform.CtlSocketPath contract test.
//
// Why the rule exists at all: the entropy heuristic structurally cannot see
// this token. Its body is 64 hex characters and hex tops out at 4.0 bits per
// character, below leakguard's 4.5 threshold — an exclusion that is correct in
// general (a digest is not a secret) but blind to a hex-bodied credential.
func TestMintedTokenIsDetectedAsALeak(t *testing.T) {
	scanner := leakguard.NewDefault()

	// Several tokens: mint() is random, and a rule that matched only some
	// bodies would otherwise pass here by luck.
	for range 20 {
		value, err := mint()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if !strings.HasPrefix(value, TokenPrefix) {
			t.Fatalf("minted %q, which does not carry the frozen prefix", value)
		}

		findings := scanner.Scan("the agent token is " + value)
		if len(findings) == 0 {
			t.Fatalf("leakguard did not flag a freshly minted token %q; "+
				"its rule no longer matches what mint() produces", value)
		}
		var matched bool
		for _, f := range findings {
			if f.Rule == "agenthub-agent-token" {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("token %q was flagged, but not by the agenthub-agent-token rule: %+v",
				value, findings)
		}
	}
}

// A token is a live credential granting tool access at a tier, so the finding
// must be one that actually REDACTS. A low-severity, report-only signal would
// name the leak in the security log and still hand the token to the model.
func TestMintedTokenFindingRedacts(t *testing.T) {
	value, err := mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	findings := leakguard.NewDefault().Scan("token=" + value)
	if len(findings) == 0 {
		t.Fatal("no finding for a minted token")
	}
	for _, f := range findings {
		if f.Rule != "agenthub-agent-token" {
			continue
		}
		if f.Severity != leakguard.SeverityHigh {
			t.Errorf("severity = %v, want high: this is a live credential", f.Severity)
		}
		if f.Redaction == leakguard.RedactNone {
			t.Error("redaction = none: the token would still reach the model")
		}
	}
}
