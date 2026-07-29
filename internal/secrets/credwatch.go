package secrets

import (
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// CredWatcher reports server ids whose stored credentials were announced as
// changed (announce.go). It mirrors the registry watcher's shape on purpose
// — fsnotify as the primary signal, a poll as the safety net, a per-key
// applied baseline so an event names exactly what moved — because the two
// planes have the same job and a reader should not have to learn two idioms.
//
// It does NOT suppress its own process's writes. The gateway refreshes
// tokens too, so it will see announcements it caused; acting on one costs a
// single vault read that returns the value just written. Suppression would
// buy that read back in exchange for a fingerprint set to keep correct, and
// the plane is a hint — it must stay cheap to reason about.
type CredWatcher struct {
	dir  string
	poll time.Duration

	ch   chan string
	done chan struct{}

	closeOnce sync.Once
	wg        sync.WaitGroup

	// applied is owned by the run goroutine exclusively.
	applied map[string]uint64
}

const (
	credWatchDebounce = 200 * time.Millisecond
	credWatchPoll     = 2 * time.Second
	credChanBuffer    = 16
)

// NewCredWatcher starts watching dir for credential announcements. The
// counters present at construction are the baseline: a watcher never
// reports what was already stored before it existed.
//
// It never fails on a missing directory or an unavailable fsnotify — the
// poll alone is a complete implementation, and a plane whose whole failure
// direction is "recovery is less prompt" must not be able to refuse to
// start.
func NewCredWatcher(dir string) *CredWatcher {
	w := &CredWatcher{
		dir:     dir,
		poll:    credWatchPoll,
		ch:      make(chan string, credChanBuffer),
		done:    make(chan struct{}),
		applied: map[string]uint64{},
	}
	for id, rev := range Revisions(dir) {
		w.applied[id] = rev
	}
	w.wg.Add(1)
	go w.run()
	return w
}

// Events returns the channel of changed server ids. It is closed by Close.
func (w *CredWatcher) Events() <-chan string { return w.ch }

// Close stops the watcher and closes Events. Safe to call more than once.
func (w *CredWatcher) Close() {
	w.closeOnce.Do(func() {
		close(w.done)
		w.wg.Wait()
	})
}

func (w *CredWatcher) run() {
	defer w.wg.Done()
	defer close(w.ch)

	var (
		fsnEvents chan fsnotify.Event
		fsnErrors chan error
	)
	if fw, err := fsnotify.NewWatcher(); err == nil {
		if aerr := fw.Add(w.dir); aerr == nil {
			defer func() { _ = fw.Close() }()
			fsnEvents, fsnErrors = fw.Events, fw.Errors
		} else {
			_ = fw.Close()
		}
	}

	poll := time.NewTicker(w.poll)
	defer poll.Stop()
	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()

	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-fsnEvents:
			if !ok {
				fsnEvents, fsnErrors = nil, nil
				continue
			}
			if filepath.Base(ev.Name) == credentialsRevFile {
				debounce.Reset(credWatchDebounce)
			}
		case _, ok := <-fsnErrors:
			if !ok {
				fsnErrors = nil
				continue
			}
			// Non-fatal: the poll covers anything fsnotify dropped.
		case <-debounce.C:
			w.scan()
		case <-poll.C:
			w.scan()
		}
	}
}

// scan diffs the announcement counters against the applied baseline and
// emits one id per server that moved.
func (w *CredWatcher) scan() {
	cur := Revisions(w.dir)
	for id, rev := range cur {
		if w.applied[id] == rev {
			continue
		}
		w.applied[id] = rev
		w.emit(id)
	}
}

// emit delivers an id without ever blocking the scan loop. A full channel
// DROPS the event rather than parking it, which is safe here and is not in
// the registry watcher: this signal only ever makes a recovery prompter, and
// a consumer far enough behind to fill the buffer has 16 more recoveries
// queued that will do the same job.
func (w *CredWatcher) emit(id string) {
	select {
	case w.ch <- id:
	default:
	}
}
