package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// auditClaim matches prose asserting that agenthub records an audit trail:
// "every write is audited", "the audit stream", "an audit line", "audit
// records", "auditNonReg", or a route named /v1/audit.
//
// Deliberately not matched: a sentence that DENIES the trail. Those are the
// comments this test exists to keep in place, and they have to be able to
// say the word.
var auditClaim = regexp.MustCompile(`(?i)\b(audit(?:ed|s|ing)?\s+(?:stream|line|record|trail)|(?:is|are|every\s+write\s+is)\s+audited|auditNonReg|/v1/audit)\b`)

// auditDenial marks a comment that says the trail does NOT exist. A line
// carrying one is exempt: it is the record of the absence, not a claim.
var auditDenial = regexp.MustCompile(`(?i)(no audit|not audited|never (?:existed|recorded)|nothing is audited|there is no audit|used to (?:read|promise)|deliberately not recorded)`)

// TestNoCodeClaimsAnAuditTrailThatDoesNotExist keeps a control the tree does
// not implement from being described as though it did.
//
// The 2026-07-31 security sweep found six comment sites in internal/ctlapi
// asserting an audit trail — "every write is audited with the key and both
// values, so 'blockOnInjection went off at 03:00' is answerable after the
// fact", "See auditNonReg", an Options.LogsDir documented as feeding
// /v1/audit and /v1/security — while nothing in the tree wrote a record,
// auditNonReg had never existed, and neither route was served. The claims
// were removed rather than implemented; building the trail is its own change
// with its own argument.
//
// This is the cheapest possible guard and it is worth having, because the
// failure is silent in the worst direction: a reviewer reading those
// comments concludes a control is in place, and a comment is exactly what
// the next sweep's finder reads first. The moment an audit trail IS built,
// this test should be deleted in the commit that builds it.
func TestNoCodeClaimsAnAuditTrailThatDoesNotExist(t *testing.T) {
	root := repoRoot(t)

	// Scoped to the control plane, which is where the claims were and where
	// governance writes happen. The CLI has a real `agenthub audit` verb
	// question of its own that this test must not prejudge.
	dir := filepath.Join(root, "internal", "ctlapi")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading internal/ctlapi: %v", err)
	}

	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		found++
		for i, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "//") {
				continue
			}
			if auditDenial.MatchString(trimmed) {
				continue
			}
			if m := auditClaim.FindString(trimmed); m != "" {
				t.Errorf("internal/ctlapi/%s:%d claims an audit trail (%q) that this tree does not implement:\n\t%s\n"+
					"Either build it — and delete this test in the same commit — or say plainly that it does not exist.",
					e.Name(), i+1, m, trimmed)
			}
		}
	}
	// A precondition that fails hard: an empty scan would pass silently and
	// look like proof.
	if found == 0 {
		t.Fatal("scanned no Go files in internal/ctlapi; the check proved nothing")
	}
}
