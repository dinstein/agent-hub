package gateway

import (
	"fmt"

	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/ratelimit"
)

// This file is the gateway's quota plane: the cooperative call quotas of
// internal/ratelimit, wired into the single execute path.
//
// Placement — the one thing not to get wrong. The limiter WRAPS the call
// closures of the assembled pipeline.CallRequest (runCall in upstream.go);
// it is NOT a fifth gate. The frozen chain order (scope → token tier →
// argument precheck → HITL) is untouched, and that is deliberate on both
// sides:
//
//   - Those four gates decide whether a call is allowed AT ALL and every one
//     of them fails closed. A quota decides whether an ALLOWED call happens
//     now or a few seconds from now, and its call path fails open. Putting a
//     fail-open stage inside a fail-closed chain is the shape a limiter
//     takes when it becomes a bypass.
//   - Wrapping puts the spend after EVERY gate, immediately before the
//     downstream call. So a call HITL denied never spends a token (charging
//     a human's "no" against the agent's quota would let denied calls starve
//     approved ones), and the 7.2 argument self-heal retry is charged once,
//     not twice (both wrappers share one Admission).
//
// Configuration lives at the GLOBAL layer only, governance.json
// `rateLimits` — see registry.GovernanceDoc.RateLimits for why it is not a
// five-layer scope field.

// initRateLimits builds this process's limiter from the governance document
// the registry load just applied.
//
// Failure direction: FAIL CLOSED. A rule set that cannot be parsed, or a
// counter file that cannot be used, aborts gateway startup instead of
// starting with quotas that exist only on paper. There is no known-good
// state to fall back to at assembly time, and a configuration that claims a
// quota must be honoured or reported — never silently ignored.
func (g *gateway) initRateLimits() error {
	cfg, err := g.rateLimitConfig()
	if err != nil {
		return err
	}
	lim, err := g.buildLimiter(cfg)
	if err != nil {
		return err
	}
	g.limiter.Store(lim)
	return nil
}

// syncRateLimits re-reads the rule set after a governance change.
//
// Failure direction at RUNTIME: keep the previous, known-good rule set and
// log at Error. This is the opposite choice from startup, on purpose —
// refusing to serve would turn a typo in an unrelated governance edit into
// an outage for a running agent, while dropping to "no quotas" would be the
// silent weakening this package refuses. Keeping the last valid set can only
// ever be as tight as it already was.
func (g *gateway) syncRateLimits() {
	cfg, err := g.rateLimitConfig()
	if err != nil {
		g.log.Error("rate limit rules unusable; keeping the previously applied rule set", "error", err)
		return
	}
	lim, err := g.buildLimiter(cfg)
	if err != nil {
		g.log.Error("rate limiter rebuild failed; keeping the previously applied rule set", "error", err)
		return
	}
	g.limiter.Store(lim)
}

// rateLimitConfig translates the applied governance document into a rule
// set. No registry (docs/flows.md: the gateway then serves from the tool
// cache) means no governance document, hence no rules — the same
// no-authority mode the scope gate runs in.
func (g *gateway) rateLimitConfig() (ratelimit.Config, error) {
	snap := g.snap.Load()
	if snap == nil {
		return ratelimit.Config{}, nil
	}
	return ratelimit.ConfigFromGovernance(snap.Governance.V)
}

// buildLimiter returns the limiter for cfg, or nil when no rule is
// configured.
//
// nil is the zero-cost path and it is load-bearing for "no quotas
// configured behaves exactly as before": no state directory is created, no
// counter file is opened, and runCall leaves the CallRequest byte for byte
// as it was before quotas existed.
func (g *gateway) buildLimiter(cfg ratelimit.Config) (*ratelimit.Limiter, error) {
	if len(cfg.Rules) == 0 {
		return nil, nil
	}
	if g.rlStore == nil {
		dir, err := g.resolver.StateDir()
		if err != nil {
			return nil, fmt.Errorf("gateway: rate limit rules are configured but the state dir is unresolved: %w", err)
		}
		store, err := ratelimit.NewStore(dir)
		if err != nil {
			return nil, err
		}
		// Kept across rebuilds: the counter file identity must not change
		// when a rule is edited, or the edit would hand every bucket a fresh
		// start.
		g.rlStore = store
	}
	return ratelimit.New(ratelimit.Options{
		Config:  cfg,
		Store:   g.rlStore,
		Logger:  g.log,
		OnEvent: g.onRateLimitEvent,
	})
}

// onRateLimitEvent records one enforcement decision. The Event carries
// IDENTIFIERS ONLY — client, server, tool, rule id — never arguments and
// never payload (audit never records args).
//
// Both branches log, and the degraded one logs at Error. A degraded decision
// means the call was admitted WITHOUT being counted, i.e. the quota is not
// being enforced at all — precisely the state an attacker would want to
// engineer by making the counter file unreadable. internal/ratelimit admits
// such a call on purpose (a broken counter must not take every agent on this
// machine offline), so the whole guarantee rests on the admission being
// visible: a quota that never fires and a quota that is not running must
// never look alike.
func (g *gateway) onRateLimitEvent(ev ratelimit.Event) {
	attrs := []any{
		logx.Server(ev.Key.Server),
		logx.Tool(ev.Key.Tool),
		"rule", ev.Rule,
	}
	if ev.Degraded {
		g.log.Error("rate limit counters unusable; call admitted UNCOUNTED (quota not enforced)", attrs...)
		return
	}
	g.log.Warn("rate limit exceeded; call rejected",
		append(attrs, "retry_after", ev.RetryAfter.String())...)
}
