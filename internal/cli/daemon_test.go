package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/platform"
)

// setDaemonEnv points both the data dir and the control socket at fresh
// temp locations (the socket needs a SHORT path: t.TempDir embeds test
// names and can exceed the sun_path limit on macOS).
func setDaemonEnv(t *testing.T) (dataDir, socket string) {
	t.Helper()
	dataDir = setDataDir(t)
	sockDir, err := os.MkdirTemp("", "ahcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socket = filepath.Join(sockDir, "ctl.sock")
	t.Setenv(platform.EnvSocket, socket)
	return dataDir, socket
}

func TestDaemonCommandTable(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string // substring of stdout ("" = no assertion)
		wantErr  string // substring of stderr ("" = no assertion)
	}{
		{
			name:     "bare group shows help",
			args:     []string{"daemon"},
			wantCode: ExitOK,
			wantOut:  "start",
		},
		{
			name:     "unknown subcommand is a usage error",
			args:     []string{"daemon", "explode"},
			wantCode: ExitUsage,
		},
		{
			name:     "unknown flag is a usage error",
			args:     []string{"daemon", "status", "--bogus"},
			wantCode: ExitUsage,
		},
		{
			name:     "status offline exits 4",
			args:     []string{"daemon", "status"},
			wantCode: ExitDaemonDown,
			wantOut:  "daemon is not running",
		},
		{
			name:     "stop when not running is idempotent success",
			args:     []string{"daemon", "stop"},
			wantCode: ExitOK,
			wantOut:  "daemon is not running",
		},
		{
			name:     "logs without a log file is not-found",
			args:     []string{"daemon", "logs"},
			wantCode: ExitNotFound,
		},
		{
			name:     "logs rejects a bad level",
			args:     []string{"daemon", "logs", "--level", "loud"},
			wantCode: ExitUsage,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setDaemonEnv(t)
			code, out, errOut := runCLI(t, "", tc.args...)
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)", code, tc.wantCode, out, errOut)
			}
			if tc.wantOut != "" && !strings.Contains(out, tc.wantOut) {
				t.Errorf("stdout %q does not contain %q", out, tc.wantOut)
			}
			if tc.wantErr != "" && !strings.Contains(errOut, tc.wantErr) {
				t.Errorf("stderr %q does not contain %q", errOut, tc.wantErr)
			}
		})
	}
}

func TestDaemonStatusOfflineJSONEnvelope(t *testing.T) {
	_, socket := setDaemonEnv(t)
	code, out, _ := runCLI(t, "", "daemon", "status", "--json")
	if code != ExitDaemonDown {
		t.Fatalf("exit = %d, want %d", code, ExitDaemonDown)
	}
	env := decodeEnvelope(t, out)
	if !env.OK {
		t.Fatalf("envelope not ok: %s", out)
	}
	var status DaemonStatus
	if err := json.Unmarshal(env.Data, &status); err != nil {
		t.Fatal(err)
	}
	if status.Running || status.Socket != socket {
		t.Fatalf("status = %+v", status)
	}
}

func TestDaemonStartForegroundRefusesSecondInstance(t *testing.T) {
	_, socket := setDaemonEnv(t)
	// A live listener on the socket: the daemon's stale-socket probe dials
	// it successfully and must refuse to start.
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	code, out, errOut := runCLI(t, "", "daemon", "start", "--foreground", "--json")
	if code != ExitGeneral {
		t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)", code, ExitGeneral, out, errOut)
	}
	env := decodeEnvelope(t, out)
	if env.Error == nil || env.Error.Code != CodeDaemonRunning {
		t.Fatalf("error envelope = %s", out)
	}
}

// writeDaemonLog seeds <data>/logs/daemon.log with synthetic slog JSON
// lines: an old info, a recent info, a recent warn and one garbage line.
func writeDaemonLog(t *testing.T, dataDir string) {
	t.Helper()
	logsDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	line := func(ts time.Time, level, msg string) string {
		return fmt.Sprintf(`{"time":%q,"level":%q,"msg":%q,"pid":42}`,
			ts.Format(time.RFC3339Nano), level, msg)
	}
	content := strings.Join([]string{
		line(now.Add(-3*time.Hour), "INFO", "ancient info"),
		line(now.Add(-time.Minute), "INFO", "recent info"),
		line(now.Add(-30*time.Second), "WARN", "recent warn"),
		"not json at all",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(logsDir, "daemon.log"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonLogsFiltering(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    []string
		exclude []string
	}{
		{
			name: "no filters shows everything including unparseable lines",
			args: []string{"daemon", "logs"},
			want: []string{"ancient info", "recent info", "recent warn", "not json at all"},
		},
		{
			name:    "level filter keeps only warn and above and drops garbage",
			args:    []string{"daemon", "logs", "--level", "warn"},
			want:    []string{"recent warn"},
			exclude: []string{"ancient info", "recent info", "not json at all"},
		},
		{
			name:    "since filter drops old lines",
			args:    []string{"daemon", "logs", "--since", "1h"},
			want:    []string{"recent info", "recent warn"},
			exclude: []string{"ancient info", "not json at all"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir, _ := setDaemonEnv(t)
			writeDaemonLog(t, dataDir)
			code, out, errOut := runCLI(t, "", tc.args...)
			if code != ExitOK {
				t.Fatalf("exit = %d (stderr %q)", code, errOut)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q:\n%s", w, out)
				}
			}
			for _, e := range tc.exclude {
				if strings.Contains(out, e) {
					t.Errorf("output should not contain %q:\n%s", e, out)
				}
			}
		})
	}
}

func TestDaemonLogsJSONEmitsRawLines(t *testing.T) {
	dataDir, _ := setDaemonEnv(t)
	writeDaemonLog(t, dataDir)
	code, out, _ := runCLI(t, "", "daemon", "logs", "--level", "warn", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1:\n%s", len(lines), out)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &parsed); err != nil {
		t.Fatalf("line is not raw JSON: %v", err)
	}
	if parsed["msg"] != "recent warn" || parsed["pid"] != float64(42) {
		t.Fatalf("line = %v", parsed)
	}
}
