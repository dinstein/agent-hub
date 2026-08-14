//go:build windows

package api

import (
	"fmt"
	"os/user"
)

// currentUserSID returns the current user's SID string, which is what the
// control pipe's name hashes. os/user fills Uid with the SID on Windows, so
// this needs no syscall and no dependency — the same trick
// internal/platform.currentUserSID uses, duplicated here because this package
// may not import it (docs/conventions.md#dependency-directions rule 1).
func currentUserSID() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	if u.Uid == "" {
		return "", fmt.Errorf("empty SID for user %q", u.Username)
	}
	return u.Uid, nil
}
