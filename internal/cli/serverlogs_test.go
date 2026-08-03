package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedFrames writes a day of frame records the way a gateway would: one
// process-owned file inside one UTC day of the ledger.
func seedFrames(t *testing.T, dataDir string, lines ...string) string {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	dir := filepath.Join(dataDir, "calls", day)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "frames-abc123-p42.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// frameLine builds one record with the current day's timestamp, so `since`
// day-partition skipping cannot hide the fixture.
func frameLine(t *testing.T, fields map[string]any) string {
	t.Helper()
	rec := map[string]any{"v": 1, "ts": time.Now().UTC().Format(time.RFC3339Nano), "pid": 42}
	for k, v := range fields {
		rec[k] = v
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestServerLogsRendersFrames(t *testing.T) {
	dir := setDataDir(t)
	seedFrames(t, dir,
		frameLine(t, map[string]any{
			"event": "sent", "server": "github", "method": "tools/call",
			"callId": "abcdef0123456789", "cause": "call", "seq": 1, "bytes": 42,
		}),
		frameLine(t, map[string]any{
			"event": "recv", "server": "github", "method": "tools/call",
			"callId": "abcdef0123456789", "cause": "call", "seq": 1, "durationMs": 12,
		}),
		frameLine(t, map[string]any{
			"event": "sent", "server": "other", "method": "ping", "cause": "probe",
		}),
	)

	code, out, errOut := runCLI(t, "", "server", "logs", "github")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	if !strings.Contains(out, "sent") || !strings.Contains(out, "recv") {
		t.Fatalf("both directions should be shown:\n%s", out)
	}
	// The join key is the whole reason the frames moved into the ledger.
	if !strings.Contains(out, "abcdef012345") {
		t.Fatalf("the call id is missing from the table:\n%s", out)
	}
	if strings.Contains(out, "ping") {
		t.Fatalf("another server's frames leaked into this one's view:\n%s", out)
	}
}

// A frame that belongs to no call is not an exception, and the reason it
// exists has to be on the line: otherwise a health probe reads as a call
// whose id went missing.
func TestServerLogsShowsTheCauseOfCallLessFrames(t *testing.T) {
	dir := setDataDir(t)
	seedFrames(t, dir, frameLine(t, map[string]any{
		"event": "sent", "server": "github", "method": "ping", "cause": "probe",
	}))

	code, out, errOut := runCLI(t, "", "server", "logs", "github")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	if !strings.Contains(out, "probe") {
		t.Fatalf("a frame with no call must say why it happened:\n%s", out)
	}
}

func TestServerLogsLimitKeepsTail(t *testing.T) {
	dir := setDataDir(t)
	var lines []string
	for i := range 5 {
		lines = append(lines, frameLine(t, map[string]any{
			"event": "sent", "server": "github", "method": "m" + string(rune('0'+i)),
		}))
	}
	seedFrames(t, dir, lines...)

	code, out, errOut := runCLI(t, "", "server", "logs", "github", "--limit", "2")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	if strings.Contains(out, "m0") || !strings.Contains(out, "m4") {
		t.Fatalf("--limit must keep the NEWEST frames:\n%s", out)
	}
}

// Nothing recorded is a normal state, and the message has to distinguish it
// from a server that sat idle — the switch is off by default.
func TestServerLogsWithNothingRecordedExplainsItself(t *testing.T) {
	setDataDir(t)

	code, out, errOut := runCLI(t, "", "server", "logs", "github")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "server trace github on") {
		t.Fatalf("the empty result does not say how to record anything:\n%s", out)
	}
}

func TestServerLogsCountsUndecodableLines(t *testing.T) {
	dir := setDataDir(t)
	seedFrames(t, dir,
		frameLine(t, map[string]any{"event": "sent", "server": "github", "method": "ping"}),
		"{not json at all",
	)

	code, out, errOut := runCLI(t, "", "server", "logs", "github")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	if !strings.Contains(out, "1 undecodable line") {
		t.Fatalf("a torn line was dropped silently:\n%s", out)
	}
}

// --json is the machine face of the same read, and it carries the fields the
// table has no room for.
func TestServerLogsJSONCarriesTheJoinKeys(t *testing.T) {
	dir := setDataDir(t)
	seedFrames(t, dir, frameLine(t, map[string]any{
		"event": "recv", "server": "github", "method": "tools/call",
		"callId": "call-1", "cause": "call", "seq": 3, "inst": "work", "durationMs": 7,
	}))

	code, out, errOut := runCLI(t, "", "--json", "server", "logs", "github")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	var env struct {
		Data ServerLogs `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(env.Data.Frames) != 1 {
		t.Fatalf("frames = %+v", env.Data.Frames)
	}
	f := env.Data.Frames[0]
	if f.CallID != "call-1" || f.Seq != 3 || f.Cause != "call" || f.Inst != "work" || f.DurMs != 7 {
		t.Fatalf("frame lost its join keys: %+v", f)
	}
}
