package downstream

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dinstein/agent-hub/internal/audit"
)

// readTraceLines returns every line of a trace file.
func readTraceLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out = append(out, l)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read trace: %v", err)
	}
	return out
}

// TestTraceKeepsLargeFramesInsteadOfDroppingThem is the regression test for
// a trace that silently lost exactly the frames it was opened for.
//
// The payload cap and the writer's line bound were both 4096, which cannot
// work: a payload cut to 4096 raw bytes serializes to far more once escaped
// into a JSON string, so the line exceeded the bound and audit.Writer
// replaced the whole record with a marker. A real 64 KB tools/list response
// produced no frame at all — and `server logs` rendered the marker as a
// blank row, so nothing said so.
func TestTraceKeepsLargeFramesInsteadOfDroppingThem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := OpenServerLog(dir, "big", true)
	if err != nil {
		t.Fatalf("OpenServerLog: %v", err)
	}

	// A body shaped like the real thing: a tools/list result is mostly
	// quoted strings, and it is the quotes that double under escaping.
	var b strings.Builder
	b.WriteString(`{"tools":[`)
	for i := 0; b.Len() < 64<<10; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"name":"tool_x","description":"does a thing \"quoted\" and more"}`)
	}
	b.WriteString(`]}`)
	payload := json.RawMessage(b.String())

	l.out("", "tools/list", payload)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readTraceLines(t, ServerLogPath(dir, "big"))
	if len(lines) != 1 {
		t.Fatalf("wrote %d lines, want 1", len(lines))
	}
	if _, isMarker := audit.DecodeOversize([]byte(lines[0])); isMarker {
		t.Fatal("the frame was replaced by an oversize marker: a large body must be truncated, not dropped")
	}

	var f TraceFrame
	if err := json.Unmarshal([]byte(lines[0]), &f); err != nil {
		t.Fatalf("frame does not decode: %v", err)
	}
	if f.Method != "tools/list" || f.Dir != TraceOut {
		t.Errorf("frame = %s/%s, want out/tools/list", f.Dir, f.Method)
	}
	if f.Bytes != len(payload) {
		t.Errorf("bytes = %d, want the PRE-truncation size %d", f.Bytes, len(payload))
	}
	if !f.Truncated {
		t.Error("a cut payload must be marked truncated")
	}
	if f.Payload == "" {
		t.Error("the whole payload was dropped; the point is to keep as much as fits")
	}
	if !utf8.ValidString(f.Payload) {
		t.Error("the payload was cut mid-rune")
	}
	if got := len(lines[0]) + 1; got > audit.DefaultMaxLineBytes {
		t.Errorf("line is %d bytes, over the %d bound", got, audit.DefaultMaxLineBytes)
	}
}

// TestTraceFitsEveryFrameUnderTheLineBound sweeps payload sizes across the
// escaping cliff: the failure was not "big payloads break" but "payloads in
// a range whose ESCAPED form crosses the bound break", which a single size
// can miss.
func TestTraceFitsEveryFrameUnderTheLineBound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := OpenServerLog(dir, "sweep", true)
	if err != nil {
		t.Fatalf("OpenServerLog: %v", err)
	}
	for n := 1; n <= 12000; n += 137 {
		// All quotes: the worst realistic case, every byte doubling.
		l.out("", "tools/call", json.RawMessage(strings.Repeat(`"`, n)))
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i, line := range readTraceLines(t, ServerLogPath(dir, "sweep")) {
		if _, isMarker := audit.DecodeOversize([]byte(line)); isMarker {
			t.Fatalf("line %d became an oversize marker", i)
		}
		if got := len(line) + 1; got > audit.DefaultMaxLineBytes {
			t.Fatalf("line %d is %d bytes, over the %d bound", i, got, audit.DefaultMaxLineBytes)
		}
	}
}

func TestTrimValidUTF8(t *testing.T) {
	t.Parallel()
	const s = "aé漢" // 1 + 2 + 3 bytes
	for n, want := range map[int]string{
		0: "", 1: "a", 2: "a", 3: "aé", 4: "aé", 5: "aé", 6: "aé漢", 99: "aé漢",
	} {
		if got := trimValidUTF8(s, n); got != want {
			t.Errorf("trimValidUTF8(%q, %d) = %q, want %q", s, n, got, want)
		}
	}
}
