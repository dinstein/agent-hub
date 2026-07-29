package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
)

// writeTrace lays down a per-server trace log the way internal/downstream
// writes it, resolving the path through the WRITER's own function so a
// rename there breaks this test instead of silently breaking the command.
func writeTrace(t *testing.T, dataDir, serverID string, frames []downstream.TraceFrame) string {
	t.Helper()
	logsDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := downstream.ServerLogPath(logsDir, serverID)
	var b strings.Builder
	for _, f := range frames {
		line, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func sampleFrames() []downstream.TraceFrame {
	ts := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	return []downstream.TraceFrame{
		{TS: ts, Server: "fake", Dir: downstream.TraceOut, Method: "tools/call",
			Bytes: 12, Payload: `{"name":"echo"}`},
		{TS: ts.Add(time.Millisecond), Server: "fake", Dir: downstream.TraceIn,
			Method: "tools/call", Bytes: 20, Payload: `{"content":[]}`, DurMs: 1},
	}
}

func TestServerLogsRendersFrames(t *testing.T) {
	dir := setDataDir(t)
	writeTrace(t, dir, "fake", sampleFrames())

	code, out, stderr := runCLI(t, "", "server", "logs", "fake", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	env := decodeEnvelope(t, out)
	if !env.OK {
		t.Fatalf("envelope not ok: %s", out)
	}
	var logs ServerLogs
	if err := json.Unmarshal(env.Data, &logs); err != nil {
		t.Fatalf("data is not a ServerLogs: %v\n%s", err, env.Data)
	}
	if logs.Server != "fake" {
		t.Errorf("server = %q, want fake", logs.Server)
	}
	if len(logs.Frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(logs.Frames))
	}
	if logs.Frames[0].Dir != downstream.TraceOut || logs.Frames[1].Dir != downstream.TraceIn {
		t.Errorf("directions = %q/%q, want out/in in order",
			logs.Frames[0].Dir, logs.Frames[1].Dir)
	}
	if logs.Frames[0].Method != "tools/call" {
		t.Errorf("method = %q", logs.Frames[0].Method)
	}
	// Human mode renders the same data.
	code, human, _ := runCLI(t, "", "server", "logs", "fake")
	if code != ExitOK {
		t.Fatalf("human exit = %d", code)
	}
	if !strings.Contains(human, "tools/call") {
		t.Errorf("human output does not mention the method:\n%s", human)
	}
}

// --limit keeps the LAST n frames: a trace is read from its tail.
func TestServerLogsLimitKeepsTail(t *testing.T) {
	dir := setDataDir(t)
	ts := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	var frames []downstream.TraceFrame
	for i := 0; i < 10; i++ {
		frames = append(frames, downstream.TraceFrame{
			TS: ts.Add(time.Duration(i) * time.Second), Server: "fake",
			Dir: downstream.TraceOut, Method: "m", Payload: strings.Repeat("x", i+1),
		})
	}
	writeTrace(t, dir, "fake", frames)

	code, out, _ := runCLI(t, "", "server", "logs", "fake", "--limit", "3", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var logs ServerLogs
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &logs); err != nil {
		t.Fatal(err)
	}
	if len(logs.Frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(logs.Frames))
	}
	if logs.Frames[2].Payload != strings.Repeat("x", 10) {
		t.Errorf("last frame payload = %q, want the newest frame", logs.Frames[2].Payload)
	}
}

// A missing log is not an error: tracing is off by default, and the command
// must SAY that instead of failing or printing nothing.
func TestServerLogsMissingFileExplainsItself(t *testing.T) {
	setDataDir(t)
	code, out, stderr := runCLI(t, "", "server", "logs", "never-traced", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	var logs ServerLogs
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &logs); err != nil {
		t.Fatal(err)
	}
	if len(logs.Frames) != 0 {
		t.Fatalf("frames = %d, want 0", len(logs.Frames))
	}
	if !strings.Contains(logs.Note, "tracing is off by default") {
		t.Errorf("note = %q, want the tracing-is-off explanation", logs.Note)
	}
	if logs.Path == "" {
		t.Error("note carries no path; the operator cannot tell where to look")
	}
}

// A torn last line (a writer killed mid-append) must not make the whole
// trace unreadable — it is counted and skipped, like the audit reader does.
func TestServerLogsCountsUndecodableLines(t *testing.T) {
	dir := setDataDir(t)
	path := writeTrace(t, dir, "fake", sampleFrames())
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"ts\":\"2026-07-26T12:00\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	code, out, _ := runCLI(t, "", "server", "logs", "fake", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var logs ServerLogs
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &logs); err != nil {
		t.Fatal(err)
	}
	if len(logs.Frames) != 2 {
		t.Fatalf("frames = %d, want the 2 intact frames", len(logs.Frames))
	}
	if logs.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", logs.Skipped)
	}
}

// The command reads exactly the file internal/downstream writes.
func TestServerLogsPathMatchesWriter(t *testing.T) {
	dir := setDataDir(t)
	path := writeTrace(t, dir, "with/slash", sampleFrames())

	code, out, stderr := runCLI(t, "", "server", "logs", "with/slash", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	var logs ServerLogs
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &logs); err != nil {
		t.Fatal(err)
	}
	if logs.Path != path {
		t.Fatalf("path = %q, want %q", logs.Path, path)
	}
	if len(logs.Frames) != 2 {
		t.Fatalf("frames = %d, want 2 (reader and writer disagree on the file)", len(logs.Frames))
	}
}
