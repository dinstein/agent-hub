package gateway

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/platform"
)

// cacheResolver points a Resolver at a temp data dir and returns it with the
// tool-cache directory underneath it.
func cacheResolver(t *testing.T) (*platform.Resolver, string) {
	t.Helper()
	data := t.TempDir()
	t.Setenv(platform.EnvDataDir, data)
	r := platform.Default()
	dir, err := r.CacheDir()
	if err != nil {
		t.Fatalf("CacheDir: %v", err)
	}
	return r, filepath.Join(dir, toolCacheSubdir)
}

// TestForgetToolCache pins the cleanup half of `agenthub server rm`. Without
// it `agenthub tool ls` — which reads this cache offline by design — keeps
// listing a removed server's tools forever.
func TestForgetToolCache(t *testing.T) {
	r, dir := cacheResolver(t)
	c := newToolCache(dir, slog.New(slog.DiscardHandler))
	if err := c.write("gone", []mcp.ToolDef{{Name: "t"}}); err != nil {
		t.Fatalf("write gone: %v", err)
	}
	if err := c.write("stays", []mcp.ToolDef{{Name: "t"}}); err != nil {
		t.Fatalf("write stays: %v", err)
	}

	if err := ForgetToolCache(r, "gone"); err != nil {
		t.Fatalf("forget: %v", err)
	}

	got := c.load()
	if _, ok := got["gone"]; ok {
		t.Error("the removed server's catalog survived")
	}
	if _, ok := got["stays"]; !ok {
		t.Error("the cleanup crossed into another server")
	}
}

// TestForgetToolCacheDistinguishesCollidingIDs is the reason this cleanup
// reads each file instead of unlinking a derived path: fsSafe maps every
// character outside [A-Za-z0-9_-] to '_', so "a.b" and "a:b" project onto the
// SAME file name. Deleting by derived name would take out whichever of the
// two happened to own the file.
func TestForgetToolCacheDistinguishesCollidingIDs(t *testing.T) {
	if fsSafe("a.b") != fsSafe("a:b") {
		t.Skip("fsSafe no longer collides these ids; the hazard is gone")
	}
	r, dir := cacheResolver(t)
	c := newToolCache(dir, slog.New(slog.DiscardHandler))
	// Two ids, one file name: the second write wins the file, and its
	// Server field is what says who really owns it.
	if err := c.write("a.b", []mcp.ToolDef{{Name: "t"}}); err != nil {
		t.Fatalf("write a.b: %v", err)
	}
	if err := c.write("a:b", []mcp.ToolDef{{Name: "t"}}); err != nil {
		t.Fatalf("write a:b: %v", err)
	}

	// Removing the id that does NOT own the surviving file must delete
	// nothing at all.
	if err := ForgetToolCache(r, "a.b"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, ok := c.load()["a:b"]; !ok {
		t.Error("removing one server deleted a colliding server's catalog")
	}

	if err := ForgetToolCache(r, "a:b"); err != nil {
		t.Fatalf("forget owner: %v", err)
	}
	if len(c.load()) != 0 {
		t.Error("the owning server's catalog survived its own removal")
	}
}

// TestForgetToolCacheMissing pins the StateForgetter contract: no cache
// directory (no gateway has ever run) and no matching entry are both no-ops,
// never errors — `server rm` must not warn about nothing.
func TestForgetToolCacheMissing(t *testing.T) {
	r, dir := cacheResolver(t)
	if err := ForgetToolCache(r, "never-ran"); err != nil {
		t.Errorf("a missing cache dir errored: %v", err)
	}
	c := newToolCache(dir, slog.New(slog.DiscardHandler))
	if err := c.write("other", []mcp.ToolDef{{Name: "t"}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := ForgetToolCache(r, "never-connected"); err != nil {
		t.Errorf("an unmatched server errored: %v", err)
	}
	if len(c.load()) != 1 {
		t.Error("an unmatched cleanup deleted something")
	}
}
