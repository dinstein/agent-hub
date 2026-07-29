package gateway

import (
	"path/filepath"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/router"
	"github.com/dinstein/agent-hub/internal/secrets"
	"github.com/dinstein/agent-hub/internal/skills"
)

// This file wires skills-over-MCP (docs/modules/config.md) into the gateway.
//
// The design decision worth stating once: the skills face is a
// router.Provider, NOT a second surface. Its tools are aggregated by the
// same router, named by the same exposed-name rules, projected through the
// same EffectiveScope, and executed by the same pipeline.Execute — the
// gateway's single execute path (upstream.go execTool). The only thing that
// differs is where the bytes come from.
//
// Three consequences follow, all intended:
//
//   - "skills" is an ordinary SCOPE SUBJECT. A profile or client layer that
//     lists its servers explicitly hides the whole face (the layer is an
//     intersection); one that stays silent leaves it visible. That is the
//     docs/modules/config.md skillScope chain expressed in the chain that already
//     exists, instead of a parallel one.
//   - the injection scanner runs over skill text on the way out, because
//     defend_and_shape runs over every pipeline result. SKILL.md is a
//     first-class injection carrier (5.1.5) and this path does not get to
//     opt out of the scan.
//   - the face is CALLABLE while downstreams are still connecting: it has
//     nothing to connect to. execTool checks providers before the busy gate.
//
// Governance switch (`skillsOverMcp`) defaults OFF: the face adds a new
// supply channel of untrusted text into the model's context, so it is opted
// into, never inherited by an upgrade.

// skillsDirName is the skills library directory under <data>. It matches
// what `agenthub skill …` uses (internal/cli): one library, two readers.
const skillsDirName = "skills"

// syncSkills brings the skills face in line with the applied governance
// document: built (and refreshed) when the switch is on, dropped when it is
// off. Returns true when the face's presence CHANGED, so the caller knows
// whether the catalog must be rebuilt.
//
// Every failure ends with no face and a warning: a broken skills store must
// never keep the gateway from serving tools, and half a face (listed but
// unreadable) would be worse than none.
func (g *gateway) syncSkills(resolver *platform.Resolver) bool {
	on := false
	if snap := g.snap.Load(); snap != nil {
		on = snap.Governance.V.SkillsOverMCP
	}

	g.mu.Lock()
	cur := g.skills
	g.mu.Unlock()

	switch {
	case !on:
		if cur == nil {
			return false
		}
		g.mu.Lock()
		g.skills = nil
		g.mu.Unlock()
		g.log.Info("skills-over-MCP disabled by governance")
		return true

	case cur != nil:
		// Already on: re-read the library so an enable/disable/add since the
		// last sync is reflected. A refresh failure keeps the previous
		// snapshot (skills.Provider contract) — serving the last known-good
		// set beats dropping the face because a lock was busy.
		if err := cur.Refresh(g.lifeCtx); err != nil {
			g.log.Warn("skills refresh failed; keeping the previous skill set", "error", err)
		}
		return false
	}

	dir, err := resolver.DataDir()
	if err != nil {
		g.log.Warn("skills-over-MCP requested but the data dir is unresolved", "error", err)
		return false
	}
	mgr, err := skills.Open(filepath.Join(dir, skillsDirName), skills.Options{})
	if err != nil {
		g.log.Warn("skills-over-MCP requested but the library could not be opened", "error", err)
		return false
	}
	p := skills.NewProvider(mgr)
	if err := p.Refresh(g.lifeCtx); err != nil {
		g.log.Warn("skills-over-MCP requested but the library could not be read", "error", err)
		return false
	}
	g.mu.Lock()
	g.skills = p
	g.mu.Unlock()
	g.log.Info("skills-over-MCP enabled", "skills", len(p.ToolNames()))
	return true
}

// providers returns the host-served tool providers of the current catalog
// build. nil when the skills face is off — the pre-M2 shape exactly.
func (g *gateway) providers() []router.Provider {
	g.mu.Lock()
	p := g.skills
	g.mu.Unlock()
	if p == nil {
		return nil
	}
	return []router.Provider{p}
}

// poolOptions builds the derived-instance pool configuration: the caller's
// caps and timers, this gateway's dial path and logger. The pool dials
// through the SAME Deps as a base connection (spawn guard, secrets, SSRF
// screening), so a derived instance cannot be a way around them.
func (g *gateway) poolOptions() downstream.PoolOptions {
	opts := g.cfg.DerivedPool
	opts.Deps = g.downstreamDeps()
	opts.Log = g.log
	return opts
}

// downstreamDeps is the single description of how this gateway dials a
// downstream — base connection and derived instance alike.
func (g *gateway) downstreamDeps() downstream.Deps {
	return downstream.Deps{
		Log:            g.log,
		Dial:           g.cfg.Dial,
		ConnectTimeout: g.cfg.ConnectTimeout,
		// Without a resolver every ${SECRET_X} placeholder is a dial error
		// (downstream.ErrNoResolver, fail-closed) — which is exactly what
		// used to happen here: the gateway is the path that serves real
		// clients, and it was the one path where injected credentials did
		// not work at all.
		Secrets: g.cfg.Secrets,
		// The OAuth bearer. Its absence was the sibling of the Secrets bug
		// above: a nil Auth attaches no credential and attempts no refresh,
		// so every HTTP downstream got a bare request and answered 401 while
		// the vault held a valid, refreshable token the whole time.
		//
		// AuthFor (not Auth) because the credential is per (server, scope):
		// Deps is shared by every instance of the derived pool, so a single
		// shared source would hand one server's bearer to another.
		AuthFor: func(spec downstream.Spec) downstream.TokenSource {
			if g.cfg.Auth == nil || !spec.IsHTTP() {
				// A stdio child gets its credentials through the environment,
				// which Secrets already covers.
				return nil
			}
			scopeName := spec.ScopeName
			if scopeName == "" {
				scopeName = secrets.DefaultScope
			}
			return g.cfg.Auth(spec.ID, scopeName)
		},
	}
}
