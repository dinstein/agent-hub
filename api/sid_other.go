//go:build !windows

package api

import (
	"fmt"
	"runtime"
)

// currentUserSID has no meaning outside Windows. It is only reachable if a
// caller forces goos == "windows" on this platform without injecting a SID,
// which is a test doing something it should not, and the error says so — the
// same shape as internal/platform's stand-in.
//
// Failure direction: an error, never a plausible-looking value. Returning the
// numeric uid would produce a syntactically valid pipe name derived from the
// wrong identity, and two users on one machine would then hash to whatever
// their uids collided into.
func currentUserSID() (string, error) {
	return "", fmt.Errorf("api: no Windows SID on %s: inject one", runtime.GOOS)
}
