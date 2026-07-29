package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/audit"
)

// TestServerLogsRendersAnOversizeMarker: a marker must read as "this frame
// was dropped, and here is what it was", not as a blank row.
//
// The marker shares its "ts" field with a frame, so the previous reader
// unmarshaled it into TraceFrame without error and got a zero value. The
// table then printed an empty line — the least informative possible output
// for the one frame large enough to matter.
func TestServerLogsRendersAnOversizeMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "server-big.log")
	marker := `{"ts":"2026-07-29T14:56:20.023916Z","oversize":true,"origBytes":4634,` +
		`"prefix":"{\"ts\":\"2026-07-29T14:56:20.0Z\",\"server\":\"linear\",\"dir\":\"in\",\"method\":\"tools/list\",\"bytes\":64211,\"payload\":\"{"}`
	if err := os.WriteFile(path, []byte(marker+"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	logs, err := readServerLogs("big", path, 0)
	if err != nil {
		t.Fatalf("readServerLogs: %v", err)
	}
	if len(logs.Frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(logs.Frames))
	}
	if logs.Skipped != 0 {
		t.Errorf("skipped = %d; a marker is a record, not an undecodable line", logs.Skipped)
	}
	f := logs.Frames[0]
	if f.Bytes != 4634 {
		t.Errorf("bytes = %d, want the dropped record's size 4634", f.Bytes)
	}
	if !strings.Contains(f.Error, "dropped") {
		t.Errorf("error = %q, want it to say the frame was dropped", f.Error)
	}
	// Recovered from the prefix: which frame was lost is the whole question.
	if f.Dir != "in" || f.Method != "tools/list" {
		t.Errorf("dir/method = %s/%s, want in/tools/list recovered from the prefix", f.Dir, f.Method)
	}
	if f.Server != "linear" {
		t.Errorf("server = %q, want linear", f.Server)
	}

	var out strings.Builder
	if err := logs.Human(&out); err != nil {
		t.Fatalf("Human: %v", err)
	}
	if !strings.Contains(out.String(), "tools/list") || !strings.Contains(out.String(), "dropped") {
		t.Errorf("table does not report the dropped frame:\n%s", out.String())
	}
}

// TestServerLogsStillCountsGarbage: recognising markers must not turn a
// genuinely undecodable line into a silent success.
func TestServerLogsStillCountsGarbage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "server-x.log")
	if err := os.WriteFile(path, []byte("{not json\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	logs, err := readServerLogs("x", path, 0)
	if err != nil {
		t.Fatalf("readServerLogs: %v", err)
	}
	if logs.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", logs.Skipped)
	}
}

// TestDecodeOversizeRejectsAPlainRecord pins the discriminator: without the
// oversize flag being REQUIRED, an ordinary frame would decode as a marker
// and every line would render as dropped.
func TestDecodeOversizeRejectsAPlainRecord(t *testing.T) {
	t.Parallel()
	plain := `{"ts":"2026-07-29T14:56:19.5Z","server":"linear","dir":"out","method":"tools/list","bytes":0,"pid":13146}`
	if _, ok := audit.DecodeOversize([]byte(plain)); ok {
		t.Error("a normal frame was taken for an oversize marker")
	}
}
