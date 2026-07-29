package ctlapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dinstein/agent-hub/api"
)

// report posts one runtime snapshot on the gateway face, exactly as a real
// gateway process does.
func (fg *fakeGateway) report(states ...GatewayServerState) {
	fg.t.Helper()
	body, _ := json.Marshal(GatewayServersReport{Servers: states})
	resp, err := fg.hc.Post("http://d/v1/gateway/"+fg.sid+"/servers",
		"application/json", bytes.NewReader(body))
	if err != nil {
		fg.t.Fatalf("report: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fg.t.Fatalf("report status = %d, want 200", resp.StatusCode)
	}
}

// TestServersReflectGatewayReports is the end-to-end proof of the wiring
// this file exists for: a registered gateway reports what it actually sees,
// and GET /v1/servers answers with it instead of the old "unknown / 0 tools
// / green" triple.
func TestServersReflectGatewayReports(t *testing.T) {
	states := NewGatewayStates()
	client, env := startServer(t, func(o *Options) {
		o.States = states
		o.ServerReports = states
	})
	seedServer(t, env.reg, "elk", true)
	seedServer(t, env.reg, "docs", true)

	// Before any gateway exists: nothing is connected and nothing is
	// claimed. "unknown" plus "not observed" — never "ok".
	before, err := client.Servers.List(t.Context())
	if err != nil {
		t.Fatalf("Servers.List: %v", err)
	}
	for _, s := range before {
		if s.State != "unknown" || s.Tools != 0 {
			t.Errorf("pre-report %s = state %q tools %d, want unknown/0", s.ID, s.State, s.Tools)
		}
		if s.Health.Level != api.HealthLevelHealthy || s.Health.Summary != "not observed" {
			t.Errorf("pre-report %s health = %+v, want healthy/not observed", s.ID, s.Health)
		}
	}

	fg := registerFakeGateway(t, env.sock, "claude-code")
	defer fg.closeLink()
	fg.openLink()

	fg.report(
		GatewayServerState{ID: "elk", Conn: string(ConnConnected), Tools: 5},
		GatewayServerState{ID: "docs", Conn: string(ConnError), Detail: "spawn: no such file"},
	)

	after := serversByID(t, client)
	if got := after["elk"]; got.State != "connected" || got.Tools != 5 {
		t.Errorf("elk = state %q tools %d, want connected/5", got.State, got.Tools)
	}
	if got := after["elk"].Health; got.Level != api.HealthLevelHealthy || got.Summary != "ok" {
		t.Errorf("elk health = %+v, want healthy/ok", got)
	}
	if got := after["docs"]; got.State != "error" {
		t.Errorf("docs state = %q, want error", got.State)
	}
	// The detail names WHO saw it: a single-client failure must not read as
	// a global verdict.
	if got := after["docs"].Health; got.Level != api.HealthLevelUnhealthy ||
		got.Detail != "claude-code: spawn: no such file" {
		t.Errorf("docs health = %+v", got)
	}

	// A report is a full snapshot, not a delta: dropping "docs" from it
	// retracts that server's state instead of freezing the last error.
	fg.report(GatewayServerState{ID: "elk", Conn: string(ConnConnected), Tools: 7})
	after = serversByID(t, client)
	if got := after["elk"]; got.Tools != 7 {
		t.Errorf("elk tools = %d, want 7", got.Tools)
	}
	if got := after["docs"]; got.State != "unknown" || got.Health.Summary != "not observed" {
		t.Errorf("docs after retraction = %+v", got)
	}

	// The observer leaving takes its observations with it.
	fg.closeLink()
	waitFor(t, "report dropped with the link", func() bool { return states.Sessions() == 0 })
	gone := serversByID(t, client)
	if got := gone["elk"]; got.State != "unknown" || got.Tools != 0 || got.Health.Summary != "not observed" {
		t.Errorf("elk after link close = %+v", got)
	}
}

// TestGatewayReportsAggregateAcrossClients pins the multi-instance rule:
// one gateway process per client means the same server has several
// independent states, and the fold must be worst-wins with the tool count
// preserved and the reporter named.
func TestGatewayReportsAggregateAcrossClients(t *testing.T) {
	states := NewGatewayStates()
	client, env := startServer(t, func(o *Options) {
		o.States = states
		o.ServerReports = states
	})
	seedServer(t, env.reg, "elk", true)

	healthy := registerFakeGateway(t, env.sock, "claude-code")
	defer healthy.closeLink()
	healthy.openLink()
	broken := registerFakeGateway(t, env.sock, "cursor")
	defer broken.closeLink()
	broken.openLink()

	healthy.report(GatewayServerState{ID: "elk", Conn: string(ConnConnected), Tools: 5})
	broken.report(GatewayServerState{ID: "elk", Conn: string(ConnError), Detail: "connection refused"})

	got := serversByID(t, client)["elk"]
	if got.State != "error" {
		t.Errorf("state = %q, want error (worst wins)", got.State)
	}
	// The healthy instance's catalog survives the broken one's zero.
	if got.Tools != 5 {
		t.Errorf("tools = %d, want 5 (max across reporters)", got.Tools)
	}
	want := "cursor (worst of 2 reporting clients): connection refused"
	if got.Health.Detail != want {
		t.Errorf("detail = %q, want %q", got.Health.Detail, want)
	}

	// The broken client goes away: the server is fine again, and the
	// detail stops naming anybody.
	broken.closeLink()
	waitFor(t, "broken client forgotten", func() bool { return states.Sessions() == 1 })
	got = serversByID(t, client)["elk"]
	if got.State != "connected" || got.Health.Summary != "ok" || got.Health.Detail != "" {
		t.Errorf("after the broken client left: %+v", got)
	}
}

// TestGatewayStatesFold covers the folding rules that the wire tests above
// do not reach: unions, ORs, worst-token, and the deliberately un-normalized
// unrecognized state.
func TestGatewayStatesFold(t *testing.T) {
	g := NewGatewayStates()
	if _, ok := g.ServerRuntime("nobody"); ok {
		t.Error("empty aggregator reported knowledge of a server")
	}

	g.ReportServers("s1", "claude-code", []GatewayServerState{{
		ID: "elk", Conn: string(ConnConnected), Tools: 5,
		MissingSecrets: []string{"B"}, Token: string(TokenExpiring),
	}})
	g.ReportServers("s2", "cursor", []GatewayServerState{{
		ID: "elk", Conn: string(ConnConnecting), Tools: 0,
		MissingSecrets: []string{"A", "B"}, CallAuthFailed: true,
		Token: string(TokenExpired), OAuthConfigError: "bad issuer",
	}})

	rt, ok := g.ServerRuntime("elk")
	if !ok {
		t.Fatal("no runtime for a reported server")
	}
	if rt.Conn != ConnConnecting {
		t.Errorf("conn = %q, want connecting (worst of connected/connecting)", rt.Conn)
	}
	if rt.Tools != 5 {
		t.Errorf("tools = %d, want 5", rt.Tools)
	}
	if len(rt.MissingSecrets) != 2 || rt.MissingSecrets[0] != "A" || rt.MissingSecrets[1] != "B" {
		t.Errorf("missing secrets = %v, want sorted union [A B]", rt.MissingSecrets)
	}
	if !rt.CallAuthFailed || rt.OAuthConfigError != "bad issuer" || rt.Token != TokenExpired {
		t.Errorf("folded runtime = %+v", rt)
	}

	// An unrecognized state outranks every known one so ComputeHealth's
	// default branch surfaces it (fail toward visibility, not green).
	g.ReportServers("s3", "zed", []GatewayServerState{{ID: "elk", Conn: "sideways"}})
	rt, _ = g.ServerRuntime("elk")
	if rt.Conn != ConnState("sideways") {
		t.Errorf("conn = %q, want the unrecognized value preserved", rt.Conn)
	}
	if h := ComputeHealth(HealthInput{Conn: rt.Conn}); h.Level != api.HealthLevelUnhealthy {
		t.Errorf("unrecognized state health = %+v, want unhealthy", h)
	}

	g.DropSession("s2")
	g.DropSession("s3")
	rt, _ = g.ServerRuntime("elk")
	if rt.Conn != ConnConnected || rt.Token != TokenExpiring || rt.CallAuthFailed {
		t.Errorf("after drops = %+v, want only s1's contribution", rt)
	}
}

// TestGatewayReportWithoutSinkIs404 pins the half-wired-daemon shape: a
// server assembled without a state sink answers the uniform 404 rather than
// pretending the report landed.
func TestGatewayReportWithoutSinkIs404(t *testing.T) {
	_, env := startServer(t, nil)
	fg := registerFakeGateway(t, env.sock, "cursor")
	defer fg.closeLink()
	fg.openLink()

	body, _ := json.Marshal(GatewayServersReport{
		Servers: []GatewayServerState{{ID: "elk", Conn: string(ConnConnected)}},
	})
	resp, err := fg.hc.Post("http://d/v1/gateway/"+fg.sid+"/servers",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// serversByID fetches /v1/servers and indexes it.
func serversByID(t *testing.T, client *api.Client) map[string]api.Server {
	t.Helper()
	list, err := client.Servers.List(t.Context())
	if err != nil {
		t.Fatalf("Servers.List: %v", err)
	}
	out := make(map[string]api.Server, len(list))
	for _, s := range list {
		out[s.ID] = s
	}
	return out
}
