package cli

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/audit"
)

// writeAuditLog materializes an audit.jsonl with the given records.
func writeAuditLog(t *testing.T, dataDir string, lines ...string) string {
	t.Helper()
	logsDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logsDir, audit.AuditFileName)
	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func auditLine(ts, client, server, tool, decision string) string {
	return `{"ts":"` + ts + `","actor":"client","client":"` + client +
		`","session":"` + client + `:1","server":"` + server + `","tool":"` + tool +
		`","argsHash":"deadbeef","decision":"` + decision + `","durMs":12,"requestID":"req-1"}`
}

func TestAuditTailFilters(t *testing.T) {
	dir := setDataDir(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	writeAuditLog(t, dir,
		auditLine(now, "claude-code", "github", "list_prs", "allowed"),
		auditLine(now, "claude-code", "github", "create_pr", "held"),
		auditLine(now, "cursor", "linear", "search", "denied"),
		auditLine(now, "cursor", "linear", "create", "error"),
		"{ this line is torn",
	)

	var tail AuditTail
	decodeInto(t, mustRun(t, "", "audit", "tail", "--json"), &tail)
	if len(tail.Records) != 4 {
		t.Fatalf("records = %d, want 4: %+v", len(tail.Records), tail.Records)
	}
	if tail.Skipped != 1 {
		t.Errorf("skipped = %d, want the torn line counted (never silently dropped)", tail.Skipped)
	}

	var held AuditTail
	decodeInto(t, mustRun(t, "", "audit", "tail", "--held", "--json"), &held)
	if len(held.Records) != 1 || held.Records[0].Decision != string(audit.DecisionHeld) {
		t.Errorf("--held = %+v", held.Records)
	}

	var errs AuditTail
	decodeInto(t, mustRun(t, "", "audit", "tail", "--errors", "--json"), &errs)
	if len(errs.Records) != 2 {
		t.Errorf("--errors must cover denied AND error, got %+v", errs.Records)
	}

	var byServer AuditTail
	decodeInto(t, mustRun(t, "", "audit", "tail", "--server", "linear", "--json"), &byServer)
	if len(byServer.Records) != 2 {
		t.Errorf("--server = %+v", byServer.Records)
	}

	var limited AuditTail
	decodeInto(t, mustRun(t, "", "audit", "tail", "--limit", "1", "--json"), &limited)
	if len(limited.Records) != 1 || limited.Records[0].Tool != "create" {
		t.Errorf("--limit must keep the MOST RECENT records, got %+v", limited.Records)
	}
}

// TestAuditTailFollowNeedsDaemon pins the online/offline matrix: following
// an append-only file with no writer would hang pretending to work.
func TestAuditTailFollowNeedsDaemon(t *testing.T) {
	dir := setDataDir(t)
	writeAuditLog(t, dir)
	code, _, stderr := runCLI(t, "", "audit", "tail", "-f")
	if code != ExitDaemonDown {
		t.Fatalf("exit = %d, want %d", code, ExitDaemonDown)
	}
	if !strings.Contains(stderr, "daemon") {
		t.Errorf("stderr must name the daemon: %q", stderr)
	}
}

// TestAuditExportDefusesFormulaInjection is the CSV guard: a hostile tool
// name that a spreadsheet would evaluate must be neutralized in the export.
func TestAuditExportDefusesFormulaInjection(t *testing.T) {
	dir := setDataDir(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	writeAuditLog(t, dir,
		auditLine(now, "claude-code", "evil", `=cmd|' /C calc'!A0`, "allowed"),
		auditLine(now, "+attack", "evil", "@SUM(1+1)", "denied"),
	)
	out := filepath.Join(t.TempDir(), "audit.csv")
	var exp AuditExport
	decodeInto(t, mustRun(t, "", "audit", "export", "--csv", out, "--json"), &exp)
	if exp.Rows != 2 {
		t.Fatalf("rows = %d, want 2", exp.Rows)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("export is not valid CSV: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("csv rows = %d, want header + 2", len(rows))
	}
	for i, row := range rows[1:] {
		for j, cell := range row {
			if cell == "" {
				continue
			}
			switch cell[0] {
			case '=', '+', '-', '@', '\t', '\r':
				t.Errorf("row %d col %d is still formula-evaluable: %q", i, j, cell)
			}
		}
	}
	// The neutralized cells keep their content behind the quote prefix, so
	// the export is still faithful.
	joined := strings.Join(rows[1], ",") + strings.Join(rows[2], ",")
	if !strings.Contains(joined, "'=cmd") || !strings.Contains(joined, "'@SUM(1+1)") {
		t.Errorf("sanitization dropped content instead of prefixing it:\n%v", rows)
	}
}

func TestAuditExportRequiresDestination(t *testing.T) {
	setDataDir(t)
	if code, _, _ := runCLI(t, "", "audit", "export"); code != ExitUsage {
		t.Errorf("export without --csv exit = %d, want %d", code, ExitUsage)
	}
}

// TestAuditTailMissingLogIsEmpty: nothing logged yet is a normal state, not
// an error.
func TestAuditTailMissingLogIsEmpty(t *testing.T) {
	setDataDir(t)
	var tail AuditTail
	decodeInto(t, mustRun(t, "", "audit", "tail", "--json"), &tail)
	if len(tail.Records) != 0 {
		t.Errorf("records = %+v, want empty", tail.Records)
	}
}

func TestActivityAggregation(t *testing.T) {
	dir := setDataDir(t)
	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	body := strings.Join([]string{
		`{"ts":"` + now + `","client":"claude-code","server":"github","mode":"lazy-discovery","baselineTokens":1000,"actualTokens":200,"savedTokens":800}`,
		`{"ts":"` + now + `","client":"claude-code","server":"github","mode":"shaping","baselineTokens":500,"actualTokens":250,"savedTokens":250}`,
		`{"ts":"` + now + `","client":"cursor","server":"linear","mode":"lazy-discovery","baselineTokens":500,"actualTokens":100,"savedTokens":400}`,
		"not json",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(logsDir, audit.SavingsFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var report ActivityReport
	decodeInto(t, mustRun(t, "", "activity", "--json"), &report)
	if report.Total.Calls != 3 || report.Total.Saved != 1450 || report.Total.Baseline != 2000 {
		t.Fatalf("total = %+v", report.Total)
	}
	if report.Skipped != 1 {
		t.Errorf("skipped = %d, want the undecodable line counted", report.Skipped)
	}
	if len(report.ByMode) != 2 || len(report.Servers) != 2 {
		t.Errorf("by_mode = %+v, by_server = %+v", report.ByMode, report.Servers)
	}
	// The search-trace projection keeps only the discovery mechanisms.
	if len(report.SearchTrace) != 1 || report.SearchTrace[0].Key != "lazy-discovery" {
		t.Errorf("search_trace = %+v", report.SearchTrace)
	}
	if report.Total.SavedPct < 72 || report.Total.SavedPct > 73 {
		t.Errorf("saved_pct = %v, want ~72.5", report.Total.SavedPct)
	}
}

// TestActivityEmptyIsZeroNotNaN: a zero baseline must not produce NaN,
// which would poison the JSON encode and read as "saved everything".
func TestActivityEmptyIsZeroNotNaN(t *testing.T) {
	setDataDir(t)
	var report ActivityReport
	decodeInto(t, mustRun(t, "", "activity", "--json"), &report)
	if report.Total.Calls != 0 || report.Total.SavedPct != 0 {
		t.Errorf("empty report = %+v", report.Total)
	}
}
