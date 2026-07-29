package skills

import (
	"context"
	"os"
	"time"
)

// lockPollInterval is how often a blocked acquirer retries the non-blocking
// flock before its deadline.
const lockPollInterval = 5 * time.Millisecond

// fileLock is a held cross-process advisory lock. One sibling ".lock" file
// guards the whole skills directory: N gateways plus the daemon plus the
// CLI all mutate this state, so multi-writer discipline is mandatory, not
// an optimization.
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

// release drops the lock. Errors are ignored: closing the descriptor
// releases the flock regardless.
func (l *fileLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = flockUnlock(l.f)
	_ = l.f.Close()
	l.f = nil
}
