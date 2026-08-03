package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/daemon"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/logx"
)

// `logs` is the reader for the hub's PROCESS logs — what a daemon or a
// gateway was doing — as opposed to `audit` (what a client called) and
// `server logs` (what bytes crossed one downstream connection).
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
// `daemon logs` stays as it is. It belongs to the `daemon` group and answers
// a question about one process; adding an origin column there would make a
// single-process view claim to be something it is not.

const (
	// logsDefaultLimit is how many merged records `logs` shows by default.
	// A merged read has to buffer in order to sort, so unlike `daemon logs`
	// — which streams and can afford to be unbounded — this one is bounded
	// by default and opts into everything with --limit 0.
	logsDefaultLimit = 200
	// logsMaxLine bounds one line while reading. slog writes a record per
	// line and logx scrubs it; a longer line means a foreign or corrupt file.
	logsMaxLine = 1 << 20
)

// logOrigin names which kind of process produced a record. It is the one
// piece of provenance the file carries and the record does not.
type logOrigin string

const (
	originDaemon  logOrigin = "daemon"
	originGateway logOrigin = "gateway"
)

// logSourceValues are the --source choices. "all" is the default: the whole
// reason this command exists is that reading one kind in isolation is what
// hid the gateway logs in the first place.
var logSourceValues = []string{"all", string(originDaemon), string(originGateway)}

// logRecord is one parsed line plus where it came from.
type logRecord struct {
	Origin logOrigin
	// Raw is the line verbatim, which is what --json emits: the file is
	// already machine-readable and re-encoding it could only lose fields.
	Raw string
	// TS/Level are the parsed sort and filter keys; the *OK flags say
	// whether the parse succeeded, because a filter must not admit a line it
	// cannot classify.
	TS      time.Time
	TSOK    bool
	Level   slog.Level
	LevelOK bool
	LevelText,
	Msg string
	Fields map[string]any
}

// logSelector is everything the command was asked to narrow by.
type logSelector struct {
	filter logFilter
	source string
	client string
	server string
}

// admit applies the field selectors on top of the shared --since/--level
// filter. Failure direction matches logFilter.admit and for the same reason:
// when a selector is active and the field is absent, the record is DROPPED.
// A daemon record carries no client, so `--client x` showing daemon lines
// would be a filtered view smuggling in records it cannot classify.
func (s logSelector) admit(r logRecord) bool {
	if !s.filter.admit(r.TS, r.TSOK, r.Level, r.LevelOK) {
		return false
	}
	if s.client != "" && fieldString(r.Fields, logx.FieldClient) != s.client {
		return false
	}
	if s.server != "" && fieldString(r.Fields, logx.FieldServer) != s.server {
		return false
	}
	return true
}

func fieldString(fields map[string]any, key string) string {
	s, _ := fields[key].(string)
	return s
}

func (a *App) newLogsCmd() *cobra.Command {
	var (
		follow bool
		since  time.Duration
		level  string
		limit  int
		sel    logSelector
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
			"is misbehaving; `agenthub audit` answers what was CALLED, and\n" +
			"`agenthub server logs` shows the raw frames of one connection.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !slices.Contains(logSourceValues, sel.source) {
				e := Usagef("unknown --source %q", sel.source)
				e.Hint = "use one of: " + strings.Join(logSourceValues, ", ")
				return e
			}
			logsDir, err := a.resolver.LogsDir()
			if err != nil {
				return err
			}
			if sel.filter, err = newLogFilter(since, level); err != nil {
				return err
			}
			files, err := logFilesFor(logsDir, sel)
			if err != nil {
				return err
			}
			if len(files) == 0 {
				e := NotFoundf(CodeNotFound, "no process logs under %s", logsDir)
				e.Hint = "the daemon writes daemon.log on first start, and each " +
					"'agenthub connect' writes its own gateway log"
				return e
			}
			return a.streamLogs(cmd.Context(), files, sel, limit, follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep reading as the processes append")
	cmd.Flags().DurationVar(&since, "since", 0, "only records newer than this age (e.g. 1h, 30m)")
	cmd.Flags().StringVar(&level, "level", "", "minimum level: debug, info, warn or error")
	cmd.Flags().IntVar(&limit, "limit", logsDefaultLimit, "how many records to show (0 = all of them)")
	cmd.Flags().StringVar(&sel.source, "source", "all", "which processes: "+strings.Join(logSourceValues, ", "))
	cmd.Flags().StringVar(&sel.client, "client", "", "only records from the gateway serving this client")
	cmd.Flags().StringVar(&sel.server, "server", "", "only records about this downstream server")
	return cmd
}

// logFile is one open-able source.
type logFile struct {
	path   string
	origin logOrigin
}

// logFilesFor lists the files the selector can possibly match. Missing files
// are simply absent — a daemon that has never run, or a client that has never
// connected, is a normal state and not an error.
//
// --client narrows by FILE NAME here, which is a superset (gateway.LogPath is
// many-to-one), and the exact match happens per record in logSelector.admit.
// Doing only one of the two would be wrong in a different direction each way:
// the name alone can over-select, and the field alone would read every file.
func logFilesFor(logsDir string, sel logSelector) ([]logFile, error) {
	var out []logFile
	if sel.source != string(originGateway) && sel.client == "" {
		if path := filepath.Join(logsDir, daemon.LogFileName); fileExists(path) {
			out = append(out, logFile{path: path, origin: originDaemon})
		}
	}
	if sel.source == string(originDaemon) {
		return out, nil
	}
	if sel.client != "" {
		if path := gateway.LogPath(logsDir, sel.client); fileExists(path) {
			out = append(out, logFile{path: path, origin: originGateway})
		}
		return out, nil
	}
	matches, err := filepath.Glob(filepath.Join(logsDir,
		gateway.LogFilePrefix+"*"+gateway.LogFileExt))
	if err != nil {
		return nil, fmt.Errorf("list gateway logs in %s: %w", logsDir, err)
	}
	slices.Sort(matches)
	for _, path := range matches {
		out = append(out, logFile{path: path, origin: originGateway})
	}
	return out, nil
}

// streamLogs prints the merged tail and, with follow, everything appended
// after it.
//
// Ordering is exact for the initial read and per-batch while following: each
// tick collects whatever every file appended since the last one and sorts
// that batch. Two processes writing in the same tick are ordered correctly
// against each other; a record cannot be re-ordered ahead of one already
// printed, which is the property that would make a stream unreadable.
func (a *App) streamLogs(ctx context.Context, files []logFile, sel logSelector, limit int, follow bool) error {
	offsets := make(map[string]int64, len(files))
	records, err := readLogBatch(files, offsets, sel)
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
		batch, err := readLogBatch(files, offsets, sel)
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
func readLogBatch(files []logFile, offsets map[string]int64, sel logSelector) ([]logRecord, error) {
	var out []logRecord
	for _, f := range files {
		size := fileSize(f.path)
		if size < offsets[f.path] {
			offsets[f.path] = 0
		}
		if size == offsets[f.path] {
			continue
		}
		records, err := readLogFrom(f, offsets[f.path], sel)
		if err != nil {
			return nil, err
		}
		offsets[f.path] = size
		out = append(out, records...)
	}
	// Stable, so records sharing a timestamp keep file order rather than
	// shuffling between reads of the same data.
	slices.SortStableFunc(out, func(x, y logRecord) int { return x.TS.Compare(y.TS) })
	return out, nil
}

// readLogFrom decodes one file's admitted records from offset to EOF.
//
// An unparseable line is DROPPED rather than counted or shown. That differs
// from `server logs`, which counts them, and the reason is the merge: a line
// that does not parse has no timestamp, so there is no position in a merged
// stream where showing it would be truthful.
func readLogFrom(f logFile, offset int64, sel logSelector) ([]logRecord, error) {
	file, err := os.Open(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // rotated away between the stat and the open
		}
		return nil, fmt.Errorf("open %s: %w", f.path, err)
	}
	defer func() { _ = file.Close() }()
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
	}
	var out []logRecord
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 64<<10), logsMaxLine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		rec, ok := parseLogRecord(line, f.origin)
		if !ok || !sel.admit(rec) {
			continue
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", f.path, err)
	}
	return out, nil
}

// parseLogRecord decodes one slog JSON line. It reports false for anything
// that is not a JSON object, which is the only shape logx writes.
func parseLogRecord(line string, origin logOrigin) (logRecord, bool) {
	var fields map[string]any
	if json.Unmarshal([]byte(line), &fields) != nil {
		return logRecord{}, false
	}
	rec := logRecord{Origin: origin, Raw: line, Fields: fields}
	if s, ok := fields["time"].(string); ok {
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			rec.TS, rec.TSOK = ts, true
		}
	}
	if s, ok := fields["level"].(string); ok {
		rec.LevelText = s
		rec.LevelOK = rec.Level.UnmarshalText([]byte(s)) == nil
	}
	rec.Msg, _ = fields["msg"].(string)
	return rec, true
}

// renderLogRecords writes the batch. Like `daemon logs` this is a stream
// rather than an envelope: --json emits the raw lines, which are already the
// machine-readable form, one per line.
func (a *App) renderLogRecords(records []logRecord) {
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
func formatLogRecord(r logRecord) string {
	var b strings.Builder
	if r.TSOK {
		b.WriteString(r.TS.Format(time.RFC3339))
	} else {
		b.WriteString("????-??-??T??:??:??Z")
	}
	fmt.Fprintf(&b, " %-5s %-7s %s", r.LevelText, r.Origin, r.Msg)
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
