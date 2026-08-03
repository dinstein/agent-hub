package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/calllog"
)

// `server logs` is the reader for the frames of ONE downstream conversation.
//
// It used to read a file of its own, logs/server-<id>.log, and that file is
// gone: the frames are records in the call ledger now, so every one of them
// carries the id of the client call that caused it. The command keeps its
// name and its shape — canonical.md §3 fixes what `logs`, `daemon logs` and
// `server logs` each mean, and this is still the third of those — while
// answering a question it never could: which call a frame belongs to, and
// which attempt of it.
//
// Like every other record reader it is READ-ONLY, and it needs no daemon: a
// stdio gateway writes these records with nothing else running.

const (
	// serverLogsDefault is how many frames `server logs` shows by default.
	serverLogsDefault = 100
	// serverLogsInterval is the --follow re-read period. It matches
	// eventsFollowInterval so the two record readers under Observe that a
	// user is most likely to run side by side feel the same.
	serverLogsInterval = 500 * time.Millisecond
	// serverLogsDetailWidth caps the DETAIL column of the human table.
	serverLogsDetailWidth = 120
)

// ServerLogRow is one frame as both output modes render it.
//
// The payload is NOT on it, and that is the one real change for a reader: a
// frame body now lives in the ledger's encrypted pack, so it is shown by
// `agenthub calls show <call-id>` — which has the key, and the whole story
// around it — rather than by a tail. What a tail needs is here: the
// direction, the method, the size, the duration, and the call.
type ServerLogRow struct {
	TS     string `json:"ts"`
	Server string `json:"server"`
	// Dir is "sent" or "recv", which is the record's own kind.
	Dir    string `json:"dir"`
	Method string `json:"method"`
	Bytes  int    `json:"bytes"`
	Error  string `json:"error,omitempty"`
	DurMs  int64  `json:"durMs,omitempty"`
	// CallID joins this frame to the client call that caused it, and Cause
	// says why a frame with no call id exists at all — a health probe, a
	// tools refresh. Neither was answerable from the old per-server file.
	CallID string `json:"callId,omitempty"`
	Cause  string `json:"cause,omitempty"`
	// Seq numbers the attempts within one call: a retry ladder produces 1, 2,
	// 3, and without it three attempts read as one exchange reported thrice.
	Seq int `json:"seq,omitempty"`
	// PID and Inst answer "who wrote this", the question a shared history
	// forces. One server is spoken to by every gateway process (PID) and,
	// inside one process, by every derived instance of it (Inst).
	PID  int    `json:"pid,omitempty"`
	Inst string `json:"inst,omitempty"`
}

// ServerLogs is the `server logs` result.
type ServerLogs struct {
	Server string         `json:"server"`
	Path   string         `json:"path"`
	Frames []ServerLogRow `json:"frames"`
	// Skipped counts undecodable lines (a torn tail from a killed writer, a
	// foreign file). Counted, never silently dropped.
	Skipped int `json:"skipped,omitempty"`
	// Note carries the "nothing here yet" explanation so the JSON consumer
	// sees the same diagnosis the human does.
	Note string `json:"note,omitempty"`
}

// Human renders the frame table.
func (l ServerLogs) Human(w io.Writer) error {
	if len(l.Frames) == 0 {
		if l.Note != "" {
			_, err := fmt.Fprintln(w, l.Note)
			return err
		}
		_, err := fmt.Fprintf(w, "no frames recorded for %s (%s)\n", l.Server, l.Path)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tPID\tDIR\tMETHOD\tBYTES\tMS\tCALL\tDETAIL")
	for _, f := range l.Frames {
		detail := f.Error
		if detail == "" {
			detail = f.Cause
			if f.Seq > 1 {
				detail = fmt.Sprintf("%s attempt %d", detail, f.Seq)
			}
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n",
			f.TS, dashInt(f.PID), f.Dir, dash(f.Method), f.Bytes, f.DurMs,
			dash(shortCallID(f.CallID)), oneLine(detail, serverLogsDetailWidth))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if l.Skipped > 0 {
		_, err := fmt.Fprintf(w, "%d undecodable line(s) skipped\n", l.Skipped)
		return err
	}
	return nil
}

// dashInt renders a pid, or "-" when there is none.
func dashInt(n int) string {
	if n == 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

func (a *App) newServerLogsCmd() *cobra.Command {
	var (
		follow   bool
		limit    int
		sinceRaw string
	)
	cmd := &cobra.Command{
		Use:   "logs <id> [--follow]",
		Short: "Show the recorded conversation between agenthub and one server",
		Long: "Reads the frames recorded for one downstream server, for when a server\n" +
			"connects but a tool call misbehaves. Recording is off unless it was switched\n" +
			"on for that server ('agenthub server trace <id> on'), so an empty result means\n" +
			"nothing was recorded, not that the server sat idle.\n\n" +
			"Every frame names the call it belongs to; 'agenthub calls show <call-id>' then\n" +
			"shows that call's whole story, bodies included.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			root, err := calllog.DefaultDir(a.resolver)
			if err != nil {
				return err
			}
			cutoff, err := observeSince(sinceRaw)
			if err != nil {
				return err
			}
			if !follow {
				logs, err := readServerFrames(root, id, cutoff, limit)
				if err != nil {
					return err
				}
				return a.printer().Emit(logs)
			}
			return a.followServerFrames(cmd.Context(), root, id, cutoff, limit)
		},
	}
	bindObserveFlags(cmd, "frames", &sinceRaw, &limit, &follow, serverLogsDefault)
	return cmd
}

// readServerFrames collects one server's frames, keeping the newest `limit`.
func readServerFrames(root, server string, since time.Time, limit int) (ServerLogs, error) {
	out := ServerLogs{Server: server, Path: root}
	var frames []calllog.Event
	skipped, err := calllog.ScanFramesSince(root, since, func(e calllog.Event) error {
		if e.Server != server {
			return nil
		}
		frames = append(frames, e)
		return nil
	})
	if err != nil {
		return ServerLogs{}, err
	}
	out.Skipped = skipped
	// The frames of N gateway processes are interleaved across N files, so
	// the merge has to sort: a per-file order is the order one process saw,
	// and printing file by file reads as one conversation jumping backwards.
	slices.SortStableFunc(frames, func(a, b calllog.Event) int { return a.TS.Compare(b.TS) })
	if limit > 0 && len(frames) > limit {
		frames = frames[len(frames)-limit:]
	}
	for _, e := range frames {
		out.Frames = append(out.Frames, serverLogRow(e))
	}
	if len(out.Frames) == 0 {
		out.Note = fmt.Sprintf("no frames recorded for %s; switch recording on with "+
			"'agenthub server trace %s on'", server, server)
	}
	return out, nil
}

func serverLogRow(e calllog.Event) ServerLogRow {
	return ServerLogRow{
		TS: e.TS.UTC().Format(time.RFC3339), Server: e.Server,
		Dir: string(e.Kind), Method: e.Method, Bytes: e.Bytes,
		Error: e.Error, DurMs: e.DurationMs,
		CallID: e.CallID, Cause: string(e.Cause), Seq: e.Seq,
		PID: e.PID, Inst: e.Instance,
	}
}

// followServerFrames prints the current tail and then every newly recorded
// frame.
//
// It tracks a record TIMESTAMP rather than a byte offset, for the reason
// followEvents does: one server's frames live in one file per process per
// day, read as a single sequence, so an offset into "the file" is not a
// position in the stream at all.
func (a *App) followServerFrames(ctx context.Context, root, server string, since time.Time, limit int) error {
	p := a.printer()
	logs, err := readServerFrames(root, server, since, limit)
	if err != nil {
		return err
	}
	if err := p.Emit(logs); err != nil {
		return err
	}
	cursor := since
	if n := len(logs.Frames); n > 0 {
		if ts, perr := time.Parse(time.RFC3339, logs.Frames[n-1].TS); perr == nil {
			cursor = ts
		}
	}
	ticker := time.NewTicker(serverLogsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		batch, err := readServerFrames(root, server, cursor, 0)
		if err != nil {
			return err
		}
		fresh := ServerLogs{Server: server, Path: root}
		for _, f := range batch.Frames {
			ts, perr := time.Parse(time.RFC3339, f.TS)
			// Strictly after: a record whose second matches the cursor was
			// already printed, and reprinting it reads as the exchange having
			// happened twice.
			if perr != nil || !ts.After(cursor) {
				continue
			}
			fresh.Frames = append(fresh.Frames, f)
			cursor = ts
		}
		if len(fresh.Frames) == 0 {
			continue
		}
		if err := p.Emit(fresh); err != nil {
			return err
		}
	}
}
