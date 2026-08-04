package downstream_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/guard"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
)

// TestSpawnIsScreenedByDefault is the regression for a gap that was invisible
// from either side: transport.StdioConfig.Screen was implemented and called on
// both spawn paths, spawnguard was implemented and tested exhaustively, and
// nothing in production ever joined the two. Deps.Spawn was declared `any`
// with the comment "reserved for M1 spawnguard wiring; nil today" and had no
// reader, so every host-runtime command spawned unscreened while the guard's
// own test suite stayed green.
//
// A zero Deps is the case that matters. An assembly that does not mention
// screening is the one that will exist next, so that is the one asserted here.
func TestSpawnIsScreenedByDefault(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		spec downstream.Spec
	}{
		{
			name: "shell -c",
			spec: downstream.Spec{ID: "s", Kind: transport.Stdio, Command: "sh", Args: []string{"-c", "echo hi"}},
		},
		{
			name: "LD_PRELOAD in env",
			spec: downstream.Spec{
				ID: "s", Kind: transport.Stdio, Command: "/bin/echo",
				Env: map[string]string{"LD_PRELOAD": "/tmp/evil.so"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := downstream.Connect(context.Background(), tc.spec, downstream.Deps{})
			if err == nil {
				t.Fatal("Connect with a zero Deps spawned a guard-tripping command")
			}
			if !errors.Is(err, guard.ErrBlocked) {
				t.Fatalf("error %v does not unwrap to guard.ErrBlocked — it was refused for some other reason, "+
					"which would keep passing if the screen were removed again", err)
			}
		})
	}
}

// TestBlockedEnvSaysWhereTheVariableCameFrom covers the half of the diagnosis
// spawnguard cannot make. The guard sees one flat environment; only this layer
// knows whether the refused variable was declared by the entry or inherited
// from the process agenthub was started in, and the two have opposite fixes.
//
// The inherited case is the one that costs real time: the operator greps the
// registry for the variable, finds nothing, and stops believing the message.
func TestBlockedEnvSaysWhereTheVariableCameFrom(t *testing.T) {
	t.Run("declared by the entry", func(t *testing.T) {
		t.Parallel()
		_, err := downstream.Connect(context.Background(),
			downstream.Spec{
				ID: "s", Kind: transport.Stdio, Command: "/bin/echo",
				Env: map[string]string{"LD_PRELOAD": "/tmp/evil.so"},
			}, downstream.Deps{})
		if !errors.Is(err, guard.ErrBlocked) {
			t.Fatalf("error %v does not unwrap to guard.ErrBlocked", err)
		}
		if got := err.Error(); !strings.Contains(got, "own env block") {
			t.Fatalf("error %q does not say the entry declared LD_PRELOAD", got)
		}
	})

	t.Run("inherited from the process environment", func(t *testing.T) {
		// Setenv makes this the real path rather than a simulated one: the
		// variable reaches the child through buildEnv's os.Environ() copy,
		// which is how it arrives in production. Setenv also forbids
		// t.Parallel() here, so this subtest deliberately runs serially.
		t.Setenv("LD_PRELOAD", "/tmp/evil.so")
		_, err := downstream.Connect(context.Background(),
			downstream.Spec{ID: "s", Kind: transport.Stdio, Command: "/bin/echo"},
			downstream.Deps{})
		if !errors.Is(err, guard.ErrBlocked) {
			t.Fatalf("error %v does not unwrap to guard.ErrBlocked", err)
		}
		got := err.Error()
		if !strings.Contains(got, "inherited") {
			t.Fatalf("error %q does not say the variable was inherited", got)
		}
		if strings.Contains(got, "own env block sets") {
			t.Fatalf("error %q blames the entry for a variable it never declared", got)
		}
	})
}

// TestSpawnScreenIsInjectableAndDefeatable pins both explicit forms, so that
// the default above is demonstrably a default rather than a hard-coded refusal.
func TestSpawnScreenIsInjectableAndDefeatable(t *testing.T) {
	t.Parallel()

	t.Run("injected screen is consulted", func(t *testing.T) {
		t.Parallel()
		sentinel := errors.New("screened")
		var sawCommand string
		_, err := downstream.Connect(context.Background(),
			downstream.Spec{ID: "s", Kind: transport.Stdio, Command: "/bin/echo"},
			downstream.Deps{Spawn: func(command string, _, _ []string) error {
				sawCommand = command
				return sentinel
			}})
		if !errors.Is(err, sentinel) {
			t.Fatalf("Connect error = %v, want the injected screen's error", err)
		}
		if sawCommand != "/bin/echo" {
			t.Fatalf("screen saw command %q, want the final host command", sawCommand)
		}
	})

	t.Run("SpawnUnscreened reaches the spawn", func(t *testing.T) {
		t.Parallel()
		// The command still fails to speak MCP; what matters is that it is
		// no longer refused by the guard before it is ever started.
		_, err := downstream.Connect(context.Background(),
			downstream.Spec{ID: "s", Kind: transport.Stdio, Command: "sh", Args: []string{"-c", "exit 0"}},
			downstream.Deps{SpawnUnscreened: true})
		if errors.Is(err, guard.ErrBlocked) {
			t.Fatalf("SpawnUnscreened still screened: %v", err)
		}
	})
}
