// Package depguardtest proves that the four dependency-direction
// constraints frozen in docs/conventions.md#package-layout are actually enforced by the
// depguard configuration in .golangci.yml — not merely written down.
// "A lint rule that is configured but silently inert is worse than no
// rule at all" (docs/conventions.md#engineering-conventions).
//
// The test in this package injects, for each rule, a deliberately
// violating probe file (named zz_depguard_probe_*.go, git-ignored and
// removed via t.Cleanup) into the constrained package, runs golangci-lint
// on just that package, and asserts that depguard reports the violation.
// Each rule also has a control: linting the same package without the
// probe must yield zero issues.
//
// Invariants:
//
//   - Probes are written into a disposable copy of the checkout, never into
//     the checkout itself. The real tree is read-only for this package,
//     because `go test ./...` builds it from other packages at the same
//     time (test/e2e's TestMain) and a probe appearing and vanishing under
//     a concurrent `go build` breaks that build. Within the copy each probe
//     is still removed by t.Cleanup, even on failure: that is what lets the
//     control case lint the same package clean immediately afterwards.
//   - Every probe imports a package that is present in go.mod and
//     type-checks, so a lint failure can only come from depguard,
//     never from the compiler.
//   - The test skips (with instructions) when no golangci-lint binary
//     is found; CI installs the binary before `make test` so the proof
//     really executes there.
package depguardtest
