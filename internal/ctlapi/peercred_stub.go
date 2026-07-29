//go:build !linux && !darwin

package ctlapi

import (
	"fmt"
	"net"
	"runtime"

	"github.com/dinstein/agent-hub/internal/platform"
)

// peerCredSupported gates Listen: platforms without a peer credential
// implementation (Windows named pipes + SDDL arrive in M2) must not serve
// the control socket at all — fail-closed, never "listen without checking".
const peerCredSupported = false

// peerUID is the unsupported-platform seam. It always fails, and because
// peerCredSupported is false, Listen refuses to create a listener before
// any connection could reach this path.
func peerUID(*net.UnixConn) (uint32, error) {
	return 0, fmt.Errorf("ctlapi: peer cred on %s: %w", runtime.GOOS, platform.ErrUnsupportedPlatform)
}
