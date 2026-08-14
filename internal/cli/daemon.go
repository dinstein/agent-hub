package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/daemon"
	"github.com/dinstein/agent-hub/internal/jsonl"
	"github.com/dinstein/agent-hub/internal/platform"
)

// Timing constants for daemon lifecycle commands.
const (
	// daemonStartDeadline bounds background start-and-poll.
	daemonStartDeadline = 10 * time.Second
	// daemonStopDeadline bounds the graceful-stop wait.
	daemonStopDeadline = 10 * time.Second
	// daemonPollInterval is the readiness/liveness poll period.
	daemonPollInterval = 100 * time.Millisecond
	// daemonPingTimeout bounds one liveness ping.
	daemonPingTimeout = 2 * time.Second
	// logsFollowInterval is the -f re-read period for BOTH process-log
	// readers — `daemon logs`, declared here, and the merged `logs`. They
	// read the same directory, so one period keeps a record from surfacing
	// in one view before the other. It is deliberately shorter than the
	// 500ms the record readers use: these two are what someone watches
	// while restarting something.
	logsFollowInterval = 300 * time.Millisecond
)

// stderrFileName captures a background daemon's raw stderr (pre-logging
// failures); truncated on every start. The structured log is daemon.log.
const stderrFileName = "daemon.stderr.log"

// DaemonStatus is the `daemon status` / `daemon start` result.
type DaemonStatus struct {
	Running bool `json:"running"`
	// AlreadyRunning marks an idempotent `daemon start` against a live
	// daemon (omitted by status).
	AlreadyRunning bool   `json:"already_running,omitempty"`
	Pid            int    `json:"pid,omitempty"`
	Version        string `json:"version,omitempty"`
	Generation     uint64 `json:"generation,omitempty"`
	Sessions       int    `json:"sessions"`
	Socket         string `json:"socket"`
	// Owner is the pid of the application this hub belongs to, or 0 for a
	// headless one. It answers the question an operator otherwise cannot ask
	// from here — whether this hub disappears when somebody quits AgentHub —
	// and it comes from the daemon over the control socket, so it describes
	// the hub that is answering rather than whatever run/daemon.json was left
	// saying by a process that died.
	Owner int `json:"owner,omitempty"`
}

// Human renders the status.
func (s DaemonStatus) Human(w io.Writer) error {
	if !s.Running {
		_, err := fmt.Fprintf(w, "daemon is not running (socket: %s)\n", s.Socket)
		return err
	}
	state := "running"
	if s.AlreadyRunning {
		state = "already running"
	}
	_, err := fmt.Fprintf(w, "daemon %s: pid %d, version %s, %s, %d session(s), socket %s\n",
		state, s.Pid, s.Version, s.ownerText(), s.Sessions, s.Socket)
	return err
}

// ownerText says who the hub belongs to, in the terms that matter to whoever
// is reading: whether it will still be here after somebody quits an
// application.
func (s DaemonStatus) ownerText() string {
	if s.Owner == 0 {
		return "headless"
	}
	return fmt.Sprintf("owned by pid %d", s.Owner)
}

// DaemonStopResult is the `daemon stop` result.
type DaemonStopResult struct {
	Stopped bool   `json:"stopped"`
	Pid     int    `json:"pid,omitempty"`
	Forced  bool   `json:"forced,omitempty"`
	Message string `json:"message"`
}

// Human renders the stop outcome.
func (r DaemonStopResult) Human(w io.Writer) error {
	_, err := fmt.Fprintln(w, r.Message)
	return err
}

// newDaemonCmd builds the daemon lifecycle command group (docs/modules/controlplane.md).
func (a *App) newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the agenthub coordination daemon (stdio sessions never depend on it)",
		Args:  cobra.ArbitraryArgs,
		RunE:  groupRunE,
	}
	cmd.AddCommand(
		a.newDaemonStatusCmd(),
		a.newDaemonStartCmd(),
		a.newDaemonStopCmd(),
		a.newDaemonRestartCmd(),
		a.newDaemonLogsCmd(),
	)
	return cmd
}

// httpFlags are the MCP data plane's opt-in switches (see daemon.Config).
//
// The zero value no longer means "nothing listens" — it means "the stored
// configuration decides" (`agenthub config set http.addr …`), which for an
// installation that has never set one is still nothing. The flags remain the
// more specific statement and REPLACE the stored set whenever any of them is
// given, so an operator who names an address on the command line cannot be
// let through by a confirmation somebody stored for a different one
// (daemon.resolveHTTPFace).
type httpFlags struct {
	addr             string
	allowRemote      bool
	insecureLoopback bool
}

// args renders the flags back into argv for the background fork. Only
// non-default values are emitted, so a plain `daemon start` forks a plain
// `daemon start --foreground` and the child reads the stored configuration
// exactly as this process would have.
func (f httpFlags) args() []string {
	var out []string
	if f.addr != "" {
		out = append(out, "--http-addr", f.addr)
	}
	if f.allowRemote {
		out = append(out, "--http-allow-remote")
	}
	if f.insecureLoopback {
		out = append(out, "--insecure-loopback")
	}
	return out
}

// bind registers the flags on cmd.
func (f *httpFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.addr, "http-addr", "",
		"serve MCP over Streamable HTTP on this address, replacing the stored http.* configuration for this run")
	cmd.Flags().BoolVar(&f.allowRemote, "http-allow-remote", false,
		"confirm a non-loopback --http-addr; without it a non-loopback address is refused")
	cmd.Flags().BoolVar(&f.insecureLoopback, "insecure-loopback", false,
		"accept unauthenticated loopback callers on --http-addr (never covers a non-loopback bind)")
}

func (a *App) newDaemonStartCmd() *cobra.Command {
	var foreground bool
	var http httpFlags
	var owner ownerFlags
	cmd := &cobra.Command{
		Use:   "start [--headless] [--foreground] [--http-addr host:port]",
		Short: "Start the hub (the AgentHub application does this; --headless is the operator's own)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if foreground {
				return a.runDaemonForeground(cmd.Context(), http, owner)
			}
			res, err := a.startDaemonBackground(cmd.Context(), http, owner)
			if err != nil {
				return err
			}
			return a.printer().Emit(res)
		},
	}
	cmd.Flags().BoolVar(&foreground, "foreground", false,
		"run in this process until interrupted (what the background fork executes)")
	http.bind(cmd)
	owner.bind(cmd)
	return cmd
}

// runDaemonForeground runs daemon.Run in-process until SIGINT/SIGTERM.
// SIGTERM triggers the graceful path: stop accepting, drain, clean up
// socket + daemon.json (docs/modules/controlplane.md).
func (a *App) runDaemonForeground(ctx context.Context, http httpFlags, of ownerFlags) error {
	owner, err := of.resolve(true)
	if err != nil {
		return err
	}
	sctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = daemon.Run(sctx, daemon.Config{
		Version:   a.version,
		Resolver:  a.resolver,
		LogWriter: a.stderr,
		// Who this hub belongs to, and how it learns that they are gone
		// (daemonowner.go). The zero value is a headless hub.
		Owner: owner,
		// The data plane is opt-in through the flag alone. The admin bearer
		// comes from the environment because a credential passed on argv is
		// readable by every process on the machine.
		HTTPAddr:             http.addr,
		HTTPAllowRemote:      http.allowRemote,
		HTTPInsecureLoopback: http.insecureLoopback,
		HTTPAdminToken:       a.env(platform.EnvHTTPToken),
	})
	if errors.Is(err, ctlapi.ErrAlreadyRunning) {
		return &Error{
			Code: CodeDaemonRunning, ExitCode: ExitGeneral,
			Message: "another daemon is already serving the control socket",
			Hint:    "see 'agenthub daemon status'",
			Err:     err,
		}
	}
	if err != nil {
		return &Error{Code: CodeGeneral, ExitCode: ExitGeneral, Message: "daemon terminated", Err: err}
	}
	return nil
}

// startDaemonBackground forks `<self> daemon start --foreground` into its
// own session and polls run/daemon.json + ping until ready (the readiness
// handshake of docs/architecture.md#on-disk: no port-probe TOCTOU). Idempotent: a live
// daemon yields AlreadyRunning instead of an error.
func (a *App) startDaemonBackground(ctx context.Context, http httpFlags, of ownerFlags) (DaemonStatus, error) {
	// Refused here as well as in the child: a start that is going to be
	// rejected must say so on the terminal that ran it, not in a log file
	// belonging to a process that already exited.
	if _, err := of.resolve(false); err != nil {
		return DaemonStatus{}, err
	}
	socket, runDir, logsDir, err := a.daemonPaths()
	if err != nil {
		return DaemonStatus{}, err
	}
	if hello, perr := pingDaemon(ctx, socket); perr == nil {
		return DaemonStatus{
			Running: true, AlreadyRunning: true,
			Pid: hello.Pid, Version: hello.Version, Generation: hello.Generation, Owner: hello.Owner,
			Socket: socket,
		}, nil
	}

	if err := platform.EnsureDir(logsDir); err != nil {
		return DaemonStatus{}, err
	}
	// Raw stderr goes to a file, NOT a pipe: the parent exits after
	// readiness, and writes into a widowed pipe would SIGPIPE the daemon.
	stderrPath := filepath.Join(logsDir, stderrFileName)
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return DaemonStatus{}, fmt.Errorf("open daemon stderr file: %w", err)
	}

	childArgs := append([]string{"daemon", "start", "--foreground"}, http.args()...)
	spawn := exec.Command(a.executable(), append(childArgs, of.args()...)...)
	spawn.Stdout = stderrFile
	spawn.Stderr = stderrFile
	spawn.SysProcAttr = daemonSysProcAttr()
	if err := spawn.Start(); err != nil {
		_ = stderrFile.Close()
		return DaemonStatus{}, &Error{
			Code: CodeGeneral, ExitCode: ExitGeneral,
			Message: "starting daemon process", Err: err,
		}
	}
	_ = stderrFile.Close() // the child holds its own descriptor

	waitCh := make(chan error, 1)
	go func() { waitCh <- spawn.Wait() }()

	deadline := time.NewTimer(daemonStartDeadline)
	defer deadline.Stop()
	tick := time.NewTicker(daemonPollInterval)
	defer tick.Stop()
	for {
		// Prefer the endpoint from daemon.json (the daemon knows its actual
		// bind); fall back to the resolved socket while it is not yet ready.
		endpoint := socket
		if info, ierr := daemon.ReadInfo(runDir); ierr == nil && info.Endpoint != "" {
			endpoint = info.Endpoint
		}
		if hello, perr := pingDaemon(ctx, endpoint); perr == nil {
			return DaemonStatus{
				Running: true,
				Pid:     hello.Pid, Version: hello.Version, Generation: hello.Generation,
				Owner:  hello.Owner,
				Socket: endpoint,
			}, nil
		}
		select {
		case <-ctx.Done():
			return DaemonStatus{}, ctx.Err()
		case werr := <-waitCh:
			// The child died before becoming ready: surface its real
			// failure (stderr tail), never a bare timeout.
			return DaemonStatus{}, &Error{
				Code: CodeGeneral, ExitCode: ExitGeneral,
				Message: fmt.Sprintf("daemon exited before becoming ready (%v; stderr: %s)",
					werr, fileTail(stderrPath, 4<<10)),
			}
		case <-deadline.C:
			return DaemonStatus{}, &Error{
				Code: CodeGeneral, ExitCode: ExitGeneral,
				Message: fmt.Sprintf("daemon did not become ready within %v (stderr: %s)",
					daemonStartDeadline, fileTail(stderrPath, 4<<10)),
			}
		case <-tick.C:
		}
	}
}

func (a *App) newDaemonStopCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop [--force]",
		Short: "Stop the daemon gracefully (drain in-flight work; --force kills the process group)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := a.stopDaemon(cmd.Context(), force)
			if err != nil {
				return err
			}
			return a.printer().Emit(res)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "SIGKILL the daemon process group instead of draining")
	return cmd
}

// stopDaemon implements stop/restart. Stopping a stopped daemon is a
// success (idempotent), not an error.
func (a *App) stopDaemon(ctx context.Context, force bool) (DaemonStopResult, error) {
	socket, runDir, _, err := a.daemonPaths()
	if err != nil {
		return DaemonStopResult{}, err
	}
	// The pid this signals comes from the daemon that ANSWERED, never from
	// run/daemon.json. daemonproc_unix.go states the direction — stop and
	// status "never signal a pid they cannot verify" — and that file cannot
	// carry the proof: an abrupt death leaves it behind (a state daemon.go
	// explicitly designs for), after which the OS is free to hand the
	// number to something else. Signalling it then hits a stranger, and
	// with --force the signal goes to that stranger's whole process group.
	//
	// A successful ping settles both halves at once: it proves a daemon is
	// there, and Hello.Pid is that daemon naming itself over a socket that
	// is 0600 with a peer-credential check. Reading the answer and then
	// discarding it in favour of the file also lost the narrower race where
	// a replacement has bound the socket but not yet rewritten daemon.json.
	info, ierr := daemon.ReadInfo(runDir)
	hello, perr := pingDaemon(ctx, socket)

	if perr != nil {
		// Nothing answered, so nothing can be verified. When the run file
		// still names a live pid, say so rather than signal it: refusing
		// is the whole point of this branch. The cost is that a daemon
		// wedged badly enough to stop serving its socket can no longer be
		// stopped through the CLI, so the hint names the pid and the file.
		if ierr == nil && info.Pid > 0 && daemonAlive(info.Pid) {
			return DaemonStopResult{}, &Error{
				Code: CodeGeneral, ExitCode: ExitGeneral,
				Message: fmt.Sprintf(
					"run/daemon.json names pid %d, but nothing answers on the control socket: that pid cannot be confirmed to be the daemon",
					info.Pid),
				Hint: fmt.Sprintf(
					"the file may be stale and the pid reused by an unrelated process. If you are certain pid %d is a wedged agenthub daemon, stop it yourself; otherwise delete %s",
					info.Pid, filepath.Join(runDir, daemon.InfoFileName)),
				Err: perr,
			}
		}
		return DaemonStopResult{Stopped: false, Message: "daemon is not running"}, nil
	}

	pid := hello.Pid
	if pid <= 0 {
		return DaemonStopResult{}, &Error{
			Code: CodeGeneral, ExitCode: ExitGeneral,
			Message: "daemon is reachable but did not report its pid; nothing here can be signalled safely",
			Hint:    "remove the stale socket only if you are sure no daemon is running",
			Err:     ierr,
		}
	}

	if force {
		if err := daemonKillGroup(pid); err != nil {
			return DaemonStopResult{}, &Error{
				Code: CodeGeneral, ExitCode: ExitGeneral,
				Message: fmt.Sprintf("force-killing daemon pid %d", pid), Err: err,
			}
		}
		return DaemonStopResult{
			Stopped: true, Pid: pid, Forced: true,
			Message: fmt.Sprintf("daemon (pid %d) killed", pid),
		}, nil
	}

	// Two ways to ask, one per platform, and the difference is not taste:
	// Windows has no signal to deliver, so the stop is REQUESTED over the
	// same socket this function read the pid from a few lines above. What
	// follows is identical either way, because neither a delivered SIGTERM
	// nor an accepted request means the daemon has finished — the poll below
	// is what decides that.
	if daemonStopBySignal {
		if err := daemonSignalStop(pid); err != nil {
			return DaemonStopResult{}, &Error{
				Code: CodeGeneral, ExitCode: ExitGeneral,
				Message: fmt.Sprintf("signaling daemon pid %d", pid), Err: err,
			}
		}
	} else if err := requestDaemonStop(ctx, socket); err != nil {
		return DaemonStopResult{}, &Error{
			Code: CodeGeneral, ExitCode: ExitGeneral,
			Message: fmt.Sprintf("asking daemon pid %d to stop", pid),
			Hint:    "'agenthub daemon stop --force' ends it if it will not answer",
			Err:     err,
		}
	}
	deadline := time.NewTimer(daemonStopDeadline)
	defer deadline.Stop()
	tick := time.NewTicker(daemonPollInterval)
	defer tick.Stop()
	for {
		if !daemonAlive(pid) {
			return DaemonStopResult{
				Stopped: true, Pid: pid,
				Message: fmt.Sprintf("daemon (pid %d) stopped", pid),
			}, nil
		}
		select {
		case <-ctx.Done():
			return DaemonStopResult{}, ctx.Err()
		case <-deadline.C:
			return DaemonStopResult{}, &Error{
				Code: CodeGeneral, ExitCode: ExitGeneral,
				Message: fmt.Sprintf("daemon (pid %d) did not stop within %v", pid, daemonStopDeadline),
				Hint:    "retry with 'agenthub daemon stop --force'",
			}
		case <-tick.C:
		}
	}
}

func (a *App) newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report daemon liveness, pid, version and session count (exit 4 when offline)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			socket, _, _, err := a.daemonPaths()
			if err != nil {
				return err
			}
			hello, perr := pingDaemon(cmd.Context(), socket)
			if perr != nil {
				if err := a.printer().Emit(DaemonStatus{Running: false, Socket: socket}); err != nil {
					return err
				}
				// Frozen exit-code table (docs/modules/controlplane.md): 4 = daemon offline.
				return &silentExitError{code: ExitDaemonDown}
			}
			status := DaemonStatus{
				Running: true,
				Pid:     hello.Pid, Version: hello.Version, Generation: hello.Generation,
				Owner:  hello.Owner,
				Socket: socket,
			}
			// Session count is best-effort decoration; a race with a dying
			// daemon must not fail the whole status report.
			client := api.New(socket)
			defer client.Close()
			lctx, cancel := context.WithTimeout(cmd.Context(), daemonPingTimeout)
			defer cancel()
			if sessions, serr := client.Sessions.List(lctx); serr == nil {
				status.Sessions = len(sessions)
			}
			return a.printer().Emit(status)
		},
	}
}

func (a *App) newDaemonRestartCmd() *cobra.Command {
	var http httpFlags
	var owner ownerFlags
	cmd := &cobra.Command{
		Use:   "restart [--headless] [--http-addr host:port]",
		Short: "Stop then start the hub (stdio sessions are unaffected by design)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Ownership is checked BEFORE the stop, not after it. The
			// replacement is subject to the same admission as any other start,
			// and finding that out afterwards would leave the machine with the
			// hub stopped and no hub started — the one outcome a restart must
			// never produce.
			if _, err := owner.resolve(false); err != nil {
				return err
			}
			if _, err := a.stopDaemon(cmd.Context(), false); err != nil {
				return err
			}
			// The flags are NOT inherited from the daemon being replaced: a
			// listener must be asked for every time, or a restart would
			// resurrect an endpoint whose original opt-in nobody remembers.
			res, err := a.startDaemonBackground(cmd.Context(), http, owner)
			if err != nil {
				return err
			}
			return a.printer().Emit(res)
		},
	}
	http.bind(cmd)
	owner.bind(cmd)
	return cmd
}

func (a *App) newDaemonLogsCmd() *cobra.Command {
	var (
		follow bool
		since  time.Duration
		level  string
	)
	cmd := &cobra.Command{
		Use:   "logs [-f] [--since 1h] [--level warn]",
		Short: "Query the daemon's structured log (JSON lines from <data>/logs/daemon.log)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, logsDir, err := a.daemonPaths()
			if err != nil {
				return err
			}
			filter, err := newLogFilter(since, level)
			if err != nil {
				return err
			}
			path := filepath.Join(logsDir, daemon.LogFileName)
			// Streaming output: lines go straight to stdout. --json emits
			// the raw JSON lines (already machine-readable); the envelope
			// convention does not fit an unbounded stream.
			return a.streamDaemonLogs(cmd.Context(), path, filter, follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep reading as the daemon appends")
	cmd.Flags().DurationVar(&since, "since", 0, "only lines newer than this age (e.g. 1h, 30m)")
	cmd.Flags().StringVar(&level, "level", "", "minimum level: debug, info, warn or error")
	return cmd
}

// logFilter holds the parsed --since/--level criteria.
type logFilter struct {
	cutoff   time.Time
	minLevel slog.Level
	hasLevel bool
}

func newLogFilter(since time.Duration, level string) (logFilter, error) {
	var f logFilter
	if since > 0 {
		f.cutoff = time.Now().Add(-since)
	}
	if level != "" {
		if err := f.minLevel.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
			return f, Usagef("invalid --level %q (use debug, info, warn or error)", level)
		}
		f.hasLevel = true
	}
	return f, nil
}

// admit decides whether one parsed log line passes the filters.
// Failure direction: when a filter is active and the corresponding field is
// unparseable, the line is dropped — a filtered view must not smuggle in
// lines it cannot classify.
func (f logFilter) admit(t time.Time, tOK bool, lvl slog.Level, lvlOK bool) bool {
	if !f.cutoff.IsZero() {
		if !tOK || t.Before(f.cutoff) {
			return false
		}
	}
	if f.hasLevel {
		if !lvlOK || lvl < f.minLevel {
			return false
		}
	}
	return true
}

// streamDaemonLogs reads the daemon's stream line by line, filtering and
// rendering. With follow it keeps polling the active file for appended data
// until ctx is done.
//
// The rotated segments are read first, oldest to newest. Reading only the
// active file would make a `--since 24h` answer "nothing happened" for
// everything rotation moved aside — the same trap the merged reader avoids.
func (a *App) streamDaemonLogs(ctx context.Context, path string, filter logFilter, follow bool) error {
	segments := jsonl.Segments(path)
	for _, seg := range segments[:len(segments)-1] {
		if err := a.renderLogFile(seg, filter); err != nil {
			return err
		}
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			e := NotFoundf(CodeNotFound, "no daemon log at %s", path)
			e.Hint = "the hub writes it the first time it runs; open AgentHub, or start one with --headless"
			return e
		}
		return err
	}
	defer func() { _ = f.Close() }()

	r := bufio.NewReader(f)
	var offset int64
	for {
		line, rerr := r.ReadString('\n')
		if line != "" {
			offset += int64(len(line))
			a.renderLogLine(strings.TrimRight(line, "\n"), filter)
		}
		if rerr == nil {
			continue
		}
		if !errors.Is(rerr, io.EOF) {
			return rerr
		}
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(logsFollowInterval):
		}
		if st, serr := os.Stat(path); serr == nil && st.Size() < offset {
			// The file shrank: it was rotated away, or truncated. Rotation
			// RENAMES the active file, so this descriptor still points at the
			// segment that moved — seeking it back to the top would re-print
			// that history instead of following the new file. Reopen instead.
			nf, oerr := os.Open(path)
			if oerr != nil {
				return oerr
			}
			_ = f.Close()
			f = nf
			r.Reset(f)
			offset = 0
		}
	}
}

// renderLogFile prints one whole file through the same filter. A missing file
// is not an error: retention can prune a segment between the listing and the
// read, and a gap in old history is not worth failing a tail over.
func (a *App) renderLogFile(path string, filter logFilter) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()

	r := bufio.NewReader(f)
	for {
		line, rerr := r.ReadString('\n')
		if line != "" {
			a.renderLogLine(strings.TrimRight(line, "\n"), filter)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

// renderLogLine filters and prints one raw JSON log line. Unparseable lines
// are shown verbatim only when no filter is active (visibility over
// polish), and dropped otherwise (see logFilter.admit).
func (a *App) renderLogLine(raw string, filter logFilter) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	var fields map[string]any
	parsed := json.Unmarshal([]byte(raw), &fields) == nil

	var (
		t      time.Time
		tOK    bool
		lvl    slog.Level
		lvlOK  bool
		msg    string
		lvlStr string
	)
	if parsed {
		if s, ok := fields["time"].(string); ok {
			if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
				t, tOK = ts, true
			}
		}
		if s, ok := fields["level"].(string); ok {
			lvlStr = s
			lvlOK = lvl.UnmarshalText([]byte(s)) == nil
		}
		msg, _ = fields["msg"].(string)
	}
	if !parsed {
		if filter.hasLevel || !filter.cutoff.IsZero() {
			return
		}
		_, _ = fmt.Fprintln(a.stdout, raw)
		return
	}
	if !filter.admit(t, tOK, lvl, lvlOK) {
		return
	}
	if a.jsonOut {
		_, _ = fmt.Fprintln(a.stdout, raw)
		return
	}
	var b strings.Builder
	if tOK {
		b.WriteString(t.Format(time.RFC3339))
	} else {
		b.WriteString("????-??-??T??:??:??Z")
	}
	fmt.Fprintf(&b, " %-5s %s", lvlStr, msg)
	extras := make([]string, 0, len(fields))
	for k, v := range fields {
		if k == "time" || k == "level" || k == "msg" {
			continue
		}
		extras = append(extras, fmt.Sprintf("%s=%v", k, v))
	}
	slices.Sort(extras)
	if len(extras) > 0 {
		b.WriteString(" | ")
		b.WriteString(strings.Join(extras, " "))
	}
	_, _ = fmt.Fprintln(a.stdout, b.String())
}

// env reads one environment variable through the invocation's resolver, so a
// test can inject it without mutating the process environment.
func (a *App) env(key string) string {
	if a.resolver != nil && a.resolver.LookupEnv != nil {
		v, _ := a.resolver.LookupEnv(key)
		return v
	}
	return os.Getenv(key)
}

// daemonPaths resolves the socket, run and logs locations.
func (a *App) daemonPaths() (socket, runDir, logsDir string, err error) {
	if socket, err = a.resolver.CtlSocketPath(); err != nil {
		return "", "", "", err
	}
	if runDir, err = a.resolver.RunDir(); err != nil {
		return "", "", "", err
	}
	if logsDir, err = a.resolver.LogsDir(); err != nil {
		return "", "", "", err
	}
	return socket, runDir, logsDir, nil
}

// pingDaemon probes the daemon at socket with a bounded deadline.
func pingDaemon(ctx context.Context, socket string) (api.Hello, error) {
	client := api.New(socket)
	defer client.Close()
	pctx, cancel := context.WithTimeout(ctx, daemonPingTimeout)
	defer cancel()
	return client.Ping(pctx)
}

// fileTail returns the last limit bytes of the file at path ("<empty>" when
// missing or empty) for start-failure diagnostics.
func fileTail(path string, limit int64) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return "<empty>"
	}
	if int64(len(b)) > limit {
		b = b[int64(len(b))-limit:]
		// The cut lands on an arbitrary byte, so for any non-ASCII log it
		// lands INSIDE a rune and the tail opens with U+FFFD. Drop the
		// orphaned continuation bytes (at most three). This text is the
		// daemon's stderr shown when startup failed — the most important
		// diagnostic the user gets, and the one place mojibake is least
		// affordable.
		for len(b) > 0 && !utf8.RuneStart(b[0]) {
			b = b[1:]
		}
	}
	return strings.TrimSpace(string(b))
}

// requestDaemonStop asks the daemon to stop over the control plane, for the
// platforms that have no signal to send. It mirrors pingDaemon: its own
// short-lived client, its own timeout, nothing retained.
func requestDaemonStop(ctx context.Context, socket string) error {
	client := api.New(socket)
	defer client.Close()
	sctx, cancel := context.WithTimeout(ctx, daemonPingTimeout)
	defer cancel()
	_, err := client.RequestStop(sctx)
	return err
}
