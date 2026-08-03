package jsonl_test

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/jsonl"
)

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// The shape that matters: slog's JSON handler writing through the sink must
// produce one decodable object per record.
func TestLineWriterCarriesSlogRecordsOnePerLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway-test.log")
	sink, err := jsonl.NewLineWriter(path, jsonl.WriterOptions{})
	if err != nil {
		t.Fatalf("NewLineWriter: %v", err)
	}
	log := slog.New(slog.NewJSONHandler(sink, nil))

	log.Info("connected", "server", "github")
	log.Warn("respawn failed", "server", "github")
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), lines)
	}
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not one JSON object (%v): %s", i, err, line)
		}
		if rec["server"] != "github" {
			t.Errorf("line %d lost its attrs: %s", i, line)
		}
	}
}

// A caller handing over several lines at once must not produce one line
// holding several records — that is the shape every reader here rejects.
func TestLineWriterSplitsAMultiLineWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multi.log")
	sink, err := jsonl.NewLineWriter(path, jsonl.WriterOptions{})
	if err != nil {
		t.Fatalf("NewLineWriter: %v", err)
	}

	n, err := sink.Write([]byte("{\"a\":1}\n{\"a\":2}\n"))
	if err != nil || n != 16 {
		t.Fatalf("Write = (%d, %v), want (16, nil)", n, err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if lines := readLines(t, path); len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), lines)
	}
}

// Fail-open is the contract: slog discards Handle's error, so a sink that
// reported one would fail a caller for nothing. A closed sink still accepts.
func TestLineWriterNeverReportsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closed.log")
	sink, err := jsonl.NewLineWriter(path, jsonl.WriterOptions{})
	if err != nil {
		t.Fatalf("NewLineWriter: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if n, err := sink.Write([]byte("{\"after\":\"close\"}\n")); err != nil || n == 0 {
		t.Fatalf("Write after Close = (%d, %v), want the length and nil", n, err)
	}

	var nilSink *jsonl.LineWriter
	if n, err := nilSink.Write([]byte("x\n")); err != nil || n != 2 {
		t.Fatalf("nil sink Write = (%d, %v), want (2, nil)", n, err)
	}
}

// A record over the line bound must not tear a shared file: it is replaced
// by a marker naming its size, the same substitution AppendLine makes.
func TestLineWriterReplacesAnOversizeRecordWithAMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.log")
	sink, err := jsonl.NewLineWriter(path, jsonl.WriterOptions{})
	if err != nil {
		t.Fatalf("NewLineWriter: %v", err)
	}
	log := slog.New(slog.NewJSONHandler(sink, nil))

	log.Info("huge", "payload", strings.Repeat("x", 8<<10))
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), lines)
	}
	if len(lines[0])+1 > jsonl.DefaultMaxLineBytes {
		t.Errorf("line is %d bytes, over the %d bound", len(lines[0])+1, jsonl.DefaultMaxLineBytes)
	}
	m, ok := jsonl.DecodeOversize([]byte(lines[0]))
	if !ok {
		t.Fatalf("oversize record was not replaced by a marker: %s", lines[0])
	}
	if m.OrigBytes < 8<<10 {
		t.Errorf("marker reports %d original bytes, want at least %d", m.OrigBytes, 8<<10)
	}
}

func TestNewWriterPrunesToKeepSegments(t *testing.T) {
	dir := t.TempDir()
	active, segments := touchSegments(t, dir, 4)

	sink, err := jsonl.NewLineWriter(active, jsonl.WriterOptions{KeepSegments: 1})
	if err != nil {
		t.Fatalf("NewLineWriter: %v", err)
	}
	defer func() { _ = sink.Close() }()

	for i, p := range segments {
		_, err := os.Stat(p)
		gone := os.IsNotExist(err)
		wantGone := i < len(segments)-1
		if gone != wantGone {
			t.Errorf("segment %s gone=%v, want %v", filepath.Base(p), gone, wantGone)
		}
	}
}

// Retention is opt-in: a stream that did not ask for it keeps every segment.
func TestNewWriterWithoutKeepSegmentsPrunesNothing(t *testing.T) {
	dir := t.TempDir()
	active, segments := touchSegments(t, dir, 4)

	sink, err := jsonl.NewLineWriter(active, jsonl.WriterOptions{})
	if err != nil {
		t.Fatalf("NewLineWriter: %v", err)
	}
	defer func() { _ = sink.Close() }()

	for _, p := range segments {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was pruned without KeepSegments: %v", filepath.Base(p), err)
		}
	}
}
