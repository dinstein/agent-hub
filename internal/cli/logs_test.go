package cli

import (
	"encoding/json"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/proclog"

	"github.com/dinstein/agent-hub/internal/daemon"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/logx"
)

// seedLogs writes the fixture this file's tests read: one daemon log and two
// gateway logs, interleaved in time so that a merged read has to actually
// merge rather than concatenate.
//
// Timestamps are explicit and out of file order on purpose. A reader that
// concatenated files would still print every line, and would still look
// correct on a fixture whose files happen to be chronologically disjoint.
func seedLogs(t *testing.T, dataDir string) {
	t.Helper()
	logsDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
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
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(logsDir, daemon.LogFileName),
		line("2020-01-02T10:00:00Z", "INFO", "daemon started")+
			line("2020-01-02T10:00:04Z", "INFO", "listener bound"))
	write(gateway.LogPath(logsDir, "claude-code"),
		line("2020-01-02T10:00:01Z", "INFO", "downstream connected",
			logx.FieldClient, "claude-code", logx.FieldServer, "github")+
			line("2020-01-02T10:00:03Z", "WARN", "circuit opened",
				logx.FieldClient, "claude-code", logx.FieldServer, "github"))
	write(gateway.LogPath(logsDir, "cursor"),
		line("2020-01-02T10:00:02Z", "ERROR", "respawn failed",
			logx.FieldClient, "cursor", logx.FieldServer, "linear"))
}

// msgsOf returns the message column of each rendered line, which is enough to
// identify a record and stays readable in a failure message.
func msgsOf(out string) []string {
	var msgs []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		// "<ts> <level> <origin> <msg> | k=v ..."
		fields := strings.SplitN(l, " | ", 2)
		parts := strings.Fields(fields[0])
		if len(parts) < 4 {
			continue
		}
		msgs = append(msgs, strings.Join(parts[3:], " "))
	}
	return msgs
}

// The whole point of the command: seven files' worth of one story, in the
// order it happened, not in the order the files were opened.
func TestLogsMergesProcessesInTimeOrder(t *testing.T) {
	dir := setDataDir(t)
	seedLogs(t, dir)

	code, out, errOut := runCLI(t, "", "logs")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	want := []string{
		"daemon started",       // 10:00:00 daemon.log
		"downstream connected", // 10:00:01 gateway-claude-code.log
		"respawn failed",       // 10:00:02 gateway-cursor.log
		"circuit opened",       // 10:00:03 gateway-claude-code.log
		"listener bound",       // 10:00:04 daemon.log
	}
	if got := msgsOf(out); !equalStringSlices(got, want) {
		t.Fatalf("merged order = %v, want %v\n%s", got, want, out)
	}
	// The origin column is the one piece of provenance the record does not
	// carry: without it a merged stream cannot say which half spoke.
	if !strings.Contains(out, "gateway") || !strings.Contains(out, "daemon ") {
		t.Fatalf("rendered lines do not name their origin:\n%s", out)
	}
}

// --server is the flag that makes this useful when one downstream misbehaves,
// and it must reach across processes.
func TestLogsFiltersByServerAcrossProcesses(t *testing.T) {
	dir := setDataDir(t)
	seedLogs(t, dir)

	code, out, errOut := runCLI(t, "", "logs", "--server", "github")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	want := []string{"downstream connected", "circuit opened"}
	if got := msgsOf(out); !equalStringSlices(got, want) {
		t.Fatalf("--server github = %v, want %v", got, want)
	}
	// Fail-closed, matching logFilter.admit: the daemon lines carry no
	// server field, so a server-filtered view must not include them. A
	// filtered view that smuggles in records it cannot classify is worse
	// than one that is too narrow, because the reader cannot tell.
	if strings.Contains(out, "daemon started") {
		t.Fatal("a record with no server field survived --server")
	}
}

// --client narrows to one gateway. The file name is only a superset (fsSafe
// is many-to-one), so the exactness has to come from the record's own field.
func TestLogsFiltersByClient(t *testing.T) {
	dir := setDataDir(t)
	seedLogs(t, dir)

	code, out, errOut := runCLI(t, "", "logs", "--client", "cursor")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := msgsOf(out); !equalStringSlices(got, []string{"respawn failed"}) {
		t.Fatalf("--client cursor = %v", got)
	}
}

// --source is how a reader gets back the single-process view on purpose,
// rather than by accident of which files happen to exist.
func TestLogsSourceSelectsOneKind(t *testing.T) {
	dir := setDataDir(t)
	seedLogs(t, dir)

	for _, tc := range []struct {
		source string
		want   []string
	}{
		{"daemon", []string{"daemon started", "listener bound"}},
		{"gateway", []string{"downstream connected", "respawn failed", "circuit opened"}},
	} {
		code, out, errOut := runCLI(t, "", "logs", "--source", tc.source)
		if code != 0 {
			t.Fatalf("--source %s: exit %d: %s", tc.source, code, errOut)
		}
		if got := msgsOf(out); !equalStringSlices(got, tc.want) {
			t.Errorf("--source %s = %v, want %v", tc.source, got, tc.want)
		}
	}

	code, _, errOut := runCLI(t, "", "logs", "--source", "nonsense")
	if code == 0 {
		t.Fatal("an unknown --source succeeded")
	}
	if !strings.Contains(errOut, "daemon") || !strings.Contains(errOut, "gateway") {
		t.Errorf("the rejection does not name the valid values: %s", errOut)
	}
}

// --level and --since share newLogFilter with `daemon logs`, so this checks
// the wiring rather than the predicate.
func TestLogsAppliesLevelAndSince(t *testing.T) {
	dir := setDataDir(t)
	seedLogs(t, dir)

	code, out, errOut := runCLI(t, "", "logs", "--level", "warn")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := msgsOf(out); !equalStringSlices(got, []string{"respawn failed", "circuit opened"}) {
		t.Fatalf("--level warn = %v", got)
	}

	// The fixture is dated 2020, so anything relative to now excludes all of
	// it, which is the assertion that --since is wired at all.
	if _, out, _ := runCLI(t, "", "logs", "--since", "1s"); strings.TrimSpace(out) != "" {
		t.Fatalf("--since 1s returned records from a fixed-date fixture:\n%s", out)
	}
}

// --json is the raw line, not a re-encode: the file is already the machine
// readable form and round-tripping it could only lose fields.
func TestLogsJSONEmitsTheRawLines(t *testing.T) {
	dir := setDataDir(t)
	seedLogs(t, dir)

	code, out, errOut := runCLI(t, "", "--json", "logs", "--client", "cursor")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	line := strings.TrimSpace(out)
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("--json line is not JSON: %v\n%s", err, line)
	}
	if rec["msg"] != "respawn failed" || rec[logx.FieldServer] != "linear" {
		t.Fatalf("raw line lost fields: %+v", rec)
	}
}

// --limit keeps the LAST n after the merge, not the last n of some file.
func TestLogsLimitKeepsTheNewest(t *testing.T) {
	dir := setDataDir(t)
	seedLogs(t, dir)

	code, out, errOut := runCLI(t, "", "logs", "--limit", "2")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := msgsOf(out); !equalStringSlices(got, []string{"circuit opened", "listener bound"}) {
		t.Fatalf("--limit 2 = %v", got)
	}
}

// An empty logs directory is a normal state — nothing has run yet — and the
// error has to say which of the two writers would have produced a file.
func TestLogsWithNothingWrittenExplainsItself(t *testing.T) {
	setDataDir(t)
	code, _, errOut := runCLI(t, "", "logs")
	if code == 0 {
		t.Fatal("logs succeeded with no log files at all")
	}
	if !strings.Contains(errOut, "daemon.log") || !strings.Contains(errOut, "connect") {
		t.Errorf("the error does not say what would write a log: %s", errOut)
	}
}

// Unparseable lines are dropped rather than shown. `server logs` counts them
// instead; the difference is the merge — a line with no timestamp has no
// truthful position in a time-ordered stream.
func TestLogsDropsLinesItCannotPlace(t *testing.T) {
	dir := setDataDir(t)
	seedLogs(t, dir)
	path := filepath.Join(dir, "logs", daemon.LogFileName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("this is not json at all\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	code, out, errOut := runCLI(t, "", "logs")
	if code != 0 {
		t.Fatalf("a torn line made the whole read fail: exit %d: %s", code, errOut)
	}
	if strings.Contains(out, "not json at all") {
		t.Fatalf("an unplaceable line was rendered into a time-ordered stream:\n%s", out)
	}
	if len(msgsOf(out)) != 5 {
		t.Fatalf("the torn line cost other records: %v", msgsOf(out))
	}
}

// A rotated file shrinks. The reader must restart at the top of the new
// segment rather than seeking past its end and going silent forever.
func TestLogsFollowRestartsOnRotation(t *testing.T) {
	dir := t.TempDir()
	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logsDir, daemon.LogFileName)
	long := `{"time":"2020-01-02T10:00:00Z","level":"INFO","msg":"before rotation"}` + "\n"
	if err := os.WriteFile(path, []byte(strings.Repeat(long, 4)), 0o600); err != nil {
		t.Fatal(err)
	}

	files := []proclog.File{{Path: path, Origin: proclog.OriginDaemon}}
	offsets := map[string]int64{}
	if got, err := readLogBatch(files, offsets, proclog.Query{}); err != nil || len(got) != 4 {
		t.Fatalf("first batch = %d records, err %v", len(got), err)
	}

	// Rotation: the active path is replaced by a shorter, brand new segment.
	if err := os.WriteFile(path, []byte(
		`{"time":"2020-01-02T11:00:00Z","level":"INFO","msg":"after rotation"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readLogBatch(files, offsets, proclog.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Msg != "after rotation" {
		t.Fatalf("after rotation = %+v, want the one record of the new segment", got)
	}
}

func equalStringSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Rotation moves history aside into a segment, and the gateway glob matches
// those too. So the segment must be read — but exactly once: as part of the
// stream it belongs to, never also as a stream of its own.
func TestLogsReadsRotatedSegments(t *testing.T) {
	dir := setDataDir(t)
	seedLogs(t, dir)
	logsDir := filepath.Join(dir, "logs")
	rotated := filepath.Join(logsDir, "gateway-claude-code-20200102T090000.000000000Z.p7.log")
	if err := os.WriteFile(rotated, []byte(
		`{"time":"2020-01-02T09:00:00Z","level":"INFO","msg":"before rotation",`+
			`"client":"claude-code","server":"github"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI(t, "", "logs")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	msgs := msgsOf(out)
	if len(msgs) == 0 || msgs[0] != "before rotation" {
		t.Fatalf("rotated history missing or out of order: %v\n%s", msgs, out)
	}
	// Counted once: the glob matches the segment too, and reading it both as
	// its own stream and as part of gateway-claude-code.log would double it.
	seen := 0
	for _, m := range msgs {
		if m == "before rotation" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("the rotated record was read %d times, want 1:\n%s", seen, out)
	}
}

// --client narrows by file name before it narrows by field, so it has its own
// path into the file list and its own way to miss the segments.
func TestLogsByClientReadsRotatedSegments(t *testing.T) {
	dir := setDataDir(t)
	seedLogs(t, dir)
	rotated := filepath.Join(dir, "logs", "gateway-cursor-20200102T090000.000000000Z.p7.log")
	if err := os.WriteFile(rotated, []byte(
		`{"time":"2020-01-02T09:00:00Z","level":"INFO","msg":"cursor history","client":"cursor"}`+"\n"),
		0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI(t, "", "logs", "--client", "cursor")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	if !strings.Contains(out, "cursor history") {
		t.Fatalf("--client read only the active file:\n%s", out)
	}
}
