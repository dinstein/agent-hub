//go:build !linux && !darwin

package ctlapi

import (
	"fmt"
	"net"
	"runtime"

	"github.com/dinstein/agent-hub/internal/platform"
)

// peerCredSupported gates Listen: platforms without a peer credential
// implementation must not serve the control socket at all — fail-closed, never
// "listen without checking".
//
// It is still false on Windows, and that is no longer a gap. Its subject is a
// UNIX SOCKET, which Windows has and for which it has no SO_PEERCRED
// equivalent. The Windows control endpoint is a named pipe, taken earlier in
// Listen and authorized by its SDDL (pipelisten_windows.go). A Windows daemon
// pointed at a socket path — AGENTHUB_SOCKET is honoured on every platform —
// therefore refuses, which is the right answer: it could bind one and never
// learn who was connecting.
const peerCredSupported = false

// peerUID is the unsupported-platform seam. It always fails, and because
// peerCredSupported is false, Listen refuses to create a listener before
// any connection could reach this path.
func peerUID(*net.UnixConn) (uint32, error) {
	return 0, fmt.Errorf("ctlapi: peer cred on %s: %w", runtime.GOOS, platform.ErrUnsupportedPlatform)
}
