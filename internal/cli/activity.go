package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/savings"
)

// `activity` is the savings/search-trace projection of savings.jsonl: how
// many tokens lazy discovery, grouping and result shaping actually saved,
// grouped by mechanism and by server.
//
// It is a pure read of an append-only file, so it works offline — the
// numbers describe what already happened, and a daemon being up or down
// cannot change history.

// ActivityBucket is one aggregation row (by mode or by server).
type ActivityBucket struct {
	Key      string `json:"key"`
	Calls    int64  `json:"calls"`
	Baseline int64  `json:"baseline_tokens"`
	Actual   int64  `json:"actual_tokens"`
	Saved    int64  `json:"saved_tokens"`
	// SavedPct is the saving as a percentage of baseline, rounded to one
	// decimal. It is computed here (not by the consumer) so the human table
	// and the JSON agree by construction.
	SavedPct float64 `json:"saved_pct"`
}

// ActivityReport is the `activity` result.
type ActivityReport struct {
	Since   string           `json:"since,omitempty"`
	Total   ActivityBucket   `json:"total"`
	ByMode  []ActivityBucket `json:"by_mode"`
	Servers []ActivityBucket `json:"by_server"`
	// SearchTrace is the search-mechanism slice of the same stream: the
	// discovery modes that answered instead of a full tools/list.
	SearchTrace []ActivityBucket `json:"search_trace"`
	Source      string           `json:"source"`
	Skipped     int              `json:"skipped,omitempty"`
}

// Human renders the aggregation.
// maxJSONLLine bounds one JSONL line while reading. The writer bounds what
// it appends, so a longer line means a foreign or corrupt file.
const maxJSONLLine = 1 << 20

func (r ActivityReport) Human(w io.Writer) error {
	if r.Total.Calls == 0 {
		_, err := fmt.Fprintln(w, "no activity recorded yet")
		return err
	}
	if _, err := fmt.Fprintf(w, "total: %d record(s), %d tokens saved of %d baseline (%.1f%%)\n",
		r.Total.Calls, r.Total.Saved, r.Total.Baseline, r.Total.SavedPct); err != nil {
		return err
	}
	if err := writeActivityTable(w, "by mechanism", "MODE", r.ByMode); err != nil {
		return err
	}
	if err := writeActivityTable(w, "by server", "SERVER", r.Servers); err != nil {
		return err
	}
	if err := writeActivityTable(w, "search trace", "MODE", r.SearchTrace); err != nil {
		return err
	}
	if r.Skipped > 0 {
		_, err := fmt.Fprintf(w, "%d undecodable line(s) skipped\n", r.Skipped)
		return err
	}
	return nil
}

func writeActivityTable(w io.Writer, title, keyHeader string, rows []ActivityBucket) error {
	if len(rows) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s:\n", title); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "%s\tRECORDS\tBASELINE\tACTUAL\tSAVED\tSAVED%%\n", keyHeader)
	for _, b := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%.1f\n",
			b.Key, b.Calls, b.Baseline, b.Actual, b.Saved, b.SavedPct)
	}
	return tw.Flush()
}

func (a *App) newActivityCmd() *cobra.Command {
	var since time.Duration
	cmd := &cobra.Command{
		Use:   "activity [--since 24h]",
		Short: "Token-savings and search-trace statistics (savings.jsonl projection)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logsDir, err := a.resolver.LogsDir()
			if err != nil {
				return err
			}
			path := filepath.Join(logsDir, savings.FileName)
			var cutoff time.Time
			if since > 0 {
				cutoff = time.Now().Add(-since)
			}
			report, err := readActivity(path, cutoff)
			if err != nil {
				return err
			}
			if since > 0 {
				report.Since = cutoff.UTC().Format(time.RFC3339)
			}
			return a.printer().Emit(report)
		},
	}
	cmd.Flags().DurationVar(&since, "since", 0, "only records newer than this age (e.g. 24h)")
	return cmd
}

// searchModes are the mechanisms that answered a discovery request instead
// of shipping a full tools/list; they form the search-trace projection.
var searchModes = map[string]bool{
	"lazy-discovery": true,
	"lazy":           true,
	"grouped":        true,
	"search":         true,
	"search_tools":   true,
}

// readActivity aggregates savings.jsonl. A missing file is an empty report,
// not an error: nothing has been recorded yet is a normal state.
func readActivity(path string, cutoff time.Time) (ActivityReport, error) {
	report := ActivityReport{Source: path}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return report, nil
		}
		return ActivityReport{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	byMode := map[string]*ActivityBucket{}
	byServer := map[string]*ActivityBucket{}
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 64<<10), maxJSONLLine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec savings.Record
		if json.Unmarshal([]byte(line), &rec) != nil {
			report.Skipped++
			continue
		}
		if !cutoff.IsZero() && rec.TS.Before(cutoff) {
			continue
		}
		add(&report.Total, rec)
		mode := rec.Mode
		if mode == "" {
			mode = "(unset)"
		}
		if byMode[mode] == nil {
			byMode[mode] = &ActivityBucket{Key: mode}
		}
		add(byMode[mode], rec)
		if rec.Server != "" {
			if byServer[rec.Server] == nil {
				byServer[rec.Server] = &ActivityBucket{Key: rec.Server}
			}
			add(byServer[rec.Server], rec)
		}
	}
	if err := sc.Err(); err != nil {
		return ActivityReport{}, fmt.Errorf("read %s: %w", path, err)
	}

	report.Total.Key = "total"
	finish(&report.Total)
	for _, k := range sortedKeys(byMode) {
		b := *byMode[k]
		finish(&b)
		report.ByMode = append(report.ByMode, b)
		if searchModes[k] {
			report.SearchTrace = append(report.SearchTrace, b)
		}
	}
	for _, k := range sortedKeys(byServer) {
		b := *byServer[k]
		finish(&b)
		report.Servers = append(report.Servers, b)
	}
	return report, nil
}

func add(b *ActivityBucket, rec savings.Record) {
	b.Calls++
	b.Baseline += rec.BaselineTokens
	b.Actual += rec.ActualTokens
	b.Saved += rec.SavedTokens
}

// finish computes the percentage. A zero baseline yields 0% rather than a
// NaN or an infinity: those would poison a JSON encode and read as "saved
// everything" in a table, which is the opposite of what no data means.
func finish(b *ActivityBucket) {
	if b.Baseline <= 0 {
		b.SavedPct = 0
		return
	}
	b.SavedPct = float64(b.Saved) * 100 / float64(b.Baseline)
}
