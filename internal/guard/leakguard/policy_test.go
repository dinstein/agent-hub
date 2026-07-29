package leakguard

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	ghToken   = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	entropyTk = "aG7fQ2mZx9Kw3PLbTs01VuYr8NdEjHi5XcOgAqWvRt4B"
)

func TestParseMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeAudit, false}, // ruling #17: unset means the audit hook stays on
		{"audit", ModeAudit, false},
		{"off", ModeOff, false},
		{"inline", ModeInline, false},
		{"AUDIT", ModeAudit, true}, // the vocabulary is exact, not fuzzy
		{"redact", ModeAudit, true},
	}
	for _, tc := range cases {
		got, err := ParseMode(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseMode(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		// The failure direction: even on error the returned mode is audit, so
		// a typo in governance cannot silently disable the guard.
		if got != tc.want {
			t.Errorf("ParseMode(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	// The zero value of Mode IS audit — the whole default-on ruling rests on
	// it, so pin it explicitly.
	var zero Mode
	if zero != ModeAudit || zero.String() != "audit" {
		t.Fatalf("zero Mode = %v (%q), want audit", int(zero), zero.String())
	}
	var pol Policy
	if pol.Mode != ModeAudit || pol.minRedactSeverity() != SeverityMedium || pol.minSeverity() != SeverityLow {
		t.Fatalf("zero Policy = %+v, want audit / report low / redact medium", pol)
	}

	for _, m := range []Mode{ModeOff, ModeAudit, ModeInline} {
		b, err := m.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%v): %v", m, err)
		}
		var back Mode
		if err := back.UnmarshalText(b); err != nil || back != m {
			t.Fatalf("mode roundtrip %v -> %s -> %v (%v)", m, b, back, err)
		}
	}
	if _, err := Mode(9).MarshalText(); err == nil {
		t.Fatal("MarshalText accepted an invalid mode")
	}
}

func TestScanResultDispositions(t *testing.T) {
	t.Parallel()
	s := NewDefault()
	segments := []string{"clean line", "token " + ghToken}

	t.Run("off scans nothing", func(t *testing.T) {
		t.Parallel()
		res := s.ScanResult(Policy{Mode: ModeOff}, "srv", segments)
		if res.Action != ActionNone || len(res.Findings) != 0 || res.Segments != nil {
			t.Fatalf("off mode returned %+v", res)
		}
	})
	t.Run("audit reports without rewriting", func(t *testing.T) {
		t.Parallel()
		res := s.ScanResult(Policy{}, "srv", segments)
		if res.Action != ActionAudit || len(res.Findings) != 1 {
			t.Fatalf("audit mode returned %+v", res)
		}
		if res.Segments != nil || res.Redacted != 0 {
			t.Fatalf("audit mode rewrote something: %+v", res)
		}
		if res.Findings[0].Segment != 1 {
			t.Fatalf("finding segment = %d, want 1", res.Findings[0].Segment)
		}
	})
	t.Run("inline rewrites", func(t *testing.T) {
		t.Parallel()
		res := s.ScanResult(Policy{Mode: ModeInline}, "srv", segments)
		if res.Action != ActionRedact || res.Redacted != 1 {
			t.Fatalf("inline mode returned %+v", res)
		}
		if len(res.Segments) != len(segments) {
			t.Fatalf("segments = %v, want one per input", res.Segments)
		}
		if res.Segments[0] != segments[0] {
			t.Errorf("clean segment was modified: %q", res.Segments[0])
		}
		if want := "token " + Label("github-token"); res.Segments[1] != want {
			t.Errorf("redacted segment = %q, want %q", res.Segments[1], want)
		}
		if strings.Contains(strings.Join(res.Segments, "\n"), ghToken) {
			t.Fatal("the secret survived inline redaction")
		}
	})
	t.Run("clean content in inline mode is untouched", func(t *testing.T) {
		t.Parallel()
		res := s.ScanResult(Policy{Mode: ModeInline}, "srv", []string{"nothing to see"})
		if res.Action != ActionNone || res.Segments != nil {
			t.Fatalf("clean inline scan returned %+v", res)
		}
	})
	t.Run("exempt server skips the scan", func(t *testing.T) {
		t.Parallel()
		res := s.ScanResult(Policy{Mode: ModeInline, PerServerExempt: []string{"srv"}}, "srv", segments)
		if res.Action != ActionNone || !res.Exempted || len(res.Findings) != 0 {
			t.Fatalf("exempt scan returned %+v", res)
		}
		other := s.ScanResult(Policy{Mode: ModeInline, PerServerExempt: []string{"other"}}, "srv", segments)
		if other.Action != ActionRedact {
			t.Fatalf("non-exempt server was skipped: %+v", other)
		}
	})
	t.Run("report threshold filters findings", func(t *testing.T) {
		t.Parallel()
		res := s.ScanResult(Policy{MinSeverity: SeverityHigh}, "srv", []string{"blob " + entropyTk})
		if res.Action != ActionNone || len(res.Findings) != 0 {
			t.Fatalf("low finding survived MinSeverity=high: %+v", res)
		}
	})
	t.Run("entropy is never redacted", func(t *testing.T) {
		t.Parallel()
		// Two independent guards: RedactNone on the rule, and the default
		// redaction floor at medium. Force the floor down to prove the rule
		// strategy alone still holds the line.
		pol := Policy{Mode: ModeInline, MinRedactSeverity: SeverityLow}
		res := s.ScanResult(pol, "srv", []string{"blob " + entropyTk})
		if res.Action != ActionAudit || res.Redacted != 0 {
			t.Fatalf("entropy finding was redacted: %+v", res)
		}
	})
	t.Run("redaction threshold", func(t *testing.T) {
		t.Parallel()
		pair := []string{"alice@example.org:Tr0ub4dor&3"} // medium severity
		if res := s.ScanResult(Policy{Mode: ModeInline}, "srv", pair); res.Redacted != 1 {
			t.Fatalf("medium finding not redacted at the default floor: %+v", res)
		}
		high := Policy{Mode: ModeInline, MinRedactSeverity: SeverityHigh}
		res := s.ScanResult(high, "srv", pair)
		if res.Action != ActionAudit || res.Redacted != 0 {
			t.Fatalf("medium finding redacted above the floor: %+v", res)
		}
	})
	t.Run("both branches share the entry point", func(t *testing.T) {
		t.Parallel()
		// The API cannot tell a result segment from an error message, which
		// is exactly why a hostile server cannot dodge the scan by answering
		// with an error.
		asResult, _ := json.Marshal(s.ScanResult(Policy{}, "srv", []string{"token " + ghToken}))
		asError, _ := json.Marshal(s.ScanResult(Policy{}, "srv", []string{"token " + ghToken}))
		if string(asResult) != string(asError) {
			t.Fatalf("branches diverged: %s vs %s", asResult, asError)
		}
	})
}

func TestScanResultTruncationStillRedactsEverything(t *testing.T) {
	t.Parallel()
	s := mustScanner(t, Config{MaxFindings: 3})
	segments := []string{
		"AKIA3M7QKX9PLZW2VB01 AKIA3M7QKX9PLZW2VB02",
		"AKIA3M7QKX9PLZW2VB03 AKIA3M7QKX9PLZW2VB04",
		"AKIA3M7QKX9PLZW2VB05 AKIA3M7QKX9PLZW2VB06",
	}
	res := s.ScanResult(Policy{Mode: ModeInline}, "srv", segments)
	if len(res.Findings) != 3 || !res.Truncated {
		t.Fatalf("want 3 reported findings and Truncated, got %d / %v", len(res.Findings), res.Truncated)
	}
	// The report is bounded; the REWRITE is not — a capped report must never
	// mean a capped redaction.
	if res.Redacted != 6 {
		t.Fatalf("redacted %d spans, want all 6", res.Redacted)
	}
	for _, seg := range res.Segments {
		if strings.Contains(seg, "AKIA") {
			t.Fatalf("segment %q still carries a key", seg)
		}
	}
}

func TestRedact(t *testing.T) {
	t.Parallel()
	s := NewDefault()

	t.Run("replaces only the eligible spans", func(t *testing.T) {
		t.Parallel()
		content := "before " + ghToken + " middle blob=" + entropyTk + " after"
		fs := s.Scan(content)
		got, n := Redact(content, fs, SeverityMedium)
		if n != 1 {
			t.Fatalf("redacted %d spans, want 1", n)
		}
		if strings.Contains(got, ghToken) {
			t.Fatal("the token survived")
		}
		if !strings.Contains(got, entropyTk) {
			t.Fatal("the low-confidence entropy hit was rewritten")
		}
		if !strings.HasPrefix(got, "before "+Label("github-token")+" middle") || !strings.HasSuffix(got, " after") {
			t.Fatalf("surrounding text was damaged: %q", got)
		}
	})
	t.Run("no eligible findings returns the input", func(t *testing.T) {
		t.Parallel()
		content := "blob=" + entropyTk
		got, n := Redact(content, s.Scan(content), SeverityMedium)
		if n != 0 || got != content {
			t.Fatalf("Redact rewrote %d spans: %q", n, got)
		}
	})
	t.Run("bogus spans are skipped, never panic", func(t *testing.T) {
		t.Parallel()
		// Findings from a DIFFERENT string: the precondition is violated, so
		// the out-of-range spans must be dropped rather than trusted.
		content := "short"
		bogus := []Finding{
			{Rule: "x", Severity: SeverityHigh, Redaction: RedactMatch, Start: 2, End: 99},
			{Rule: "y", Severity: SeverityHigh, Redaction: RedactMatch, Start: 4, End: 4},
			{Rule: "z", Severity: SeverityHigh, Redaction: RedactMatch, Start: -3, End: 2},
		}
		got, n := Redact(content, bogus, SeverityLow)
		if n != 0 || got != content {
			t.Fatalf("Redact accepted bogus spans: %q (%d)", got, n)
		}
	})
	t.Run("multiple spans keep the text between them", func(t *testing.T) {
		t.Parallel()
		content := "a AKIA3M7QKX9PLZW2VB01 b AKIA3M7QKX9PLZW2VB02 c"
		got, n := Redact(content, s.Scan(content), SeverityMedium)
		want := "a " + Label("aws-access-key-id") + " b " + Label("aws-access-key-id") + " c"
		if n != 2 || got != want {
			t.Fatalf("Redact = %q (%d), want %q", got, n, want)
		}
	})
}

func TestRecordsAreContentFree(t *testing.T) {
	t.Parallel()
	s := NewDefault()
	content := "token " + ghToken
	recs := Records(s.ScanResult(Policy{}, "srv", []string{content}).Findings)
	if len(recs) != 1 {
		t.Fatalf("Records = %+v, want 1", recs)
	}
	raw, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	// The audit red line: no content, and not even the (already redacted)
	// preview — the record carries rule, severity, position and length only.
	for _, forbidden := range []string{ghToken, "ghp_", "preview", "REDACTED"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("audit record %s contains %q", raw, forbidden)
		}
	}
	got := recs[0]
	if got.Rule != "github-token" || got.Severity != SeverityHigh || got.Length != len(ghToken) || got.Start != 6 {
		t.Fatalf("record = %+v", got)
	}
	if Records(nil) != nil {
		t.Fatal("Records(nil) should stay nil")
	}
}
