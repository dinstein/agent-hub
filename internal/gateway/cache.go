package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/platform"
)

// toolCache persists per-server tool lists under
// <data>/cache/tools/<server>.json so a fresh gateway can answer
// tools/list before any downstream has connected (docs/flows.md).
//
// Files are written atomically (same-dir temp file, chmod 0600, rename) and
// read defensively: an unreadable entry is skipped with a log line, never
// fatal — the cache is an accelerator, not a source of truth.
//
// Writes are serialized and can be SEALED. See seal for why that is the
// mechanism that makes "a stopped gateway does not write" true.
type toolCache struct {
	dir string
	log *slog.Logger

	// mu is held for the WHOLE of write, not just the flag check. seal
	// therefore blocks until any write in flight has finished, which is what
	// makes the guarantee airtight rather than merely likely.
	mu     sync.Mutex
	sealed bool
}

// errCacheSealed is returned by write after seal. Callers treat it as an
// expected outcome of shutdown, not as a failure worth reporting.
var errCacheSealed = errors.New("gateway: tool cache sealed")

// seal permanently refuses further writes, and waits for any write already in
// progress to finish.
//
// WHY THIS EXISTS. connectAll starts one goroutine per downstream and nothing
// joins them: shutdown waits on handlers, the watcher, the credential watcher,
// the ctl link and the pool, but not on these. A connect that wins the race
// against lifeCtx cancellation goes on to call persistTools, so "the gateway
// has stopped" and "the gateway is still writing to disk" could both be true
// at once. For a product that treats on-disk state as the source of truth that
// is worse than the test flake it usually shows up as (a TempDir cleanup
// failing because <cache>/tools refilled itself): a shutdown triggered BY a
// configuration change could leave behind a catalog collected under the
// configuration that was just replaced.
//
// Sealing the resource is deliberately not the same fix as joining the
// goroutines. downstream.Connect is bounded by ConnectTimeout — 120s by
// default, sized for cold launcher caches — so a WaitGroup in shutdown would
// hand a downstream that ignores cancellation the power to hang teardown for
// two minutes. That trades a rare, bounded-size race for an unbounded stall,
// which is the worse direction. Here the wait is one file write, and the
// invariant holds no matter how long the connect goroutine outlives us.
func (c *toolCache) seal() {
	c.mu.Lock()
	c.sealed = true
	c.mu.Unlock()
}

// toolCacheSubdir is the cache subdirectory holding the per-server entries.
const toolCacheSubdir = "tools"

// LoadToolCache reads every persisted per-server tool list for an OFFLINE
// reader — `agenthub tool ls`, which must be able to show the catalog
// without spawning a gateway or connecting to anything.
//
// The format stays owned by this file: one writer, and one decode. This is
// a PROJECTION of LoadToolCacheEntries — load() drops each entry's SavedAt —
// so the two exported readers cannot come to disagree about what is on disk.
// A caller that adds a third must project it the same way rather than
// parsing the files again.
//
// A missing cache is an empty map, not an error (it only means no gateway
// has connected yet); log may be nil.
func LoadToolCache(resolver *platform.Resolver, log *slog.Logger) (map[string][]mcp.ToolDef, error) {
	if resolver == nil {
		resolver = platform.Default()
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	dir, err := resolver.CacheDir()
	if err != nil {
		return nil, err
	}
	return newToolCache(filepath.Join(dir, toolCacheSubdir), log).load(), nil
}

// ToolCacheEntry is one server's cached catalog together with the moment it
// was written.
//
// SavedAt is what separates "this server offers no tools" from "nothing has
// ever connected to it", and an hours-old catalog from one written a minute
// ago. It is carried out of this package rather than derived from the file's
// mtime by the reader: the file name is a lossy projection of the server id
// (fsSafe), so a reader that went looking for it by name would sometimes
// stat another server's entry.
type ToolCacheEntry struct {
	Tools   []mcp.ToolDef `json:"tools"`
	SavedAt time.Time     `json:"savedAt"`
}

// LoadToolCacheEntries is LoadToolCache for a reader that also needs to say
// HOW OLD the answer is — `agenthub server inspect`, which reports a cached
// catalog next to a live one and must not let the two be mistaken for each
// other. Same offline contract: a missing cache is an empty map, not an
// error, and log may be nil.
func LoadToolCacheEntries(resolver *platform.Resolver, log *slog.Logger) (map[string]ToolCacheEntry, error) {
	if resolver == nil {
		resolver = platform.Default()
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	dir, err := resolver.CacheDir()
	if err != nil {
		return nil, err
	}
	return newToolCache(filepath.Join(dir, toolCacheSubdir), log).loadEntries(), nil
}

// ForgetToolCache deletes the persisted tool list of one server — the
// cleanup half of `agenthub server rm`. Without it `agenthub tool ls`, which
// reads this cache offline by design, keeps listing a removed server's tools
// forever, and a server re-added under the same id starts life showing
// another server's catalog.
//
// It matches on the Server FIELD rather than deriving a file name from the
// id: fsSafe is a lossy projection (two ids can share one file name), so
// unlinking a derived path could delete a different server's entry. Reading
// each file to confirm ownership is the same discipline load uses.
//
// A missing cache directory or no matching entry is a no-op, not an error
// (the confops.StateForgetter contract): no gateway has ever run, or this
// server never connected.
func ForgetToolCache(resolver *platform.Resolver, serverID string) error {
	if serverID == "" {
		return errors.New("gateway: tool cache: server id must not be empty")
	}
	if resolver == nil {
		resolver = platform.Default()
	}
	dir, err := resolver.CacheDir()
	if err != nil {
		return err
	}
	toolsDir := filepath.Join(dir, toolCacheSubdir)
	entries, err := os.ReadDir(toolsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(toolsDir, e.Name())
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			// An unreadable entry may be this server's. Report it rather than
			// claim a clean sweep.
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		var cf cacheFile
		if json.Unmarshal(data, &cf) != nil || cf.Server != serverID {
			continue
		}
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) && firstErr == nil {
			firstErr = rmErr
		}
	}
	return firstErr
}

// cacheFile is the on-disk format of one entry. Server carries the real
// server ID (the file name is only its filesystem-safe projection).
type cacheFile struct {
	Server  string        `json:"server"`
	SavedAt time.Time     `json:"savedAt"`
	Tools   []mcp.ToolDef `json:"tools"`
}

func newToolCache(dir string, log *slog.Logger) *toolCache {
	return &toolCache{dir: dir, log: log}
}

// load reads every cache entry. Missing directory means empty cache.
func (c *toolCache) load() map[string][]mcp.ToolDef {
	out := make(map[string][]mcp.ToolDef)
	for id, entry := range c.loadEntries() {
		out[id] = entry.Tools
	}
	return out
}

// loadEntries reads every cache entry WITH its write time. Missing directory
// means empty cache.
func (c *toolCache) loadEntries() map[string]ToolCacheEntry {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			c.log.Warn("tool cache unreadable", "error", err)
		}
		return map[string]ToolCacheEntry{}
	}
	out := make(map[string]ToolCacheEntry, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(c.dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			c.log.Warn("tool cache entry unreadable", "path", path, "error", err)
			continue
		}
		var cf cacheFile
		if err := json.Unmarshal(data, &cf); err != nil || cf.Server == "" {
			c.log.Warn("tool cache entry malformed; ignoring", "path", path, "error", err)
			continue
		}
		out[cf.Server] = ToolCacheEntry{Tools: cf.Tools, SavedAt: cf.SavedAt}
	}
	return out
}

// write atomically persists one server's tool list. It returns
// errCacheSealed, having written nothing, once the gateway has shut down.
//
// The ladder deliberately stops at rename: no fsync of the file, and none of
// the parent directory. A reader still sees either the old entry or the whole
// new one, which is all the atomicity a cache owes; durability it does not,
// because an entry lost to a crash costs one re-fetch from the downstream, and
// two fsyncs per server on every tools/list is the wrong price for that.
func (c *toolCache) write(serverID string, tools []mcp.ToolDef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sealed {
		return errCacheSealed
	}
	if err := platform.EnsureDir(c.dir); err != nil {
		return err
	}
	data, err := json.Marshal(cacheFile{Server: serverID, SavedAt: time.Now().UTC(), Tools: tools})
	if err != nil {
		return fmt.Errorf("gateway: encode tool cache: %w", err)
	}
	final := filepath.Join(c.dir, fsSafe(serverID)+".json")
	tmp, err := os.CreateTemp(c.dir, ".tools-*.tmp")
	if err != nil {
		return fmt.Errorf("gateway: tool cache temp file: %w", err)
	}
	defer func() {
		_ = os.Remove(tmp.Name()) // no-op after a successful rename
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("gateway: tool cache chmod: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("gateway: tool cache write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("gateway: tool cache sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("gateway: tool cache close: %w", err)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return fmt.Errorf("gateway: tool cache rename: %w", err)
	}
	return nil
}
