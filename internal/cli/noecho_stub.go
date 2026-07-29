//go:build !darwin && !linux

package cli

import (
	"errors"
	"os"
)

// readNoEcho is unavailable on platforms outside the M1 support matrix
// (macOS + Linux). Returning an error rather than falling back to an
// echoing read keeps the "a credential never reaches the scrollback"
// invariant platform-independent: the caller turns this into a usage error
// pointing at --stdin.
func readNoEcho(*os.File) (string, error) {
	return "", errors.New("no-echo terminal input is not supported on this platform")
}
