//go:build linux

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

// peerUID returns the uid of the process on the other end of a Unix domain
// socket connection via SO_PEERCRED.
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
		ucred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if e != nil {
			sockErr = e
			return
		}
		uid = ucred.Uid
	}); err != nil {
		return 0, fmt.Errorf("ctlapi: peer cred: %w", err)
	}
	if sockErr != nil {
		return 0, fmt.Errorf("ctlapi: peer cred: %w", sockErr)
	}
	return uid, nil
}
