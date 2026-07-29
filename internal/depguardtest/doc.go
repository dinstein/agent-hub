// Package depguardtest proves that the four dependency-direction
// constraints frozen in canonical.md §2 are actually enforced by the
// depguard configuration in .golangci.yml — not merely written down.
// "A lint rule that is configured but silently inert is worse than no
// rule at all" (canonical.md §6).
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
//   - Probes are written only under the repository tree and are always
//     removed by t.Cleanup, even on test failure. If internal/pipeline
//     did not exist before the test, the whole directory is removed.
//   - Every probe imports a package that is present in go.mod and
//     type-checks, so a lint failure can only come from depguard,
//     never from the compiler.
//   - The test skips (with instructions) when no golangci-lint binary
//     is found; CI installs the binary before `make test` so the proof
//     really executes there.
package depguardtest
