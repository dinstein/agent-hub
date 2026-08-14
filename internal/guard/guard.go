// Package guard is the root of the security guard foundation
// (docs/conventions.md#package-layout: internal/guard/* are zero-business-dependency
// foundations; depguard restricts them to the standard library plus
// internal/guard itself).
//
// The actual guards live in the subpackages:
//
//   - spawnguard: anti-smuggling checks on spawn command lines
//   - netguard:   SSRF host/IP predicates and the dial-time control hook
//
// Both refuse a destination or a process regardless of who asked, which is
// why they survived the removal of the runtime governance surfaces: an
// injection scanner over downstream results and a sensitive-data egress
// detector were listed here too, and each went with the stage that read its
// verdict. Nothing in agenthub inspects what a downstream returned.
//
// This package only holds what the subpackages share: the decidable
// rejection sentinel. Per docs/conventions.md#engineering-conventions ("error handling"), every typed
// guard rejection — *spawnguard.Blocked, *netguard.BlockedError — must
// satisfy errors.Is(err, guard.ErrBlocked) so callers can classify guard
// rejections without importing every subpackage.
package guard

import "errors"

// ErrBlocked is the sentinel every guard-layer rejection unwraps to.
// Typed errors in the subpackages carry the machine-readable code and the
// human-readable reason; this sentinel only answers "was it a guard block?".
var ErrBlocked = errors.New("guard: blocked")
