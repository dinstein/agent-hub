package injection

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// normText is a normalized view of some original content plus the byte-offset
// map back into it.
//
// Invariant: len(offs) == len(text)+1. offs[i] is the byte offset in the
// original content of the rune that produced normalized byte i; offs[len(text)]
// is len(original). A match [s,e) on text therefore maps to the original span
// [offs[s], offs[e]) — approximate under normalization (stripped characters
// adjacent to the match are absorbed into the span), which is acceptable: the
// span locates the payload, it is not used to slice out an exact quote.
type normText struct {
	text string
	offs []int
}

// normalizeContent produces the canonical scan form of content, the NFKC-like
// normalization achievable with the standard library alone (docs/flows.md;
// x/text is out of reach for a zero-dependency foundation package):
//
//   - zero-width, bidi-control, variation-selector and soft-hyphen code
//     points are stripped (defeats zero-width interleaving)
//   - combining marks (Mn) are stripped (defeats diacritic cloaking of
//     ASCII payloads)
//   - fullwidth ASCII variants U+FF01..U+FF5E fold to their ASCII forms
//   - every whitespace run collapses to a single ' '
//   - everything is lowercased
//
// Rules (phrases and regexes) are matched against this form only.
func normalizeContent(s string) normText {
	var b strings.Builder
	b.Grow(len(s))
	offs := make([]int, 0, len(s)+1)
	lastSpace := false
	for i, r := range s {
		if isStrippedRune(r) || unicode.Is(unicode.Mn, r) {
			continue
		}
		if r >= 0xFF01 && r <= 0xFF5E { // fullwidth ASCII variants
			r -= 0xFEE0
		}
		if unicode.IsSpace(r) {
			if lastSpace {
				continue
			}
			r = ' '
			lastSpace = true
		} else {
			lastSpace = false
		}
		r = unicode.ToLower(r)
		if f, ok := confusableFold[r]; ok {
			r = f
		}
		for range utf8.RuneLen(r) {
			offs = append(offs, i)
		}
		b.WriteRune(r)
	}
	offs = append(offs, len(s))
	return normText{text: b.String(), offs: offs}
}

// confusableFold maps the Cyrillic and Greek letters that are visually
// identical (or near-identical) to an ASCII letter onto that letter.
//
// This is the last of the cloaking techniques the normalization exists to
// defeat. Zero-width interleaving, diacritic cloaking and fullwidth variants
// were all handled; a payload could still swap one Latin "o" for Cyrillic
// U+043E and go through untouched, which costs an attacker a single keystroke
// and blinds every phrase rule at once.
//
// Folding is applied AFTER lowercasing, so lowercase entries cover both cases:
// Cyrillic "В" lowercases to "в" before reaching this table. That is also why
// entries like в→b and н→h look wrong in isolation — the resemblance is in the
// UPPERCASE forms (В/B, Н/H), which is the pair an attacker uses.
//
// Not Unicode confusables in full (TR39 is a large data table and x/text is
// out of reach for a zero-dependency foundation). This is the subset that maps
// to ASCII letters, which is all the English-phrase rules need.
//
// False positives are implausible rather than merely unlikely: the rules are
// multi-word English phrases, so foreign text would have to fold into one by
// construction. Nothing here touches CJK, which shares no forms with ASCII.
var confusableFold = map[rune]rune{
	// Cyrillic
	'\u0430': 'a', '\u0432': 'b', '\u0435': 'e', '\u043A': 'k', '\u043C': 'm',
	'\u043D': 'h', '\u043E': 'o', '\u0440': 'p', '\u0441': 'c', '\u0442': 't',
	'\u0443': 'y', '\u0445': 'x', '\u0455': 's', '\u0456': 'i', '\u0458': 'j',
	'\u04BB': 'h', '\u04CF': 'l', '\u0501': 'd', '\u051B': 'q', '\u051D': 'w',
	// Greek
	'\u03B1': 'a', '\u03B2': 'b', '\u03B5': 'e', '\u03B9': 'i', '\u03BA': 'k',
	'\u03BD': 'v', '\u03BF': 'o', '\u03C1': 'p', '\u03C4': 't', '\u03C5': 'u',
	'\u03C7': 'x', '\u03B3': 'y', '\u03B7': 'n',
}

// isStrippedRune reports whether r is an invisible/format code point that an
// injection payload can interleave to defeat literal matching. Stripping is
// unconditional: these code points never change the meaning of legitimate
// text for scanning purposes.
func isStrippedRune(r rune) bool {
	switch r {
	case 0x00AD, // soft hyphen
		0x034F,                 // combining grapheme joiner
		0x180E,                 // Mongolian vowel separator
		0x200B, 0x200C, 0x200D, // zero-width space / non-joiner / joiner
		0x200E, 0x200F, // LRM / RLM
		0x2060,                         // word joiner
		0x2061, 0x2062, 0x2063, 0x2064, // invisible operators
		0xFEFF: // zero-width no-break space (BOM)
		return true
	}
	if r >= 0x202A && r <= 0x202E { // bidi embedding/override controls
		return true
	}
	if r >= 0x2066 && r <= 0x2069 { // bidi isolates
		return true
	}
	if r >= 0xFE00 && r <= 0xFE0F { // variation selectors
		return true
	}
	return false
}

// runeBoundary returns the smallest index >= i that starts a rune in s, so
// window slicing never splits a multi-byte rune.
func runeBoundary(s string, i int) int {
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}
