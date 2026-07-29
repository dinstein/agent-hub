package daemon_test

import (
	"context"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
)

// TestServersReflectGatewayRuntime is the production-assembly proof for the
// runtime state source: nothing here is injected by the test. A real daemon
// (daemon.Run) and a real gateway process talk over the real control socket,
// and GET /v1/servers reports what the gateway's live connection actually
// is.
//
// The regression it guards is the whole point of the wiring: before it, the
// daemon knew nothing about any downstream, so this endpoint answered
// state="unknown" / tools=0 while ComputeHealth cheerfully returned "ok" —
// one gap wearing three faces.
func TestServersReflectGatewayRuntime(t *testing.T) {
	h := startDaemon(t, nil)
	startLinkedGateway(t, h) // seeds registry server "fake" and connects it

	client := api.New(h.socket)
	defer client.Close()

	var got api.Server
	waitFor(t, "server state reported by the gateway", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		servers, err := client.Servers.List(ctx)
		if err != nil || len(servers) != 1 {
			return false
		}
		got = servers[0]
		return got.State == "connected"
	})

	// fakemcp.Minimal serves exactly one tool: the count is the live
	// catalog, not the persisted cache.
	if got.ID != "fake" || got.Tools != 1 {
		t.Errorf("server = %+v, want id fake with 1 tool", got)
	}
	if got.Health.Level != api.HealthLevelHealthy || got.Health.Summary != "ok" {
		t.Errorf("health = %+v, want healthy/ok", got.Health)
	}
}
