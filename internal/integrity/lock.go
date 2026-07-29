package integrity

import (
	"context"
	"os"
	"time"
)

// lockPollInterval is how often a blocked acquirer retries the non-blocking
// flock before its deadline.
const lockPollInterval = 5 * time.Millisecond

// fileLock is a held cross-process advisory lock (flock on darwin/linux).
// Each state file has its own sibling "<file>.lock"; multi-writer discipline
// (N gateways + daemon, docs/flows.md) makes this mandatory, not optional.
type fileLock struct {
	f *os.File
}

// acquireLock takes an exclusive flock on path, polling until timeout.
// On timeout it returns *LockTimeoutError (errors.Is(_, ErrLockTimeout)).
// Context cancellation is honored between polls.
func acquireLock(ctx context.Context, path string, timeout time.Duration) (*fileLock, error) {
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
