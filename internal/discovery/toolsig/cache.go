package toolsig

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// DefaultCacheSize bounds the shared cache. A large deployment aggregates a
// few hundred tools; 4096 entries covers that with room for catalog churn,
// and every entry is a short string.
const DefaultCacheSize = 4096

// Cache memoizes signatures on a fingerprint of the inputs Of actually reads.
// Rendering is cheap but it happens once per tool per surface rebuild, and a
// surface is rebuilt on every scope or catalog change — the cache is what
// keeps that O(tools) work off the request path (docs/modules/dataplane.md:
// "memoize by tool fingerprint, warm during catalog indexing").
//
// Safe for concurrent use.
//
// Eviction is a full flush at DefaultCacheSize rather than an LRU. The access
// pattern is "the same few hundred fingerprints, over and over, until the
// catalog changes": an LRU's bookkeeping would cost more than the occasional
// re-render after a flush, and a flush cannot leak a stale entry the way a
// mis-ordered LRU can.
type Cache struct {
	mu   sync.Mutex
	max  int
	sigs map[[32]byte]Signature
}

// NewCache builds a cache holding at most max entries (<= 0 means
// DefaultCacheSize).
func NewCache(max int) *Cache {
	if max <= 0 {
		max = DefaultCacheSize
	}
	return &Cache{max: max, sigs: make(map[[32]byte]Signature)}
}

var (
	sharedOnce sync.Once
	sharedCase *Cache
)

// Shared returns the process-wide cache. docs/modules/dataplane.md calls for a single
// instance: the catalog index warms it, and a second instance would silently
// throw that warm-up away (correct results, wasted work). Callers should use
// Shared unless a test needs isolation.
func Shared() *Cache {
	sharedOnce.Do(func() { sharedCase = NewCache(DefaultCacheSize) })
	return sharedCase
}

// Of returns the memoized signature of def under its own name.
func (c *Cache) Of(def mcp.ToolDef) Signature { return c.OfWith(def.Name, def, Options{}) }

// OfNamed returns the memoized signature of def under an explicit name — the
// exposed (namespaced) name callers actually invoke.
func (c *Cache) OfNamed(name string, def mcp.ToolDef) Signature {
	return c.OfWith(name, def, Options{})
}

// OfWith returns the memoized signature of def under opts. A nil cache
// renders without memoizing, so a zero-valued struct field is usable rather
// than a panic.
func (c *Cache) OfWith(name string, def mcp.ToolDef, opts Options) Signature {
	if c == nil {
		return Named(name, def, opts)
	}
	key := fingerprint(name, def, opts)

	c.mu.Lock()
	sig, hit := c.sigs[key]
	c.mu.Unlock()
	if hit {
		return sig
	}

	sig = Named(name, def, opts)

	c.mu.Lock()
	if len(c.sigs) >= c.max {
		c.sigs = make(map[[32]byte]Signature, c.max)
	}
	c.sigs[key] = sig
	c.mu.Unlock()
	return sig
}

// Warm renders a whole catalog into the cache. Called at index build time so
// the first search of a session does not pay for the rendering.
func (c *Cache) Warm(defs []mcp.ToolDef, opts Options) {
	for _, def := range defs {
		c.OfWith(def.Name, def, opts)
	}
}

// Len reports the number of memoized entries (diagnostics and tests).
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sigs)
}

// fingerprint hashes exactly the inputs Of reads, each length-prefixed so no
// two different input tuples can produce the same byte stream (a tool named
// "a" with schema "bc" must not collide with "ab" and "c").
func fingerprint(name string, def mcp.ToolDef, opts Options) [32]byte {
	h := sha256.New()
	write := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	write([]byte(name))
	write(def.InputSchema)
	write(def.OutputSchema)
	var m [8]byte
	binary.BigEndian.PutUint64(m[:], uint64(opts.maxBytes()))
	h.Write(m[:])

	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
