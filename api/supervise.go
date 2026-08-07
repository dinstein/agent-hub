package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// Starting a hub the caller stays responsible for.
//
// DialOrStart answers "make sure a daemon exists"; this answers "run one, and
// own it". The difference is the whole of the desktop application's
// relationship to the hub: it starts a daemon in the FOREGROUND as a direct
// child, so there is a process to watch rather than a pid to guess at, and it
// hands that child a lifeline so the hub stops even when the application never
// gets to say goodbye (internal/daemon/owner.go).
//
// Three things follow from being the parent rather than a bystander:
//
//   - Death is reported, not polled. exec.Cmd.Wait is the signal; Exited
//     closes and Err says what happened, so a supervisor can restart on the
//     spot instead of noticing at the next call that something is wrong.
//   - Stopping is exact. The pid comes from the process handle, not from a
//     file that outlives an abrupt death and names a number the OS is free to
//     reuse.
//   - The daemon is admitted. `daemon start` refuses a hub that belongs to
//     nobody, and the owner flags this assembles are how the application says
//     the hub is its own.

// daemonStderrName is where a supervised daemon's raw stderr goes: the same
// file the CLI's background start uses, so `agenthub doctor` and a person
// reading the log directory find one file rather than two.
const daemonStderrName = "daemon.stderr.log"

// stopGrace bounds Stop's wait for the child to finish draining.
//
// It is the daemon's own ShutdownGrace plus room to exit: a stop that gave up
// sooner would report a failure for a hub that was shutting down exactly as
// designed, and the caller's most likely reaction — kill it — would abandon
// the in-flight tool call the drain existed to finish.
const stopGrace = 8 * time.Second

// stopAskTimeout bounds the control-plane stop REQUEST. It is short because
// the daemon answers 202 before it drains: this waits for an
// acknowledgement, never for a shutdown, and a hub that cannot acknowledge
// within it is one the terminate fallback should reach sooner rather than
// later.
const stopAskTimeout = 2 * time.Second

// Supervised is a daemon this process started and is responsible for.
//
// It is safe for concurrent use. Stop is idempotent.
type Supervised struct {
	// Client talks to the daemon. It is live from the moment
	// StartSupervised returns and is closed by Stop.
	Client *Client

	cmd *exec.Cmd
	// stderrPath is where the child's raw stderr went, read back only to
	// explain a start that failed.
	stderrPath string
	// lifeline is the WRITE end of the owner pipe. Nothing is ever written to
	// it; the daemon watches for its close, which happens when this process
	// exits however it exits. Holding the reference keeps it open.
	lifeline *os.File

	exited   chan struct{}
	waitErr  error
	stopOnce sync.Once
	stopErr  error
}

// StartSupervised runs a daemon as a child of this process and waits until it
// is serving.
//
// opts.DaemonArgs replaces the base command (default `daemon start
// --foreground`); the owner flags are appended either way, because a
// supervised daemon that did not know its owner would be a daemon this
// process could not legitimately stop.
func StartSupervised(ctx context.Context, opts StartOptions) (*Supervised, error) {
	socket, err := opts.resolve("daemon", "start", "--foreground")
	if err != nil {
		return nil, err
	}

	// A daemon that is ALREADY serving is not ours to supervise, and starting
	// a second one would only fail on the bound socket. The caller is told so
	// by name: adopting somebody else's hub as a child we may stop is the one
	// mistake this whole path exists to prevent.
	if c, err := tryDial(ctx, socket); err == nil {
		c.Close()
		return nil, ErrDaemonNotOurs
	}

	args := append([]string{}, opts.DaemonArgs...)
	args = append(args, "--owner-pid", strconv.Itoa(os.Getpid()))

	cmd := exec.Command(opts.DaemonBinary, args...)

	// The lifeline: the child gets the read end, we keep the write end. Only
	// the read end goes into ExtraFiles, so the daemon never holds the end
	// whose close it is waiting for.
	lifelineR, lifelineW, err := newLifeline()
	if err != nil {
		return nil, fmt.Errorf("api: owner lifeline: %w", err)
	}
	if lifelineR != nil {
		if fd := attachLifeline(cmd, lifelineR); fd > 0 {
			cmd.Args = append(cmd.Args, "--owner-lifeline-fd", strconv.Itoa(fd))
		}
	}
	closeLifeline := func() {
		if lifelineR != nil {
			_ = lifelineR.Close()
		}
		if lifelineW != nil {
			_ = lifelineW.Close()
		}
	}

	// Raw stderr to a file rather than a pipe: nothing here reads it while the
	// daemon runs, and a pipe nobody drains eventually blocks the writer —
	// which would wedge the hub rather than lose a log line.
	stderr, err := openDaemonStderr()
	if err != nil {
		closeLifeline()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = stderr, stderr

	if err := cmd.Start(); err != nil {
		_ = stderr.Close()
		closeLifeline()
		return nil, fmt.Errorf("api: starting %q: %w", opts.DaemonBinary, err)
	}
	// Our copy of the child's descriptors is done: the child holds its own.
	// The read end especially — keeping it open here would mean the pipe never
	// reaches EOF, and the lifeline would report nothing at all.
	_ = stderr.Close()
	if lifelineR != nil {
		_ = lifelineR.Close()
	}

	s := &Supervised{
		cmd: cmd, lifeline: lifelineW, exited: make(chan struct{}),
		stderrPath: stderr.Name(),
	}
	go func() {
		s.waitErr = cmd.Wait()
		close(s.exited)
	}()

	client, err := s.awaitReady(ctx, socket, opts)
	if err != nil {
		// Nothing is left running on a failed start: a half-started hub that
		// nobody holds a handle to is the orphan this design exists to
		// prevent, and the caller has no way to reach it.
		_ = s.Stop(context.Background())
		return nil, err
	}
	s.Client = client
	return s, nil
}

// ErrDaemonNotOurs reports that a daemon was already serving, so this process
// did not start it and must not stop it. Test with errors.Is.
var ErrDaemonNotOurs = errors.New("api: a daemon is already running; this process does not own it")

// awaitReady polls the readiness handshake until the daemon answers, the
// child dies, or the deadline passes.
func (s *Supervised) awaitReady(ctx context.Context, socket string, opts StartOptions) (*Client, error) {
	deadline := time.NewTimer(opts.Deadline)
	defer deadline.Stop()
	tick := time.NewTicker(opts.PollInterval)
	defer tick.Stop()
	for {
		endpoint := socket
		if info, err := readDaemonInfo(filepath.Join(opts.RunDir, "daemon.json")); err == nil && info.Endpoint != "" {
			endpoint = info.Endpoint
		}
		if c, err := tryDial(ctx, endpoint); err == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.exited:
			// The child died before serving: report ITS failure. A timeout
			// here would name the deadline and hide the refusal — an
			// inadmissible start, a bound socket, a broken registry — that
			// the operator actually has to act on.
			return nil, fmt.Errorf("api: the hub exited before it was ready (%v; stderr: %s)",
				s.waitErr, fileTail(s.stderrPath, stderrTailLimit))
		case <-deadline.C:
			return nil, fmt.Errorf("api: the hub did not become ready within %v (stderr: %s)",
				opts.Deadline, fileTail(s.stderrPath, stderrTailLimit))
		case <-tick.C:
		}
	}
}

// Exited is closed once the daemon process has ended, for any reason. A
// supervisor selects on it instead of discovering the death at the next call.
func (s *Supervised) Exited() <-chan struct{} { return s.exited }

// Err reports why the daemon ended. It is only meaningful after Exited is
// closed; nil there means it exited successfully, which for a hub still means
// it is gone.
func (s *Supervised) Err() error { return s.waitErr }

// Pid is the daemon's process id, taken from the process handle rather than
// from any file.
func (s *Supervised) Pid() int {
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// Stop ends the daemon and waits for it to go, bounded by ctx or stopGrace,
// whichever comes first. It is idempotent and safe to call on a daemon that
// has already exited.
//
// It asks rather than kills (on the platforms that can tell the difference):
// the daemon's graceful path finishes an in-flight tool call and writes back
// what it has, and shaving a second off our own shutdown is not worth
// abandoning a call somebody's agent is waiting on.
func (s *Supervised) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() { s.stopErr = s.stop(ctx) })
	return s.stopErr
}

func (s *Supervised) stop(ctx context.Context) error {
	// Ask over the control plane FIRST, while the client is still open.
	// This is what makes the stop graceful where there is no signal to send:
	// terminate() on Windows is a kill, so without this every quit of the
	// application abandons whatever tool call was in flight. A daemon too old
	// to serve the route answers 404, asked stays false, and the fallback
	// below runs — which is what used to happen unconditionally.
	asked := false
	if s.Client != nil && s.cmd != nil && s.cmd.Process != nil {
		actx, cancel := context.WithTimeout(ctx, stopAskTimeout)
		_, err := s.Client.RequestStop(actx)
		cancel()
		asked = err == nil
	}
	if s.Client != nil {
		s.Client.Close()
	}
	// Closing the lifeline is a second, independent way to say the same
	// thing, and it is closed FIRST: if the signal below cannot be delivered
	// (Windows), this is what the daemon acts on.
	if s.lifeline != nil {
		_ = s.lifeline.Close()
	}
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	select {
	case <-s.exited:
		return nil // already gone; nothing to signal
	default:
	}
	// An accepted request already has the daemon draining; adding the
	// platform's terminate on top would be the kill this set out to avoid.
	// What confirms is unchanged either way: the wait below.
	if !asked {
		if err := terminate(s.cmd.Process); err != nil {
			return fmt.Errorf("api: stopping the hub (pid %d): %w", s.Pid(), err)
		}
	}

	grace := time.NewTimer(stopGrace)
	defer grace.Stop()
	select {
	case <-s.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-grace.C:
		return fmt.Errorf("api: the hub (pid %d) did not stop within %v", s.Pid(), stopGrace)
	}
}

// openDaemonStderr opens the raw stderr sink. It is truncated per start, like
// the CLI's.
func openDaemonStderr() (*os.File, error) {
	dir, err := daemonLogsDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("api: creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, daemonStderrName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("api: opening %s: %w", path, err)
	}
	return f, nil
}

// daemonLogsDir resolves <data>/logs, from the environment rather than from
// the run directory.
//
// That is deliberately NOT how the socket's sibling paths are resolved
// (runDirFor derives them from the endpoint it was handed). Logs follow the
// DATA directory, not the run directory, and on Linux the two are not even
// siblings — the run directory can sit under XDG_RUNTIME_DIR while the logs
// stay under <data>. Deriving one from the other would put a daemon's stderr
// in a tmpfs that is wiped at logout, in exactly the configuration CI uses.
// This is also the resolution the CLI's own background start performs, so a
// person reading the log directory finds one file however the hub was started.
func daemonLogsDir() (string, error) {
	data, err := dataDir(runtime.GOOS, os.LookupEnv, os.UserHomeDir)
	if err != nil {
		return "", fmt.Errorf("api: resolve logs directory: %w", err)
	}
	return filepath.Join(data, "logs"), nil
}

// fileTail returns the last n bytes of path, or a marker. Diagnostic
// decoration on an error path: it never fails the caller.
func fileTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return "<unavailable>"
	}
	defer func() { _ = f.Close() }()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return "<unavailable>"
	}
	if size == 0 {
		return "<empty>"
	}
	off := int64(0)
	if size > n {
		off = size - n
	}
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return "<unavailable>"
	}
	return string(buf)
}
