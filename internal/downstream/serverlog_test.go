package downstream_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// readFrames decodes the trace log, skipping nothing: a malformed line in
// our OWN writer is a bug, not something to tolerate here.
func readFrames(t *testing.T, path string) []downstream.TraceFrame {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open trace log: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out []downstream.TraceFrame
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var fr downstream.TraceFrame
		if err := json.Unmarshal([]byte(line), &fr); err != nil {
			t.Fatalf("undecodable trace line %q: %v", line, err)
		}
		out = append(out, fr)
	}
	return out
}

// The trace log is off by default: enabling it must be an explicit act, or
// every gateway would silently write every frame of every server to disk.
func TestServerLogOffByDefaultRecordsNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log, err := downstream.OpenServerLog(dir, "fake", false)
	if err != nil {
		t.Fatalf("OpenServerLog: %v", err)
	}
	s := startServer(t, downstream.Deps{Trace: log}, fakemcp.Minimal("echo"))
	if _, err := s.Call(context.Background(), "echo", json.RawMessage(`"x"`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if frames := readFrames(t, log.Path()); len(frames) != 0 {
		t.Fatalf("trace disabled but %d frames were written", len(frames))
	}
}

// With tracing on, each call produces an out/in pair naming the method, and
// the file lands where the CLI looks for it.
func TestServerLogRecordsFramePairs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log, err := downstream.OpenServerLog(dir, "fake", true)
	if err != nil {
		t.Fatalf("OpenServerLog: %v", err)
	}
	if want := downstream.ServerLogPath(dir, "fake"); log.Path() != want {
		t.Fatalf("log path = %q, want %q (writer and CLI reader must agree)", log.Path(), want)
	}
	s := startServer(t, downstream.Deps{Trace: log}, fakemcp.Minimal("echo"))
	if _, err := s.Call(context.Background(), "echo", json.RawMessage(`"payload-marker"`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	frames := readFrames(t, log.Path())
	var out, in int
	var sawPayload bool
	for _, f := range frames {
		if f.Server != "fake" {
			t.Fatalf("frame carries server %q, want %q", f.Server, "fake")
		}
		switch f.Dir {
		case downstream.TraceOut:
			out++
		case downstream.TraceIn:
			in++
		default:
			t.Fatalf("unknown direction %q", f.Dir)
		}
		if f.Method == mcp.MethodToolsCall && strings.Contains(f.Payload, "payload-marker") {
			sawPayload = true
		}
	}
	if out == 0 || in == 0 {
		t.Fatalf("frames = %d out / %d in, want both directions recorded", out, in)
	}
	if !sawPayload {
		t.Fatalf("no tools/call frame carried the argument payload; frames: %+v", frames)
	}
}

// Toggling at runtime takes effect without reopening the file.
func TestServerLogToggle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log, err := downstream.OpenServerLog(dir, "fake", false)
	if err != nil {
		t.Fatalf("OpenServerLog: %v", err)
	}
	s := startServer(t, downstream.Deps{Trace: log}, fakemcp.Minimal("echo"))
	if _, err := s.Call(context.Background(), "echo", json.RawMessage(`"before"`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	log.SetEnabled(true)
	if _, err := s.Call(context.Background(), "echo", json.RawMessage(`"after"`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	blob := readAll(t, log.Path())
	if strings.Contains(blob, "before") {
		t.Error("a frame from the disabled window was recorded")
	}
	if !strings.Contains(blob, "after") {
		t.Error("a frame from the enabled window was NOT recorded")
	}
}

// A server id must not be able to escape the logs directory or collide with
// another server's file through path characters.
func TestServerLogNameSanitizesID(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"plain":        "server-plain.log",
		"with/slash":   "server-with_slash.log",
		"../../escape": "server-______escape.log",
		"":             "server-_.log",
		"..":           "server-__.log",
	}
	for id, want := range cases {
		if got := downstream.ServerLogName(id); got != want {
			t.Errorf("ServerLogName(%q) = %q, want %q", id, got, want)
		}
		full := downstream.ServerLogPath("/logs", id)
		if filepath.Dir(full) != "/logs" {
			t.Errorf("ServerLogPath(%q) = %q escaped the logs directory", id, full)
		}
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
