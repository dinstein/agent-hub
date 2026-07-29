//go:build !windows

package platform

import (
	"fmt"
	"os/user"
)

// Non-Windows stand-ins for the app-model probes. They exist so the Windows
// resolution logic in windows.go stays one file, compiled everywhere and
// unit tested everywhere through the Resolver hooks.

// currentPackageIdentity: no app containers outside Windows.
func currentPackageIdentity() PackageIdentity { return PackageIdentity{} }

// currentUserSID has no meaning here. It is only reachable if a caller
// forces GOOS="windows" on a Resolver without injecting Resolver.UserSID,
// which is a test doing something it should not; the error says so.
func currentUserSID() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("no Windows SID on this platform (uid %s): inject Resolver.UserSID", u.Uid)
}
