package jsonl

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Writer defaults. Exported so callers and docs agree on the numbers.
const (
	// DefaultMaxLineBytes bounds one serialized line including the trailing
	// newline. 4096 is PIPE_BUF on Linux (POSIX guarantees >= 512); regular
	// -file O_APPEND writes are additionally serialized by the kernel inode
	// lock on local filesystems, so a line within this bound is written
	// atomically — concurrent appenders can interleave lines but never tear
	// one.
	DefaultMaxLineBytes = 4096
	// DefaultMaxBytes is the rotation threshold for the active file.
	DefaultMaxBytes = 32 << 20 // 32 MiB
	// DefaultBufferSize is the writer channel capacity.
	DefaultBufferSize = 1024
	// DefaultKeepSegments is the retention a stream here uses unless it has
	// a reason of its own: the active file plus three rotated segments. It
	// is one constant rather than one per stream so "how much history do
	// these files keep" has a single answer.
	DefaultKeepSegments = 3
)

// WriterOptions configures a Writer. The zero value uses the defaults
// above and disables nothing.
type WriterOptions struct {
	// MaxBytes is the rotation threshold. 0 means DefaultMaxBytes;
	// negative disables rotation.
	MaxBytes int64
	// MaxLineBytes bounds a single line (0 = DefaultMaxLineBytes).
	// Records longer than the bound are replaced by an oversize marker —
	// see the truncation policy on AppendLine.
	MaxLineBytes int
	// BufferSize is the channel capacity (0 = DefaultBufferSize).
	BufferSize int
	// KeepSegments is how many rotated segments survive, newest first. 0
	// keeps every one of them — retention is opt-in, because a stream that
	// is someone's archive must not lose history to a default.
	//
	// The sweep runs once, in NewWriter, rather than on a timer: rotation
	// happens at MaxBytes and is therefore rare, while these files are
	// opened constantly (one per `agenthub connect`), so the check is
	// frequent in practice and costs one directory listing.
	KeepSegments int
	// Clock overrides time.Now (tests). Used for record timestamps and
	// segment names.
	Clock func() time.Time

	// testHookBeforeWrite, when set, runs in the writer goroutine before
	// each file write. Same-package tests use it to hold the goroutine and
	// provoke backpressure deterministically.
	testHookBeforeWrite func()
}

// message travels from AppendLine/Sync to the writer goroutine.
type message struct {
	line []byte        // one full line ending in '\n'; nil for pure barriers
	ack  chan struct{} // non-nil: closed by the goroutine after processing
}

// Writer is a multi-process-safe JSONL appender.
//
// Invariants (docs/architecture.md §10, docs/modules/security.md multi-writer discipline):
//   - The file is opened O_APPEND|O_CREATE|O_WRONLY 0600 and every record
//     is exactly one write(2) of one '\n'-terminated line.
//   - Lines are bounded by MaxLineBytes so concurrent appends from other
//     processes can never tear a line (see DefaultMaxLineBytes).
//   - Rotation renames the active file to a fresh segment (atomic rename)
//     and reopens; the file is never read back and truncated. Writers in
//     other processes holding the renamed segment keep appending to it
//     without loss and reattach to the new active file on their next
//     write.
//   - In-process, all appends are serialized by one goroutine behind a
//     buffered channel. AppendLine never blocks: overflow increments the
//     drop counter (fail-open for the caller — audit pressure must never
//     stall the data plane).
type Writer struct {
	path     string
	maxBytes int64
	maxLine  int
	clock    func() time.Time
	testHook func()

	mu     sync.Mutex // guards closed + sends on ch
	closed bool
	ch     chan message
	done   chan struct{}

	f        *os.File // owned by the writer goroutine after start
	closeErr error    // written by the goroutine before done is closed

	dropped     atomic.Uint64
	writeErrors atomic.Uint64
}

// NewWriter opens (creating if needed) the JSONL file at path and starts
// the writer goroutine. The parent directory must already exist.
func NewWriter(path string, opts WriterOptions) (*Writer, error) {
	w := &Writer{
		path:     path,
		maxBytes: opts.MaxBytes,
		maxLine:  opts.MaxLineBytes,
		clock:    opts.Clock,
		testHook: opts.testHookBeforeWrite,
		done:     make(chan struct{}),
	}
	if w.maxBytes == 0 {
		w.maxBytes = DefaultMaxBytes
	}
	if w.maxLine <= 0 {
		w.maxLine = DefaultMaxLineBytes
	}
	if w.clock == nil {
		w.clock = time.Now
	}
	size := opts.BufferSize
	if size <= 0 {
		size = DefaultBufferSize
	}
	w.ch = make(chan message, size)

	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	w.f = f
	if opts.KeepSegments > 0 {
		Prune(path, opts.KeepSegments)
	}
	go w.run()
	return w, nil
}

func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// AppendLine enqueues one serialized record (a single JSON document with
// no raw newlines — encoding/json output satisfies this). The trailing
// newline is added here. The slice is copied; callers may reuse it.
//
// Truncation policy for oversize records: a record whose line would exceed
// MaxLineBytes is replaced by a marker object
//
//	{"ts":..., "oversize":true, "origBytes":N, "prefix":"..."}
//
// carrying the first bytes of the original record (budgeted so the marker
// itself stays within the bound). The original record is not written —
// a bounded marker is preferable to a torn line that corrupts the stream
// for every consumer.
func (w *Writer) AppendLine(record []byte) {
	line := w.boundLine(record)
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		w.dropped.Add(1)
		return
	}
	select {
	case w.ch <- message{line: line}:
		w.mu.Unlock()
	default:
		w.mu.Unlock()
		w.dropped.Add(1)
	}
}

// Sync blocks until every record enqueued before the call has been written
// to the file. It is primarily a test/shutdown barrier.
func (w *Writer) Sync() {
	ack := make(chan struct{})
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	// Blocking send is safe: the writer goroutine never takes w.mu, so it
	// always drains the channel and this send cannot deadlock.
	w.ch <- message{ack: ack}
	w.mu.Unlock()
	<-ack
}

// Close drains the queue, fsyncs and closes the file. Subsequent appends
// are counted as dropped. Close is idempotent.
func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		<-w.done
		return w.closeErr
	}
	w.closed = true
	close(w.ch)
	w.mu.Unlock()
	<-w.done
	return w.closeErr
}

// Dropped reports records discarded due to backpressure or post-close
// appends.
func (w *Writer) Dropped() uint64 { return w.dropped.Load() }

// WriteErrors reports failed write attempts (after the one automatic
// reopen-and-retry).
func (w *Writer) WriteErrors() uint64 { return w.writeErrors.Load() }

// Clock returns the writer's time source, so a stream layered on top of a
// Writer stamps its records from the same clock the writer rotates by.
func (w *Writer) Clock() func() time.Time { return w.clock }

// Path returns the active file path.
func (w *Writer) Path() string { return w.path }

// run is the single writer goroutine: it owns w.f exclusively.
func (w *Writer) run() {
	defer close(w.done)
	for msg := range w.ch {
		if msg.line != nil {
			if w.testHook != nil {
				w.testHook()
			}
			w.writeLine(msg.line)
		}
		if msg.ack != nil {
			close(msg.ack)
		}
	}
	if w.f != nil {
		err := w.f.Sync()
		if cerr := w.f.Close(); err == nil {
			err = cerr
		}
		w.closeErr = err
	}
}

// writeLine performs the reattach → rotate → reattach → single-write
// sequence for one line.
func (w *Writer) writeLine(line []byte) {
	w.ensureCurrent()
	w.maybeRotate(len(line))
	w.ensureCurrent()
	if w.f == nil {
		w.writeErrors.Add(1)
		return
	}
	if _, err := w.f.Write(line); err != nil {
		// One reopen-and-retry: the file may have been unlinked or the fd
		// invalidated. A second failure is counted and the line dropped.
		w.reopen()
		if w.f == nil {
			w.writeErrors.Add(1)
			return
		}
		if _, err := w.f.Write(line); err != nil {
			w.writeErrors.Add(1)
		}
	}
}

// ensureCurrent reattaches w.f to w.path when another process rotated the
// active file away (path missing or pointing at a different inode).
func (w *Writer) ensureCurrent() {
	if w.f == nil {
		w.reopen()
		return
	}
	pfi, err := os.Stat(w.path)
	if err != nil {
		w.reopen()
		return
	}
	ffi, err := w.f.Stat()
	if err != nil || !os.SameFile(pfi, ffi) {
		w.reopen()
	}
}

func (w *Writer) reopen() {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	f, err := openAppend(w.path)
	if err != nil {
		w.writeErrors.Add(1)
		return
	}
	w.f = f
}

// maybeRotate renames the active file to a new segment when appending next
// bytes would exceed MaxBytes. The rename is atomic; losing the rename
// race to another process is fine (ENOENT is ignored) — the follow-up
// ensureCurrent reattaches either way. The active file is never truncated
// or rewritten.
func (w *Writer) maybeRotate(next int) {
	if w.maxBytes <= 0 || w.f == nil {
		return
	}
	fi, err := w.f.Stat()
	if err != nil {
		return
	}
	if fi.Size() == 0 || fi.Size()+int64(next) <= w.maxBytes {
		return
	}
	seg := segmentPath(w.path, w.clock())
	if err := os.Rename(w.path, seg); err != nil && !errors.Is(err, fs.ErrNotExist) {
		w.writeErrors.Add(1)
	}
}

// segmentPath derives the rotated segment name:
// audit.jsonl -> audit-20260726T120000.123456789Z.p1234.jsonl
// The pid suffix makes concurrent same-instant rotations by different
// processes collision-free.
func segmentPath(path string, t time.Time) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	stamp := t.UTC().Format("20060102T150405.000000000Z")
	return base + "-" + stamp + ".p" + strconv.Itoa(os.Getpid()) + ext
}

// boundLine copies record into a fresh '\n'-terminated line, substituting
// the oversize marker when the bound is exceeded.
func (w *Writer) boundLine(record []byte) []byte {
	if len(record)+1 > w.maxLine {
		record = w.oversizeMarker(record)
	}
	line := make([]byte, 0, len(record)+1)
	line = append(line, record...)
	line = append(line, '\n')
	return line
}

// OversizeMarker is the substitute line written in place of a record that
// would exceed MaxLineBytes. It is EXPORTED because it is part of the file
// format, not an implementation detail: every reader of a stream this
// package writes will meet one, and a reader that does not recognise it
// decodes the marker as its own record type and gets a zero value — which
// renders as a blank row that says nothing happened, when in fact the one
// frame worth reading is the frame that was dropped.
type OversizeMarker struct {
	TS       string `json:"ts"`
	Oversize bool   `json:"oversize"`
	// OrigBytes is the size of the record that did not fit.
	OrigBytes int `json:"origBytes"`
	// Prefix is the head of that record, kept so a reader can still tell
	// what it was.
	Prefix string `json:"prefix"`
}

// DecodeOversize reports whether line is an oversize marker, and decodes it.
// A reader should try this BEFORE its own record type: the two shapes share
// the "ts" field, so a marker unmarshals into most record types without
// error and produces an empty-looking record.
func DecodeOversize(line []byte) (OversizeMarker, bool) {
	var m OversizeMarker
	if err := json.Unmarshal(line, &m); err != nil || !m.Oversize {
		return OversizeMarker{}, false
	}
	return m, true
}

// oversizeMarker builds the bounded substitute for an oversize record.
// The prefix budget is maxLine/8 raw bytes: JSON escaping expands at most
// 6x (\u00XX), keeping the marker safely under the line bound.
func (w *Writer) oversizeMarker(orig []byte) []byte {
	budget := w.maxLine / 8
	if budget > len(orig) {
		budget = len(orig)
	}
	prefix := strings.ToValidUTF8(string(orig[:budget]), "")
	m := OversizeMarker{
		TS:        w.clock().UTC().Format(time.RFC3339Nano),
		Oversize:  true,
		OrigBytes: len(orig),
		Prefix:    prefix,
	}
	b, err := json.Marshal(m)
	if err != nil || len(b)+1 > w.maxLine {
		// Unreachable by construction; keep the bound guaranteed anyway.
		m.Prefix = ""
		b, _ = json.Marshal(m)
	}
	return b
}

// MarshalLine encodes one record as a single JSON line for AppendLine. A
// value that cannot be marshalled becomes a marshalError line rather than a
// dropped record: a stream that silently loses entries is worse than one
// that says it could not encode them.
//
// No production caller uses it today. internal/downstream and
// internal/eventlog both marshal their own records and return on error,
// which is the discipline this function exists to offer and neither takes
// up. Whether they should is a behaviour question, not a naming one — the
// failure is unreachable for the plain record types both write, so nothing
// is known to be lost; it is noted here because the gap is invisible from
// either call site, each of which looks locally reasonable.
func MarshalLine(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"marshalError": err.Error()})
	}
	return b
}
