package ctlapi

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/dinstein/agent-hub/internal/event"
)

// Runtime state source (docs/subsystems/docs/subsystems/controlplane.md). WHO observes a downstream server is
// the whole question, and the answer is: the stdio gateway, never the
// daemon.
//
// A gateway process holds the real connection to every enabled downstream of
// the client it serves. The daemon holds none: it is the control and
// coordination plane, and (until httpbridge is assembled) it never dials a
// downstream at all. Two ways to give /v1/servers a truthful `state` were on
// the table:
//
//	(a) the daemon opens its OWN probe-only connection per registry entry;
//	(b) each gateway reports the state of the connections it already has and
//	    the daemon aggregates.
//
// (b) is what this file implements. (a) would spawn a second child process
// per stdio server — permanently, whether or not any client is using it —
// double the OAuth/rate-limit footprint of every remote server, and force
// the daemon to re-assemble half the data plane (secret resolution,
// netguard, spawnguard) just to paint a status dot. Paying a real downstream
// process to display a colour is the wrong trade; the component that is
// already connected is the one that should speak.
//
// What (b) costs is the aggregation rule below: with one gateway process per
// AI client, the same server id has as many live instances as there are
// connected clients, and they can disagree.

// GatewayServerState is one downstream server's runtime condition as
// observed by ONE gateway process. It is the wire element of
// POST /v1/gateway/{sid}/servers.
//
// Conn/Token carry the ConnState/TokenState wire strings. An unrecognized
// value is deliberately NOT normalized away here: it reaches ComputeHealth
// and surfaces as "unknown connection state" (fail toward visibility).
type GatewayServerState struct {
	// ID is the registry server id (the operator-configured name, never a
	// derived-instance key).
	ID string `json:"id"`
	// Conn is the observed connection state (ConnState wire string).
	Conn string `json:"conn"`
	// Detail elaborates a non-connected state (last error text).
	Detail string `json:"detail,omitempty"`
	// Tools is the size of this instance's live tool catalog.
	Tools int `json:"tools"`
	// MissingSecrets lists unresolved secret names blocking the connection.
	MissingSecrets []string `json:"missing_secrets,omitempty"`
	// OAuthConfigError describes a broken OAuth configuration ("" = none).
	OAuthConfigError string `json:"oauth_config_error,omitempty"`
	// NeedsAuth reports a 401/403 that prevented the initial MCP handshake.
	NeedsAuth bool `json:"needs_auth,omitempty"`
	// CallAuthFailed reports auth failures observed on tool calls.
	CallAuthFailed bool `json:"call_auth_failed,omitempty"`
	// Token is the OAuth token lifecycle state (TokenState wire string).
	Token string `json:"token,omitempty"`
	// HasRefreshToken says the credential renews without a human. Absent
	// means "not known to", never "does not have one": a gateway too old to
	// send the field, or one that never looked, then produces `login`, which
	// repairs the expiry either way.
	HasRefreshToken bool `json:"has_refresh_token,omitempty"`
}

// GatewayServersReport is the body of POST /v1/gateway/{sid}/servers.
//
// It is a FULL SNAPSHOT, never a delta: a server absent from Servers is
// dropped for this session. That is what makes the report self-healing — a
// gateway that reconnects, reloads its registry, or missed a push converges
// on its next report instead of accumulating ghosts.
type GatewayServersReport struct {
	Servers []GatewayServerState `json:"servers"`
}

// ServerStateSink receives gateway runtime reports. The control-plane server
// writes into it; ServerStateSource reads out of it. They are separate
// interfaces so the daemon stays free to inject a different producer later
// (the httpbridge shared pool is the obvious second one) without the read
// side noticing.
type ServerStateSink interface {
	// ReportServers replaces sessionID's whole reported set.
	ReportServers(sessionID, clientID string, servers []GatewayServerState)
	// DropSession forgets everything sessionID reported. Called when the
	// gateway link dies: state whose observer is gone must not linger as if
	// someone were still watching.
	DropSession(sessionID string)
}

// GatewayStates aggregates the per-session reports of every registered
// gateway into the single per-server view /v1/servers renders. It satisfies
// both ServerStateSource and ServerStateSink.
//
// Aggregation rules — with one gateway process per client, N clients using
// the same server produce N independent instance states, so "the" state of a
// server has to be defined rather than assumed:
//
//   - Conn: the WORST state wins (unrecognized > error > disconnected >
//     connecting > connected). A server that is broken for one client is not
//     healthy, and rounding the disagreement toward green is exactly the
//     failure direction health.go refuses.
//   - ConnDetail names the reporting client, and says how many clients
//     reported at all, so the operator can answer "whose state is this?"
//     instead of assuming the server is globally down.
//   - Tools: the MAXIMUM. An instance that is still connecting reports 0,
//     and that 0 must not erase a catalog another instance actually listed.
//   - MissingSecrets: sorted union; OAuthConfigError: first non-empty in
//     client order; NeedsAuth / CallAuthFailed / Quarantined: logical OR;
//     Token: worst; HasRefreshToken: logical AND, because it announces a
//     repair rather than a problem (see the fold).
//     All of these are properties of the server's configuration rather than
//     of one connection, so any reporter seeing a problem is enough.
//
// Freshness is bounded by the link, not by a timer: a report lives exactly
// as long as the gateway session that sent it (DropSession on link close),
// so a stale entry cannot outlive the process that observed it.
type GatewayStates struct {
	mu      sync.Mutex
	reports map[string]gatewayReport // session id -> report
}

// gatewayReport is one session's latest snapshot.
type gatewayReport struct {
	clientID string
	servers  map[string]GatewayServerState
}

// NewGatewayStates builds an empty aggregator.
func NewGatewayStates() *GatewayStates {
	return &GatewayStates{reports: make(map[string]gatewayReport)}
}

var (
	_ ServerStateSource = (*GatewayStates)(nil)
	_ ServerStateSink   = (*GatewayStates)(nil)
)

// ReportServers implements ServerStateSink.
func (g *GatewayStates) ReportServers(sessionID, clientID string, servers []GatewayServerState) {
	byID := make(map[string]GatewayServerState, len(servers))
	for _, st := range servers {
		if st.ID == "" {
			continue // an unnamed server cannot be attributed to anything
		}
		byID[st.ID] = st
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reports[sessionID] = gatewayReport{clientID: clientID, servers: byID}
}

// DropSession implements ServerStateSink.
func (g *GatewayStates) DropSession(sessionID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.reports, sessionID)
}

// Sessions reports how many gateway sessions currently have state on file
// (diagnostics and tests; never part of the wire contract).
func (g *GatewayStates) Sessions() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.reports)
}

// reporter is one (client, state) pair contributing to an aggregate.
type reporter struct {
	client string
	sess   string
	state  GatewayServerState
}

// ServerRuntime implements ServerStateSource: fold every live report of id
// into one ServerRuntime. ok=false means NO gateway currently holds this
// server — which is the normal steady state of a daemon with no client
// attached, not a fault.
func (g *GatewayStates) ServerRuntime(id string) (ServerRuntime, bool) {
	g.mu.Lock()
	var list []reporter
	for sid, rep := range g.reports {
		if st, ok := rep.servers[id]; ok {
			list = append(list, reporter{client: rep.clientID, sess: sid, state: st})
		}
	}
	g.mu.Unlock()

	if len(list) == 0 {
		return ServerRuntime{}, false
	}
	// Deterministic order: the same set of reports must always fold to the
	// same bytes (push and pull share this payload, and tests pin it).
	slices.SortFunc(list, func(a, b reporter) int {
		return cmp.Or(cmp.Compare(a.client, b.client), cmp.Compare(a.sess, b.sess))
	})

	worst := 0
	var rt ServerRuntime
	secrets := map[string]struct{}{}
	for i, r := range list {
		if sev := connSeverity(ConnState(r.state.Conn)); i == 0 || sev > worst {
			worst = sev
			rt.Conn = ConnState(r.state.Conn)
			rt.ConnDetail = r.state.Detail
		}
		rt.Tools = max(rt.Tools, r.state.Tools)
		for _, s := range r.state.MissingSecrets {
			secrets[s] = struct{}{}
		}
		if rt.OAuthConfigError == "" {
			rt.OAuthConfigError = r.state.OAuthConfigError
		}
		rt.NeedsAuth = rt.NeedsAuth || r.state.NeedsAuth
		rt.CallAuthFailed = rt.CallAuthFailed || r.state.CallAuthFailed
		if tokenSeverity(TokenState(r.state.Token)) > tokenSeverity(rt.Token) {
			rt.Token = TokenState(r.state.Token)
		}
		// AND, not OR — the one field here folded that way. Every other flag
		// reports a PROBLEM, where one witness is enough; this one reports
		// that a repair is available, and offering `agenthub auth refresh` to
		// someone who has no refresh token hands them a command that fails.
		// A reporter that says nothing (an older gateway, one that never
		// looked) therefore drags the answer to login, which repairs the
		// expiry either way.
		rt.HasRefreshToken = (i == 0 || rt.HasRefreshToken) && r.state.HasRefreshToken
	}
	if len(secrets) > 0 {
		rt.MissingSecrets = make([]string, 0, len(secrets))
		for s := range secrets {
			rt.MissingSecrets = append(rt.MissingSecrets, s)
		}
		slices.Sort(rt.MissingSecrets)
	}
	rt.ConnDetail = attributeDetail(rt.ConnDetail, winnerOf(list, worst), len(list))
	return rt, true
}

// winnerOf returns the client id of the first reporter at severity worst
// (list is already in deterministic order).
func winnerOf(list []reporter, worst int) string {
	for _, r := range list {
		if connSeverity(ConnState(r.state.Conn)) == worst {
			return r.client
		}
	}
	return ""
}

// attributeDetail prefixes the winning reporter's detail with WHO reported
// it. With a single reporter and no detail the result stays empty: naming a
// client that has nothing to say is noise, and ComputeHealth only renders
// Detail on the rungs that already went wrong.
func attributeDetail(detail, client string, reporters int) string {
	switch {
	case client == "":
		return detail
	case reporters > 1 && detail != "":
		return fmt.Sprintf("%s (worst of %d reporting clients): %s", client, reporters, detail)
	case reporters > 1:
		return fmt.Sprintf("%s (worst of %d reporting clients)", client, reporters)
	case detail != "":
		return client + ": " + detail
	}
	return ""
}

// connSeverity ranks connection states for the worst-wins fold. An
// unrecognized value ranks ABOVE every known one so a state source bug
// reaches the operator through ComputeHealth's default branch instead of
// being averaged away.
func connSeverity(c ConnState) int {
	switch c {
	case ConnUnknown:
		return 0
	case ConnConnected:
		return 1
	case ConnConnecting:
		return 2
	case ConnDisconnected:
		return 3
	case ConnError:
		return 4
	default:
		return 5
	}
}

// tokenSeverity ranks token states for the worst-wins fold.
func tokenSeverity(t TokenState) int {
	switch t {
	case TokenOK:
		return 0
	case TokenExpiring:
		return 1
	case TokenExpired:
		return 2
	case TokenRevoked:
		return 3
	default:
		return 4
	}
}

// TopicServerRuntime is the bus topic a runtime report publishes on. The
// "server." prefix maps onto the coalesced `servers` SSE topic, so a
// frontend watching /v1/events sees state changes without polling; the 50ms
// coalescer coping with a connect storm is exactly why the payload is built
// lazily there.
const TopicServerRuntime event.Topic = "server.runtime"

// handleGatewayServers implements POST /v1/gateway/{sid}/servers: one
// gateway's full runtime snapshot.
//
// Deliberately not recorded anywhere. It is an observation that changes
// nothing an operator can be held to, and every connect and refresh of
// every client would land there.
func (s *Server) handleGatewayServers(w http.ResponseWriter, r *http.Request, sid string) {
	reqID := requestIDFrom(r.Context())
	link, ok := s.gatewayFor(sid)
	if !ok {
		writeNotFound(w, r)
		return
	}
	if s.opts.ServerReports == nil {
		// A daemon assembled without a state sink has no place to put this;
		// "unavailable on this daemon" is the uniform 404 shape frontends
		// already handle (same rule as the other optional surfaces).
		writeNotFound(w, r)
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", reqID)
		return
	}
	var report GatewayServersReport
	if err := json.Unmarshal(body, &report); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"decoding servers report body: "+err.Error(), "", reqID)
		return
	}
	s.opts.ServerReports.ReportServers(sid, link.clientID, report.Servers)
	s.opts.Bus.Publish(event.Event{Topic: TopicServerRuntime, Key: sid})
	writeOK(w, http.StatusOK, struct{}{})
}

// dropServerReports forgets a dead session's runtime state and tells
// subscribers to re-read. Called from the single link-cleanup path.
func (s *Server) dropServerReports(sid string) {
	if s.opts.ServerReports == nil {
		return
	}
	s.opts.ServerReports.DropSession(sid)
	s.opts.Bus.Publish(event.Event{Topic: TopicServerRuntime, Key: sid})
}
