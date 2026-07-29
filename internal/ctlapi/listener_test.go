package ctlapi

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// shortSocketPath returns a socket path short enough for the sun_path
// limit (t.TempDir can exceed it on macOS because it embeds test names).
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ahctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "ctl.sock")
}

func requireUnixy(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("control socket requires linux or darwin")
	}
}

func TestListenPermissions(t *testing.T) {
	requireUnixy(t)
	sock := shortSocketPath(t)
	l, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	fi, err := os.Lstat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("not a socket: %v", fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket perm = %o, want 600", perm)
	}
	di, err := os.Stat(filepath.Dir(sock))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}
}

func TestListenAlreadyRunning(t *testing.T) {
	requireUnixy(t)
	sock := shortSocketPath(t)
	l, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	// The live socket must never be deleted: the probe dial succeeds, so
	// the second Listen fails with ErrAlreadyRunning.
	go acceptAll(l) // answer the probe dial
	if _, err := Listen(sock); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Listen err = %v, want ErrAlreadyRunning", err)
	}
	if _, err := os.Lstat(sock); err != nil {
		t.Fatalf("live socket was removed: %v", err)
	}
}

// acceptAll drains l until it closes (the probe dial in removeStaleSocket
// only needs the connect to succeed).
func acceptAll(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}
}

func TestListenRemovesStaleSocket(t *testing.T) {
	requireUnixy(t)
	sock := shortSocketPath(t)
	// Simulate a crashed daemon: bind, keep the file, stop listening.
	ul, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ul.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ul.Close()
	if _, err := os.Lstat(sock); err != nil {
		t.Fatalf("stale socket missing before test: %v", err)
	}

	l, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	_ = l.Close()
}

func TestListenRefusesNonSocketFile(t *testing.T) {
	requireUnixy(t)
	sock := shortSocketPath(t)
	if err := os.WriteFile(sock, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(sock); err == nil {
		t.Fatal("Listen succeeded over a regular file")
	}
	// The unrelated file must survive.
	b, err := os.ReadFile(sock)
	if err != nil || string(b) != "precious" {
		t.Fatalf("regular file was destroyed: %v %q", err, b)
	}
}

// TestSameUser pins the peer credential predicate. Failure direction: only
// an exact uid match passes; root (uid 0) has no bypass.
func TestSameUser(t *testing.T) {
	cases := []struct {
		peer uint32
		self int
		want bool
	}{
		{1000, 1000, true},
		{0, 0, true},
		{1000, 1001, false},
		{0, 1000, false},  // root peer is NOT trusted
		{1000, 0, false},  // non-root peer of a root server rejected
		{1000, -1, false}, // invalid self uid rejects everything
		{4294967295, -1, false},
	}
	for _, tc := range cases {
		if got := sameUser(tc.peer, tc.self); got != tc.want {
			t.Errorf("sameUser(%d, %d) = %v, want %v", tc.peer, tc.self, got, tc.want)
		}
	}
}

// fakeListener hands out pre-made connections.
type fakeListener struct {
	conns chan net.Conn
}

func (f *fakeListener) Accept() (net.Conn, error) {
	c, ok := <-f.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (f *fakeListener) Close() error   { return nil }
func (f *fakeListener) Addr() net.Addr { return &net.UnixAddr{Name: "fake", Net: "unix"} }

// TestCredListenerRejectsFailedCheck proves the failure direction of the
// accept loop: a connection whose credential check fails is closed and
// accepting continues with the next connection.
func TestCredListenerRejectsFailedCheck(t *testing.T) {
	bad, badPeer := net.Pipe()
	good, goodPeer := net.Pipe()
	defer func() { _ = badPeer.Close(); _ = goodPeer.Close(); _ = good.Close() }()

	inner := &fakeListener{conns: make(chan net.Conn, 2)}
	inner.conns <- bad
	inner.conns <- good

	calls := 0
	l := &credListener{Listener: inner, selfUID: 1000, check: func(c net.Conn, self int) error {
		calls++
		if calls == 1 {
			return errors.New("peer uid mismatch")
		}
		return nil
	}}

	got, err := l.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if got != good {
		t.Fatal("Accept returned the rejected connection")
	}
	// The rejected connection must be closed: a write on its peer fails.
	_ = badPeer.SetDeadline(time.Now().Add(time.Second))
	if _, err := badPeer.Write([]byte("x")); err == nil {
		t.Error("rejected connection was not closed")
	}
}

// TestCheckPeerRejectsNonUnixConn: the production check refuses anything
// that is not a *net.UnixConn (fail-closed on unverifiable transports).
func TestCheckPeerRejectsNonUnixConn(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	if err := checkPeer(a, os.Getuid()); err == nil {
		t.Fatal("checkPeer accepted a non-unix connection")
	}
}

// TestCheckPeerSameUIDPasses covers the accept branch end-to-end on a real
// unix socketpair: the dialer is this test process, so uids match.
func TestCheckPeerSameUIDPasses(t *testing.T) {
	requireUnixy(t)
	sock := shortSocketPath(t)
	ul, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ul.Close() }()

	done := make(chan error, 1)
	go func() {
		c, err := net.Dial("unix", sock)
		if err == nil {
			defer func() { _ = c.Close() }()
		}
		done <- err
	}()
	conn, err := ul.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := checkPeer(conn, os.Getuid()); err != nil {
		t.Fatalf("same-uid peer rejected: %v", err)
	}
	// And the mismatch branch of the same code path:
	if err := checkPeer(conn, os.Getuid()+1); err == nil {
		t.Fatal("different-uid peer accepted")
	}
}
