package jsonl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// readLines returns the raw lines of one file, requiring a trailing
// newline on the last line (a missing one would indicate a torn write).
func readLines(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(data) == 0 {
		return nil
	}
	if data[len(data)-1] != '\n' {
		t.Fatalf("%s does not end in a newline (torn write): ...%q", path, data[max(0, len(data)-40):])
	}
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return lines
}

// streamFiles globs the active file plus every rotated segment for base.
func streamFiles(t *testing.T, dir, base string) []string {
	t.Helper()
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	matches, err := filepath.Glob(filepath.Join(dir, stem+"*"+ext))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return matches
}

func TestWriterAppendAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := NewWriter(path, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		w.AppendLine([]byte(`{"i":` + strconv.Itoa(i) + `}`))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, path)
	if len(lines) != 10 {
		t.Fatalf("got %d lines, want 10", len(lines))
	}
	for i, l := range lines {
		want := `{"i":` + strconv.Itoa(i) + `}`
		if string(l) != want {
			t.Errorf("line %d = %q, want %q", i, l, want)
		}
	}
	if w.Dropped() != 0 || w.WriteErrors() != 0 {
		t.Errorf("dropped=%d writeErrors=%d, want 0/0", w.Dropped(), w.WriteErrors())
	}
	// Idempotent close, and post-close appends count as dropped.
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	w.AppendLine([]byte(`{}`))
	if w.Dropped() != 1 {
		t.Errorf("post-close append: dropped=%d, want 1", w.Dropped())
	}
}

func TestWriterOversizeTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := NewWriter(path, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	big, err := json.Marshal(map[string]string{"payload": strings.Repeat("A", 10*1024)})
	if err != nil {
		t.Fatal(err)
	}
	w.AppendLine(big)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if len(lines[0])+1 > DefaultMaxLineBytes {
		t.Fatalf("marker line is %d bytes, exceeds bound %d", len(lines[0])+1, DefaultMaxLineBytes)
	}
	var m struct {
		Oversize  bool   `json:"oversize"`
		OrigBytes int    `json:"origBytes"`
		Prefix    string `json:"prefix"`
	}
	if err := json.Unmarshal(lines[0], &m); err != nil {
		t.Fatalf("marker is not valid JSON: %v (%q)", err, lines[0])
	}
	if !m.Oversize || m.OrigBytes != len(big) || m.Prefix == "" {
		t.Errorf("marker = %+v, want oversize with origBytes=%d and a prefix", m, len(big))
	}
}

func TestWriterBackpressureDrops(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once bool
	hook := func() {
		if !once {
			once = true
			close(entered)
			<-release
		}
	}
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := NewWriter(path, WriterOptions{BufferSize: 1, testHookBeforeWrite: hook})
	if err != nil {
		t.Fatal(err)
	}
	w.AppendLine([]byte(`{"n":0}`)) // picked up by the goroutine, holds in hook
	<-entered
	// Buffer capacity is 1: exactly one of the next appends is queued,
	// the rest are dropped without blocking.
	for i := 1; i <= 5; i++ {
		w.AppendLine([]byte(`{"n":` + strconv.Itoa(i) + `}`))
	}
	if d := w.Dropped(); d != 4 {
		t.Errorf("dropped = %d, want 4", d)
	}
	close(release)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := len(readLines(t, path)); got != 2 {
		t.Errorf("wrote %d lines, want 2 (held + one buffered)", got)
	}
}

func TestWriterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	w, err := NewWriter(path, WriterOptions{MaxBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	const n = 200
	pad := strings.Repeat("y", 80)
	for i := 0; i < n; i++ {
		line, err := json.Marshal(appendLine{Proc: "p", Seq: i, Pad: pad})
		if err != nil {
			t.Fatal(err)
		}
		w.AppendLine(line)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	files := streamFiles(t, dir, "audit.jsonl")
	if len(files) < 2 {
		t.Fatalf("expected rotation to produce segments, got files %v", files)
	}
	seen := make(map[int]bool, n)
	for _, f := range files {
		for _, l := range readLines(t, f) {
			var rec appendLine
			if err := json.Unmarshal(l, &rec); err != nil {
				t.Fatalf("torn/invalid line in %s: %v (%q)", f, err, l)
			}
			if seen[rec.Seq] {
				t.Errorf("seq %d duplicated", rec.Seq)
			}
			seen[rec.Seq] = true
		}
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		// Each segment holds at most threshold + one line's slack.
		if fi.Size() > 2048+256 {
			t.Errorf("segment %s is %d bytes, exceeds threshold slack", f, fi.Size())
		}
	}
	if len(seen) != n {
		t.Errorf("recovered %d distinct lines, want %d (lost lines)", len(seen), n)
	}
	if w.WriteErrors() != 0 {
		t.Errorf("writeErrors = %d, want 0", w.WriteErrors())
	}
}

// runAppendHelpers launches procs helper processes appending n lines each
// and waits for all of them.
func runAppendHelpers(t *testing.T, path string, procs, n int, maxBytes int64) {
	t.Helper()
	type proc struct {
		cmd  *exec.Cmd
		errB *bytes.Buffer
	}
	ps := make([]proc, procs)
	for i := range ps {
		errB := &bytes.Buffer{}
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(),
			helperModeEnv+"=append",
			helperPathEnv+"="+path,
			helperIDEnv+"="+strconv.Itoa(i),
			helperNEnv+"="+strconv.Itoa(n),
			helperMaxEnv+"="+strconv.FormatInt(maxBytes, 10),
		)
		cmd.Stderr = errB
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper %d: %v", i, err)
		}
		ps[i] = proc{cmd: cmd, errB: errB}
	}
	for i, p := range ps {
		if err := p.cmd.Wait(); err != nil {
			t.Fatalf("helper %d failed: %v\nstderr: %s", i, err, p.errB.String())
		}
	}
}

// verifyAppendLines re-parses every line across every segment and asserts
// complete per-process sequence coverage: no torn lines, no lost lines.
func verifyAppendLines(t *testing.T, dir string, procs, n int) {
	t.Helper()
	seen := make(map[string]map[int]bool)
	total := 0
	for _, f := range streamFiles(t, dir, "audit.jsonl") {
		for _, l := range readLines(t, f) {
			var rec appendLine
			if err := json.Unmarshal(l, &rec); err != nil {
				t.Fatalf("torn/invalid line in %s: %v (%q)", f, err, l)
			}
			if seen[rec.Proc] == nil {
				seen[rec.Proc] = make(map[int]bool)
			}
			if seen[rec.Proc][rec.Seq] {
				t.Errorf("proc %s seq %d duplicated", rec.Proc, rec.Seq)
			}
			seen[rec.Proc][rec.Seq] = true
			total++
		}
	}
	if total != procs*n {
		t.Errorf("recovered %d lines, want %d", total, procs*n)
	}
	for p := 0; p < procs; p++ {
		id := strconv.Itoa(p)
		for i := 0; i < n; i++ {
			if !seen[id][i] {
				t.Errorf("proc %s seq %d missing (lost line)", id, i)
			}
		}
	}
}

func TestCrossProcessAppend(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process test skipped in -short mode")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	const procs, n = 4, 300
	runAppendHelpers(t, path, procs, n, -1) // rotation disabled
	verifyAppendLines(t, dir, procs, n)
}

func TestCrossProcessAppendWithRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process test skipped in -short mode")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	const procs, n = 4, 300
	runAppendHelpers(t, path, procs, n, 4096) // aggressive rotation
	verifyAppendLines(t, dir, procs, n)
	if len(streamFiles(t, dir, "audit.jsonl")) < 2 {
		t.Error("expected at least one rotated segment under aggressive MaxBytes")
	}
}

func TestSegmentPathShape(t *testing.T) {
	ts := time.Date(2026, 7, 26, 12, 0, 0, 123456789, time.UTC)
	got := segmentPath("/logs/audit.jsonl", ts)
	wantPrefix := "/logs/audit-20260726T120000.123456789Z.p"
	if !strings.HasPrefix(got, wantPrefix) || !strings.HasSuffix(got, ".jsonl") {
		t.Errorf("segmentPath = %q, want %q<pid>.jsonl", got, wantPrefix)
	}
}
