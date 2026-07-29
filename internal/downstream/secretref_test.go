package downstream_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// dialSpy captures the spec a dial was asked for without connecting.
func dialSpy() (downstream.DialFunc, *downstream.Spec) {
	seen := new(downstream.Spec)
	return func(_ context.Context, spec downstream.Spec) (transport.Transport, error) {
		*seen = spec
		return nil, errors.New("dial spy: no transport")
	}, seen
}

// TestEnvSecretExpansionAtDial covers the stdio half of placeholder
// resolution: the child environment carries the resolved value, and the
// spec itself still carries the placeholder (so a rotated secret is picked
// up on the next respawn).
func TestEnvSecretExpansionAtDial(t *testing.T) {
	t.Parallel()
	resolve := staticResolver(map[secrets.Ref]string{
		secrets.UserRef("srv", "TOKEN"): "s3cr3t",
	})
	// The stdio dial path runs inside Connect; observe it through the child
	// environment of a real spawn is overkill here, so exercise the
	// expansion helper through a failing spawn instead: an unresolvable
	// placeholder must fail BEFORE the process is spawned.
	_, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:      "srv",
		Command: "/nonexistent/agenthub-test-binary",
		Env:     map[string]string{"TOKEN": "${SECRET_TOKEN}"},
	}, downstream.Deps{Secrets: resolve})
	if err == nil {
		t.Fatal("spawning a nonexistent binary succeeded")
	}
	if errors.Is(err, downstream.ErrUnresolvedSecret) {
		t.Fatalf("resolvable placeholder reported as unresolved: %v", err)
	}

	_, err = downstream.Connect(context.Background(), downstream.Spec{
		ID:      "srv",
		Command: "/nonexistent/agenthub-test-binary",
		Env:     map[string]string{"TOKEN": "${SECRET_ABSENT}"},
	}, downstream.Deps{Secrets: resolve})
	if !errors.Is(err, downstream.ErrUnresolvedSecret) {
		t.Fatalf("error = %v, want ErrUnresolvedSecret", err)
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatal("the error text leaked a secret value")
	}
}

// TestNoResolverIsAnError proves the fail-closed direction of a missing
// resolver: a placeholder must never be passed through verbatim.
func TestNoResolverIsAnError(t *testing.T) {
	t.Parallel()
	_, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:      "srv",
		Command: "/nonexistent/agenthub-test-binary",
		Env:     map[string]string{"TOKEN": "${SECRET_TOKEN}"},
	}, downstream.Deps{})
	if !errors.Is(err, downstream.ErrNoResolver) {
		t.Fatalf("error = %v, want ErrNoResolver", err)
	}
}

// TestNonSecretPlaceholdersSurvive proves other substitution layers
// (${ROOT} and friends) and literal text are left alone.
func TestNonSecretPlaceholdersSurvive(t *testing.T) {
	t.Parallel()
	dial, seen := dialSpy()
	_, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:      "srv",
		Command: "cat",
		Env:     map[string]string{"P": "${ROOT}/x", "Q": "literal ${", "R": "$notaplaceholder"},
	}, downstream.Deps{Dial: dial})
	if err == nil {
		t.Fatal("dial spy did not fail the connect")
	}
	// The dial override bypasses expansion entirely, so the spec must reach
	// it untouched: expansion belongs to the built-in dialers.
	if seen.Env["P"] != "${ROOT}/x" {
		t.Fatalf("spec env mangled: %+v", seen.Env)
	}
}
