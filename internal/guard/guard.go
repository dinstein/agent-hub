// Package guard is the root of the security guard foundation
// (canonical.md §2: internal/guard/* are zero-business-dependency
// foundations; depguard restricts them to the standard library plus
// internal/guard itself).
//
// The actual guards live in the subpackages:
//
//   - injection:  prompt-injection scanning of downstream results
//   - spawnguard: anti-smuggling checks on spawn command lines
//   - netguard:   SSRF host/IP predicates and the dial-time control hook
//   - leakguard:  sensitive-data egress detection
//
// This package only holds what the subpackages share: the decidable
// rejection sentinel. Per docs/modules/foundation.md ("error-handling conventions"), every typed
// guard rejection — *spawnguard.Blocked, *netguard.BlockedError — must
// satisfy errors.Is(err, guard.ErrBlocked) so callers can classify guard
// rejections without importing every subpackage.
package guard

import "errors"

// ErrBlocked is the sentinel every guard-layer rejection unwraps to.
// Typed errors in the subpackages carry the machine-readable code and the
// human-readable reason; this sentinel only answers "was it a guard block?".
var ErrBlocked = errors.New("guard: blocked")
