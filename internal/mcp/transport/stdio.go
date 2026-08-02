package transport

import (
	"fmt"
	"os/exec"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// stderrTailSize is how much child stderr is retained for error reports
// (stdio stderr tail 4 KiB).
const stderrTailSize = 4 << 10

// stderrDrainGrace bounds how long Stderr waits for a dead child's stderr
// copier to finish. It is a diagnostic path: a tail that arrives late is
// worth a moment, a caller blocked forever is not.
const stderrDrainGrace = 2 * time.Second

// killGrace is how long Close waits after closing stdin before escalating
// to SIGKILL.
const killGrace = 3 * time.Second

// StdioConfig describes the child process for a stdio transport. This
// package spawns exactly what it is given: spawn-guard policy, AGENTHUB_*
// env stripping, and secret resolution are the caller's job, as is
// process-group management (the gateway setsids itself).
type StdioConfig struct {
	Command string
	Args    []string
	// Env is the complete child environment (exec.Cmd semantics: nil
	// inherits the parent environment, empty slice means empty env).
	//
	// Its PATH also decides where Command is looked up, which exec.Cmd does
	// NOT do on its own — see resolveCommand. A caller that hands the child
	// a PATH gets that PATH used for the lookup too, instead of silently
	// resolving against this process's.
	Env []string
	Cwd string

	// Docker selects the container runtime (docs/modules/foundation.md). nil — the
	// default, and what every registry entry without a `runtime` field
	// produces — spawns Command/Args directly on the host. Non-nil turns
	// Command/Args into the command executed INSIDE the container while
	// the process actually spawned on this host is `docker run`; Env then
	// belongs to the docker CLI itself (it needs PATH/HOME/DOCKER_HOST and
	// the credential helpers) and the container's own environment comes
	// from DockerConfig.Env.
	Docker *DockerConfig

	// Screen, when non-nil, vets the FINAL host command line — after any
	// docker rewrite — and refuses the spawn when it returns an error.
	// The signature is deliberately spawnguard.(*Guard).Check's: this
	// package owns no spawn policy (it is standard-library only and must
	// not import internal/guard), so the single guard implementation is
	// injected here instead of being re-implemented. nil means "the caller
	// screened already"; it is not a policy decision taken here.
	Screen func(command string, args, env []string) error
}

// SpawnStdio starts the child process and returns a Transport speaking
// newline-delimited JSON-RPC over its stdin/stdout. Spawn failures return
// *Error with ClassUnavailable (they count toward the circuit breaker).
//
// When cfg.Docker is set the child is `docker run -i --rm ...` instead;
// see SpawnDocker for the isolation defaults.
//
// Lifecycle: when the child exits or closes stdout, every pending call
// fails with ClassUnavailable. Close closes stdin, waits killGrace for a
// voluntary exit, then SIGKILLs; the process is always reaped.
func SpawnStdio(cfg StdioConfig) (Transport, error) {
	if cfg.Docker != nil {
		return SpawnDocker(cfg)
	}
	if cfg.Command == "" {
		return nil, &Error{Class: ClassFatal, Err: fmt.Errorf("stdio transport: empty command")}
	}
	if err := screen(cfg.Screen, cfg.Command, cfg.Args, cfg.Env); err != nil {
		return nil, err
	}
	// Resolved AFTER the screen, and it makes no difference to the screen:
	// spawnguard matches on the command's basename, so it reaches the same
	// verdict for "npx" and for /opt/homebrew/bin/npx. Screening the name the
	// configuration actually wrote is the more legible of two equal options.
	command, err := resolveCommand(cfg.Command, cfg.Env)
	if err != nil {
		return nil, &Error{Class: ClassUnavailable, Err: fmt.Errorf("spawn %q: %w", cfg.Command, err)}
	}
	cmd := exec.Command(command, cfg.Args...)
	cmd.Dir = cfg.Cwd
	cmd.Env = cfg.Env
	return launch(cmd, cfg.Command, nil, nil)
}

// screen runs the injected spawn-guard predicate. A rejection is ClassFatal:
// it is a verdict on the configuration, not a transient failure, so it must
// never feed the circuit breaker and must never be retried.
//
// The command is named in the wrapped error because of WHEN this fires. The
// screen sits on the spawn path, so its verdict arrives when a server is
// connected — possibly long after the configuration that caused it was
// written, and to someone reading a failing server rather than a rejected
// `server add`. The guard's own message already names itself and the
// offending token; this adds the one thing it cannot know, which is what was
// being started.
func screen(fn func(string, []string, []string) error, command string, args, env []string) error {
	if fn == nil {
		return nil
	}
	if err := fn(command, args, env); err != nil {
		return &Error{Class: ClassFatal, Err: fmt.Errorf("refusing to spawn %q: %w", command, err)}
	}
	return nil
}

// launch starts cmd with its pipes wired to a conn.
//
//   - diagnose, when non-nil, decorates the stderr tail with a
//     human-readable classification of a known failure mode (docker uses it
//     so that "image not found" never reaches the operator as a bare
//     deadline-exceeded),
//   - cleanup, when non-nil, runs once the process has been reaped — on the
//     voluntary-exit path, the SIGKILL path, and on a failed Start.
//
// Invariant (os/exec pipe contract): cmd.Wait must not run before every
// stdout read has finished, so reaping is keyed off the read loop ending.
// The read loop always ends once the process dies (stdout EOF).
func launch(cmd *exec.Cmd, what string, diagnose func(string) string, cleanup func()) (Transport, error) {
	ring := newTailBuffer(stderrTailSize)
	cmd.Stderr = ring

	stdin, err := cmd.StdinPipe()
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, &Error{Class: ClassUnavailable, Err: fmt.Errorf("stdin pipe: %w", err)}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, &Error{Class: ClassUnavailable, Err: fmt.Errorf("stdout pipe: %w", err)}
	}
	if err := cmd.Start(); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, &Error{Class: ClassUnavailable, Err: fmt.Errorf("spawn %q: %w", what, err)}
	}

	c := newConn(stdin, stdout, mcp.MaxFrameSize)
	procDone := make(chan struct{})

	// exec copies the child's stderr into the ring on its own goroutine and
	// only cmd.Wait guarantees that copy finished. Reading the ring before
	// then can yield an empty tail exactly when it matters most — the child
	// died and its last words are the diagnosis. So once the transport is
	// known dead, wait for the reaping before reporting. While the child is
	// alive there is nothing to wait for: the tail is a live snapshot by
	// definition.
	//
	// The trigger is failedErr, not readDone, and the difference is the whole
	// bug this replaced. A child that dies instantly — `docker run` on a
	// missing image, exit 125 — breaks the PIPE first, so the very next
	// WriteFrame fails and Call returns while the read loop has not yet
	// reached EOF. readDone was still open at that instant, the wait was
	// skipped, and the caller got an empty diagnosis precisely in the case
	// the diagnosis existed. failedErr is set by both paths that kill the
	// transport — the read loop ending AND a failed write — so it covers
	// that gap.
	//
	// A live child never pays this wait: a handshake that fails on a protocol
	// error returns ClassFatal without calling fail(), so failedErr stays nil
	// and the ring is read straight through.
	tail := func() string {
		if c.failedErr() != nil {
			deadline := time.NewTimer(stderrDrainGrace)
			defer deadline.Stop()
			// The read loop closing is what releases exec's stderr copier to
			// finish; the reap is what guarantees it did.
			select {
			case <-c.readDone:
				select {
				case <-procDone:
				case <-deadline.C:
				}
			case <-deadline.C:
			}
		}
		return ring.String()
	}
	c.stderrFn = tail
	if diagnose != nil {
		c.stderrFn = func() string { return diagnose(tail()) }
	}

	go func() {
		<-c.readDone
		_ = cmd.Wait()
		close(procDone)
	}()

	c.shutdown = func() {
		select {
		case <-procDone:
		case <-time.After(killGrace):
			_ = cmd.Process.Kill()
			<-procDone
		}
		if cleanup != nil {
			cleanup()
		}
	}

	c.start()
	return c, nil
}
