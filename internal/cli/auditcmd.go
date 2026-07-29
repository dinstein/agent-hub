package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/audit"
)

// The `audit` group is a READ-ONLY projection of the governance streams
// (audit.jsonl and its rotated segments). It never opens the streams for
// writing, so running it can never disturb the multi-writer discipline that
// N gateways plus the daemon rely on.
//
// `tail -f` is the one online-only command here (docs/modules/controlplane.md matrix): with
// no daemon nothing new is appended, so following would hang forever
// pretending to work. It refuses with exit 4 instead.

const (
	// auditTailDefault is how many records `audit tail` shows by default.
	auditTailDefault = 50
	// auditFollowInterval is the -f re-read period.
	auditFollowInterval = 500 * time.Millisecond
	// auditMaxLine bounds one JSONL line while reading; the writer bounds
	// what it appends, and a longer line means a foreign/corrupt file.
	auditMaxLine = 1 << 20
)

// AuditRow is one audit record as both output modes render it. It mirrors
// audit.Record exactly — including the deliberate ABSENCE of arguments and
// results, which the record type never carries.
type AuditRow struct {
	TS        string `json:"ts"`
	Actor     string `json:"actor"`
	Client    string `json:"client"`
	Session   string `json:"session"`
	Server    string `json:"server"`
	Tool      string `json:"tool"`
	ArgsHash  string `json:"argsHash"`
	Decision  string `json:"decision"`
	DurMs     int64  `json:"durMs"`
	RequestID string `json:"requestID"`
}

// AuditTail is the `audit tail` result.
type AuditTail struct {
	Records []AuditRow `json:"records"`
	// Source names the files the projection was read from.
	Source []string `json:"source"`
	// Skipped counts lines that could not be decoded (a truncated tail from
	// a crashed writer, or a foreign file). They are counted, never
	// silently dropped.
	Skipped int `json:"skipped,omitempty"`
}

// Human renders the record table.
func (t AuditTail) Human(w io.Writer) error {
	if len(t.Records) == 0 {
		_, err := fmt.Fprintln(w, "no audit records match")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tDECISION\tCLIENT\tSESSION\tSERVER/TOOL\tMS\tREQUEST")
	for _, r := range t.Records {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			r.TS, r.Decision, dash(r.Client), dash(r.Session),
			dash(r.Server)+"/"+dash(r.Tool), r.DurMs, dash(r.RequestID))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if t.Skipped > 0 {
		_, err := fmt.Fprintf(w, "%d undecodable line(s) skipped\n", t.Skipped)
		return err
	}
	return nil
}

// AuditExport is the `audit export` result.
type AuditExport struct {
	Path    string   `json:"path"`
	Rows    int      `json:"rows"`
	Source  []string `json:"source"`
	Skipped int      `json:"skipped,omitempty"`
}

// Human renders the export confirmation.
func (e AuditExport) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w, "exported %s to %s\n", plural(e.Rows, "record", "records"), e.Path)
	return err
}

// auditFilter is the parsed --server/--held/--errors selection.
type auditFilter struct {
	server string
	client string
	held   bool
	errs   bool
}

// matches reports whether a record passes the filter. --held and --errors
// are a union when both are given (that is the useful reading: "show me
// everything that did not just work").
func (f auditFilter) matches(r audit.Record) bool {
	if f.server != "" && r.Server != f.server {
		return false
	}
	if f.client != "" && r.Client != f.client {
		return false
	}
	if !f.held && !f.errs {
		return true
	}
	if f.held && r.Decision == audit.DecisionHeld {
		return true
	}
	if f.errs && (r.Decision == audit.DecisionError || r.Decision == audit.DecisionDenied) {
		return true
	}
	return false
}

func (a *App) newAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Query the governance audit log (read-only projection of audit.jsonl)",
		Args:  cobra.ArbitraryArgs,
		RunE:  groupRunE,
	}
	cmd.AddCommand(a.newAuditTailCmd(), a.newAuditExportCmd())
	return cmd
}

func (a *App) newAuditTailCmd() *cobra.Command {
	var (
		follow bool
		limit  int
		f      auditFilter
	)
	cmd := &cobra.Command{
		Use:   "tail [-f] [--server s] [--held] [--errors]",
		Short: "Show the most recent audit decisions (-f follows; needs the daemon)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logsDir, err := a.resolver.LogsDir()
			if err != nil {
				return err
			}
			path := filepath.Join(logsDir, audit.AuditFileName)
			if !follow {
				tail, err := readAuditTail([]string{path}, f, limit)
				if err != nil {
					return err
				}
				return a.printer().Emit(tail)
			}
			// Following an append-only file only makes sense while a writer
			// exists; with no daemon this would silently hang (docs/modules/controlplane.md).
			if _, _, err := a.requireDaemon(cmd.Context()); err != nil {
				return err
			}
			return a.followAudit(cmd.Context(), path, f, limit)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new records as they are appended")
	cmd.Flags().IntVar(&limit, "limit", auditTailDefault, "how many records to show")
	cmd.Flags().StringVar(&f.server, "server", "", "only this downstream server")
	cmd.Flags().StringVar(&f.client, "client", "", "only this client")
	cmd.Flags().BoolVar(&f.held, "held", false, "only calls waiting on human approval")
	cmd.Flags().BoolVar(&f.errs, "errors", false, "only denied or failed calls")
	return cmd
}

// followAudit prints the current tail and then every newly appended record.
// The reader keeps its own byte offset instead of re-reading the file, so a
// rotation (rename + fresh file) is detected as "the file shrank" and the
// reader restarts from the beginning of the new segment.
func (a *App) followAudit(ctx context.Context, path string, f auditFilter, limit int) error {
	p := a.printer()
	tail, err := readAuditTail([]string{path}, f, limit)
	if err != nil {
		return err
	}
	if err := p.Emit(tail); err != nil {
		return err
	}
	offset := fileSize(path)
	ticker := time.NewTicker(auditFollowInterval)
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
		records, skipped, err := readAuditFrom(path, offset)
		if err != nil {
			return err
		}
		offset = size
		batch := AuditTail{Records: []AuditRow{}, Source: []string{path}, Skipped: skipped}
		for _, r := range records {
			if f.matches(r) {
				batch.Records = append(batch.Records, auditRowOf(r))
			}
		}
		if len(batch.Records) == 0 {
			continue
		}
		if err := p.Emit(batch); err != nil {
			return err
		}
	}
}

func (a *App) newAuditExportCmd() *cobra.Command {
	var (
		out      string
		f        auditFilter
		segments bool
	)
	cmd := &cobra.Command{
		Use:   "export --csv <out.csv>",
		Short: "Export audit records to CSV (spreadsheet formula injection defused)",
		Long: "Export audit records to CSV.\n\n" +
			"Every cell — headers included — goes through the formula-injection guard\n" +
			"in internal/audit: a cell starting with =, +, -, @, tab or CR is prefixed\n" +
			"with a single quote so a spreadsheet treats it as text.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				e := Usagef("--csv <path> is required")
				e.Hint = helpHint(cmd)
				return e
			}
			logsDir, err := a.resolver.LogsDir()
			if err != nil {
				return err
			}
			sources := []string{filepath.Join(logsDir, audit.AuditFileName)}
			if segments {
				sources = append(auditSegments(logsDir), sources...)
			}
			// limit 0 = every matching record.
			tail, err := readAuditTail(sources, f, 0)
			if err != nil {
				return err
			}
			rows := make([][]string, 0, len(tail.Records))
			for _, r := range tail.Records {
				rows = append(rows, r.csvRow())
			}
			file, err := os.Create(out)
			if err != nil {
				return fmt.Errorf("create %s: %w", out, err)
			}
			// The sanitization lives in internal/audit so the CLI and any
			// other exporter cannot drift apart on what counts as dangerous.
			if err := audit.ExportCSV(file, audit.AuditCSVHeader, rows); err != nil {
				_ = file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
			return a.printer().Emit(AuditExport{
				Path: out, Rows: len(rows), Source: tail.Source, Skipped: tail.Skipped,
			})
		},
	}
	cmd.Flags().StringVar(&out, "csv", "", "destination CSV file")
	cmd.Flags().StringVar(&f.server, "server", "", "only this downstream server")
	cmd.Flags().StringVar(&f.client, "client", "", "only this client")
	cmd.Flags().BoolVar(&f.held, "held", false, "only calls waiting on human approval")
	cmd.Flags().BoolVar(&f.errs, "errors", false, "only denied or failed calls")
	cmd.Flags().BoolVar(&segments, "all-segments", false, "include rotated audit segments")
	return cmd
}

// csvRow renders the row in audit.AuditCSVHeader column order. It rebuilds
// an audit.Record so the column order has exactly ONE definition (the audit
// package's), which is what keeps the CSV stable across releases.
func (r AuditRow) csvRow() []string {
	ts, err := time.Parse(time.RFC3339Nano, r.TS)
	if err != nil {
		ts = time.Time{}
	}
	return audit.Record{
		TS: ts, Actor: r.Actor, Client: r.Client, Session: r.Session,
		Server: r.Server, Tool: r.Tool, ArgsHash: r.ArgsHash,
		Decision: audit.Decision(r.Decision), DurMs: r.DurMs, RequestID: r.RequestID,
	}.CSVRow()
}

func auditRowOf(r audit.Record) AuditRow {
	return AuditRow{
		TS: r.TS.UTC().Format(time.RFC3339Nano), Actor: r.Actor, Client: r.Client,
		Session: r.Session, Server: r.Server, Tool: r.Tool, ArgsHash: r.ArgsHash,
		Decision: string(r.Decision), DurMs: r.DurMs, RequestID: r.RequestID,
	}
}

// readAuditTail reads the given files in order, keeps the matching records
// and returns the last `limit` of them (limit <= 0 = all). A missing file is
// not an error: nothing has been logged yet.
func readAuditTail(paths []string, f auditFilter, limit int) (AuditTail, error) {
	out := AuditTail{Records: []AuditRow{}, Source: []string{}}
	var matched []audit.Record
	for _, path := range paths {
		records, skipped, err := readAuditFrom(path, 0)
		if err != nil {
			return AuditTail{}, err
		}
		if records == nil && skipped == 0 && !fileExists(path) {
			continue
		}
		out.Source = append(out.Source, path)
		out.Skipped += skipped
		for _, r := range records {
			if f.matches(r) {
				matched = append(matched, r)
			}
		}
	}
	if limit > 0 && len(matched) > limit {
		matched = matched[len(matched)-limit:]
	}
	for _, r := range matched {
		out.Records = append(out.Records, auditRowOf(r))
	}
	return out, nil
}

// readAuditFrom decodes every JSONL record from offset to EOF. Undecodable
// lines are COUNTED and skipped rather than aborting the read: a crashed
// writer's torn last line must not make the whole log unreadable.
func readAuditFrom(path string, offset int64) ([]audit.Record, int, error) {
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
		records []audit.Record
		skipped int
	)
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 64<<10), auditMaxLine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r audit.Record
		if json.Unmarshal([]byte(line), &r) != nil {
			skipped++
			continue
		}
		records = append(records, r)
	}
	if err := sc.Err(); err != nil {
		return nil, skipped, fmt.Errorf("read %s: %w", path, err)
	}
	return records, skipped, nil
}

// auditSegments lists rotated audit segments in chronological order (the
// timestamp is embedded in the name, so lexical order is chronological).
func auditSegments(logsDir string) []string {
	ext := filepath.Ext(audit.AuditFileName)
	base := strings.TrimSuffix(audit.AuditFileName, ext)
	matches, err := filepath.Glob(filepath.Join(logsDir, base+"-*"+ext))
	if err != nil {
		return nil
	}
	slices.Sort(matches)
	return matches
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
