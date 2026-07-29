// Package buildrules proves that rules living in the BUILD system — not in
// Go code, and therefore invisible to the compiler and to golangci-lint —
// are actually in effect.
//
// It is the same discipline as internal/depguardtest one level out: that
// package proves the dependency directions in .golangci.yml really block,
// this one proves the Makefile's own registries have not fallen behind the
// tree they describe. A rule that is written down but silently inert is
// worse than no rule at all (canonical.md §6).
package buildrules
