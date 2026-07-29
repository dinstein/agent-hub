package gateway

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
)

func countCacheEntries(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read cache dir: %v", err)
	}
	return len(entries)
}

// A sealed cache accepts nothing further, and says so with a distinguishable
// error rather than a generic one — persistTools has to tell "shutting down"
// apart from "the disk is broken".
func TestToolCacheSealRefusesWrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), toolCacheSubdir)
	c := newToolCache(dir, slog.New(slog.DiscardHandler))

	if err := c.write("before", []mcp.ToolDef{{Name: "t"}}); err != nil {
		t.Fatalf("write before seal: %v", err)
	}
	if got := countCacheEntries(t, dir); got != 1 {
		t.Fatalf("entries before seal = %d, want 1", got)
	}

	c.seal()

	err := c.write("after", []mcp.ToolDef{{Name: "t"}})
	if !errors.Is(err, errCacheSealed) {
		t.Fatalf("write after seal = %v, want errCacheSealed", err)
	}
	if got := countCacheEntries(t, dir); got != 1 {
		t.Fatalf("entries after seal = %d, want the directory untouched at 1", got)
	}
	// Sealing is permanent: there is no reopening a cache whose owner is gone.
	if err := c.write("after2", nil); !errors.Is(err, errCacheSealed) {
		t.Fatalf("second write after seal = %v, want errCacheSealed", err)
	}
}

// The invariant that matters is temporal, not just logical: once seal RETURNS,
// the directory must be quiescent. This is the property the gateway's shutdown
// leans on, because connect goroutines are never joined — a check-then-write
// race inside the cache would leave exactly the window shutdown is trying to
// close, and it would show up as a TempDir cleanup failure under -race rather
// than as anything that names the real problem.
func TestToolCacheSealWaitsForInflightWrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), toolCacheSubdir)
	c := newToolCache(dir, slog.New(slog.DiscardHandler))

	const writers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Errors are fine here: half of these are expected to lose the
			// race against seal. What must NOT happen is a write landing
			// after seal has returned.
			_ = c.write("server-"+string(rune('a'+i)), []mcp.ToolDef{{Name: "t"}})
		}()
	}

	close(start)
	c.seal()

	// Sample immediately after seal returns, then let every writer finish and
	// sample again. A cache that only checked a flag would grow between these
	// two reads.
	settled := countCacheEntries(t, dir)
	wg.Wait()
	if got := countCacheEntries(t, dir); got != settled {
		t.Fatalf("cache grew from %d to %d entries after seal returned", settled, got)
	}
}
