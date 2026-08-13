package gateway

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/scope"
)

// reportTimeout bounds one runtime-state push to the daemon. The report is
// pure coordination: it must never hold up a connect, a refresh or a
// reconnect, so it gets a short deadline and its failure is a log line.
const reportTimeout = 5 * time.Second

// reportServers pushes this gateway's view of its downstream connections to
// the daemon, which folds it together with every other gateway's view into
// the `state` and `tools` columns of /v1/servers.
//
// The gateway is the only process that holds these connections, so it is the
// only one that can answer. Nothing in the data plane depends on the report
// landing: with no daemon (or no link yet) this is a no-op, exactly like
// every other thing on the control link (docs/architecture.md §2).
//
// Callers run on their own goroutines (connect, refresh, hot reload, link
// registration), so the push is synchronous — one bounded round trip on a
// goroutine that was going to finish anyway beats an unbounded fan-out of
// reporters racing shutdown.
func (g *gateway) reportServers() {
	if g.ctl == nil {
		return
	}
	g.ctl.reportServers(g.lifeCtx, g.serverStates())
}

// serverStates snapshots the runtime condition of every APPLIED downstream
// spec. The spec list — not the connected map — is the domain, so a server
// that failed to connect or has not been reached yet still appears; a report
// that silently omitted them would read on the daemon side as "no such
// server", which is the one thing this whole path exists to stop saying.
func (g *gateway) serverStates() []ctlapi.GatewayServerState {
	g.mu.Lock()
	specs := make([]downstream.Spec, len(g.specs))
	copy(specs, g.specs)
	servers := make(map[string]*downstream.Server, len(g.servers))
	for id, srv := range g.servers {
		servers[id] = srv
	}
	failures := make(map[string]connectFailure, len(g.connErr))
	for id, failure := range g.connErr {
		failures[id] = failure
	}
	g.mu.Unlock()

	out := make([]ctlapi.GatewayServerState, 0, len(specs))
	for _, spec := range specs {
		st := ctlapi.GatewayServerState{ID: spec.ID}
		switch srv := servers[spec.ID]; {
		case srv != nil:
			// Wired into the catalog = the handshake completed.
			st.Conn = string(ctlapi.ConnConnected)
			st.Tools = len(srv.Tools())
			// A gateway runs no background prober (Deps.PingInterval == 0 —
			// one short-lived client process does not need one), so
			// Health() sits at its ConnConnecting seed value forever unless
			// something actually pinged. Only a probe that RAN may override
			// what the connection itself proved; LastProbe is that witness.
			if h := srv.Health(); !h.LastProbe.IsZero() {
				st.Conn = string(h.State)
				st.Detail = h.Detail
			}
		case failures[spec.ID].detail != "":
			st.Conn = string(ctlapi.ConnError)
			st.Detail = failures[spec.ID].detail
			st.NeedsAuth = failures[spec.ID].needsAuth
		default:
			// Neither connected nor failed: still dialing. npx/uvx cold
			// starts live here for minutes, and "connecting" is the honest
			// answer for all of it.
			st.Conn = string(ctlapi.ConnConnecting)
		}
		out = append(out, st)
	}
	return out
}

// connDiagnosis renders the downstream connection report that the discovery
// status reply appends after its visibility block, for the caller whose
// effective scope is es.
//
// discovery.Surface knows only what is VISIBLE, so on its own it answers a
// total connection failure and a genuinely empty registry with the same
// "0 server(s) visible" — the caller then has no way to tell "nothing is
// configured" from "everything is 401-ing". noteConnectResult already
// records why each server failed for the control link; this puts the same
// witness in front of the one who can act on it.
//
// It is scoped and redacted because it crosses back to an MCP caller, which
// serverStates() (the trusted daemon control link) is not: only servers in the
// caller's effective scope appear, and the raw connect detail — which can quote
// a server's URL query, i.e. a credential — is NOT rendered; a server ID plus
// a needs-auth hint is the actionable part. es == nil means no scope authority
// (a local session), which sees every server, matching the scope gate's
// allow-when-no-authority branch; an empty es hides everything.
//
// It only ever ADDS text: a server that is merely slow stays "connecting",
// and a fully connected session yields the empty string.
func (g *gateway) connDiagnosis(es *scope.EffectiveScope) string {
	inScope := func(id string) bool {
		if es == nil {
			return true
		}
		_, ok := es.Servers[id]
		return ok
	}
	var failed, connecting []ctlapi.GatewayServerState
	for _, st := range g.serverStates() {
		if !inScope(st.ID) {
			continue
		}
		switch st.Conn {
		case string(ctlapi.ConnError):
			failed = append(failed, st)
		case string(ctlapi.ConnConnecting):
			connecting = append(connecting, st)
		}
	}
	if len(failed) == 0 && len(connecting) == 0 {
		return ""
	}
	slices.SortFunc(failed, func(a, b ctlapi.GatewayServerState) int { return cmp.Compare(a.ID, b.ID) })
	slices.SortFunc(connecting, func(a, b ctlapi.GatewayServerState) int { return cmp.Compare(a.ID, b.ID) })

	var b strings.Builder
	if len(failed) > 0 {
		fmt.Fprintf(&b, "\n%d server(s) failed to connect:\n", len(failed))
		for _, st := range failed {
			if st.NeedsAuth {
				fmt.Fprintf(&b, "  %s (needs auth)\n", st.ID)
			} else {
				fmt.Fprintf(&b, "  %s\n", st.ID)
			}
		}
	}
	if len(connecting) > 0 {
		ids := make([]string, 0, len(connecting))
		for _, st := range connecting {
			ids = append(ids, st.ID)
		}
		fmt.Fprintf(&b, "\n%d server(s) still connecting: %s\n", len(ids), strings.Join(ids, ", "))
	}
	return b.String()
}

// noteConnectResult records (or clears) the last connect failure of one
// server so a spec that never produced a *downstream.Server can still be
// reported as an error with its reason, instead of hiding behind a
// perpetual "connecting".
//
// It is also the single place the re-dial ladder (redial.go) is armed and
// disarmed, so the two can never disagree about whether a server is broken:
// a recorded failure always has a rung waiting, and a success always clears
// one.
type connectFailure struct {
	detail    string
	needsAuth bool
}

func (g *gateway) noteConnectResult(id string, failure error) {
	var rung int
	var delay time.Duration

	g.mu.Lock()
	if failure == nil {
		delete(g.connErr, id)
		g.resetLadderLocked(id)
	} else {
		g.connErr[id] = connectFailure{
			detail:    failure.Error(),
			needsAuth: transport.IsAuthStatus(failure),
		}
		rung, delay = g.armLocked(id, time.Now())
	}
	g.mu.Unlock()

	if rung > 0 {
		// Debug: the ladder's own arithmetic, wanted only by someone asking
		// why a server has not been retried yet. Until now the log showed the
		// dials — one Info per attempt, carrying the attempt count — and
		// nothing about the gaps between them, so by the rungs where it
		// matters (45s, 135s, then five minutes forever) "it has given up"
		// and "it is waiting out a backoff it earned" read identically.
		//
		// Written outside g.mu, deliberately: see armLocked.
		g.log.Debug("re-dial armed", logx.Server(id), "rung", rung, "in_ms", delay.Milliseconds())
	}
}

// reportServers posts one runtime snapshot on the control link. It is a
// no-op before registration: the report is keyed by session id, and the
// register path sends a fresh full snapshot the moment one exists.
func (l *ctlLink) reportServers(ctx context.Context, states []ctlapi.GatewayServerState) {
	sid := l.Session()
	if sid == "" {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	err := l.post(rctx, "/v1/gateway/"+url.PathEscape(sid)+"/servers", "gateway:"+sid,
		ctlapi.GatewayServersReport{Servers: states}, nil)
	if err != nil && ctx.Err() == nil {
		// Debug, not Warn: an older daemon answers 404 here, and a daemon
		// that just died is normal operation. Losing a report costs a stale
		// status column until the next event, never a served call.
		l.g.log.Debug("server state report failed", "error", err)
	}
}
