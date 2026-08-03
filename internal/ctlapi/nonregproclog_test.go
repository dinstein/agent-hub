package ctlapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/dinstein/agent-hub/api"
)

// seedProcLogs writes a daemon log and two gateway logs, interleaved in time
// so a merged read has to actually merge rather than concatenate.
func seedProcLogs(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := func(ts, level, msg string, kv ...string) string {
		rec := map[string]any{"time": ts, "level": level, "msg": msg}
		for i := 0; i+1 < len(kv); i += 2 {
			rec[kv[i]] = kv[i+1]
		}
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("daemon.log",
		line("2020-01-02T10:00:00Z", "INFO", "daemon ready")+
			line("2020-01-02T10:00:04Z", "INFO", "data plane listening"))
	write("gateway-claude-code.log",
		line("2020-01-02T10:00:01Z", "INFO", "downstream connected", "client", "claude-code", "server", "github")+
			line("2020-01-02T10:00:03Z", "WARN", "circuit opened", "client", "claude-code", "server", "github"))
	write("gateway-cursor.log",
		line("2020-01-02T10:00:02Z", "ERROR", "respawn failed", "client", "cursor", "server", "linear"))
}

// The point of the endpoint: seven files' worth of one story, newest first,
// with the origin the record itself does not carry.
func TestProcLogsMergeNewestFirstAndCarryTheirOrigin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	seedProcLogs(t, dir)
	client, _ := startServer(t, func(o *Options) { o.NonRegistry.LogsDir = dir })

	page, err := client.Logs.List(t.Context(), api.ProcLogFilter{})
	if err != nil {
		t.Fatal(err)
	}

	if len(page.Records) != 5 {
		t.Fatalf("got %d records, want 5: %+v", len(page.Records), page.Records)
	}
	want := []string{
		"data plane listening", "circuit opened", "respawn failed",
		"downstream connected", "daemon ready",
	}
	for i, msg := range want {
		if page.Records[i].Message != msg {
			t.Fatalf("record %d = %q, want %q (newest first)", i, page.Records[i].Message, msg)
		}
	}
	if page.Records[0].Origin != "daemon" || page.Records[1].Origin != "gateway" {
		t.Errorf("origins lost: %+v", page.Records[:2])
	}
}

// A page is a prefix and the cursor names its last row, so the two pages
// together are the whole list with nothing repeated and nothing skipped.
func TestProcLogsPageWithACursor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	seedProcLogs(t, dir)
	client, _ := startServer(t, func(o *Options) { o.NonRegistry.LogsDir = dir })

	first, err := client.Logs.List(t.Context(), api.ProcLogFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Records) != 2 || first.NextCursor == "" || first.Total != 5 {
		t.Fatalf("first page = %+v", first)
	}
	second, err := client.Logs.List(t.Context(), api.ProcLogFilter{Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Records) != 2 {
		t.Fatalf("second page = %+v", second)
	}
	for _, a := range first.Records {
		for _, b := range second.Records {
			if a.Message == b.Message {
				t.Fatalf("%q appeared on both pages", a.Message)
			}
		}
	}
	// The last page ends the walk by handing back no cursor, which is how a
	// client stops rather than looping on an empty page forever.
	last, err := client.Logs.List(t.Context(), api.ProcLogFilter{Limit: 10, Cursor: second.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Records) != 1 || last.NextCursor != "" {
		t.Fatalf("last page = %+v", last)
	}
}

func TestProcLogsFilterByLevelClientAndSource(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	seedProcLogs(t, dir)
	client, _ := startServer(t, func(o *Options) { o.NonRegistry.LogsDir = dir })

	warn, err := client.Logs.List(t.Context(), api.ProcLogFilter{Level: "warn"})
	if err != nil {
		t.Fatal(err)
	}
	if len(warn.Records) != 2 {
		t.Fatalf("level=warn = %d records, want the warn and the error", len(warn.Records))
	}
	byClient, err := client.Logs.List(t.Context(), api.ProcLogFilter{Client: "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	// A daemon record carries no client, so a client filter must not admit
	// one: a filtered view smuggling in records it cannot classify is worse
	// than an empty one.
	if len(byClient.Records) != 1 || byClient.Records[0].Client != "cursor" {
		t.Fatalf("client=cursor = %+v", byClient.Records)
	}
	daemonOnly, err := client.Logs.List(t.Context(), api.ProcLogFilter{Source: "daemon"})
	if err != nil {
		t.Fatal(err)
	}
	if len(daemonOnly.Records) != 2 {
		t.Fatalf("source=daemon = %d records, want 2", len(daemonOnly.Records))
	}
}

// A typo in a closed set is answered with an error, never with an empty page:
// "no records" is the same response as "nothing has been logged".
func TestProcLogsRejectAnUnknownSource(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	seedProcLogs(t, dir)
	_, env := startServer(t, func(o *Options) { o.NonRegistry.LogsDir = dir })

	status, body := nrDo(t, env.sock, http.MethodGet, "/v1/logs?source=weird", nil)

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", status, body)
	}
}

// Extra slog attributes reach the caller. A UI that showed only the named
// columns would drop whatever the next log line adds.
func TestProcLogsKeepUnnamedFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"time":"2020-01-02T10:00:00Z","level":"WARN","msg":"circuit opened",` +
		`"client":"claude-code","server":"github","pid":42,"failures":3,"cooldown":"20s"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gateway-claude-code.log"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	client, _ := startServer(t, func(o *Options) { o.NonRegistry.LogsDir = dir })

	page, err := client.Logs.List(t.Context(), api.ProcLogFilter{})
	if err != nil {
		t.Fatal(err)
	}

	if len(page.Records) != 1 {
		t.Fatalf("records = %+v", page.Records)
	}
	rec := page.Records[0]
	if rec.PID != 42 || rec.Server != "github" {
		t.Errorf("join keys lost: %+v", rec)
	}
	if rec.Fields["failures"] != "3" || rec.Fields["cooldown"] != "20s" {
		t.Errorf("unnamed fields lost: %+v", rec.Fields)
	}
	// And the named ones are not repeated inside them.
	for _, k := range []string{"time", "level", "msg", "client", "server", "pid"} {
		if _, dup := rec.Fields[k]; dup {
			t.Errorf("%q appears both as a column and in fields", k)
		}
	}
}
