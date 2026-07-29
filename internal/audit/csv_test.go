package audit

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestSanitizeCSVCell(t *testing.T) {
	tests := []struct{ in, want string }{
		{`=1+2`, `'=1+2`},
		{`+cmd`, `'+cmd`},
		{`-2+3`, `'-2+3`},
		{`@SUM(A1)`, `'@SUM(A1)`},
		{"\tpayload", "'\tpayload"},
		{"\rpayload", "'\rpayload"},
		{`plain`, `plain`},
		{``, ``},
		{`a=b`, `a=b`}, // only a leading formula char is dangerous
	}
	for _, tt := range tests {
		if got := SanitizeCSVCell(tt.in); got != tt.want {
			t.Errorf("SanitizeCSVCell(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestExportCSVGolden(t *testing.T) {
	r := Record{
		TS:        time.Date(2026, 7, 26, 12, 0, 0, 123456789, time.UTC),
		Actor:     "client",
		Client:    "claude-code",
		Session:   "sess-1",
		Server:    "github",
		Tool:      "=HYPERLINK(evil)", // hostile downstream tool name
		ArgsHash:  "abc",
		Decision:  DecisionDenied,
		DurMs:     -1, // negative numbers get the cosmetic quote too
		RequestID: "req-1",
	}
	var buf bytes.Buffer
	if err := ExportCSV(&buf, AuditCSVHeader, [][]string{r.CSVRow()}); err != nil {
		t.Fatal(err)
	}
	want := "ts,actor,client,session,server,tool,argsHash,decision,durMs,requestID\n" +
		"2026-07-26T12:00:00.123456789Z,client,claude-code,sess-1,github," +
		"'=HYPERLINK(evil),abc,denied,'-1,req-1\n"
	if buf.String() != want {
		t.Errorf("csv golden mismatch:\n got %q\nwant %q", buf.String(), want)
	}
	if strings.Contains(buf.String(), "\n=") || strings.Contains(buf.String(), ",=") {
		t.Error("unsanitized formula cell leaked into CSV")
	}
}
