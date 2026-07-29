package leakguard

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func mustScanner(t *testing.T, cfg Config) *Scanner {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	return s
}

func ruleIDs(fs []Finding) []string {
	ids := make([]string, 0, len(fs))
	for _, f := range fs {
		if !slices.Contains(ids, f.Rule) {
			ids = append(ids, f.Rule)
		}
	}
	slices.Sort(ids)
	return ids
}

// maxSeverity is what the negative cases assert on: the entropy heuristic is
// allowed to fire on documentation samples (it is explicitly low-confidence
// and audit-only), a high-confidence rule is not.
func maxSeverity(fs []Finding) Severity {
	var m Severity
	for _, f := range fs {
		if f.Severity > m {
			m = f.Severity
		}
	}
	return m
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		rules []Rule
	}{
		{"no id", []Rule{{Regex: "x", Severity: SeverityLow, Redaction: RedactMatch}}},
		{"no regex", []Rule{{ID: "r", Severity: SeverityLow, Redaction: RedactMatch}}},
		{"bad regex", []Rule{{ID: "r", Regex: "(", Severity: SeverityLow, Redaction: RedactMatch}}},
		{"bad severity", []Rule{{ID: "r", Regex: "x", Redaction: RedactMatch}}},
		{"bad redaction", []Rule{{ID: "r", Regex: "x", Severity: SeverityLow}}},
		{"reserved id", []Rule{{ID: EntropyRuleID, Regex: "x", Severity: SeverityLow, Redaction: RedactMatch}}},
		{"duplicate id", []Rule{
			{ID: "r", Regex: "x", Severity: SeverityLow, Redaction: RedactMatch},
			{ID: "r", Regex: "y", Severity: SeverityLow, Redaction: RedactMatch},
		}},
		// The load-bearing one: RedactSecret without the group would redact
		// the whole match instead of the secret, silently.
		{"redact secret without group", []Rule{{ID: "r", Regex: "x(y)", Severity: SeverityLow, Redaction: RedactSecret}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(Config{Rules: tc.rules}); err == nil {
				t.Fatalf("New accepted invalid rules %+v", tc.rules)
			}
		})
	}
	_ = NewDefault() // the built-in table must always compile
}

// Positive cases: one per built-in rule, each a realistic (synthetic) secret.
func TestRulePositives(t *testing.T) {
	t.Parallel()
	s := NewDefault()
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"pem block", "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA7x\n-----END RSA PRIVATE KEY-----", "private-key-block"},
		{"pem header only", "leaked: -----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA", "private-key-block"},
		{"pem pgp block", "-----BEGIN PGP PRIVATE KEY BLOCK-----\nlQOYBF\n-----END PGP PRIVATE KEY BLOCK-----", "private-key-block"},
		{"aws key id", "aws_access_key_id = AKIA3M7QKX9PLZW2VBNQ", "aws-access-key-id"},
		{"aws sts key id", "ASIA3M7QKX9PLZW2VBNQ is temporary", "aws-access-key-id"},
		{"aws secret", `aws_secret_access_key = "kJ7pQz2mNx8vRt5wYh3LcB6dEfG9sAu1TnV4pQ0z"`, "aws-secret-access-key"},
		{"github classic", "token ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8", "github-token"},
		{"github oauth", "gho_Z9y8X7w6V5u4T3s2R1q0P9o8N7m6L5k4J3h2", "github-token"},
		{"github server", "ghs_Q1w2E3r4T5y6U7i8O9p0A1s2D3f4G5h6J7k8", "github-token"},
		{"github fine grained", "github_pat_11ABCDE0Y0aBcDeFgHiJkL_MnOpQrStUvWxYz0123456789aBcDeFgHiJkL", "github-token"},
		{"slack bot", "xoxb-2374829174-4823947192-8sKd93jLPqmVn2Xw", "slack-token"},
		{"slack user", "xoxp-9182736450-1029384756-KdmZq83LsPwn41Xt", "slack-token"},
		{"google api key", "key=AIzaSyD-9tSrke72PouQMnMX-a7eZSW0jkFMBWY", "google-api-key"},
		{"stripe sk", "sk_live_51H8xQ2eZvKYlo2CkVbNmPqRs", "stripe-secret-key"},
		{"stripe rk", "rk_live_9dKw2PqmZv73XnLbTs01", "stripe-secret-key"},
		{"jwt", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "jwt"},
		{"authorization header", "Authorization: Bearer sk-abc123XYZ789tokenvalue0011", "authorization-header"},
		{"authorization basic", `"authorization": "Basic YWRtaW46c3VwM3JzM2NyM3Q="`, "authorization-header"},
		{"bare bearer", "curl -H 'x-auth: Bearer aGVsbG8gd29ybGQgdG9rZW4xMjM0'", "bearer-token"},
		{"card spaced", "paid with 4539 5787 6362 1486", "credit-card"},
		{"card plain", "card 4539578763621486 exp 12/29", "credit-card"},
		{"card amex", "amex 3782 822463 10005", "credit-card"},
		{"email password colon", "alice@example.org:Tr0ub4dor&3", "email-password-pair"},
		{"email password keyword", "login bob@corp.io password=Hx7!kdmQz2", "email-password-pair"},
		{"credential url public", "https://svcuser:S3cretPass99@api.vendor.com/v1", "credential-url"},
		{"credential url internal", "postgres://svc:Kx7pQz2mNx8@db.internal:5432/app", "internal-credential-url"},
		{"credential url rfc1918", "redis://cache:Zq83LsPwn41@10.4.2.9:6379", "internal-credential-url"},
		{"high entropy", "opaque=Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MFFXRVJUWXVpb3A", EntropyRuleID},
		// agenthub's own token. The entropy pass structurally cannot see this
		// one — a 64-hex body tops out at 4.0 bits/char, under the 4.5
		// threshold — so without the labelled rule it is invisible.
		{"agenthub agent token", "AGENTHUB_HTTP_TOKEN=agt_" +
			"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "agenthub-agent-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ids := ruleIDs(s.Scan(tc.content))
			if !slices.Contains(ids, tc.want) {
				t.Fatalf("Scan(%q) rules = %v, want %q among them", tc.content, ids, tc.want)
			}
		})
	}

	// Every built-in rule must be exercised above; a rule nobody tests is a
	// rule nobody can trust to keep working.
	covered := make(map[string]bool, len(cases))
	for _, tc := range cases {
		covered[tc.want] = true
	}
	for _, r := range DefaultRules() {
		if !covered[r.ID] {
			t.Errorf("rule %q has no positive case", r.ID)
		}
	}
}

// Negative cases: the false positives that would make an operator turn the
// guard off — documentation samples, obvious placeholders, commented-out
// template lines, and structurally similar non-secrets.
func TestRuleNegatives(t *testing.T) {
	t.Parallel()
	s := NewDefault()
	cases := []struct{ name, content string }{
		{"public key block", "-----BEGIN PUBLIC KEY-----\nMIIBIjANBg\n-----END PUBLIC KEY-----"},
		{"certificate block", "-----BEGIN CERTIFICATE-----\nMIIDdzCCAl\n-----END CERTIFICATE-----"},
		{"aws docs key id", "Use AKIAIOSFODNN7EXAMPLE as the access key id."},
		{"aws docs secret", "aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		{"commented placeholder token", "# export GITHUB_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
		{"template token", "GITHUB_TOKEN=ghp_your-token-here-000000000000000000"},
		{"slack placeholder", "SLACK_BOT_TOKEN=xoxb-your-token-here"},
		{"google placeholder", "AIzaSyEXAMPLEabcdefghijklmnopqrstuvwxyz"},
		{"stripe test key", "sk_test_51H8xQ2eZvKYlo2CkVbNmPqRs"},
		{"three dotted words", "package abc.def.ghijklmnop is fine"},
		{"jwt-shaped without alg", "eyJ0eXAiOiJKV1QifQ.eyJzdWIiOiIxIn0.c2lnbmF0dXJl"},
		{"jwt-shaped undecodable header", "eyJub3RqYXNvbg.aaaaaaaaaaaa.bbbbbbbb"},
		{"bearer placeholder", "Authorization: Bearer <your-token-here>"},
		{"luhn failure", "reference 4111111111111112"},
		{"unknown issuer", "order id 1234567890123456"},
		{"mixed separators", "ticket 4111-1111 1111 1111"},
		{"too long digit run", "seq 45395787636214861234"},
		{"too short digit run", "code 453957876362"},
		{"email in a sentence", "write to bob@corp.example; thanks for the report"},
		{"email then port", "support@corp.io:8080"},
		{"url without credentials", "https://api.github.com/repos/dinstein/agent-hub"},
		{"url with placeholder password", "postgres://user:password@localhost:5432/app"},
		{"url with numeric password", "https://user:12345678@host.example.com/"},
		{"hex digest", "sha256 deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
		{"uuid", "id 3f2504e0-4f89-11d3-9a0c-0305e82c3301"},
		{"prose", "The quick brown fox jumps over the lazy dog, again and again and again."},
		{"lowercase identifier", "constant_name_that_is_quite_long_but_boring_indeed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := s.Scan(tc.content)
			if sev := maxSeverity(fs); sev >= SeverityMedium {
				t.Fatalf("Scan(%q) produced %v findings %+v, want nothing above low", tc.content, sev, fs)
			}
		})
	}
}

func TestLuhnBoundaries(t *testing.T) {
	t.Parallel()
	t.Run("checksum", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			digits string
			want   bool
		}{
			{"4539578763621486", true},
			{"4539578763621487", false},
			{"79927398713", true},
			{"79927398710", false},
			{"378282246310005", true}, // Amex
			{"6011111111111117", true},
			{"0", false}, // too short to have a checksum
			{"18", true},
			{"19", false},
		}
		for _, tc := range cases {
			if got := luhnValid(tc.digits); got != tc.want {
				t.Errorf("luhnValid(%q) = %v, want %v", tc.digits, got, tc.want)
			}
		}
	})
	t.Run("card gates", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			run  string
			want bool
		}{
			{"4539578763621486", true},
			{"4539 5787 6362 1486", true},
			{"4539-5787-6362-1486", true},
			{"4539-5787 6362-1486", false}, // mixed separators
			{"453957876362148", false},     // 15 digits, Visa: checksum fails
			{"1234567890123452", false},    // Luhn-valid but no known issuer
			{"4539578763621486124", false}, // 19 digits, checksum fails
			{"4539578763621486123", true},  // 19 digits, checksum holds
			{"453957876362", false},        // too short
		}
		for _, tc := range cases {
			if got := isPlausibleCard(tc.run); got != tc.want {
				t.Errorf("isPlausibleCard(%q) = %v, want %v", tc.run, got, tc.want)
			}
		}
	})
}

func TestEntropyHeuristic(t *testing.T) {
	t.Parallel()
	// A 44-char random-looking base64 token: mixed classes, ~4.7 bits/char.
	token := "aG7fQ2mZx9Kw3PLbTs01VuYr8NdEjHi5XcOgAqWvRt4B"
	hex := "9f8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f"

	t.Run("fires above threshold", func(t *testing.T) {
		t.Parallel()
		fs := NewDefault().Scan("blob=" + token)
		if len(fs) != 1 || fs[0].Rule != EntropyRuleID {
			t.Fatalf("Scan = %+v, want one %s finding", fs, EntropyRuleID)
		}
		f := fs[0]
		if f.Severity != SeverityLow || f.Redaction != RedactNone {
			t.Fatalf("entropy finding = %+v, want low severity and RedactNone", f)
		}
		if f.Entropy < 4.5 {
			t.Fatalf("entropy = %v, want >= 4.5", f.Entropy)
		}
	})
	t.Run("threshold is configurable and exclusive", func(t *testing.T) {
		t.Parallel()
		// A threshold above what the token reaches silences it.
		s := mustScanner(t, Config{EntropyThreshold: 6.5})
		if fs := s.Scan("blob=" + token); len(fs) != 0 {
			t.Fatalf("threshold 6.5 still reported %+v", fs)
		}
	})
	t.Run("min length gate", func(t *testing.T) {
		t.Parallel()
		// Note the interaction the default values encode: a token shorter
		// than 23 characters cannot reach 4.5 bits/char at all (log2(22) <
		// 4.46), so lowering the length gate alone changes nothing — the
		// threshold has to come down with it.
		short := token[:20]
		if fs := NewDefault().Scan("blob=" + short); len(fs) != 0 {
			t.Fatalf("20-char token reported: %+v", fs)
		}
		s := mustScanner(t, Config{EntropyMinLen: 16, EntropyThreshold: 4.0})
		if fs := s.Scan("blob=" + short); len(fs) != 1 {
			t.Fatalf("EntropyMinLen=16 found %+v, want 1", fs)
		}
	})
	t.Run("negative min length disables", func(t *testing.T) {
		t.Parallel()
		s := mustScanner(t, Config{EntropyMinLen: -1})
		if fs := s.Scan("blob=" + token); len(fs) != 0 {
			t.Fatalf("entropy disabled but reported %+v", fs)
		}
	})
	t.Run("hex digests stay below the threshold", func(t *testing.T) {
		t.Parallel()
		if e := shannonBits(hex); e >= 4.5 {
			t.Fatalf("shannonBits(hex) = %v, want < 4.5 (hex tops out at 4.0)", e)
		}
		if fs := NewDefault().Scan("digest " + hex); len(fs) != 0 {
			t.Fatalf("hex digest reported: %+v", fs)
		}
	})
	t.Run("character classes gate", func(t *testing.T) {
		t.Parallel()
		if got := charClasses("abcDEF123-"); got != 4 {
			t.Fatalf("charClasses = %d, want 4", got)
		}
		if got := charClasses("abcdef"); got != 1 {
			t.Fatalf("charClasses = %d, want 1", got)
		}
	})
	t.Run("high confidence hit wins the overlap", func(t *testing.T) {
		t.Parallel()
		// docs/modules/security.md: entropy hits are deduplicated against regex hits.
		fs := NewDefault().Scan("token ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8")
		if len(fs) != 1 || fs[0].Rule != "github-token" {
			t.Fatalf("Scan = %+v, want only the github-token finding", fs)
		}
	})
}

// The invariant this package exists to keep: nothing that leaves the scanner
// carries the matched bytes.
func TestPreviewNeverEchoesTheSecret(t *testing.T) {
	t.Parallel()
	s := NewDefault()
	secrets := []string{
		"ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8",
		"AKIA3M7QKX9PLZW2VBNQ",
		"xoxb-2374829174-4823947192-8sKd93jLPqmVn2Xw",
		"AIzaSyD-9tSrke72PouQMnMX-a7eZSW0jkFMBWY",
		"4539578763621486",
		"aG7fQ2mZx9Kw3PLbTs01VuYr8NdEjHi5XcOgAqWvRt4B",
	}
	for _, secret := range secrets {
		fs := s.Scan("value: " + secret)
		if len(fs) == 0 {
			t.Fatalf("no finding for %q", secret)
		}
		for _, f := range fs {
			if want := fmt.Sprintf("[REDACTED:%s](%dB)", f.Rule, f.Length); f.Preview != want {
				t.Errorf("Preview = %q, want %q", f.Preview, want)
			}
			if strings.Contains(f.Preview, secret) {
				t.Errorf("Preview %q echoes the secret", f.Preview)
			}
			// No 6-gram of the secret may appear in the preview either: the
			// preview must be a function of (rule, length) alone.
			for i := 0; i+6 <= len(secret); i++ {
				if strings.Contains(f.Preview, secret[i:i+6]) {
					t.Fatalf("Preview %q contains secret fragment %q", f.Preview, secret[i:i+6])
				}
			}
			// The same holds for the audit projection.
			rec, err := json.Marshal(Records([]Finding{f}))
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i+6 <= len(secret); i++ {
				if strings.Contains(string(rec), secret[i:i+6]) {
					t.Fatalf("audit record %s contains secret fragment %q", rec, secret[i:i+6])
				}
			}
		}
	}
}

func TestHeadTailWindows(t *testing.T) {
	t.Parallel()
	s := mustScanner(t, Config{WindowBytes: 1024})
	payload := "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	filler := strings.Repeat("lorem ipsum dolor sit amet ", 100) // ~2.7 KB

	t.Run("middle escapes the windows", func(t *testing.T) {
		t.Parallel()
		// Documented fail-open trade-off: bounded work over completeness.
		if fs := s.Scan(filler + payload + filler); len(fs) != 0 {
			t.Fatalf("middle content was scanned despite windowing: %+v", fs)
		}
	})
	t.Run("head hit", func(t *testing.T) {
		t.Parallel()
		fs := s.Scan(payload + filler + filler)
		if len(fs) != 1 || fs[0].Window != WindowHead {
			t.Fatalf("want one head-window finding, got %+v", fs)
		}
	})
	t.Run("tail hit keeps absolute offsets", func(t *testing.T) {
		t.Parallel()
		content := filler + filler + payload
		fs := s.Scan(content)
		if len(fs) != 1 || fs[0].Window != WindowTail {
			t.Fatalf("want one tail-window finding, got %+v", fs)
		}
		f := fs[0]
		if got := content[f.Start:f.End]; got != payload {
			t.Fatalf("tail span [%d,%d) = %q, want the payload", f.Start, f.End, got)
		}
	})
	t.Run("small content is scanned whole", func(t *testing.T) {
		t.Parallel()
		fs := s.Scan(payload)
		if len(fs) != 1 || fs[0].Window != WindowFull {
			t.Fatalf("want one full-window finding, got %+v", fs)
		}
	})
	t.Run("negative window disables windowing", func(t *testing.T) {
		t.Parallel()
		full := mustScanner(t, Config{WindowBytes: -1})
		fs := full.Scan(filler + payload + filler)
		if len(fs) != 1 || fs[0].Window != WindowFull {
			t.Fatalf("want the middle payload found in a full scan, got %+v", fs)
		}
	})
	t.Run("multi-byte runes are never split", func(t *testing.T) {
		t.Parallel()
		// Every window edge lands on a rune boundary, so the sliced windows
		// stay valid UTF-8 whatever the offset arithmetic produces.
		unicodeFiller := strings.Repeat("日本語テキスト ", 300)
		content := unicodeFiller + payload
		fs := s.Scan(content)
		if len(fs) != 1 || content[fs[0].Start:fs[0].End] != payload {
			t.Fatalf("unicode filler broke the span: %+v", fs)
		}
	})
}

func TestMaxFindingsCap(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for i := range 20 {
		fmt.Fprintf(&b, "AKIA3M7QKX9PLZW2VB%02d\n", i)
	}
	content := b.String()

	s := mustScanner(t, Config{MaxFindings: 5})
	if fs := s.Scan(content); len(fs) != 5 {
		t.Fatalf("MaxFindings=5 returned %d findings", len(fs))
	}
	unlimited := mustScanner(t, Config{MaxFindings: -1})
	if fs := unlimited.Scan(content); len(fs) != 20 {
		t.Fatalf("unlimited scan returned %d findings, want 20", len(fs))
	}
}

func TestScanIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	s := NewDefault()
	corpus := goldenCorpus()
	want := make([][]Finding, len(corpus))
	for i, c := range corpus {
		want[i] = s.Scan(c.Content)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i, c := range corpus {
				got, _ := json.Marshal(s.Scan(c.Content))
				exp, _ := json.Marshal(want[i])
				if string(got) != string(exp) {
					t.Errorf("concurrent scan of %q diverged:\n got %s\nwant %s", c.Name, got, exp)
				}
			}
		}()
	}
	wg.Wait()
}

func TestInternalHostClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		host string
		want bool
	}{
		{"10.4.2.9", true},
		{"192.168.0.1:5432", true},
		{"127.0.0.1:8080", true},
		{"localhost", true},
		{"db.internal", true},
		{"printer.local", true},
		{"jira.corp:8443", true},
		{"redis", true}, // single label: a compose service, not a public name
		{"api.vendor.com", false},
		{"8.8.8.8", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isInternalHost(tc.host); got != tc.want {
			t.Errorf("isInternalHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestTextRoundTrips(t *testing.T) {
	t.Parallel()
	for _, s := range []Severity{SeverityLow, SeverityMedium, SeverityHigh} {
		b, err := s.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%v): %v", s, err)
		}
		var back Severity
		if err := back.UnmarshalText(b); err != nil || back != s {
			t.Fatalf("severity roundtrip %v -> %s -> %v (%v)", s, b, back, err)
		}
	}
	if _, err := Severity(0).MarshalText(); err == nil {
		t.Fatal("MarshalText accepted the zero severity")
	}
	var sev Severity
	if err := sev.UnmarshalText([]byte("nope")); err == nil {
		t.Fatal("UnmarshalText accepted junk severity")
	}

	for _, r := range []Redaction{RedactNone, RedactMatch, RedactSecret} {
		b, err := r.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%v): %v", r, err)
		}
		var back Redaction
		if err := back.UnmarshalText(b); err != nil || back != r {
			t.Fatalf("redaction roundtrip %v -> %s -> %v (%v)", r, b, back, err)
		}
	}
	if _, err := Redaction(0).MarshalText(); err == nil {
		t.Fatal("MarshalText accepted the zero redaction")
	}
	var red Redaction
	if err := red.UnmarshalText([]byte("nope")); err == nil {
		t.Fatal("UnmarshalText accepted junk redaction")
	}
}

// goldenEntry pins the exact findings (rule, severity, redaction, span,
// length, window, preview) for a fixed corpus — determinism is contract
// (canonical.md §6), and the preview column is the golden proof that no
// matched byte reaches the record.
type goldenEntry struct {
	Name     string    `json:"name"`
	Findings []Finding `json:"findings"`
}

func goldenCorpus() []struct{ Name, Content string } {
	return []struct{ Name, Content string }{
		{"clean", "The quick brown fox jumps over the lazy dog. 12345 files processed."},
		{"pem-block", "key:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA7x\n-----END RSA PRIVATE KEY-----\n"},
		{"aws-pair", "AKIA3M7QKX9PLZW2VBNQ\naws_secret_access_key=kJ7pQz2mNx8vRt5wYh3LcB6dEfG9sAu1TnV4pQ0z"},
		{"aws-docs-sample", "AKIAIOSFODNN7EXAMPLE and wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		{"vendor-tokens", "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8 xoxb-2374829174-4823947192-8sKd93jLPqmVn2Xw sk_live_51H8xQ2eZvKYlo2CkVbNmPqRs"},
		{"jwt", "id_token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"},
		{"authorization", "GET /v1 HTTP/1.1\nAuthorization: Bearer sk-abc123XYZ789tokenvalue0011\n"},
		{"card", "charged 4539 5787 6362 1486 for 12 USD"},
		{"email-password", "alice@example.org:Tr0ub4dor&3"},
		{"credential-urls", "postgres://svc:Kx7pQz2mNx8@db.internal:5432/app https://svcuser:S3cretPass99@api.vendor.com/v1"},
		{"entropy-only", "opaque=Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MFFXRVJUWXVpb3A"},
		{"placeholders", "GITHUB_TOKEN=ghp_your-token-here-000000000000000000 postgres://user:password@localhost:5432/app"},
	}
}

func TestScanGolden(t *testing.T) {
	t.Parallel()
	s := NewDefault()
	entries := make([]goldenEntry, 0, len(goldenCorpus()))
	for _, c := range goldenCorpus() {
		fs := s.Scan(c.Content)
		if fs == nil {
			fs = []Finding{}
		}
		entries = append(entries, goldenEntry{Name: c.Name, Findings: fs})
	}
	got, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	goldenPath := filepath.Join("testdata", "scan_golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("golden mismatch (run with -update after intentional rule changes)\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
