package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/dinstein/agent-hub/internal/platform"
)

// The owner watch: what makes the hub's lifetime the application's lifetime.
//
// A daemon started by the desktop application exists because that application
// wanted one, and must not outlive it. The application's own exit path stops
// it (a SIGTERM, the graceful path), but that path only runs when the exit is
// orderly. A crash, a `kill -9`, a force-quit from the Dock — none of them get
// to send anything, and every one of them used to leave a hub running that
// nothing on the machine would ever stop again. Under the previous "optional
// value-add" model that was untidy; now that the application is the only thing
// that starts a hub, an orphan is worse than untidy — the next launch finds a
// daemon it did not start, cannot claim, and must not kill.
//
// So the daemon watches its owner instead of trusting it to say goodbye. Two
// mechanisms, deliberately unequal:
//
//   - THE LIFELINE is the read end of a pipe whose write end the owner holds
//     and never writes to. The kernel closes it when the owner dies, however
//     it dies, and the read here returns EOF within microseconds. It is exact,
//     it needs no timer, and it cannot be fooled by a recycled pid. It is not
//     available everywhere: os/exec cannot pass an extra descriptor to a child
//     on Windows, so there the daemon has the poll alone (docs/windows.md).
//   - THE POLL asks the OS whether the owner's pid is still a live process,
//     every OwnerPollInterval. It is the backstop for a lifeline that was
//     never established, and its failure direction is the important part:
//     "cannot tell" reads as ALIVE. A hub that keeps running past its owner is
//     recovered by the next launch, which finds it and adopts it; a hub that
//     shuts down under a live owner cuts off every connected client to fix
//     nothing.
//
// Both are absent for an ownerless (headless) daemon, which is the entire
// meaning of headless: it belongs to no application and stops when an operator
// stops it.

// DefaultOwnerPollInterval is how often an owned daemon re-asks whether its
// owner is still alive when it has no lifeline to watch.
//
// Two seconds is chosen against the cost of being wrong in each direction,
// which is not symmetric: the poll only ever runs alongside the lifeline
// (which answers instantly) or on Windows (where nothing else answers at
// all), so tightening it buys a faster reaction on the platform that cannot
// verify it, while every tick is a syscall for the life of the process.
const DefaultOwnerPollInterval = 2 * time.Second

// Owner names the application process a daemon belongs to. The zero value is
// an ownerless — headless — daemon, which nothing here watches.
type Owner struct {
	// PID is the owning process. 0 means ownerless.
	PID int
	// Lifeline is the read end of a pipe the owner holds open for its
	// lifetime. nil means the poll is the only mechanism. The daemon closes
	// it on shutdown; nothing ever reads a byte from it, because a byte is
	// not the signal — the close is.
	Lifeline *os.File
}

// Owned reports whether this daemon has an owner to watch at all.
func (o Owner) Owned() bool { return o.PID > 0 || o.Lifeline != nil }

// errOwnerGone is the cancellation cause recorded when the owner goes away,
// so the shutdown log says why this daemon stopped rather than reporting the
// bare "context canceled" a signal produces.
var errOwnerGone = errors.New("the owning application exited")

// ownerWatch is a running owner watch. Close ends it; it is idempotent.
type ownerWatch struct {
	stop     chan struct{}
	lifeline *os.File
	// closing distinguishes OUR close of the lifeline from the owner's. Both
	// end the read, and only one of them means the owner is gone.
	closing atomic.Bool
}

// watchOwner starts the mechanisms described above and calls lost exactly
// once when the owner disappears. It returns nil for an ownerless daemon,
// and a nil *ownerWatch's Close is a no-op, so callers need no branch.
func watchOwner(o Owner, poll time.Duration, log *slog.Logger, lost context.CancelCauseFunc) *ownerWatch {
	if !o.Owned() {
		return nil
	}
	if poll <= 0 {
		poll = DefaultOwnerPollInterval
	}
	w := &ownerWatch{stop: make(chan struct{}), lifeline: o.Lifeline}

	if o.Lifeline != nil {
		go func() {
			// One read that is expected never to succeed: the owner writes
			// nothing, so this blocks until the descriptor is closed at one
			// end or the other.
			var b [1]byte
			_, err := o.Lifeline.Read(b[:])
			if w.closing.Load() {
				return // our own Close, during an unrelated shutdown
			}
			if err != nil && !errors.Is(err, io.EOF) {
				// A read that failed for some other reason has not shown that
				// the owner is gone. Say so and leave the poll to decide.
				log.Warn("owner lifeline failed; falling back to the pid poll alone", "error", err)
				return
			}
			log.Info("owner lifeline closed; stopping", "owner", o.PID)
			lost(errOwnerGone)
		}()
	}

	if o.PID > 0 {
		go func() {
			t := time.NewTicker(poll)
			defer t.Stop()
			for {
				select {
				case <-w.stop:
					return
				case <-t.C:
					alive, known := platform.ProcessAlive(o.PID)
					if !known || alive {
						continue
					}
					log.Info("owning application exited; stopping", "owner", o.PID)
					lost(errOwnerGone)
					return
				}
			}
		}()
	}
	return w
}

// Close ends the watch. The lifeline is closed here rather than left to
// process exit so that a daemon run in-process (tests, an embedder) does not
// leak a goroutine parked on a read for the life of the program.
func (w *ownerWatch) Close() {
	if w == nil {
		return
	}
	w.closing.Store(true)
	close(w.stop)
	if w.lifeline != nil {
		_ = w.lifeline.Close()
	}
}

// LifelineFromFD adopts an inherited descriptor as the owner lifeline.
//
// It exists because the descriptor arrives as a NUMBER on the command line:
// the application passes the read end of its pipe to `daemon start` through
// exec's ExtraFiles, and the child learns which fd it landed on from a flag.
// Wrapping it in an *os.File here (rather than in the CLI) keeps the one
// place that knows what the lifeline is for also owning how it is adopted.
func LifelineFromFD(fd int) (*os.File, error) {
	if fd < 3 {
		// 0, 1 and 2 are the standard streams. Adopting one of those as a
		// lifeline would make the daemon shut down the moment its stderr was
		// closed, which is a thing that happens for entirely ordinary reasons.
		return nil, fmt.Errorf("daemon: lifeline fd %d is a standard stream", fd)
	}
	f := os.NewFile(uintptr(fd), "owner-lifeline")
	if f == nil {
		return nil, fmt.Errorf("daemon: lifeline fd %d is not open", fd)
	}
	// A descriptor that arrived through ExtraFiles arrives WITHOUT
	// close-on-exec: exec dup2s it into place, and dup2 clears the flag. The
	// daemon spawns a child process per stdio downstream, so without this
	// every one of them would inherit the read end of the lifeline — and a
	// downstream that outlived the application would hold the pipe open, so
	// the close the daemon is waiting for would never arrive. The one
	// mechanism that cannot be fooled by a recycled pid would then be the one
	// that silently never fires.
	markCloseOnExec(f)
	return f, nil
}
