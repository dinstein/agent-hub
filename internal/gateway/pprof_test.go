package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/diag"
)

// TestRunRefusesNonLoopbackProfilingAddr proves the refusal reaches the
// process rather than stopping inside internal/diag. A gateway asked to
// publish profiles at an address it cannot serve safely must fail to start:
// running on without the endpoint would leave the operator attached to a
// port that never answers, reading a healthy process as wedged.
//
// It is also the wiring proof — ErrNotLoopback can only come from the
// assembly step this test exists to pin.
func TestRunRefusesNonLoopbackProfilingAddr(t *testing.T) {
	t.Setenv(diag.EnvAddr, "0.0.0.0:0")

	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	err := Run(ctx, Config{
		ClientID: "cursor",
		In:       strings.NewReader(""),
		Out:      io.Discard,
		Resolver: resolver,
		Log:      slog.New(slog.DiscardHandler),
	})
	if !errors.Is(err, diag.ErrNotLoopback) {
		t.Fatalf("Run() error = %v, want ErrNotLoopback", err)
	}
}

// TestRunWithoutProfilingAddrStartsNothing pins the ordinary case: with the
// variable unset the gateway assembles and runs exactly as before, ending on
// the empty client stream rather than on a profiling error.
func TestRunWithoutProfilingAddrStartsNothing(t *testing.T) {
	t.Setenv(diag.EnvAddr, "")

	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	if err := Run(ctx, Config{
		ClientID: "cursor",
		In:       strings.NewReader(""),
		Out:      io.Discard,
		Resolver: resolver,
		Log:      slog.New(slog.DiscardHandler),
	}); err != nil {
		t.Fatalf("Run() with no profiling address = %v, want a clean end", err)
	}
}
