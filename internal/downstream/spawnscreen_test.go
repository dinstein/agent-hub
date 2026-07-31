package downstream_test

import (
	"context"
	"errors"
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
