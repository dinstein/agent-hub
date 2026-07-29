package registry

import (
	"context"
	"os"
	"time"
)

// lockFileName is the sibling lock file guarding the whole registry
// directory. A single lock (not per-document) is required because the
// generation in meta.json must be bumped atomically with the document writes
// it covers.
const lockFileName = ".lock"

// lockPollInterval is how often a blocked acquirer retries the non-blocking
// flock before its deadline.
const lockPollInterval = 5 * time.Millisecond

// fileLock is a held cross-process advisory lock (flock on darwin/linux).
type fileLock struct {
	f *os.File
}

// acquireLock takes an exclusive flock on <dir>/.lock, polling until timeout.
// On timeout it returns *LockTimeoutError (errors.Is(_, ErrLockTimeout)).
// Context cancellation is honored between polls.
func acquireLock(ctx context.Context, dir string, timeout time.Duration) (*fileLock, error) {
	path := dirLockPath(dir)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := flockExclusiveNB(f)
		if err == nil {
			return &fileLock{f: f}, nil
		}
		if !isWouldBlock(err) {
			_ = f.Close()
			return nil, err
		}
		if ctx.Err() != nil {
			_ = f.Close()
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, &LockTimeoutError{Path: path, Timeout: timeout}
		}
		time.Sleep(lockPollInterval)
	}
}

// release drops the lock. Safe to call once; errors are ignored because the
// close releases the flock regardless.
func (l *fileLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = flockUnlock(l.f)
	_ = l.f.Close()
	l.f = nil
}

func dirLockPath(dir string) string { return dir + string(os.PathSeparator) + lockFileName }
