package cli

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/proclog"
)

// `logs` is the reader for the hub's PROCESS logs — what a daemon or a
// gateway was doing — as opposed to `calls` (what a client called) and
// `server logs` (what crossed one downstream connection).
//
// It exists because only half of them had a reader. `daemon logs` reads
// daemon.log, and the daemon is the component that never dials a downstream
// (internal/ctlapi/gatewaystate.go says so, and explains why): every circuit
// transition, health flip, respawn and connection close is observed by a
// stdio gateway and written to gateway-<client>.log, which nothing in the
// tree could open. The evidence for "why did this server drop" was being
// recorded to a file with no reader.
//
// Merging them is the point, not a convenience. A daemon restart and the six
// gateways that lost their connections two seconds later are one story told
// in seven files, and re-assembling it by hand is what an operator would
// otherwise be doing at the moment they can least afford to.
//
// The READING lives in internal/proclog, because the GUI needs the same
// answer and cannot open a file. This is the terminal's face of it: the
// selectors, the merge cursor for --follow, and the rendering.
//
// `daemon logs` stays as it is. It belongs to the `daemon` group and answers
// a question about one process; adding an origin column there would make a
// single-process view claim to be something it is not.

const (
	// logsDefaultLimit is how many merged records `logs` shows by default.
	// A merged read has to buffer in order to sort, so unlike `daemon logs`
	// — which streams and can afford to be unbounded — this one is bounded
	// by default and opts into everything with --limit 0.
	logsDefaultLimit = 200
)

// logSourceValues are the --source choices. "all" is the default: the whole
// reason this command exists is that reading one kind in isolation is what
// hid the gateway logs in the first place.
var logSourceValues = append([]string{"all"}, proclog.Origins()...)

func (a *App) newLogsCmd() *cobra.Command {
	var (
		follow   bool
		sinceRaw string
		level    string
		limit    int
		source   string
		client   string
		server   string
	)
	cmd := &cobra.Command{
		Use:   "logs [-f] [--since 1h] [--level warn] [--server <id>]",
		Short: "Read what the hub's processes did, merged across daemon and gateways",
		Long: "Reads the structured logs under <data>/logs — daemon.log and every\n" +
			"gateway-<client>.log — and prints them as one time-ordered stream.\n\n" +
			"The gateways are the half worth having: the daemon never dials a\n" +
			"downstream, so connection failures, circuit transitions, health flips and\n" +
			"respawns are all observed and recorded by the gateway serving a client.\n\n" +
			"Works offline. --server is usually the fastest way in when one downstream\n" +
			"is misbehaving; `agenthub calls` answers what was CALLED, and\n" +
			"`agenthub server logs` shows the frames of one connection.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !slices.Contains(logSourceValues, source) {
				e := Usagef("unknown --source %q", source)
				e.Hint = "use one of: " + strings.Join(logSourceValues, ", ")
				return e
			}
			logsDir, err := a.resolver.LogsDir()
			if err != nil {
				return err
			}
			since, err := observeSince(sinceRaw)
			if err != nil {
				return err
			}
			q := proclog.Query{Client: client, Server: server, Since: since}
			if source != "all" {
				q.Source = proclog.Origin(source)
			}
			if level != "" {
				lvl, lerr := parseLogLevel(level)
				if lerr != nil {
					return lerr
				}
				q.MinLevel, q.Leveled = lvl, true
			}
			files, err := proclog.Files(logsDir, q)
			if err != nil {
				return err
			}
			if len(files) == 0 {
				e := NotFoundf(CodeNotFound, "no process logs under %s", logsDir)
				e.Hint = "the daemon writes daemon.log on first start, and each " +
					"'agenthub connect' writes its own gateway log"
				return e
			}
			return a.streamLogs(cmd.Context(), files, q, limit, follow)
		},
	}
	bindObserveFlags(cmd, "records", &sinceRaw, &limit, &follow, logsDefaultLimit)
	cmd.Flags().StringVar(&level, "level", "", "minimum level: debug, info, warn or error")
	cmd.Flags().StringVar(&source, "source", "all", "which processes: "+strings.Join(logSourceValues, ", "))
	cmd.Flags().StringVar(&client, "client", "", "only records from the gateway serving this client")
	cmd.Flags().StringVar(&server, "server", "", "only records about this downstream server")
	return cmd
}

// streamLogs prints the merged tail and, with follow, everything appended
// after it.
//
// Ordering is exact for the initial read and per-batch while following: each
// tick collects whatever every file appended since the last one and sorts
// that batch. Two processes writing in the same tick are ordered correctly
// against each other; a record cannot be re-ordered ahead of one already
// printed, which is the property that would make a stream unreadable.
func (a *App) streamLogs(ctx context.Context, files []proclog.File, q proclog.Query, limit int, follow bool) error {
	offsets := make(map[string]int64, len(files))
	records, err := readLogBatch(files, offsets, q)
	if err != nil {
		return err
	}
	if limit > 0 && len(records) > limit {
		records = records[len(records)-limit:]
	}
	a.renderLogRecords(records)
	if !follow {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(logsFollowInterval):
		}
		batch, err := readLogBatch(files, offsets, q)
		if err != nil {
			return err
		}
		a.renderLogRecords(batch)
	}
}

// readLogBatch reads every file from its recorded offset to EOF, advances the
// offsets, and returns the admitted records in time order.
//
// A file that shrank was rotated or truncated, so its offset resets to 0 and
// the new segment is read from the top — the same rule `server logs --follow`
// and `daemon logs -f` already use.
func readLogBatch(files []proclog.File, offsets map[string]int64, q proclog.Query) ([]proclog.Record, error) {
	var out []proclog.Record
	for _, f := range files {
		size := proclog.Size(f.Path)
		if size < offsets[f.Path] {
			offsets[f.Path] = 0
		}
		if size == offsets[f.Path] {
			continue
		}
		records, err := proclog.ReadFrom(f, offsets[f.Path], q)
		if err != nil {
			return nil, err
		}
		offsets[f.Path] = size
		out = append(out, records...)
	}
	// Stable, so records sharing a timestamp keep file order rather than
	// shuffling between reads of the same data.
	slices.SortStableFunc(out, func(x, y proclog.Record) int { return x.TS.Compare(y.TS) })
	return out, nil
}

// renderLogRecords writes the batch. Like `daemon logs` this is a stream
// rather than an envelope: --json emits the raw lines, which are already the
// machine-readable form, one per line.
func (a *App) renderLogRecords(records []proclog.Record) {
	for _, r := range records {
		if a.jsonOut {
			_, _ = fmt.Fprintln(a.stdout, r.Raw)
			continue
		}
		_, _ = fmt.Fprintln(a.stdout, formatLogRecord(r))
	}
}

// formatLogRecord renders one line: time, level, origin, message, then every
// remaining field sorted.
//
// The origin column is the reason this renderer exists rather than reusing
// `daemon logs`'. It is the one thing a merged stream must say and a
// single-file stream must not pretend to.
func formatLogRecord(r proclog.Record) string {
	var b strings.Builder
	if r.TSOK {
		b.WriteString(r.TS.Format(time.RFC3339))
	} else {
		b.WriteString("????-??-??T??:??:??Z")
	}
	fmt.Fprintf(&b, " %-5s %-7s %s", r.Text, r.Origin, r.Msg)
	extras := make([]string, 0, len(r.Fields))
	for k, v := range r.Fields {
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
	return b.String()
}

// parseLogLevel turns a --level value into a slog level. It is shared with
// `daemon logs` through newLogFilter, so the two spellings of the same flag
// cannot accept different words.
func parseLogLevel(level string) (slog.Level, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		return lvl, Usagef("invalid --level %q (use debug, info, warn or error)", level)
	}
	return lvl, nil
}
