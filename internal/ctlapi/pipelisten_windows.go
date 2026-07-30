//go:build windows

package ctlapi

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/Microsoft/go-winio"

	"github.com/dinstein/agent-hub/internal/platform"
)

// NOT VERIFIED ON REAL HARDWARE. This file cross-compiles and has never run;
// no Windows machine exists in this project's loop. See docs/windows.md.
//
// The Windows control endpoint. Everything the Unix path spends four steps on
// is done differently here, and the differences are the reason this is a
// separate function rather than a branch inside Listen:
//
//   - AUTHORIZATION IS IN THE LISTEN CALL, not in Accept. The Unix side has two
//     gates (a 0700 directory, then SO_PEERCRED on every accepted connection)
//     because either can be defeated alone: a directory mode can be
//     misconfigured. The SDDL below is applied by the kernel when the pipe is
//     created, so a process outside it cannot open the pipe at all — the
//     connection this code would have rejected never reaches user space. That
//     is a stronger layer than Unix offers, not a missing one, which is why
//     Accept admits unconditionally here.
//
//   - THERE IS NO STALE ENDPOINT. A pipe exists only while its owner holds the
//     handle, so the whole removeStaleSocket dance — probe with a dial, delete
//     only what proves dead, never delete a live one — has nothing to operate
//     on. A crashed daemon leaves nothing behind.
//
//   - ALREADY-RUNNING IS DETECTED BY THE CREATE, not by a probe dial. go-winio
//     creates the first instance with FILE_CREATE, so a second listener on the
//     same name fails in the kernel. That is atomic, and therefore better than
//     the Unix probe: two daemons starting at the same instant cannot both
//     conclude the endpoint is free.
//
// go-winio is the dependency this needs (canonical.md §7): the standard library
// has no named-pipe server, and writing one means overlapped I/O, an accept
// loop over pre-created instances, and SDDL parsing — roughly a thousand lines
// whose bugs would be unobservable from this project's platforms.

// errorPipeBusy is ERROR_PIPE_BUSY. syscall has no constant for it, and it is
// the other way "the name is taken" can surface.
const errorPipeBusy = syscall.Errno(231)

// listenPipe serves the control plane on a Windows named pipe.
//
// The SDDL comes from the current process's real identity, deliberately not
// from anything in path: the question it answers is "who is allowed to talk to
// me", and the answer is "the user I am running as" whatever pipe name a caller
// passed in.
func listenPipe(path string) (net.Listener, error) {
	sddl, err := platform.Default().CtlPipeSDDL()
	if err != nil {
		// Fail-closed: no SDDL, no pipe. Serving with go-winio's default
		// descriptor would admit Administrators and SYSTEM, and this endpoint
		// hands out every downstream credential and approves tool calls.
		return nil, fmt.Errorf("ctlapi: resolve control pipe security descriptor: %w", err)
	}
	l, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: sddl})
	if err != nil {
		if isNameTaken(err) {
			return nil, fmt.Errorf("%w: %s", ErrAlreadyRunning, path)
		}
		return nil, fmt.Errorf("ctlapi: listen on %s: %w", path, err)
	}
	return l, nil
}

// isNameTaken reports whether err means the pipe name already has an owner.
//
// Failure direction: anything unrecognised is NOT reported as
// ErrAlreadyRunning. That error tells an operator to go find the process
// holding the endpoint, and sending them after a process that does not exist
// costs more than showing them the raw error — ERROR_ACCESS_DENIED is the case
// that matters, since the pipe name is per-SID and a denial there means
// something about the security descriptor, not a second daemon.
func isNameTaken(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.ERROR_ALREADY_EXISTS || errno == errorPipeBusy
}
