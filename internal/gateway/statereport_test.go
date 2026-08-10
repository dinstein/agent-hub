package gateway

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/scope"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// TestConnDiagnosisScopesAndRedacts is the regression for the 2026-08-10
// sweep's cross-scope disclosure finding. The `status` meta-tool crosses back
// to an MCP caller (unlike serverStates(), which feeds the trusted daemon
// link), yet connDiagnosis reported EVERY applied server's id and the raw
// connect detail — which can quote a server URL's query, i.e. a credential.
// The report must be filtered to the caller's scope and must not render the
// raw detail.
func TestConnDiagnosisScopesAndRedacts(t *testing.T) {
	t.Parallel()
	g := newCredGateway("alpha", "beta")
	g.noteConnectResult("alpha", &transport.Error{
		StatusCode: 401,
		Err:        errors.New("GET https://alpha.example/mcp?token=ALPHASECRET -> 401"),
	})
	g.noteConnectResult("beta", &transport.Error{
		StatusCode: 500,
		Err:        errors.New("GET https://beta.example/mcp?api_key=BETASECRET failed"),
	})

	// A caller scoped to alpha only.
	es := &scope.EffectiveScope{Servers: map[string]scope.ToolView{"alpha": {}}}
	out := g.connDiagnosis(es)

	if strings.Contains(out, "beta") || strings.Contains(out, "BETASECRET") {
		t.Fatalf("out-of-scope server leaked into the status report:\n%s", out)
	}
	if strings.Contains(out, "ALPHASECRET") || strings.Contains(out, "token=") {
		t.Fatalf("raw connect detail (a credential-bearing URL) was rendered:\n%s", out)
	}
	if !strings.Contains(out, "alpha") {
		t.Fatalf("the in-scope failed server should still be reported:\n%s", out)
	}
	if !strings.Contains(out, "needs auth") {
		t.Fatalf("the actionable needs-auth hint should be kept:\n%s", out)
	}

	// A nil scope (a local session with no authority) sees every server, but
	// still no raw detail.
	all := g.connDiagnosis(nil)
	if !strings.Contains(all, "alpha") || !strings.Contains(all, "beta") {
		t.Fatalf("nil scope should report both servers:\n%s", all)
	}
	if strings.Contains(all, "ALPHASECRET") || strings.Contains(all, "BETASECRET") {
		t.Fatalf("nil scope still must not render raw detail:\n%s", all)
	}
}

func TestServerStatesClassifiesHandshakeAuthByTypedStatus(t *testing.T) {
	t.Parallel()
	g := newCredGateway("auth")
	g.noteConnectResult("auth", &transport.Error{
		StatusCode: 401,
		Err:        errors.New("credentials rejected"),
	})

	states := g.serverStates()
	if len(states) != 1 || !states[0].NeedsAuth || states[0].Conn != string(ctlapi.ConnError) {
		t.Fatalf("states = %+v, want one auth-required connection error", states)
	}

	// The body text is not authority: a proxy's 502 may quote an upstream
	// 401, and sending that operator to login would repair nothing.
	g.noteConnectResult("auth", &transport.Error{
		StatusCode: 502,
		Err:        errors.New("proxy body mentions http 401"),
	})
	states = g.serverStates()
	if len(states) != 1 || states[0].NeedsAuth {
		t.Fatalf("states = %+v, want the typed 502 to stay a generic failure", states)
	}
}

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
