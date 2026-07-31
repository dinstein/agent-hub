package daemon_test

import (
	"context"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/daemon"
)

// A graceful stop with a gateway attached must DRAIN, not wait out its grace
// period and force-close what it was waiting for. The link is the connection
// that never ends by itself, so it is also the only one that proves the
// stop ended the streams rather than outlasting them.
//
// This test uses the PRODUCTION grace deliberately. Every other daemon test
// shrinks it to 200ms, and the comment where they do says why: "tests must
// not spend the full production drain grace on every stop while a gateway
// link SSE connection is open". That workaround is the bug, observed from
// the inside — with a real grace the stop always took all of it.
func TestGracefulStopDrainsWithAGatewayAttached(t *testing.T) {
	h := startDaemon(t, func(cfg *daemon.Config) {
		cfg.ShutdownGrace = daemon.DefaultShutdownGrace // 5s
	})
	startLinkedGateway(t, h)

	client := api.New(h.socket)
	defer client.Close()
	waitFor(t, "gateway registration", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		sessions, err := client.Sessions.List(ctx)
		return err == nil && len(sessions) == 1
	})

	start := time.Now()
	if err := h.stop(t); err != nil {
		t.Fatalf("daemon stop: %v", err)
	}
	elapsed := time.Since(start)
	// Well under the 5s grace, well above the work: a stop that still waits
	// the streams out cannot land here, and neither can one merely made
	// faster by a smaller grace, because this one is the production value.
	if elapsed > 2*time.Second {
		t.Fatalf("graceful stop took %v with a grace of %v — the drain waited "+
			"the streams out instead of ending them", elapsed, daemon.DefaultShutdownGrace)
	}
	t.Logf("stopped in %v (grace %v)", elapsed, daemon.DefaultShutdownGrace)
}
