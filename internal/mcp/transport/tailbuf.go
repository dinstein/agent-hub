package transport

import "sync"

// tailBuffer is an io.Writer retaining only the last max bytes written.
// It backs the Stderr() tail window and is safe for concurrent use (the
// os/exec copier writes while error paths snapshot).
type tailBuffer struct {
	mu   sync.Mutex
	max  int
	data []byte
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

// Write implements io.Writer; it never fails and never blocks on more than
// the mutex.
func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.max {
		// The new chunk alone fills the window.
		b.data = append(b.data[:0], p[len(p)-b.max:]...)
		return len(p), nil
	}
	b.data = append(b.data, p...)
	if over := len(b.data) - b.max; over > 0 {
		b.data = append(b.data[:0], b.data[over:]...)
	}
	return len(p), nil
}

// String returns a snapshot of the retained tail.
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
