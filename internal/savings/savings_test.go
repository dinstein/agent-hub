package savings

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/jsonl"
)

// fakeClock is a manually advanced clock.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// readLines reads one JSONL file back as lines.
func readLines(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		out = append(out, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSavingsRecordGolden(t *testing.T) {
	r := Record{
		TS:             time.Date(2026, 7, 26, 12, 0, 0, 123456789, time.UTC),
		Client:         "claude-code",
		Session:        "sess-1",
		Server:         "github",
		Mode:           "lazy-discovery",
		BaselineTokens: 12000,
		ActualTokens:   1800,
		SavedTokens:    10200,
	}
	const want = `{"ts":"2026-07-26T12:00:00.123456789Z","client":"claude-code",` +
		`"session":"sess-1","server":"github","mode":"lazy-discovery",` +
		`"baselineTokens":12000,"actualTokens":1800,"savedTokens":10200}`
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("golden mismatch:\n got %s\nwant %s", got, want)
	}

	// Optional grouping fields drop out; the token fields always appear.
	minimal, err := json.Marshal(Record{
		TS:   time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
		Mode: "shaping",
	})
	if err != nil {
		t.Fatal(err)
	}
	const wantMinimal = `{"ts":"2026-07-26T12:00:00Z","mode":"shaping",` +
		`"baselineTokens":0,"actualTokens":0,"savedTokens":0}`
	if string(minimal) != wantMinimal {
		t.Errorf("minimal golden mismatch:\n got %s\nwant %s", minimal, wantMinimal)
	}
}

func TestStreamEndToEnd(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{now: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	s, err := NewStream(filepath.Join(dir, FileName), jsonl.WriterOptions{Clock: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	s.Append(Record{Mode: "grouped", BaselineTokens: 100, ActualTokens: 40, SavedTokens: 60})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, filepath.Join(dir, FileName))
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	var r Record
	if err := json.Unmarshal(lines[0], &r); err != nil {
		t.Fatal(err)
	}
	if !r.TS.Equal(clk.now) || r.SavedTokens != 60 {
		t.Errorf("round-trip mismatch: %+v", r)
	}
}
