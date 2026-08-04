package gateway

import (
	"sync"

	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// This file is the gateway's half of the credential announcement plane
// (internal/secrets/announce.go): it subscribes to "the stored credentials of
// server X changed" and turns each one into the two things a running gateway
// can do about it.
//
//   - The server is CONNECTED: bump its credential epoch, which drops the
//     cached bearer on its round tripper (internal/downstream/httpauth.go).
//     The next request re-reads the vault. Nothing reconnects — a token
//     refresh must not cost a handshake, and the daemon rewrites the vault
//     every 60s, so reconnecting here would be a reconnect storm rather than
//     a fix.
//
//   - The server is NOT connected: wake its re-dial rung (redial.go) so the
//     next tick dials it. This is the case the whole investigation started
//     from — `auth login` on a server whose handshake had already been
//     rejected — and the ladder alone would have made the user wait out a
//     backoff earned before the credential existed.
//
// Both reactions are safe to run on an announcement the gateway caused
// itself: the epoch bump costs one vault read that returns the value just
// written, and the wake only touches servers with a recorded failure.

// credEpochs holds the per-server credential epoch the round trippers read.
//
// It is deliberately NOT keyed by scope: a derived instance inherits its
// base server's login unless it has its own, so an announcement for the
// server must invalidate every instance's cache. Bumping one counter per
// server id is what makes that automatic — the alternative is remembering to
// bump each derivation, and forgetting one leaves it on a dead token with
// nothing to say so.
type credEpochs struct {
	mu sync.Mutex
	n  map[string]uint64
}

func newCredEpochs() *credEpochs {
	return &credEpochs{n: map[string]uint64{}}
}

func (c *credEpochs) get(serverID string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[serverID]
}

func (c *credEpochs) bump(serverID string) {
	c.mu.Lock()
	c.n[serverID]++
	c.mu.Unlock()
}

// startCredWatch subscribes to the announcement plane. It is best-effort in
// the same sense as the registry watcher: without it the gateway keeps the
// recovery paths it already had (the 401 retry and the re-dial ladder), so a
// vault directory that cannot be resolved costs promptness, not function.
func (g *gateway) startCredWatch() {
	if g.credEpochs == nil {
		return // no vault wiring (a test assembly): nothing announces
	}
	dir, err := secretsDir(g.resolver)
	if err != nil {
		g.log.Debug("credential announcements unavailable; recovery falls back to the re-dial ladder", "error", err)
		return
	}
	w := secrets.NewCredWatcher(dir)
	g.credWatcher = w
	g.credWG.Add(1)
	go func() {
		defer g.credWG.Done()
		for id := range w.Events() {
			g.onCredentialChanged(id)
		}
	}()
}

// onCredentialChanged reacts to one announcement.
func (g *gateway) onCredentialChanged(serverID string) {
	g.credEpochs.bump(serverID)

	g.mu.Lock()
	_, live := g.servers[serverID]
	woke := false
	if !live {
		woke = g.wakeLocked(serverID)
	}
	g.mu.Unlock()

	g.log.Info("credential changed downstream", logx.Server(serverID), "connected", live)
	if !live && !woke {
		// Debug, and this is the case it exists for: an announcement for a
		// server with no RECORDED FAILURE wakes nothing — it was never dialed,
		// or a dial is in flight — so the announcement is followed by no
		// re-dial at all. Without the line that sequence reads as an
		// announcement gone missing, which sends the reader looking for a
		// broken watcher instead of at a server that was never broken.
		g.log.Debug("credential announcement woke no re-dial: no recorded failure to recover from",
			logx.Server(serverID))
	}
}
