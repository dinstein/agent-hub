package audit

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/dinstein/agent-hub/internal/platform"
)

// Frozen file names inside the logs directory (docs/architecture.md §10).
const (
	AuditFileName    = "audit.jsonl"
	SecurityFileName = "security.jsonl"
	SavingsFileName  = "savings.jsonl"
	// DedupDirName holds the cross-process security dedup markers.
	DedupDirName = "security-dedup"
)

// Options configures Open.
type Options struct {
	// Writer applies to all three on-disk streams.
	Writer WriterOptions
	// DedupWindow is the security dedup window (0 = DefaultDedupWindow).
	DedupWindow time.Duration
}

// Streams bundles the four governance streams for one process.
type Streams struct {
	Audit    *AuditStream
	Security *SecurityStream
	Savings  *SavingsStream
	Inspect  *InspectRing
}

// Open creates the logs directory (0700) if needed and opens the three
// on-disk streams plus a fresh (disabled) inspect ring. Multiple processes
// may Open the same directory concurrently — that is the point of the
// multi-writer discipline.
func Open(logsDir string, opts Options) (*Streams, error) {
	if err := platform.EnsureDir(logsDir); err != nil {
		return nil, fmt.Errorf("audit: ensure logs dir: %w", err)
	}
	a, err := NewAuditStream(filepath.Join(logsDir, AuditFileName), opts.Writer)
	if err != nil {
		return nil, fmt.Errorf("audit: open audit stream: %w", err)
	}
	sec, err := NewSecurityStream(filepath.Join(logsDir, SecurityFileName), SecurityOptions{
		Window:   opts.DedupWindow,
		DedupDir: filepath.Join(logsDir, DedupDirName),
		Writer:   opts.Writer,
	})
	if err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("audit: open security stream: %w", err)
	}
	sav, err := NewSavingsStream(filepath.Join(logsDir, SavingsFileName), opts.Writer)
	if err != nil {
		_ = a.Close()
		_ = sec.Close()
		return nil, fmt.Errorf("audit: open savings stream: %w", err)
	}
	return &Streams{Audit: a, Security: sec, Savings: sav, Inspect: NewInspectRing()}, nil
}

// Close flushes and closes every on-disk stream, joining errors.
func (s *Streams) Close() error {
	return errors.Join(s.Audit.Close(), s.Security.Close(), s.Savings.Close())
}
