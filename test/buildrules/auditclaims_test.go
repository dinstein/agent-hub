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
// records", "an audit log", "auditNonReg", or a route named /v1/audit.
//
// The second alternative is for the shape that escaped the first: an audit
// named in a LIST of logs — "the audit, security and per-server trace logs",
// which is a claim about three records and was true of none of them. A comma
// was the whole reason it passed. The window is bounded at five words so the
// `audit` governance key (the call-ledger policy, a real field) can still be
// named in a sentence that later mentions a log.
//
// Deliberately not matched: a sentence that DENIES the trail. Those are the
// comments this test exists to keep in place, and they have to be able to
// say the word.
var auditClaim = regexp.MustCompile(`(?i)\b(audit(?:ed|s|ing)?\s+(?:stream|line|record|trail|log)s?` +
	`|audit(?:[\s,]+[a-z-]+){0,5}[\s,]+logs?` +
	`|(?:is|are|every\s+write\s+is)\s+audited|auditNonReg|/v1/audit)\b`)

// auditDenial marks a comment that says the trail does NOT exist. A line
// carrying one is exempt: it is the record of the absence, not a claim.
var auditDenial = regexp.MustCompile(`(?i)(no audit|not audited|never (?:existed|recorded)|nothing is audited|there is no audit|used to (?:read|promise)|deliberately not recorded)`)

// TestNoCodeClaimsAnAuditTrailThatDoesNotExist keeps a control the tree does
// not implement from being described as though it did.
//
// The trail this is about is the GOVERNANCE-WRITE trail: who changed which
// setting and when. The call ledger (internal/calllog) is a different record
// and does exist; nothing here is about it.
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

	// Scoped to the two packages a governance write passes through: ctlapi,
	// the control-plane face where the original claims were, and confops, the
	// semantic write layer underneath both front ends. confops was outside
	// the scan until RemoveServer was found claiming three records the tree
	// does not write — the claim that decides what a delete destroys, in the
	// package that performs it.
	//
	// It is deliberately NOT scoped to the call ledger, which is a real
	// record with a real reader (`agenthub calls`): this test is about a
	// GOVERNANCE-WRITE trail — who changed which setting, when — and that one
	// still does not exist. The two are easy to confuse now that one of them
	// is built, which is the reason to say so here.
	found := 0
	for _, pkg := range []string{"ctlapi", "confops"} {
		dir := filepath.Join(root, "internal", pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading internal/%s: %v", pkg, err)
		}
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
					t.Errorf("internal/%s/%s:%d claims an audit trail (%q) that this tree does not implement:\n\t%s\n"+
						"Either build it — and delete this test in the same commit — or say plainly that it does not exist.",
						pkg, e.Name(), i+1, m, trimmed)
				}
			}
		}
	}
	// A precondition that fails hard: an empty scan would pass silently and
	// look like proof.
	if found == 0 {
		t.Fatal("scanned no Go files; the check proved nothing")
	}
}
