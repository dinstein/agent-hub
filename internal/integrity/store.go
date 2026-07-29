package integrity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dinstein/agent-hub/internal/platform"
)

// State file names under <state>/ (platform.StateDir). Frozen: renaming any
// of them would orphan every existing baseline.
const (
	pinsFileName       = "tool-pins.json"
	quarantineFileName = "quarantine.json"
	approvalsFileName  = "tool-approvals.json"
)

// PolicyFiles returns the state files whose CONTENT decides whether a tool
// may be listed and called: the approval store (which carries the operator
// kill switch) and the quarantine set. A data-plane reader watches exactly
// these two — pins feed drift classification, which is an input to those
// decisions but never one itself.
func PolicyFiles(stateDir string) []string {
	return []string{
		filepath.Join(stateDir, approvalsFileName),
		filepath.Join(stateDir, quarantineFileName),
	}
}

const (
	lockSuffix         = ".lock"
	defaultLockTimeout = 10 * time.Second

	// storeVersion is the on-disk envelope version of all three state files.
	storeVersion = 1

	// readRetries: a parse failure is retried this many times (re-reading
	// the file each time) before the file is declared corrupt. All writers
	// here are atomic (rename) AND hold the lock, but the short retry window
	// absorbs rename transients from any future lock-free reader path and
	// costs nothing on the happy path.
	readRetries = 4
)

// readRetryDelay is a variable only so tests exercising the corrupt path do
// not pay the full retry ladder; production code never mutates it.
var readRetryDelay = 75 * time.Millisecond

// Options tunes a store. The zero value is usable.
type Options struct {
	// LockTimeout bounds cross-process lock acquisition (default 10s).
	LockTimeout time.Duration
	// Now overrides the clock (tests). Default time.Now.
	Now func() time.Time
}

// lockedFile is one on-disk JSON state file guarded by a sibling
// cross-process lock. Invariant: every read-modify-write cycle runs entirely
// under the flock, so concurrent processes serialize and never lose updates.
type lockedFile struct {
	path        string
	lockTimeout time.Duration
	now         func() time.Time
}

func newLockedFile(stateDir, name string, opts Options) (*lockedFile, error) {
	if stateDir == "" {
		return nil, errors.New("integrity: state dir must not be empty")
	}
	// EnsureDir enforces 0700: state files hold security decisions and must
	// never be group/world accessible.
	if err := platform.EnsureDir(stateDir); err != nil {
		return nil, err
	}
	f := &lockedFile{
		path:        filepath.Join(stateDir, name),
		lockTimeout: opts.LockTimeout,
		now:         opts.Now,
	}
	if f.lockTimeout <= 0 {
		f.lockTimeout = defaultLockTimeout
	}
	if f.now == nil {
		f.now = time.Now
	}
	return f, nil
}

// withLock runs fn while holding the sibling cross-process lock.
func (f *lockedFile) withLock(ctx context.Context, fn func() error) error {
	l, err := acquireLock(ctx, f.path+lockSuffix, f.lockTimeout)
	if err != nil {
		return err
	}
	defer l.release()
	return fn()
}

// loadStore reads and decodes the state file into a fresh T.
//
// Returns found=false with a zero T when the file does not exist — a missing
// file IS a fresh store (first run). Any other failure — unreadable file,
// unparseable JSON, trailing garbage, empty file (atomic writers never leave
// one), unsupported envelope version — is *CorruptError (fail-closed): the
// caller must abort the operation and the file is NEVER renamed or treated
// as an empty set.
func loadStore[T any](path string, version func(*T) int) (T, bool, error) {
	var zero T
	var lastErr error
	for attempt := 0; attempt <= readRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(readRetryDelay)
		}
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return zero, false, nil
		}
		if err != nil {
			lastErr = err
			continue
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			lastErr = errors.New("file is empty (atomic writers never produce empty files)")
			continue
		}
		var v T
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			lastErr = err
			continue
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			lastErr = errors.New("trailing data after JSON document")
			continue
		}
		if got := version(&v); got != storeVersion {
			// A future-versioned file is not interpretable by this binary;
			// guessing would silently drop fields carrying security state.
			lastErr = fmt.Errorf("unsupported store version %d (want %d)", got, storeVersion)
			continue
		}
		return v, true, nil
	}
	return zero, false, &CorruptError{Path: path, Err: lastErr}
}

// save persists v with the full hardening ladder (atomicWrite). Encoding is
// deterministic: MarshalIndent plus Go's sorted map keys.
func (f *lockedFile) save(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(f.path, append(b, '\n'))
}

// atomicWrite persists data to path: same-directory temp file, chmod 0600,
// write, fsync, rename over the target, fsync of the parent directory.
// Never leaves a partially written target. (Independent copy of registry's
// ladder — see doc.go for why registry is not imported.)
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory so a preceding rename is durable. Filesystems
// that do not support fsync on directories are tolerated — the rename itself
// is still atomic there.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
			return nil
		}
		return err
	}
	return nil
}
