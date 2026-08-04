package logx_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/logx"
)

func TestSetupTextOnly(t *testing.T) {
	var buf bytes.Buffer
	logger := logx.Setup(logx.Config{TextEnabled: true, TextWriter: &buf})

	logger.Info("hello", logx.Server("github"))

	out := buf.String()
	if !strings.Contains(out, "msg=hello") || !strings.Contains(out, "server=github") {
		t.Fatalf("unexpected text output: %q", out)
	}
}

func TestSetupJSONSink(t *testing.T) {
	var buf bytes.Buffer
	logger := logx.Setup(logx.Config{JSON: &buf})

	logger.Info("boot", logx.Rev(3))

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "boot" || rec["rev"] != float64(3) {
		t.Fatalf("unexpected record: %v", rec)
	}
	if _, ok := rec["time"]; !ok {
		t.Fatalf("expected time field in JSON record: %v", rec)
	}
}

func TestSetupDualSink(t *testing.T) {
	var text, jsonBuf bytes.Buffer
	logger := logx.Setup(logx.Config{TextEnabled: true, TextWriter: &text, JSON: &jsonBuf})

	logger.Info("both sinks")

	if !strings.Contains(text.String(), "both sinks") {
		t.Fatalf("text sink missing record: %q", text.String())
	}
	if !strings.Contains(jsonBuf.String(), `"msg":"both sinks"`) {
		t.Fatalf("json sink missing record: %s", jsonBuf.String())
	}
}

func TestSetupBothDisabledDiscards(t *testing.T) {
	logger := logx.Setup(logx.Config{})

	// Must not panic and must not need a sink.
	logger.Info("into the void", slog.String("token", "x"))
}

func TestSetupDefaultLevelIsInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := logx.Setup(logx.Config{TextEnabled: true, TextWriter: &buf})

	logger.Debug("hidden")

	if buf.Len() != 0 {
		t.Fatalf("debug record should be filtered at default level: %q", buf.String())
	}
}

// TestSinkLevelsAreIndependent is the point of the per-sink override: a
// gateway raising the file to debug must not also raise stderr, which in
// stdio mode is read by the MCP client that spawned it.
func TestSinkLevelsAreIndependent(t *testing.T) {
	var text, jsonBuf bytes.Buffer
	logger := logx.Setup(logx.Config{
		TextEnabled: true, TextWriter: &text, JSON: &jsonBuf,
		TextLevel: slog.LevelWarn, JSONLevel: slog.LevelDebug,
	})

	logger.Debug("detail")
	logger.Warn("trouble")

	if strings.Contains(text.String(), "detail") {
		t.Fatalf("text sink took a record below its own level: %q", text.String())
	}
	if !strings.Contains(text.String(), "trouble") {
		t.Fatalf("text sink dropped a record at its own level: %q", text.String())
	}
	for _, want := range []string{"detail", "trouble"} {
		if !strings.Contains(jsonBuf.String(), want) {
			t.Fatalf("json sink missing %q: %s", want, jsonBuf.String())
		}
	}
}

// TestSinkLevelOverridesFollowLevel pins the precedence between the two: an
// override moves one sink, and the sink without one keeps following Level
// rather than reverting to the info default.
func TestSinkLevelOverridesFollowLevel(t *testing.T) {
	var text, jsonBuf bytes.Buffer
	logger := logx.Setup(logx.Config{
		TextEnabled: true, TextWriter: &text, JSON: &jsonBuf,
		Level: slog.LevelDebug, TextLevel: slog.LevelError,
	})

	logger.Debug("detail")

	if strings.Contains(text.String(), "detail") {
		t.Fatalf("TextLevel did not override Level: %q", text.String())
	}
	if !strings.Contains(jsonBuf.String(), "detail") {
		t.Fatalf("json sink should have followed Level=debug: %s", jsonBuf.String())
	}
}

// TestDebugOverridesSinkLevels: AGENTHUB_DEBUG is the blunt switch, so a
// per-sink setting must not be able to hold part of it back — a debug run
// that silently kept one sink quiet is the failure this rules out.
func TestDebugOverridesSinkLevels(t *testing.T) {
	var text, jsonBuf bytes.Buffer
	logger := logx.Setup(logx.Config{
		TextEnabled: true, TextWriter: &text, JSON: &jsonBuf,
		Debug: true, TextLevel: slog.LevelError, JSONLevel: slog.LevelError,
	})

	logger.Debug("detail")

	if !strings.Contains(text.String(), "detail") || !strings.Contains(jsonBuf.String(), "detail") {
		t.Fatalf("Debug did not override the per-sink levels: text=%q json=%s", text.String(), jsonBuf.String())
	}
}

// TestDebugEnvDoesNotBypassScrubbing is the acceptance test for the
// "AGENTHUB_DEBUG must not unlock secrets" invariant: with AGENTHUB_DEBUG=1
// debug-level records do flow (verbosity is raised), but their secrets are
// still redacted in every sink.
func TestDebugEnvDoesNotBypassScrubbing(t *testing.T) {
	t.Setenv(logx.EnvDebug, "1")

	var text, jsonBuf bytes.Buffer
	logger := logx.Setup(logx.Config{TextEnabled: true, TextWriter: &text, JSON: &jsonBuf})

	logger.Debug("dbg dump",
		slog.String("token", "super-secret-value"),
		slog.String("hdr", "Authorization: Bearer abc123"))

	for name, out := range map[string]string{"text": text.String(), "json": jsonBuf.String()} {
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
