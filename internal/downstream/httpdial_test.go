package downstream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/guard"
)

// dialBlocked reports whether the spec's dialer refuses addr. The dial is
// expected to fail inside the Control hook, before any packet leaves, so the
// timeout below is a backstop against a hang rather than the mechanism.
func dialBlocked(t *testing.T, spec Spec, addr string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dial := dialContextFor(spec)
	if dial == nil {
		t.Fatal("dialContextFor returned no dialer: the HTTP client would be built UNSCREENED")
	}
	conn, err := dial(ctx, "tcp", addr)
	if conn != nil {
		_ = conn.Close()
	}
	return err
}

// TestEveryHTTPSpecGetsAScreenedDialer guards the seam the whole SSRF design
// rests on.
//
// internal/mcp is standard-library only, so it cannot screen addresses itself;
// it takes an injected dialer and, given none, builds a plain one with NO
// screening at all (newHTTPClient). That combination is documented as
// tests-only, which makes internal/downstream the single place responsible for
// never producing it — and nothing tested that it did.
//
// The check is per spec SHAPE rather than a single case, because the carve-out
// below means the dialer is built differently for different specs, and "some
// specs are screened" is not the property that matters.
func TestEveryHTTPSpecGetsAScreenedDialer(t *testing.T) {
	specs := []struct {
		name string
		spec Spec
	}{
		{"bare remote", Spec{ID: "s", URL: "https://x.example/mcp"}},
		{"explicitly remote", Spec{ID: "s", URL: "https://x.example/mcp", Provenance: ProvenanceRemote}},
		{"local provenance", Spec{ID: "s", URL: "http://127.0.0.1:9/mcp", Provenance: ProvenanceLocal}},
		{"unknown provenance string", Spec{ID: "s", URL: "https://x.example/mcp", Provenance: "who-knows"}},
		{"zero value", Spec{}},
	}
	// 169.254.169.254 is the cloud metadata address: the single destination an
	// SSRF is most often pointed at, and one no spec shape may reach.
	for _, tc := range specs {
		t.Run(tc.name, func(t *testing.T) {
			err := dialBlocked(t, tc.spec, "169.254.169.254:80")
			if err == nil {
				t.Fatal("the link-local metadata address was dialable")
			}
			if !errors.Is(err, guard.ErrBlocked) {
				t.Fatalf("refused with %v, which does not unwrap to guard.ErrBlocked", err)
			}
		})
	}
}

// TestLocalProvenanceCarveOutIsOnlyLoopback pins how narrow the carve-out is.
// ProvenanceLocal buys a literal loopback address and nothing else: RFC1918,
// CGNAT and link-local stay blocked even for a server the operator called
// local, because those are the ranges cloud metadata services and intranet
// hosts live in.
func TestLocalProvenanceCarveOutIsOnlyLoopback(t *testing.T) {
	local := Spec{ID: "s", URL: "http://127.0.0.1:9/mcp", Provenance: ProvenanceLocal}

	// Refused even with the carve-out active.
	for _, addr := range []string{
		"10.0.0.1:80", "192.168.1.1:80", "172.16.0.1:80",
		"169.254.169.254:80", "100.64.0.1:80",
		"[::127.0.0.1]:80", // v4-embedding form: not a literal loopback
	} {
		t.Run("refused/"+addr, func(t *testing.T) {
			if err := dialBlocked(t, local, addr); !errors.Is(err, guard.ErrBlocked) {
				t.Fatalf("%s was not blocked for a local-provenance spec: %v", addr, err)
			}
		})
	}

	// A literal loopback IS allowed past the guard — port 9 (discard) is
	// closed, so the dial fails, but it must NOT fail as a guard block.
	for _, addr := range []string{"127.0.0.1:9", "[::1]:9"} {
		t.Run("permitted/"+addr, func(t *testing.T) {
			if err := dialBlocked(t, local, addr); errors.Is(err, guard.ErrBlocked) {
				t.Fatalf("%s was blocked despite the local carve-out", addr)
			}
		})
	}

	// Without ProvenanceLocal the same loopback address is refused.
	remote := Spec{ID: "s", URL: "http://127.0.0.1:9/mcp"}
	if err := dialBlocked(t, remote, "127.0.0.1:9"); !errors.Is(err, guard.ErrBlocked) {
		t.Fatalf("loopback was reachable without local provenance: %v", err)
	}
}
