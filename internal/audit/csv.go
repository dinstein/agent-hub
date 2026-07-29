package audit

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
)

// CSV export helpers with formula-injection protection (inherited from
// toolport audit.rs; docs/modules/controlplane.md `audit export --csv`).

// AuditCSVHeader is the column set for exporting audit Records, matching
// the frozen JSON field order.
var AuditCSVHeader = []string{
	"ts", "actor", "client", "session", "server", "tool",
	"argsHash", "decision", "durMs", "requestID",
}

// CSVRow renders the record in AuditCSVHeader column order. Cells are NOT
// yet sanitized — ExportCSV does that uniformly.
func (r Record) CSVRow() []string {
	return []string{
		r.TS.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
		r.Actor, r.Client, r.Session, r.Server, r.Tool,
		r.ArgsHash, string(r.Decision),
		strconv.FormatInt(r.DurMs, 10),
		r.RequestID,
	}
}

// SanitizeCSVCell defuses spreadsheet formula injection: a cell starting
// with '=', '+', '-', '@' (or a leading tab / carriage return, which
// spreadsheets strip before re-interpreting the remainder) is prefixed
// with a single quote so spreadsheet applications treat it as text.
//
// Failure direction: FAIL CLOSED — when a cell might be interpreted as a
// formula it is always prefixed. The cost of a spurious quote is cosmetic
// (e.g. negative numbers render as '-5); the cost of a missed one is code
// execution in the user's spreadsheet.
func SanitizeCSVCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// ExportCSV writes header (if non-empty) and rows as CSV, passing every
// cell — header cells included — through SanitizeCSVCell.
func ExportCSV(w io.Writer, header []string, rows [][]string) error {
	cw := csv.NewWriter(w)
	if len(header) > 0 {
		if err := cw.Write(sanitizeRow(header)); err != nil {
			return fmt.Errorf("audit: export csv: %w", err)
		}
	}
	for _, row := range rows {
		if err := cw.Write(sanitizeRow(row)); err != nil {
			return fmt.Errorf("audit: export csv: %w", err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("audit: export csv: %w", err)
	}
	return nil
}

func sanitizeRow(row []string) []string {
	out := make([]string, len(row))
	for i, c := range row {
		out[i] = SanitizeCSVCell(c)
	}
	return out
}
