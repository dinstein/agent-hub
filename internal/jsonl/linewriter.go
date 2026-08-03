package jsonl

import "bytes"

// LineWriter is an io.Writer view of a Writer, for producers that already
// emit one newline-terminated record per call. log/slog's JSON handler is
// the one that matters: it builds a record into a buffer and hands it over in
// a single Write.
//
// It exists so the process logs (daemon.log, gateway-<client>.log) get the
// multi-writer discipline this package was written for, rather than a second
// implementation of it. Those files are shared exactly the way the streams
// here are — every `agenthub connect --client claude-code` appends to the
// same gateway-claude-code.log — and before this type they had none of it:
// no line bound, so a long record could tear against another process's; no
// rotation and no retention, so they grew without limit.
//
// internal/logx cannot construct one (it is locked to the standard library),
// which is why the assembly opens the sink and hands it over as an io.Writer.
type LineWriter struct {
	w *Writer
}

// NewLineWriter opens the file at path and returns an io.Writer over it. The
// parent directory must already exist. Options carry the same meaning as for
// NewWriter, including KeepSegments.
func NewLineWriter(path string, opts WriterOptions) (*LineWriter, error) {
	w, err := NewWriter(path, opts)
	if err != nil {
		return nil, err
	}
	return &LineWriter{w: w}, nil
}

// Write appends each whole line in p as its own record.
//
// It ALWAYS reports the full length written and a nil error. A record that
// could not be enqueued is counted by Dropped, never returned: log/slog
// discards whatever Handle returns, so an error here would reach no one while
// still tempting a caller to treat a lost log line as a failed operation.
// Fail-open is also the rule the rest of this package follows — a record on
// its way to disk must never slow down or fail the work that produced it.
//
// Lines longer than MaxLineBytes become oversize markers, exactly as they do
// through AppendLine. For a process log that means a record over ~4 KiB is
// replaced by a marker naming its size — which is the intended pressure:
// full arguments and payloads belong in the ledger, never in slog.
func (l *LineWriter) Write(p []byte) (int, error) {
	if l == nil || l.w == nil {
		return len(p), nil
	}
	for line := range bytes.SplitSeq(bytes.TrimSuffix(p, []byte("\n")), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		l.w.AppendLine(line)
	}
	return len(p), nil
}

// Close flushes pending records and closes the file.
func (l *LineWriter) Close() error {
	if l == nil || l.w == nil {
		return nil
	}
	return l.w.Close()
}

// Path returns the active file this sink writes to.
func (l *LineWriter) Path() string {
	if l == nil || l.w == nil {
		return ""
	}
	return l.w.Path()
}

// Dropped counts records discarded under backpressure.
func (l *LineWriter) Dropped() uint64 {
	if l == nil || l.w == nil {
		return 0
	}
	return l.w.Dropped()
}
