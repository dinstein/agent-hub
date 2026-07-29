package leakguard

import (
	"fmt"
	"regexp"
)

// Severity ranks a finding. Higher is worse. The zero value is invalid so an
// uninitialised Rule fails validation instead of silently scanning at "low".
type Severity int

// Severity levels, low to high.
const (
	// SeverityLow is the heuristic band: entropy signals and anything that
	// cannot be structurally confirmed. Below the default redaction floor.
	SeverityLow Severity = iota + 1
	// SeverityMedium is a structurally plausible credential that context
	// could still explain away.
	SeverityMedium
	// SeverityHigh is a credential identified by its own format.
	SeverityHigh
)

// String implements fmt.Stringer.
func (s Severity) String() string {
	switch s {
	case SeverityLow:
		return "low"
	case SeverityMedium:
		return "medium"
	case SeverityHigh:
		return "high"
	default:
		return fmt.Sprintf("severity(%d)", int(s))
	}
}

// MarshalText implements encoding.TextMarshaler (the textual form is the
// stable one: golden files and audit records carry it, not the integer).
func (s Severity) MarshalText() ([]byte, error) {
	switch s {
	case SeverityLow, SeverityMedium, SeverityHigh:
		return []byte(s.String()), nil
	default:
		return nil, fmt.Errorf("leakguard: invalid severity %d", int(s))
	}
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Severity) UnmarshalText(b []byte) error {
	switch string(b) {
	case "low":
		*s = SeverityLow
	case "medium":
		*s = SeverityMedium
	case "high":
		*s = SeverityHigh
	default:
		return fmt.Errorf("leakguard: unknown severity %q", b)
	}
	return nil
}

// Redaction is a rule's inline disposition strategy — the third component of
// a rule's identity alongside ID and severity.
type Redaction int

// Redaction strategies.
const (
	// RedactNone marks an audit-only signal: it is NEVER rewritten inline,
	// whatever the policy says. Low-confidence detectors carry it so that
	// enabling inline mode cannot make them mangle legitimate content.
	RedactNone Redaction = iota + 1
	// RedactMatch replaces the whole match. Used when the match IS the
	// secret (a PEM block, a bare token, a card number).
	RedactMatch
	// RedactSecret replaces only the `secret` capture group, keeping the
	// surrounding context readable (the assignment, the header name, the URL
	// around the password). Rules declaring it MUST have a group named
	// "secret" — compileRules enforces that.
	RedactSecret
)

// String implements fmt.Stringer.
func (r Redaction) String() string {
	switch r {
	case RedactNone:
		return "none"
	case RedactMatch:
		return "match"
	case RedactSecret:
		return "secret"
	default:
		return fmt.Sprintf("redaction(%d)", int(r))
	}
}

// MarshalText implements encoding.TextMarshaler.
func (r Redaction) MarshalText() ([]byte, error) {
	switch r {
	case RedactNone, RedactMatch, RedactSecret:
		return []byte(r.String()), nil
	default:
		return nil, fmt.Errorf("leakguard: invalid redaction %d", int(r))
	}
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (r *Redaction) UnmarshalText(b []byte) error {
	switch string(b) {
	case "none":
		*r = RedactNone
	case "match":
		*r = RedactMatch
	case "secret":
		*r = RedactSecret
	default:
		return fmt.Errorf("leakguard: unknown redaction %q", b)
	}
	return nil
}

// Match is what a rule's validator sees. It carries the matched text so the
// validator can check a checksum or decode a header; nothing here reaches a
// Finding, an audit record or a log line.
type Match struct {
	// Full is the whole match, Secret the `secret` group (== Full when the
	// rule declares none).
	Full   string
	Secret string
	groups map[string]string
}

// Group returns a named capture group, "" when absent.
func (m Match) Group(name string) string { return m.groups[name] }

// Rule is one detection rule. Regex is matched against the RAW content
// (secrets are case- and alphabet-sensitive; see scanChunk).
type Rule struct {
	// ID names the rule in findings, previews, redaction labels and audit
	// records. IDs are ABI once emitted.
	ID       string
	Severity Severity
	// Redaction is the inline strategy; see the Redaction constants.
	Redaction Redaction
	// Regex is an RE2 pattern. A group named `secret` selects the sub-span
	// that RedactSecret replaces.
	Regex string
	// Validate, when non-nil, decides whether a raw match is a real finding.
	// It is the false-positive gate: placeholder detection, Luhn, JWT header
	// decoding, host classification. Returning false drops the match
	// silently — a validator must never panic on adversarial input.
	Validate func(Match) bool
}

type compiledRule struct {
	Rule
	re         *regexp.Regexp
	secretIdx  int            // index of the `secret` group, 0 when absent
	groupNames map[string]int // named groups other than `secret`
}

// secretGroup is the reserved capture-group name RedactSecret redacts.
const secretGroup = "secret"

func compileRules(rules []Rule) ([]compiledRule, error) {
	out := make([]compiledRule, 0, len(rules))
	seen := make(map[string]bool, len(rules))
	for i, r := range rules {
		switch {
		case r.ID == "":
			return nil, fmt.Errorf("leakguard: rule %d has no ID", i)
		case seen[r.ID]:
			return nil, fmt.Errorf("leakguard: duplicate rule ID %q", r.ID)
		case r.ID == EntropyRuleID:
			return nil, fmt.Errorf("leakguard: rule ID %q is reserved for the entropy heuristic", r.ID)
		case r.Severity < SeverityLow || r.Severity > SeverityHigh:
			return nil, fmt.Errorf("leakguard: rule %q has invalid severity %d", r.ID, int(r.Severity))
		case r.Redaction < RedactNone || r.Redaction > RedactSecret:
			return nil, fmt.Errorf("leakguard: rule %q has invalid redaction %d", r.ID, int(r.Redaction))
		case r.Regex == "":
			return nil, fmt.Errorf("leakguard: rule %q has no Regex", r.ID)
		}
		seen[r.ID] = true
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			return nil, fmt.Errorf("leakguard: rule %q: %w", r.ID, err)
		}
		cr := compiledRule{Rule: r, re: re, secretIdx: re.SubexpIndex(secretGroup)}
		for idx, name := range re.SubexpNames() {
			if name == "" || name == secretGroup {
				continue
			}
			if cr.groupNames == nil {
				cr.groupNames = make(map[string]int, 2)
			}
			cr.groupNames[name] = idx
		}
		// A RedactSecret rule without the group would silently redact the
		// whole match instead of the secret — fail at construction, not in
		// production.
		if r.Redaction == RedactSecret && cr.secretIdx <= 0 {
			return nil, fmt.Errorf("leakguard: rule %q uses RedactSecret without a (?P<secret>…) group", r.ID)
		}
		out = append(out, cr)
	}
	return out, nil
}

// DefaultRules returns the built-in high-confidence table (docs/modules/security.md).
// Callers get a fresh slice and may append or replace.
//
// Every rule keys on a credential's OWN structure — a PEM header, a vendor
// prefix, a decodable JWT header, a Luhn-valid digit run — and every rule
// that could plausibly match documentation carries a validator that rejects
// placeholders. That combination, not a severity knob, is what makes inline
// redaction safe enough to be offered at all.
func DefaultRules() []Rule {
	return []Rule{
		{
			// The END anchor is preferred (leftmost-first alternation), so a
			// complete block is redacted as a whole; a truncated block still
			// matches on its header alone — a key fragment is still a key.
			ID:        "private-key-block",
			Severity:  SeverityHigh,
			Redaction: RedactMatch,
			Regex: `-----BEGIN [A-Z0-9 ]{0,24}PRIVATE KEY( BLOCK)?-----(?s:.*?)-----END [A-Z0-9 ]{0,24}PRIVATE KEY( BLOCK)?-----` +
				`|-----BEGIN [A-Z0-9 ]{0,24}PRIVATE KEY( BLOCK)?-----`,
		},
		{
			ID:        "aws-access-key-id",
			Severity:  SeverityHigh,
			Redaction: RedactMatch,
			Regex:     `\b(?:AKIA|ASIA|ABIA|ACCA|AIDA|AGPA|AIPA|ANPA|ANVA|AROA|APKA)[A-Z0-9]{16}\b`,
			Validate:  func(m Match) bool { return !isPlaceholder(m.Full) },
		},
		{
			// The 40-char secret has no distinguishing shape of its own, so
			// the rule requires the naming context and redacts only the value
			// (fail-open: an unlabelled secret is left to the entropy signal).
			ID:        "aws-secret-access-key",
			Severity:  SeverityHigh,
			Redaction: RedactSecret,
			Regex:     `(?i)aws[a-z0-9_.\-]{0,24}(?:secret|private)[a-z0-9_.\-]{0,24}["']?\s*[:=]\s*["']?(?P<secret>[A-Za-z0-9/+=]{40})`,
			Validate:  func(m Match) bool { return !isPlaceholder(m.Secret) && distinctChars(m.Secret) >= 8 },
		},
		{
			ID:        "github-token",
			Severity:  SeverityHigh,
			Redaction: RedactMatch,
			Regex:     `\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{40,255})\b`,
			Validate:  func(m Match) bool { return !isPlaceholder(m.Full) },
		},
		{
			// agenthub's OWN agent token. It needs a labelled rule because the
			// entropy heuristic structurally cannot see it: the body is 64 hex
			// characters, and hex tops out at 4.0 bits/char, below the 4.5
			// threshold. That exclusion is deliberate and right — a digest is
			// not a secret, and flagging every SHA would make the signal
			// useless — but it means a hex-bodied credential is invisible to
			// the one pass that catches unlabelled secrets.
			//
			// Worth having even though agenthub never prints these back: the
			// leak path is a downstream tool returning them. A tool that reads
			// files, greps a repo, or dumps an environment hands back whatever
			// the operator stored, and an agent token is exactly the kind of
			// thing that ends up in a .env or a shell profile.
			//
			// The prefix is spelled here rather than imported: internal/guard/*
			// is a zero-business-dependency foundation and cannot reach
			// internal/httpbridge. TestMintedTokenIsDetectedAsALeak on that
			// side mints a real token and asserts this rule fires, so the two
			// copies are held together by a test rather than by the compiler —
			// the same arrangement as api/paths.go and internal/platform.
			ID:        "agenthub-agent-token",
			Severity:  SeverityHigh,
			Redaction: RedactMatch,
			Regex:     `\bagt_[0-9a-f]{64}\b`,
			Validate:  func(m Match) bool { return !isPlaceholder(m.Full) },
		},
		{
			ID:        "slack-token",
			Severity:  SeverityHigh,
			Redaction: RedactMatch,
			Regex:     `\bxox[baprs]-[A-Za-z0-9-]{10,255}\b`,
			Validate:  func(m Match) bool { return !isPlaceholder(m.Full) },
		},
		{
			ID:        "google-api-key",
			Severity:  SeverityHigh,
			Redaction: RedactMatch,
			// No trailing \b: the alphabet includes '-', after which a word
			// boundary would not hold and the key would be missed.
			Regex:    `\bAIza[0-9A-Za-z_-]{35}`,
			Validate: func(m Match) bool { return !isPlaceholder(m.Full) },
		},
		{
			ID:        "stripe-secret-key",
			Severity:  SeverityHigh,
			Redaction: RedactMatch,
			Regex:     `\b(?:sk|rk)_live_[0-9A-Za-z]{10,255}\b`,
			Validate:  func(m Match) bool { return !isPlaceholder(m.Full) },
		},
		{
			// Three base64url segments alone are not evidence; the header
			// must actually decode to a JSON object with an `alg` claim.
			ID:        "jwt",
			Severity:  SeverityHigh,
			Redaction: RedactMatch,
			Regex:     `\beyJ[A-Za-z0-9_-]{10,1000}\.[A-Za-z0-9_-]{10,1000}\.[A-Za-z0-9_-]{8,1000}`,
			Validate:  func(m Match) bool { return jwtHeaderHasAlg(m.Full) },
		},
		{
			// The header name makes this high-confidence; the bare-Bearer
			// rule below covers the rest at a lower severity. Overlaps
			// between the two collapse to this one (resolveOverlaps).
			ID:        "authorization-header",
			Severity:  SeverityHigh,
			Redaction: RedactSecret,
			Regex:     `(?i)(?:proxy-)?authorization["']?\s*[:=]\s*["']?(?:bearer|basic|token)\s+(?P<secret>[A-Za-z0-9._~+/=-]{8,1000})`,
			Validate:  func(m Match) bool { return !isPlaceholder(m.Secret) },
		},
		{
			ID:        "bearer-token",
			Severity:  SeverityMedium,
			Redaction: RedactSecret,
			Regex:     `\b[Bb]earer\s+(?P<secret>[A-Za-z0-9._~+/=-]{20,1000})`,
			Validate:  func(m Match) bool { return !isPlaceholder(m.Secret) && distinctChars(m.Secret) >= 6 },
		},
		{
			// Digit runs are everywhere, so three gates: a plausible length,
			// a Luhn checksum, and a known issuer prefix. Anything longer
			// than 19 digits cannot match at all (the \b anchors force the
			// match to span a whole digit run), which keeps ids and
			// timestamps out.
			ID:        "credit-card",
			Severity:  SeverityHigh,
			Redaction: RedactMatch,
			Regex:     `\b\d(?:[ -]?\d){11,18}\b`,
			Validate:  func(m Match) bool { return isPlausibleCard(m.Full) },
		},
		{
			ID:        "email-password-pair",
			Severity:  SeverityMedium,
			Redaction: RedactSecret,
			// The punctuation branch allows NO whitespace: "bob@corp.com; thanks"
			// is a sentence, "bob@corp.com:Tr0ub4dor" is a credential pair.
			// The keyword branch may space out because the keyword itself is
			// the evidence.
			Regex: `(?i)\b[a-z0-9._%+-]{1,64}@[a-z0-9.-]{1,255}\.[a-z]{2,24}` +
				`(?:[:,;|]|\s{1,4}(?:password|passwd|pwd)\s*[:=]\s*)(?P<secret>[^\s'",;/@]{6,128})`,
			Validate: func(m Match) bool { return isPasswordLike(m.Secret) },
		},
		{
			// Credential URL to a public host. The private-host variant is a
			// separate rule so audit can tell "credentials for an external
			// service" from "credentials for something on this network"; the
			// two validators are complementary, so exactly one can fire.
			ID:        "credential-url",
			Severity:  SeverityHigh,
			Redaction: RedactSecret,
			Regex:     credentialURLPattern,
			Validate: func(m Match) bool {
				return isPasswordLike(m.Secret) && !isInternalHost(m.Group("host"))
			},
		},
		{
			ID:        "internal-credential-url",
			Severity:  SeverityHigh,
			Redaction: RedactSecret,
			Regex:     credentialURLPattern,
			Validate: func(m Match) bool {
				return isPasswordLike(m.Secret) && isInternalHost(m.Group("host"))
			},
		},
	}
}

// credentialURLPattern captures the password of a user:pass@host URL plus the
// host, which decides which of the two credential-url rules owns the match.
const credentialURLPattern = `(?i)\b[a-z][a-z0-9+.\-]{1,20}://[^\s/@:]{1,64}:(?P<secret>[^\s/@]{1,128})@(?P<host>[^\s/?#]{1,255})`
