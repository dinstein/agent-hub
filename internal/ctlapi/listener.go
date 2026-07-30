package ctlapi

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/dinstein/agent-hub/internal/platform"
)

// ErrAlreadyRunning is returned by Listen when another live process is
// accepting on the control socket (the stale-socket probe connected).
var ErrAlreadyRunning = errors.New("ctlapi: control socket is already being served")

// probeTimeout bounds the stale-socket liveness dial in Listen.
const probeTimeout = 500 * time.Millisecond

// Listen prepares and binds the control socket at socketPath
// (platform.CtlSocketPath for the daemon; tests pass their own path):
//
//  1. the parent directory is created/tightened to 0700 (first auth gate);
//  2. a pre-existing socket file is probed with a dial: if the dial
//     SUCCEEDS a live daemon owns it and Listen fails with
//     ErrAlreadyRunning; only when the dial FAILS is the stale socket
//     removed (never delete a live endpoint);
//  3. the socket is bound and chmodded to 0600;
//  4. the listener is wrapped so every accepted connection has its peer
//     credentials checked against this process's uid (second auth gate).
//
// On Windows socketPath is a named pipe (platform.CtlSocketPath returns
// \\.\pipe\agenthub-ctl-…) and none of the four steps above apply: there is no
// directory to tighten, no file to chmod, and nothing left behind to clean up.
// That branch is taken FIRST, before the peer-credential gate, because the
// gate's subject is a Unix socket — see listenPipe, where the authorization
// those two gates provide is done by the pipe's SDDL instead.
//
// On platforms without a peer credential implementation Listen fails with
// platform.ErrUnsupportedPlatform — fail-closed: no peer check, no
// control plane.
func Listen(socketPath string) (net.Listener, error) {
	if platform.IsPipePath(socketPath) {
		return listenPipe(socketPath)
	}
	if !peerCredSupported {
		return nil, fmt.Errorf("ctlapi: listen on %s: %w", runtime.GOOS, platform.ErrUnsupportedPlatform)
	}
	if err := platform.EnsureDir(filepath.Dir(socketPath)); err != nil {
		return nil, err
	}
	if err := removeStaleSocket(socketPath); err != nil {
		return nil, err
	}
	ul, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("ctlapi: listen: %w", err)
	}
	// 0600 after bind: the tiny pre-chmod window is covered by the 0700
	// directory (the first gate) and by the peer-cred check (the second).
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ul.Close()
		return nil, fmt.Errorf("ctlapi: chmod socket: %w", err)
	}
	return &credListener{Listener: ul, selfUID: os.Getuid(), check: checkPeer}, nil
}

// removeStaleSocket deletes a leftover socket file at path, but only after
// proving no one is serving it: a successful probe dial means a live owner
// (ErrAlreadyRunning); a failed dial means the socket is stale. A path that
// exists but is not a socket is never deleted (refuse to destroy unrelated
// files).
func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ctlapi: stat socket: %w", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("ctlapi: %s exists and is not a socket; refusing to remove", path)
	}
	conn, derr := net.DialTimeout("unix", path, probeTimeout)
	if derr == nil {
		_ = conn.Close()
		return fmt.Errorf("%w: %s", ErrAlreadyRunning, path)
	}
	// Dial failed => no live owner => the socket is stale debris.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("ctlapi: remove stale socket: %w", err)
	}
	return nil
}

// sameUser is the peer credential predicate: the control plane trusts
// exactly one identity — the uid this process runs as.
//
// Failure direction: any other uid (including root, uid 0) is rejected;
// there is no privileged bypass.
func sameUser(peer uint32, self int) bool {
	return self >= 0 && uint64(peer) == uint64(self)
}

// checkPeer verifies that conn's peer runs as selfUID. Non-Unix connections
// and any credential lookup failure are rejected (fail-closed: an
// unverifiable peer is a hostile peer).
func checkPeer(conn net.Conn, selfUID int) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("ctlapi: peer cred: not a unix socket connection (%T)", conn)
	}
	uid, err := peerUID(uc)
	if err != nil {
		return err
	}
	if !sameUser(uid, selfUID) {
		return fmt.Errorf("ctlapi: peer uid %d != server uid %d: connection rejected", uid, selfUID)
	}
	return nil
}

// credListener wraps a Unix listener and drops every accepted connection
// whose peer credentials do not match selfUID. check is injectable for
// tests; production uses checkPeer.
type credListener struct {
	net.Listener
	selfUID int
	check   func(conn net.Conn, selfUID int) error
}

// Accept returns the next connection that passes the peer credential check.
// Rejected connections are closed and accepting continues — one hostile
// dialer must not wedge the control plane.
func (l *credListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if cerr := l.check(conn, l.selfUID); cerr != nil {
			// Failure direction: verification failed => reject (close).
			_ = conn.Close()
			continue
		}
		return conn, nil
	}
}
