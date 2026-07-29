package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Cross-process acceptance tests re-exec this test binary as helper
// processes (same pattern as internal/registry): TestMain diverts to a
// helper before m.Run when the mode env is set.

const (
	helperModeEnv   = "AGENTHUB_AUDIT_TEST_HELPER"
	helperPathEnv   = "AGENTHUB_AUDIT_TEST_PATH"
	helperIDEnv     = "AGENTHUB_AUDIT_TEST_ID"
	helperNEnv      = "AGENTHUB_AUDIT_TEST_N"
	helperMaxEnv    = "AGENTHUB_AUDIT_TEST_MAXBYTES"
	helperDirEnv    = "AGENTHUB_AUDIT_TEST_DIR"
	helperWindowEnv = "AGENTHUB_AUDIT_TEST_WINDOW"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperModeEnv) {
	case "":
		os.Exit(m.Run())
	case "append":
		helperAppend()
	case "security":
		helperSecurity()
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
}

func helperFail(err error) {
	fmt.Fprintln(os.Stderr, "helper:", err)
	os.Exit(1)
}

// appendLine is the shape every append helper writes; parents re-parse it
// to prove no line was torn or lost.
type appendLine struct {
	Proc string `json:"proc"`
	Seq  int    `json:"seq"`
	Pad  string `json:"pad"`
}

// helperAppend writes n JSON lines through one Writer and requires zero
// drops so the parent can assert exact line counts.
func helperAppend() {
	path := os.Getenv(helperPathEnv)
	id := os.Getenv(helperIDEnv)
	n, err := strconv.Atoi(os.Getenv(helperNEnv))
	if err != nil || path == "" || id == "" {
		helperFail(fmt.Errorf("bad helper env (path=%q id=%q n=%v)", path, id, err))
	}
	maxBytes, err := strconv.ParseInt(os.Getenv(helperMaxEnv), 10, 64)
	if err != nil {
		helperFail(err)
	}
	w, err := NewWriter(path, WriterOptions{MaxBytes: maxBytes, BufferSize: n + 8})
	if err != nil {
		helperFail(err)
	}
	pad := make([]byte, 80)
	for i := range pad {
		pad[i] = 'x'
	}
	for i := 0; i < n; i++ {
		b, err := json.Marshal(appendLine{Proc: id, Seq: i, Pad: string(pad)})
		if err != nil {
			helperFail(err)
		}
		w.AppendLine(b)
	}
	if err := w.Close(); err != nil {
		helperFail(err)
	}
	if d := w.Dropped(); d != 0 {
		helperFail(fmt.Errorf("helper dropped %d lines", d))
	}
	if e := w.WriteErrors(); e != 0 {
		helperFail(fmt.Errorf("helper hit %d write errors", e))
	}
	os.Exit(0)
}

// helperSecurity emits the same event n times; cross-process dedup must
// collapse all emissions from all helpers into a single line.
func helperSecurity() {
	dir := os.Getenv(helperDirEnv)
	n, err := strconv.Atoi(os.Getenv(helperNEnv))
	if err != nil || dir == "" {
		helperFail(fmt.Errorf("bad helper env (dir=%q n=%v)", dir, err))
	}
	window, err := time.ParseDuration(os.Getenv(helperWindowEnv))
	if err != nil {
		helperFail(err)
	}
	s, err := NewSecurityStream(filepath.Join(dir, SecurityFileName), SecurityOptions{
		Window:   window,
		DedupDir: filepath.Join(dir, DedupDirName),
	})
	if err != nil {
		helperFail(err)
	}
	for i := 0; i < n; i++ {
		s.Emit(SecurityEvent{
			Event:    "injection.blocked",
			Severity: SeverityCritical,
			Server:   "srv",
			Tool:     "tool",
		})
	}
	if err := s.Close(); err != nil {
		helperFail(err)
	}
	os.Exit(0)
}
