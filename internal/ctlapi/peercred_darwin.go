//go:build darwin

package ctlapi

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerCredSupported gates Listen: only platforms with a working peer
// credential check may serve the control socket at all (fail-closed: no
// peer check, no listener).
const peerCredSupported = true

// peerUID returns the effective uid of the process on the other end of a
// Unix domain socket connection via LOCAL_PEERCRED (struct xucred).
//
// Failure direction: any error here causes the caller to REJECT the
// connection — an unverifiable peer is treated as a hostile peer.
func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("ctlapi: peer cred: %w", err)
	}
	var (
		uid     uint32
		sockErr error
	)
	if err := raw.Control(func(fd uintptr) {
		xucred, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if e != nil {
			sockErr = e
			return
		}
		uid = xucred.Uid
	}); err != nil {
		return 0, fmt.Errorf("ctlapi: peer cred: %w", err)
	}
	if sockErr != nil {
		return 0, fmt.Errorf("ctlapi: peer cred: %w", sockErr)
	}
	return uid, nil
}
