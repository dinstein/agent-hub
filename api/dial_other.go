//go:build !windows

package api

import (
	"context"
	"fmt"
	"net"
	"runtime"
)

// dialEndpoint connects to the control plane over a Unix domain socket.
//
// A pipe-shaped endpoint is refused rather than attempted. It is reachable the
// same way the listener's is — AGENTHUB_SOCKET travels in copied command lines
// and in configuration written on another machine — and the failure it prevents
// is the confusing one: net.Dial("unix", `\\.\pipe\agenthub-ctl-…`) reports
// "no such file or directory", which reads as "the daemon is not running" and
// sends the operator off to start a daemon that is already running, on a
// platform that will never serve that endpoint.
func dialEndpoint(ctx context.Context, path string) (net.Conn, error) {
	if isPipePath(path) {
		return nil, fmt.Errorf("api: %s is a Windows named pipe and this is %s: %w",
			path, runtime.GOOS, ErrUnsupportedPlatform)
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}
