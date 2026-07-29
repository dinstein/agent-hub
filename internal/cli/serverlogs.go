package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/audit"
	"github.com/dinstein/agent-hub/internal/downstream"
)

// `server logs` is the reader half of the per-server trace log written by
// internal/downstream ("one log file per server,
// logs/server-<name>.log … + agenthub server logs <id> --follow").
//
// Like `audit tail` it is a READ-ONLY projection: it never opens the file for
// writing, so running it can never disturb the multi-writer discipline the
// gateways and the daemon depend on. Unlike `audit tail -f` it does NOT
// require a daemon — a stdio gateway writes this file with no daemon in
// sight, so refusing to follow without one would be wrong.

const (
	// serverLogsDefault is how many frames `server logs` shows by default.
	serverLogsDefault = 100
	// serverLogsInterval is the --follow re-read period. It matches
	// auditFollowInterval so the two follow modes feel the same.
	serverLogsInterval = 500 * time.Millisecond
	// serverLogsMaxLine bounds one line while reading. The writer bounds
	// what it appends; a longer line means a foreign or corrupt file.
	serverLogsMaxLine = 1 << 20
	// serverLogsDetailWidth caps the DETAIL column of the human table. The
	// full payload is always in --json.
	serverLogsDetailWidth = 120
)

// ServerLogRow is one trace frame as both output modes render it. It mirrors
// downstream.TraceFrame; the payload is rendered as-is because a trace log
// holds protocol frames of the operator's OWN servers — the same bytes
// `agenthub inspect` shows — and never a vault secret (secrets are resolved
// into the child environment and HTTP headers, neither of which is a frame).
type ServerLogRow struct {
	TS        string `json:"ts"`
	Server    string `json:"server"`
	Dir       string `json:"dir"`
	Method    string `json:"method"`
	Bytes     int    `json:"bytes"`
	Payload   string `json:"payload,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
	DurMs     int64  `json:"durMs,omitempty"`
	// PID and Inst answer "who wrote this line", the question a shared file
	// forces. One server's log is written by every gateway process holding it
	// open (PID) and, inside one process, by every derived instance of that
	// server (Inst). Both were already recorded in the file and neither
	// reached the reader, which is what made two writers interleaving look
	// like one writer contradicting itself.
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
		_, err := fmt.Fprintf(w, "no frames logged for %s (%s)\n", l.Server, l.Path)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tPID\tDIR\tMETHOD\tBYTES\tMS\tDETAIL")
	for _, f := range l.Frames {
		detail := f.Error
		if detail == "" {
			detail = f.Payload
		}
		if f.Truncated {
			detail += "…"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			f.TS, dashInt(f.PID), f.Dir, dash(f.Method), f.Bytes, f.DurMs,
			oneLine(detail, serverLogsDetailWidth))
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

func (a *App) newServerLogsCmd() *cobra.Command {
	var (
		follow bool
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "logs <id> [--follow]",
		Short: "Show the recorded conversation between agenthub and one server",
		Long: "Reads <data>/logs/server-<id>.log, for when a server connects but a tool call\n" +
			"misbehaves. Recording is off unless it was switched on for that server, so an\n" +
			"empty log means nothing was recorded, not that the server sat idle.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			logsDir, err := a.resolver.LogsDir()
			if err != nil {
				return err
			}
			// Resolve through the writer's own path function: the reader and
			// the writer cannot drift apart on the file name.
			path := downstream.ServerLogPath(logsDir, id)
			if !follow {
				logs, err := readServerLogs(id, path, limit)
				if err != nil {
					return err
				}
				return a.printer().Emit(logs)
			}
			return a.followServerLogs(cmd.Context(), id, path, limit)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stay open and keep printing new messages as they arrive")
	cmd.Flags().IntVar(&limit, "limit", serverLogsDefault, "how many messages to show (0 = all of them)")
	return cmd
}

// followServerLogs prints the current tail and then every newly appended
// frame. Like followAudit it tracks a byte offset, so a rotation (rename +
// fresh file) shows up as "the file shrank" and the reader restarts at the
// beginning of the new segment.
func (a *App) followServerLogs(ctx context.Context, id, path string, limit int) error {
	p := a.printer()
	logs, err := readServerLogs(id, path, limit)
	if err != nil {
		return err
	}
	if err := p.Emit(logs); err != nil {
		return err
	}
	offset := fileSize(path)
	ticker := time.NewTicker(serverLogsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		size := fileSize(path)
		if size < offset {
			offset = 0 // rotated: start over on the new segment
		}
		if size == offset {
			continue
		}
		frames, skipped, err := readTraceFrom(path, offset)
		if err != nil {
			return err
		}
		offset = size
		if len(frames) == 0 && skipped == 0 {
			continue
		}
		batch := ServerLogs{Server: id, Path: path, Frames: rowsOf(frames), Skipped: skipped}
		if err := p.Emit(batch); err != nil {
			return err
		}
	}
}

// readServerLogs reads the whole file and keeps the last `limit` frames
// (limit <= 0 = all). A missing file is not an error: tracing may simply
// never have been enabled — which is what Note says.
func readServerLogs(id, path string, limit int) (ServerLogs, error) {
	out := ServerLogs{Server: id, Path: path, Frames: []ServerLogRow{}}
	if !fileExists(path) {
		out.Note = fmt.Sprintf("no trace log for %q yet (%s); frame tracing is off by default", id, path)
		return out, nil
	}
	frames, skipped, err := readTraceFrom(path, 0)
	if err != nil {
		return ServerLogs{}, err
	}
	if limit > 0 && len(frames) > limit {
		frames = frames[len(frames)-limit:]
	}
	out.Frames = rowsOf(frames)
	out.Skipped = skipped
	return out, nil
}

func rowsOf(frames []downstream.TraceFrame) []ServerLogRow {
	out := make([]ServerLogRow, 0, len(frames))
	for _, f := range frames {
		out = append(out, ServerLogRow{
			TS:     f.TS.UTC().Format(time.RFC3339Nano),
			Server: f.Server, Dir: f.Dir, Method: f.Method, Bytes: f.Bytes,
			Payload: f.Payload, Truncated: f.Truncated, Error: f.Error, DurMs: f.DurMs,
			PID: f.PID, Inst: f.Inst,
		})
	}
	return out
}

// readTraceFrom decodes every JSONL frame from offset to EOF. Undecodable
// lines are COUNTED and skipped rather than aborting the read: a killed
// writer's torn last line must not make the whole trace unreadable.
func readTraceFrom(path string, offset int64) ([]downstream.TraceFrame, int, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return nil, 0, err
		}
	}
	var (
		frames  []downstream.TraceFrame
		skipped int
	)
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 64<<10), serverLogsMaxLine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// The oversize marker FIRST: it shares the "ts" field with a frame,
		// so it unmarshals into TraceFrame without error and yields a zero
		// value — a blank row claiming nothing happened, in place of the one
		// frame big enough to be worth reading.
		if m, ok := audit.DecodeOversize([]byte(line)); ok {
			frames = append(frames, oversizeFrame(m))
			continue
		}
		var f downstream.TraceFrame
		if json.Unmarshal([]byte(line), &f) != nil {
			skipped++
			continue
		}
		frames = append(frames, f)
	}
	if err := sc.Err(); err != nil {
		return nil, skipped, fmt.Errorf("read %s: %w", path, err)
	}
	return frames, skipped, nil
}

// dashInt renders a pid, or "-" for a frame written before the field
// existed. An old trace file stays readable rather than growing a column of
// zeros that look like a process id.
func dashInt(n int) string {
	if n == 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

// oversizeFrame renders an audit oversize marker as a frame, so a reader
// sees WHAT was lost and how big it was instead of an empty row.
//
// Dir/Method come out of the marker's prefix when they are still in it: the
// prefix is the head of the original line, which is where those fields sit.
// Recovering them by parsing text is ugly, and it is worth it — "the 64 KB
// tools/list response was dropped" is a different diagnosis from "something
// was dropped", and the trace was opened to make exactly that distinction.
func oversizeFrame(m audit.OversizeMarker) downstream.TraceFrame {
	f := downstream.TraceFrame{
		Bytes: m.OrigBytes,
		Error: fmt.Sprintf("frame dropped: its line exceeded the %d-byte bound (%d bytes)",
			audit.DefaultMaxLineBytes, m.OrigBytes),
		Truncated: true,
	}
	if ts, err := time.Parse(time.RFC3339Nano, m.TS); err == nil {
		f.TS = ts
	}
	f.Dir = fieldFromPrefix(m.Prefix, "dir")
	f.Method = fieldFromPrefix(m.Prefix, "method")
	if v := fieldFromPrefix(m.Prefix, "server"); v != "" {
		f.Server = v
	}
	return f
}

// fieldFromPrefix pulls one string field out of the truncated JSON head a
// marker carries. It is deliberately literal — the prefix is a fragment, so
// it cannot be unmarshaled — and returns "" for anything it cannot find
// rather than guessing.
func fieldFromPrefix(prefix, key string) string {
	needle := `"` + key + `":"`
	i := strings.Index(prefix, needle)
	if i < 0 {
		return ""
	}
	rest := prefix[i+len(needle):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}
