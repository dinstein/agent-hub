package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// StartOptions tunes DialOrStartWith. The zero value gives production
// behavior; fields exist mainly for tests and embedders.
type StartOptions struct {
	// SocketPath overrides the control socket ("" = DefaultSocketPath).
	SocketPath string
	// DaemonBinary is the executable to spawn ("" = a sibling of the running
	// executable named agenthub, else a bare "agenthub" resolved on $PATH;
	// see defaultDaemonBinary).
	DaemonBinary string
	// DaemonArgs are the arguments (nil = ["daemon", "start"]).
	DaemonArgs []string
	// RunDir is where daemon.json is expected ("" = dir of SocketPath).
	RunDir string
	// Deadline bounds the whole start-and-poll phase (0 = 10s).
	Deadline time.Duration
	// PollInterval is the readiness poll period (0 = 100ms).
	PollInterval time.Duration
}

// daemonInfo mirrors run/daemon.json, the daemon's readiness handshake
// file (written atomically after bind, docs/architecture.md §10): actual endpoint +
// pid + version. Reading it replaces "probe the port then spawn" TOCTOU
// schemes.
type daemonInfo struct {
	Endpoint string `json:"endpoint"`
	Pid      int    `json:"pid"`
	Version  string `json:"version"`
}

// DialOrStart returns a working Client, starting the daemon if needed:
// dial the control socket first; on failure exec "agenthub daemon start"
// and poll run/daemon.json + re-dial under a deadline. If the spawned
// process exits with an error before the daemon becomes ready, the real
// error (with its stderr tail) is reported instead of a timeout —
// inherited from the desktop.rs lesson (docs/modules/controlplane.md).
//
// The start it attempts names no owner, and `daemon start` refuses a hub that
// belongs to nobody. So this dials for everyone and starts only for a caller
// that says what it wants through DaemonArgs — `--headless` for an
// operator-owned hub, or the owner handshake for an application that intends
// to stop the daemon again. That is deliberate rather than a regression: the
// alternative is a library call that quietly leaves a hub running which
// nothing on the machine is responsible for. The desktop application uses
// StartSupervised (supervise.go), which assembles the handshake itself.
func DialOrStart(ctx context.Context) (*Client, error) {
	return DialOrStartWith(ctx, StartOptions{})
}

// defaultDaemonBinary picks the agenthub to launch: a sibling of the
// running executable if there is one, otherwise a bare name for $PATH.
//
// The sibling has to win. The GUI is what makes this concrete — it ships
// beside the CLI (bin/agenthub-gui next to bin/agenthub, and the same
// layout inside an app bundle) and, when launched from Finder or a dock
// icon, inherits none of a login shell's $PATH. Resolving only through
// $PATH meant the GUI could not start the daemon sitting in its own
// directory, and the user saw the socket-missing error rather than the
// launch that never happened.
//
// When agenthub itself is the caller the sibling resolves to its own path,
// which is the right answer for a different reason: a checkout's daemon
// should be the one that checkout starts, not whichever build happens to be
// installed on $PATH.
//
// Failure direction: every uncertain branch falls back to the bare name.
// Pointing at a sibling that turns out not to be runnable would replace a
// working $PATH lookup with a broken absolute path.
func defaultDaemonBinary() string {
	name := "agenthub"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	exe, err := os.Executable()
	if err != nil {
		return name
	}
	return daemonBinaryNear(exe, name)
}

// daemonBinaryNear is defaultDaemonBinary's testable core: the sibling of
// exe called name, or name itself when there is no usable one.
func daemonBinaryNear(exe, name string) string {
	// Resolve symlinks: a binary reached through one (a Homebrew shim, a
	// dev symlink into a checkout) has its real siblings at the target.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	cand := filepath.Join(filepath.Dir(exe), name)
	fi, err := os.Stat(cand)
	if err != nil || !fi.Mode().IsRegular() {
		return name
	}
	// The execute bit is only meaningful where it exists; Windows reports a
	// constant mode, so checking it there would reject every sibling.
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		return name
	}
	return cand
}

// `DialOrStartSpawned` used to sit here: DialOrStartWith plus "did THIS call
// start the daemon", for callers that stop it again when they exit. It is
// gone, and the answer it gave is why. The launcher exits 0 both when it
// detached a daemon of its own and when it found one already running, so a
// daemon started concurrently by a terminal or a login item was reported as
// this process's own — and the caller then took down a hub somebody else was
// using, to tidy up after itself.
//
// The question is now answered where it can be answered correctly, and by
// something better than the outcome of a dial: StartSupervised holds the
// child's process handle, and /v1/ping reports the owner the daemon was
// started with. A caller that has neither did not start that hub and must not
// stop it.

// DialOrStartWith is DialOrStart with explicit options.
func DialOrStartWith(ctx context.Context, opts StartOptions) (*Client, error) {
	c, _, err := dialOrStart(ctx, opts)
	return c, err
}

func dialOrStart(ctx context.Context, opts StartOptions) (*Client, bool, error) {
	socket := opts.SocketPath
	if socket == "" {
		var err error
		if socket, err = DefaultSocketPath(); err != nil {
			return nil, false, err
		}
	}
	if opts.DaemonBinary == "" {
		opts.DaemonBinary = defaultDaemonBinary()
	}
	if opts.DaemonArgs == nil {
		opts.DaemonArgs = []string{"daemon", "start"}
	}
	if opts.RunDir == "" {
		var err error
		if opts.RunDir, err = runDirFor(socket); err != nil {
			return nil, false, err
		}
	}
	if opts.Deadline <= 0 {
		opts.Deadline = 10 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 100 * time.Millisecond
	}
	// exitGrace bounds how long the readiness deadline waits for a pending
	// launcher exit status before giving up on naming the cause.
	const exitGrace = 500 * time.Millisecond

	// Fast path: daemon already up. spawned stays false — this call found
	// it, so this call has no claim to stop it.
	if c, err := tryDial(ctx, socket); err == nil {
		return c, false, nil
	}

	// The launcher's stderr goes to a temp *os.File rather than an in-memory
	// writer on purpose: with a plain io.Writer, exec spawns a copier
	// goroutine and cmd.Wait cannot report the exit status until that
	// goroutine is scheduled — under load that lags the child's exit by
	// hundreds of milliseconds and the readiness error would blame a
	// timeout instead of naming the real failure. A file needs no copier.
	stderr := newStderrTail()
	defer stderr.close()
	cmd := exec.Command(opts.DaemonBinary, opts.DaemonArgs...)
	if stderr.file != nil {
		// Only assign a live file: a typed-nil *os.File would still satisfy
		// exec's *os.File fast path and be used as a descriptor.
		cmd.Stderr = stderr.file
	}
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("api: starting %q: %w", opts.DaemonBinary, err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	deadline := time.NewTimer(opts.Deadline)
	defer deadline.Stop()
	tick := time.NewTicker(opts.PollInterval)
	defer tick.Stop()
	detached := false
	for {
		// Prefer the endpoint from daemon.json (the daemon may have been
		// started with a non-default socket); fall back to our socket.
		endpoint := socket
		if info, err := readDaemonInfo(filepath.Join(opts.RunDir, "daemon.json")); err == nil && info.Endpoint != "" {
			endpoint = info.Endpoint
		}
		if c, err := tryDial(ctx, endpoint); err == nil {
			return c, true, nil
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case err := <-waitCh:
			if err != nil {
				// The child died: surface its real failure, not a timeout.
				return nil, false, fmt.Errorf("api: %q exited before daemon became ready: %w (stderr: %s)",
					opts.DaemonBinary, err, stderr.String())
			}
			// Exit 0: the launcher detached (daemonized). Keep polling.
			detached = true
			waitCh = nil // nil channel: never selected again
		case <-deadline.C:
			// The exit status may still be in flight: cmd.Wait only returns
			// once the stderr copier drains, which can lag the child's exit.
			// Give it a bounded grace so the error names what actually
			// happened; a daemon that really is running just costs the
			// grace on this already-failing path (a detached launcher's
			// grandchild can hold the pipe open indefinitely, hence bounded).
			if waitCh != nil {
				grace := time.NewTimer(exitGrace)
				select {
				case err := <-waitCh:
					if err != nil {
						return nil, false, fmt.Errorf("api: %q exited before daemon became ready: %w (stderr: %s)",
							opts.DaemonBinary, err, stderr.String())
					}
					detached = true
				case <-grace.C:
				}
				grace.Stop()
			}
			state := "still running"
			if detached {
				state = "launcher exited 0"
			}
			return nil, false, fmt.Errorf("api: daemon did not become ready within %v (%s; stderr: %s)",
				opts.Deadline, state, stderr.String())
		case <-tick.C:
		}
	}
}

// runDirFor answers "where is daemon.json, given this endpoint".
//
// For a socket that is its directory, and the answer must keep coming from the
// socket rather than from a fresh resolution: a caller who overrode
// SocketPath — the e2e suite, anyone running two sandboxes — has the handshake
// file beside the socket they named, not beside the default one.
//
// A named pipe has no directory. filepath.Dir would return `\\.\pipe`, a
// location that does not exist, and every readDaemonInfo would fail silently:
// DialOrStart would still work (it falls back to the endpoint it was given) but
// would lose the ability to notice that the running daemon is serving a
// DIFFERENT endpoint, which is the whole reason it reads the file. So the run
// directory is resolved from the environment instead, exactly as the daemon
// that writes the file resolves it.
func runDirFor(socket string) (string, error) {
	if !isPipePath(socket) {
		return filepath.Dir(socket), nil
	}
	dir, err := runDir(runtime.GOOS, os.LookupEnv, os.UserHomeDir)
	if err != nil {
		return "", fmt.Errorf("api: resolve run directory for %s: %w", socket, err)
	}
	return dir, nil
}

// tryDial pings the daemon at socketPath with a short per-attempt
// deadline. On failure the probe client is closed and the error returned.
func tryDial(ctx context.Context, socketPath string) (*Client, error) {
	c := New(socketPath)
	pctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := c.Ping(pctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// readDaemonInfo loads run/daemon.json. The daemon writes it atomically,
// so a well-formed read means the endpoint was live at write time.
func readDaemonInfo(path string) (daemonInfo, error) {
	var info daemonInfo
	b, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(b, &info); err != nil {
		return info, fmt.Errorf("api: parsing %s: %w", path, err)
	}
	return info, nil
}

// stderrTail captures the launcher's stderr in a temp file and reports its
// last stderrTailLimit bytes (mirroring the 4KB stderr-tail discipline). A
// real file is used so exec needs no copier goroutine — see DialOrStartWith.
// A tail that cannot be captured degrades to a marker, never an error: the
// tail is diagnostic decoration on an error path, not a decision input.
type stderrTail struct {
	file *os.File
}

const stderrTailLimit = 4 << 10

func newStderrTail() *stderrTail {
	f, err := os.CreateTemp("", "agenthub-daemon-stderr-*")
	if err != nil {
		return &stderrTail{} // nil file: exec discards stderr
	}
	// Unlink now: the fd keeps it alive, and nothing is left behind on any
	// exit path (including a panic between here and close).
	_ = os.Remove(f.Name())
	return &stderrTail{file: f}
}

func (s *stderrTail) String() string {
	if s.file == nil {
		return "<unavailable>"
	}
	size, err := s.file.Seek(0, io.SeekEnd)
	if err != nil {
		return "<unavailable>"
	}
	if size == 0 {
		return "<empty>"
	}
	off := int64(0)
	if size > stderrTailLimit {
		off = size - stderrTailLimit
	}
	buf := make([]byte, size-off)
	if _, err := s.file.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return "<unavailable>"
	}
	return string(buf)
}

func (s *stderrTail) close() {
	if s.file != nil {
		_ = s.file.Close()
	}
}
