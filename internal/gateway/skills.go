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
//   - NOTHING scans the skill text on the way out. This once read "the
//     injection scanner runs over skill text ... this path does not get to opt
//     out of the scan", which was true when defend_and_shape still defended;
//     it does not (internal/pipeline/shape.go), so the sentence promised a
//     protection that is not there. SKILL.md is a first-class injection
//     carrier, and what actually holds the line is the switch below being off
//     by default plus the import-time refusals in internal/skills — not
//     anything on the way out.
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
		// A streamable-http downstream has no other way to tell this
		// process its tool set changed. Catalog refresh is driven entirely
		// by tools/list_changed — there is no poll, no TTL, and no re-list
		// on anything but a reconnect — so with this off a remote server's
		// catalog was fixed from connect until the connection was rebuilt,
		// and nothing said so. The servers that suffer it are the ones the
		// seed catalog reaches over HTTP: hosted endpoints whose tool set
		// moves on a vendor's deploy schedule, not the local binaries that
		// only change when someone reinstalls them.
		//
		// This is also the switch the whole 2026-07-28 subscriptions/listen
		// path hangs from, so it was dead code in the shipped binary too.
		//
		// The cost against a server that offers no stream is one refused
		// request per connection and one log line saying list changes will
		// not be pushed — see transport.streamRefusedPermanently, which had
		// to learn about 400 before this line was safe to write.
		NotificationStream: true,
		// Per server rather than one log on the shared Deps: a ServerLog
		// carries the id it was opened with, so a single shared one would
		// file every server's frames under whichever server opened it.
		// traceLogs owns the id → log mapping and the registry-driven
		// enabled state.
		FramesFor: g.traces.logFor,
		// Read at connect time, not here: the pool captures Deps once, and
		// the governance switch that decides this stream is loaded after the
		// pool is built. downstream binds the per-server identity from Spec.
		Events:   g.eventStream,
		ClientID: g.cfg.ClientID,
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
			// The provenance decision travels with the id: the refresher
			// renews by server and would otherwise have to re-derive from
			// the registry what the caller is already holding.
			return g.cfg.Auth(spec.ID, scopeName, spec.AllowsLoopback())
		},
	}
}
