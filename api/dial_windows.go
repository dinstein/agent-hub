//go:build windows

package api

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

// dialEndpoint connects to the control plane.
//
// Both branches are live on Windows. The endpoint is normally a named pipe, but
// AGENTHUB_SOCKET is honoured on every platform and Windows 10 does have AF_UNIX
// sockets, so a caller may legitimately name one — for a listener this build
// will not serve (ctlapi refuses a socket here: no peer-credential check
// exists), but possibly for one something else does.
//
// No dial timeout is passed: DialPipeContext waits until ctx says otherwise,
// and every caller already arrives with a deadline (tryDial gives each probe a
// second). Passing a second, shorter one here would silently override the
// caller's, which is how a long-lived SSE subscription acquires a timeout
// nobody asked for.
func dialEndpoint(ctx context.Context, path string) (net.Conn, error) {
	if isPipePath(path) {
		return winio.DialPipeContext(ctx, path)
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}
