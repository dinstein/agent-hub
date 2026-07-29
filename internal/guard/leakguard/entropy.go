package leakguard

import (
	"math"
	"regexp"
)

// EntropyRuleID is the rule ID the entropy heuristic emits. It is reserved:
// compileRules rejects a configured rule that tries to claim it, so an audit
// record carrying this ID always means "low-confidence statistical signal",
// never "somebody redefined it".
const EntropyRuleID = "high-entropy-string"

// entropyCandidate matches a secret-shaped token: one unbroken run from the
// alphabets secrets are printed in (base64, base64url, hex). Spaces and
// quotes end a candidate, so prose can never be one; '=' is accepted only as
// trailing padding, so `KEY=value` splits into the name and the value instead
// of measuring the entropy of both together.
var entropyCandidate = regexp.MustCompile(`[A-Za-z0-9+/_-]{16,512}={0,2}`)

// scanEntropy is the LOW-confidence pass (docs/modules/security.md: Shannon entropy with
// a 4.5 bits/char threshold, deduplicated against regex hits). Its findings
// are SeverityLow with RedactNone: they exist to tell an operator "something
// secret-shaped went out here", and they can never rewrite a result.
//
// Three gates, all necessary:
//
//   - length >= EntropyMinLen — short high-entropy runs are everywhere
//     (ids, hashes fragments, colour codes)
//   - entropy >= EntropyThreshold — hex digests top out at 4.0 bits/char and
//     are excluded by construction, which is intended: a digest is not a
//     secret
//   - at least three character classes — a single-case token, however long,
//     is far more likely to be an identifier than a key
//
// Deduplication against the high-confidence rules happens in resolveOverlaps:
// severity ordering means any overlapping regex hit wins.
func (s *Scanner) scanEntropy(chunk string, base int, window string) []Finding {
	if s.entropyMin < 0 {
		return nil
	}
	var out []Finding
	for _, loc := range entropyCandidate.FindAllStringIndex(chunk, s.perRuleCap) {
		tok := chunk[loc[0]:loc[1]]
		if len(tok) < s.entropyMin || len(tok) > maxEntropyTokenLen {
			continue
		}
		if charClasses(tok) < 3 {
			continue
		}
		e := shannonBits(tok)
		if e < s.entropyBits {
			continue
		}
		f := newFinding(EntropyRuleID, SeverityLow, RedactNone, base+loc[0], base+loc[1], window)
		// Rounded so the value is stable across platforms and golden files.
		f.Entropy = math.Round(e*100) / 100
		out = append(out, f)
	}
	return out
}

// shannonBits returns the Shannon entropy of s in bits per character, over
// its bytes. Secrets are ASCII by construction, so a byte histogram is the
// right unit; on multi-byte input the measure still holds (it can only
// over-estimate, and over-estimating merely produces a low-severity signal).
func shannonBits(s string) float64 {
	if s == "" {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	bits := 0.0
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		bits -= p * math.Log2(p)
	}
	return bits
}

// charClasses counts how many of {lower, upper, digit, symbol} appear in s.
func charClasses(s string) int {
	var lower, upper, digit, symbol bool
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= '0' && c <= '9':
			digit = true
		default:
			symbol = true
		}
	}
	n := 0
	for _, ok := range []bool{lower, upper, digit, symbol} {
		if ok {
			n++
		}
	}
	return n
}
