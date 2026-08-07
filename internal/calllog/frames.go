package calllog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dinstein/agent-hub/internal/platform"
)

// The frame half of the ledger.
//
// One schema, one directory, one retention policy — and both halves fail
// OPEN, but they reach it differently, which is why they are separate FILES
// rather than separate packages:
//
//   - A lifecycle record is written synchronously, under the shared file's
//     capacity lock and durability mode, and a failure is reported to the
//     assembly. It costs the history a line.
//   - A frame is never even reported. It is a debugging instrument, and the
//     one thing an instrument must never do is take down the thing it
//     measures, so frames are queued without blocking and dropped and counted
//     under backpressure.
//
// Frames outnumber lifecycle records by one to two orders of magnitude, so
// putting them through the shared, capacity-locked, fsync-on-write path would
// make a debug switch the hot path of the file every call is described in.

// FrameCounters reports what the frame path has done since the store opened.
type FrameCounters struct {
	// Written is frames appended.
	Written uint64
	// Dropped is frames discarded under backpressure — the queue was full
	// and the caller was not made to wait.
	Dropped uint64
	// Failed is frames that reached the file and could not be written.
	Failed uint64
}

// frameQueueDepth bounds the pending frame queue. It is generous enough to
// ride out a disk hiccup during a burst and small enough that a wedged disk
// costs bounded memory rather than the process.
const frameQueueDepth = 4096

// frameJob is one queued frame, or — with a nil event kind and a non-nil ack
// — a barrier that reports when everything ahead of it has been written.
type frameJob struct {
	event Event
	body  []byte
	ack   chan struct{}
}

// frames is the per-store frame pipeline: one goroutine, one queue, one file
// per UTC day.
type frames struct {
	store *Store
	ch    chan frameJob
	done  chan struct{}

	// chMu guards the channel's OPEN/closed state and every send. It is
	// separate from mu on purpose: sync() blocks on a full queue while
	// holding it, and the writer goroutine — which takes mu for its file —
	// must never need this one to make progress, or a full queue would
	// deadlock the drain that empties it.
	chMu   sync.RWMutex
	closed bool

	// The tallies are atomics rather than fields under mu, and that is a
	// correctness requirement, not a style choice. mu is held by writerFor
	// across EnsureDir and OpenFile — a file-system round trip, once per
	// process per UTC day — and counting a drop is what a caller does when
	// the queue is full. Under one mutex those two meet, and the caller waits
	// on a disk inside the one call this package promises never to delay:
	// both conditions at once are rare, but they are widest on exactly the
	// slow disk that filled the queue. mu now guards the file alone.
	written atomic.Uint64
	dropped atomic.Uint64
	failed  atomic.Uint64

	mu  sync.Mutex
	day string
	// writer is this process's frame file for day, opened on that day's
	// first frame.
	writer *os.File
}

func newFrames(s *Store) *frames {
	f := &frames{store: s, ch: make(chan frameJob, frameQueueDepth), done: make(chan struct{})}
	go f.run()
	return f
}

// AppendFrame queues one frame. It NEVER blocks and never returns an error:
// the queue is bounded and an overflow is counted, because the alternative is
// a downstream call waiting on a trace nobody asked to be durable.
//
// body is the frame payload. It is stored only when the store has a key and
// capture is on; either way its size reaches the metadata line, so a reader
// can tell a large frame from a missing one.
func (s *Store) AppendFrame(e Event, body []byte, capture bool) {
	if s == nil || s.frames == nil {
		return
	}
	e.Bytes = len(body)
	if !capture || !s.HasKey() {
		body = nil
	}
	f := s.frames
	// The closed check and the send are under one read lock, so a frame
	// arriving during shutdown cannot reach a closed channel. The send is
	// non-blocking, so this never costs a call any latency.
	f.chMu.RLock()
	defer f.chMu.RUnlock()
	if f.closed {
		f.dropped.Add(1)
		return
	}
	select {
	case f.ch <- frameJob{event: e, body: body}:
	default:
		f.dropped.Add(1)
	}
}

// FrameCounters reports the frame path's tally.
func (s *Store) FrameCounters() FrameCounters {
	if s == nil || s.frames == nil {
		return FrameCounters{}
	}
	f := s.frames
	return FrameCounters{
		Written: f.written.Load(), Dropped: f.dropped.Load(), Failed: f.failed.Load(),
	}
}

// run drains the queue. One goroutine, so frames from this process reach the
// file in the order they happened.
func (f *frames) run() {
	defer close(f.done)
	for job := range f.ch {
		if job.ack != nil {
			close(job.ack)
			continue
		}
		f.write(job)
	}
}

func (f *frames) write(job frameJob) {
	e := job.event
	f.store.stampEnvelope(&e)
	day := utcDay(e.TS)

	// The payload goes in FIRST, exactly as the lifecycle path does: a
	// committed record pointing at bytes that were never written is the one
	// inconsistency a reader cannot recover from, while an orphan payload is
	// merely reclaimable.
	if len(job.body) > 0 {
		if ref, err := f.store.PutPayload(e.TS, e.CallID, PayloadFrame, job.body); err == nil {
			e.Frame = &ref
		}
	}
	if f.store.HasKey() {
		e.KeyID = f.store.keyID
		if mac, err := eventMAC(e, f.store.key); err == nil {
			e.MAC = mac
		}
	}
	line, err := json.Marshal(e)
	if err != nil {
		f.failed.Add(1)
		return
	}
	if len(line)+1 > MaxEventLineBytes {
		// Frames are bounded by the same line contract as everything else in
		// this directory, and they reach it honestly: the body lives in the
		// pack, so only the metadata is here and it cannot legitimately grow
		// this large. A line that does is a bug, counted rather than torn.
		f.failed.Add(1)
		return
	}
	line = append(line, '\n')

	w, err := f.writerFor(day)
	if err != nil {
		f.failed.Add(1)
		return
	}
	if _, err := w.Write(line); err != nil {
		f.failed.Add(1)
		return
	}
	f.written.Add(1)
}

// writerFor returns this process's frame file for one day, opening it on the
// first frame of that day. Nothing is created until something is recorded:
// an empty file per day per process would make an installation that never
// traced anything look like one that did.
func (f *frames) writerFor(day string) (*os.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writer != nil && f.day == day {
		return f.writer, nil
	}
	if f.writer != nil {
		_ = f.writer.Close()
		f.writer = nil
	}
	dir := filepath.Join(f.store.root, day)
	if err := platform.EnsureDir(dir); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s%s-p%d%s", FramePrefix, f.store.bootID, os.Getpid(), FrameExt)
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	f.day, f.writer = day, file
	return f.writer, nil
}

// close drains the queue and releases the file. It is called by Store.Close.
func (f *frames) close() error {
	if f == nil {
		return nil
	}
	f.chMu.Lock()
	if f.closed {
		f.chMu.Unlock()
		return nil
	}
	f.closed = true
	close(f.ch)
	f.chMu.Unlock()
	select {
	case <-f.done:
	case <-time.After(2 * time.Second):
		// A wedged disk must not hold a shutting-down gateway open. What is
		// still queued is lost, which is the same trade every frame makes.
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writer == nil {
		return nil
	}
	err := f.writer.Close()
	f.writer = nil
	return err
}

// sync blocks until everything queued so far has been written. Tests use it;
// the data path never does.
func (f *frames) sync() {
	if f == nil {
		return
	}
	f.chMu.RLock()
	if f.closed {
		f.chMu.RUnlock()
		return
	}
	ack := make(chan struct{})
	f.ch <- frameJob{ack: ack} // blocks until the drain makes room
	f.chMu.RUnlock()
	select {
	case <-ack:
	case <-f.done:
	}
}

// Sync blocks until every frame queued so far has been written. It exists for
// tests and for a shutdown that wants its trace complete; the data path never
// calls it, because waiting is the one thing the frame half promises not to
// make anybody do.
func (s *Store) Sync() {
	if s == nil {
		return
	}
	s.frames.sync()
}
