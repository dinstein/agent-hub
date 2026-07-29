package approval

import (
	"context"
	"testing"
)

// TestAllowlistForgetServer covers the cleanup half of `agenthub server rm`
// and, above all, its BOUNDARY: which grants are the removed server's.
func TestAllowlistForgetServer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a, err := OpenAllowlist(dir)
	if err != nil {
		t.Fatalf("OpenAllowlist: %v", err)
	}

	bound := Entry{Fingerprint: "fp-gone", Server: "gone", Tool: "t"}
	neighbour := Entry{Fingerprint: "fp-stays", Server: "stays", Tool: "t"}
	// No Server: bound by fingerprint alone, deliberately cross-server.
	unbound := Entry{Fingerprint: "fp-any", Tool: "t"}
	for _, e := range []Entry{bound, neighbour, unbound} {
		if err := a.Add(e); err != nil {
			t.Fatalf("seed %s: %v", e.Fingerprint, err)
		}
	}

	if err := a.ForgetServer(ctx, "gone"); err != nil {
		t.Fatalf("forget: %v", err)
	}

	// The grant the removed server earned is gone: a server re-added under
	// that id must face a human again rather than inherit "always allow".
	if a.Match(Request{Fingerprint: "fp-gone", Server: "gone", Tool: "t"}) {
		t.Error("a remember-forever grant survived its server")
	}
	if !a.Match(Request{Fingerprint: "fp-stays", Server: "stays", Tool: "t"}) {
		t.Error("the cleanup crossed into another server's grant")
	}
	// An unbound grant is bound by fingerprint, not by server. Removing a
	// server may narrow the allowlist; it may not reinterpret what an
	// operator scoped deliberately.
	if !a.Match(Request{Fingerprint: "fp-any", Server: "anyone", Tool: "t"}) {
		t.Error("a server-agnostic grant was revoked by an unrelated removal")
	}

	// The removal must be durable, not just in-memory.
	reopened, err := OpenAllowlist(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Match(Request{Fingerprint: "fp-gone", Server: "gone", Tool: "t"}) {
		t.Error("the grant came back after a reload")
	}
}

// TestAllowlistForgetServerNoMatch pins the StateForgetter contract: a server
// with no grants is a no-op, never an error — `server rm` must not warn about
// a store that simply had nothing to clean.
func TestAllowlistForgetServerNoMatch(t *testing.T) {
	a, err := OpenAllowlist(t.TempDir())
	if err != nil {
		t.Fatalf("OpenAllowlist: %v", err)
	}
	if err := a.ForgetServer(context.Background(), "never-existed"); err != nil {
		t.Errorf("forgetting an unknown server errored: %v", err)
	}
}
