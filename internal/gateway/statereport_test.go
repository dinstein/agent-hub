package gateway

import (
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// TestCtlLinkReportsServerRuntime is the production wiring of BACKLOG #1 end
// to end: a real gateway with a real downstream, a real control plane with
// the real aggregator, and /v1/servers' inputs derived from an actual
// connection instead of a placeholder.
//
// One server connects, one is scripted to fail its dial. Both must reach the
// daemon: the failure is the case that used to be indistinguishable from
// "not connected yet".
func TestCtlLinkReportsServerRuntime(t *testing.T) {
	t.Parallel()
	resolver, socket := linkResolver(t, t.TempDir())
	states := ctlapi.NewGatewayStates()
	startCtlServer(t, socket, func(o *ctlapi.Options) {
		o.States = states
		o.ServerReports = states
	})

	seedRegistry(t, resolver, "good", "bad")
	_, c, _ := startGateway(t, Config{
		ClientID: "cursor",
		Resolver: resolver,
		// "bad" has no script: scriptedDial fails it, which is the shape of
		// a downstream whose binary is missing.
		Dial:      scriptedDial(map[string]*fakemcp.Script{"good": fakemcp.Minimal("echo")}),
		LinkRetry: 50 * time.Millisecond,
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})

	waitCond(t, "connected server reported with its tool count", func() bool {
		rt, ok := states.ServerRuntime("good")
		return ok && rt.Conn == ctlapi.ConnConnected && rt.Tools == 1
	})
	waitCond(t, "failed server reported as an error", func() bool {
		rt, ok := states.ServerRuntime("bad")
		return ok && rt.Conn == ctlapi.ConnError && rt.ConnDetail != ""
	})

	// The detail must name the client, so an operator reading it knows
	// whose instance failed rather than assuming the server is down for all.
	rt, _ := states.ServerRuntime("bad")
	if want := "cursor: "; len(rt.ConnDetail) < len(want) || rt.ConnDetail[:len(want)] != want {
		t.Errorf("detail = %q, want it to start with %q", rt.ConnDetail, want)
	}

	// Health now reflects reality on both: green with a tool count, and red
	// with a reason — the triple the daemon could not produce before.
	if h := ctlapi.ComputeHealth(ctlapi.HealthInput{Conn: rt.Conn, ConnDetail: rt.ConnDetail}); h.Summary != "connection error" {
		t.Errorf("bad health = %+v, want connection error", h)
	}
}
