//go:build !windows

package ctlapi

import (
	"fmt"
	"net"
	"runtime"

	"github.com/dinstein/agent-hub/internal/platform"
)

// listenPipe is the non-Windows seam. It is reachable: AGENTHUB_SOCKET is
// honoured on every platform, so a pipe-shaped value can be handed to a Linux
// daemon — by a copied command line, or by a config written on the other
// machine.
//
// Failure direction: refuse. The alternative is to fall through to the Unix
// path and call net.Listen("unix", `\\.\pipe\agenthub-ctl-…`), which creates a
// FILE with backslashes in its name in the current directory and then serves
// the control plane on it. Nothing about that fails, and the operator who asked
// for a pipe would have an endpoint nobody dials and a stray file to discover
// months later.
func listenPipe(path string) (net.Listener, error) {
	return nil, fmt.Errorf("ctlapi: %s is a Windows named pipe and this is %s: %w",
		path, runtime.GOOS, platform.ErrUnsupportedPlatform)
}
