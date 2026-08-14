// Package buildrules proves that rules living in the BUILD system — not in
// Go code, and therefore invisible to the compiler and to golangci-lint —
// are actually in effect.
//
// It is the same discipline as internal/depguardtest one level out: that
// package proves the dependency directions in .golangci.yml really block,
// this one proves the registries and instructions that live outside the Go
// build have not fallen behind the tree they describe — the Makefile's own
// lists, the CI workflow, the documents that enumerate what is wired, and the
// release scripts. A rule that is written down but silently inert is worse
// than no rule at all (docs/conventions.md#engineering-conventions).
package buildrules
