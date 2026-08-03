package cli

import (
	"strconv"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/daemon"
)

// Who is allowed to start a hub.
//
// A daemon belongs to the desktop application: the application starts it, and
// the daemon stops when the application does (internal/daemon/owner.go). That
// only holds if an ownerless start is refused, so this is where it is refused.
// The alternative — accept the start and let the daemon notice later that
// nobody owns it — cannot work: "nobody owns me" is indistinguishable from
// "my owner has not claimed me yet", and a daemon that guesses either way is
// either unstoppable or shuts down on its own during an ordinary launch.
//
// `--headless` is the escape, and it is deliberately explicit rather than
// implied by the absence of an owner. The two are the same command line seen
// from opposite sides: an operator running a hub on a server with no desktop
// means it, while a script that forgot the handshake does not, and only a flag
// distinguishes them. CI and the e2e suite go through it, which is the point —
// a rule the tests are exempted from is a rule nobody has verified.
//
// Failure direction: fail closed. No owner and no --headless refuses the
// start rather than producing a hub nothing is responsible for.

// ownerFlags are the ownership half of `daemon start` / `daemon restart`.
// The zero value is an ownerless start, which resolve refuses.
type ownerFlags struct {
	pid        int
	lifelineFD int
	headless   bool
}

// bind registers the flags on cmd. The two handshake flags are hidden: they
// are how the application hands its identity to the child it just spawned,
// not something a person types.
func (f *ownerFlags) bind(cmd *cobra.Command) {
	cmd.Flags().IntVar(&f.pid, "owner-pid", 0,
		"pid of the application this daemon belongs to and must not outlive")
	cmd.Flags().IntVar(&f.lifelineFD, "owner-lifeline-fd", 0,
		"inherited descriptor whose close means the owning application is gone")
	cmd.Flags().BoolVar(&f.headless, "headless", false,
		"run a hub that belongs to no application (a server, CI, or an operator who will stop it)")
	for _, hidden := range []string{"owner-pid", "owner-lifeline-fd"} {
		if err := cmd.Flags().MarkHidden(hidden); err != nil {
			// Only ever fails for a flag that was not registered, three lines
			// above. Panicking here would be the assembly reporting its own
			// typo at startup; ignoring it costs a hidden flag being visible.
			continue
		}
	}
}

// args renders the flags back into argv for the background fork. The
// lifeline is absent by construction — see resolve.
func (f ownerFlags) args() []string {
	var out []string
	if f.pid > 0 {
		out = append(out, "--owner-pid", strconv.Itoa(f.pid))
	}
	if f.headless {
		out = append(out, "--headless")
	}
	return out
}

// resolve turns the flags into a daemon.Owner, refusing an ownerless start.
//
// foreground selects whether a lifeline may be adopted at all. A backgrounded
// start execs a second time and the launcher then exits, so the descriptor
// would have to be threaded through both hops to mean anything; rather than
// carry machinery for a case nothing uses, the combination is refused. Silently
// dropping it would be the worse answer by far: the caller would believe it had
// armed the exact watch it did not get.
func (f ownerFlags) resolve(foreground bool) (daemon.Owner, error) {
	if f.headless {
		if f.pid > 0 || f.lifelineFD > 0 {
			return daemon.Owner{}, Usagef("--headless and the owner handshake flags are mutually exclusive: a hub either belongs to an application or to nobody")
		}
		return daemon.Owner{}, nil
	}
	if f.pid <= 0 && f.lifelineFD <= 0 {
		return daemon.Owner{}, &Error{
			Code: CodeDaemonUnowned, ExitCode: ExitUsage,
			Message: "a hub belongs to the AgentHub application, which starts and stops it",
			Hint:    "open AgentHub, or run an operator-owned hub with 'agenthub daemon start --headless'",
		}
	}
	owner := daemon.Owner{PID: f.pid}
	if f.lifelineFD > 0 {
		if !foreground {
			return daemon.Owner{}, Usagef("--owner-lifeline-fd needs --foreground: a backgrounded start execs again and the descriptor would not reach the daemon")
		}
		lifeline, err := daemon.LifelineFromFD(f.lifelineFD)
		if err != nil {
			return daemon.Owner{}, &Error{
				Code: CodeUsage, ExitCode: ExitUsage,
				Message: "adopting the owner lifeline", Err: err,
			}
		}
		owner.Lifeline = lifeline
	}
	return owner, nil
}
