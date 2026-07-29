package logx_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/logx"
)

func TestSetupTextOnly(t *testing.T) {
	var buf bytes.Buffer
	logger, closeFn, err := logx.Setup(logx.Config{TextEnabled: true, TextWriter: &buf})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer func() {
		if err := closeFn(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()

	logger.Info("hello", logx.Server("github"))
	out := buf.String()
	if !strings.Contains(out, "msg=hello") || !strings.Contains(out, "server=github") {
		t.Fatalf("unexpected text output: %q", out)
	}
}

func TestSetupJSONFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.jsonl")
	logger, closeFn, err := logx.Setup(logx.Config{JSONPath: path})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	logger.Info("boot", logx.Rev(3))
	if err := closeFn(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &rec); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, data)
	}
	if rec["msg"] != "boot" || rec["rev"] != float64(3) {
		t.Fatalf("unexpected record: %v", rec)
	}
	if _, ok := rec["time"]; !ok {
		t.Fatalf("expected time field in JSON record: %v", rec)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("log file perm = %o, want 600", perm)
	}
}

func TestSetupDualSink(t *testing.T) {
	var buf bytes.Buffer
	path := filepath.Join(t.TempDir(), "dual.jsonl")
	logger, closeFn, err := logx.Setup(logx.Config{
		TextEnabled: true, TextWriter: &buf, JSONPath: path,
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	logger.Info("both sinks")
	if err := closeFn(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if !strings.Contains(buf.String(), "both sinks") {
		t.Fatalf("text sink missing record: %q", buf.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), `"msg":"both sinks"`) {
		t.Fatalf("json sink missing record: %s", data)
	}
}

func TestSetupBothDisabledDiscards(t *testing.T) {
	logger, closeFn, err := logx.Setup(logx.Config{})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer func() { _ = closeFn() }()
	// Must not panic and must not create any file.
	logger.Info("into the void", slog.String("token", "x"))
}

func TestSetupDefaultLevelIsInfo(t *testing.T) {
	var buf bytes.Buffer
	logger, closeFn, err := logx.Setup(logx.Config{TextEnabled: true, TextWriter: &buf})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer func() { _ = closeFn() }()

	logger.Debug("hidden")
	if buf.Len() != 0 {
		t.Fatalf("debug record should be filtered at default level: %q", buf.String())
	}
}

// TestDebugEnvDoesNotBypassScrubbing is the acceptance test for the
// "AGENTHUB_DEBUG must not unlock secrets" invariant: with AGENTHUB_DEBUG=1
// debug-level records do flow (verbosity is raised), but their secrets are
// still redacted in every sink.
func TestDebugEnvDoesNotBypassScrubbing(t *testing.T) {
	t.Setenv(logx.EnvDebug, "1")

	var buf bytes.Buffer
	path := filepath.Join(t.TempDir(), "debug.jsonl")
	logger, closeFn, err := logx.Setup(logx.Config{
		TextEnabled: true, TextWriter: &buf, JSONPath: path,
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	logger.Debug("dbg dump",
		slog.String("token", "super-secret-value"),
		slog.String("hdr", "Authorization: Bearer abc123"))
	if err := closeFn(); err != nil {
		t.Fatalf("close: %v", err)
	}

	jsonOut, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for name, out := range map[string]string{"text": buf.String(), "json": string(jsonOut)} {
		if !strings.Contains(out, "dbg dump") {
			t.Fatalf("%s: debug record missing (AGENTHUB_DEBUG should raise verbosity): %q", name, out)
		}
		if strings.Contains(out, "super-secret-value") || strings.Contains(out, "abc123") {
			t.Fatalf("%s: secret leaked despite scrubbing: %q", name, out)
		}
		if !strings.Contains(out, logx.Redacted) {
			t.Fatalf("%s: expected %s marker: %q", name, logx.Redacted, out)
		}
	}
}

func TestSetupJSONPathUnwritable(t *testing.T) {
	_, _, err := logx.Setup(logx.Config{JSONPath: filepath.Join(t.TempDir(), "missing", "x.jsonl")})
	if err == nil {
		t.Fatal("expected error for unwritable json path")
	}
}
