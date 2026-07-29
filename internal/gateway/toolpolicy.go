package gateway

import (
	"context"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/dinstein/agent-hub/internal/integrity"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/router"
)

// This file makes two operator/detector decisions real on the DATA plane:
// the kill switch (`agenthub tool disable`, stored as the approval record's
// Disabled flag) and the integrity quarantine set. Both used to exist only
// on paper — the CLI reported a tool as switched off while the gateway kept
// aggregating, listing and routing it.
//
// Enforcement point: AGGREGATION (router.Policy), not a new gate. A switched
// off or isolated tool must not be listed, searched, described or routed at
// all; a gate would leave the name visible and callable-looking and would
// have to be re-implemented for every discovery mode. The frozen gate chain
// (scope → token tier → precheck → HITL) is therefore untouched, and because
// every execute path resolves through the same router, one filter covers
// tools/list, search_tools, describe_tool, direct tools/call and lazy
// call_tool alike.
//
// Fail direction: FAIL-CLOSED on both edges, and they are not the same edge.
//
//   - UNKNOWN policy (never read successfully): the gateway exposes an EMPTY
//     catalog. An unreadable approval store is precisely what erasing a
//     disable looks like, so "cannot tell what is switched off" has to mean
//     "expose nothing", never "expose everything". The condition is
//     recoverable: the watcher below re-reads, and the catalog repopulates.
//   - RELOAD failure (a policy is already in force): the policy in force is
//     KEPT. Refusing to widen on error is the closed direction; dropping to
//     unknown because one flock timed out would take the whole catalog down
//     for an availability problem the previous answer already covers.
//
// The gateway only ever READS these two files, so no self-write suppression
// is needed here (contrast internal/registry).

const (
	// toolPolicyPoll is the safety net behind fsnotify: the state stores are
	// written by other processes (CLI, daemon, another gateway) on
	// filesystems where fsnotify may be unreliable or unavailable.
	toolPolicyPoll = 2 * time.Second
	// toolPolicyDebounce coalesces the write burst of one atomic replace
	// (temp file create + rename) into a single reload.
	toolPolicyDebounce = 200 * time.Millisecond
)

// toolPolicySource reads the <state> stores the data plane obeys.
type toolPolicySource struct {
	dir        string
	files      map[string]bool // absolute paths worth reacting to
	approvals  *integrity.ApprovalStore
	quarantine *integrity.QuarantineStore
}

// openToolPolicySource opens the approval and quarantine stores under
// <data>/state. It returns nil on any failure, which leaves the policy
// UNKNOWN — fail-closed, see the file header. The failure is logged at Error
// because a gateway that then serves nothing must say why.
func openToolPolicySource(resolver *platform.Resolver, log *slog.Logger) *toolPolicySource {
	src, err := openStateStores(resolver)
	if err != nil {
		log.Error("tool governance stores unavailable; the catalog stays empty until they can be read", "error", err)
		return nil
	}
	return src
}

func openStateStores(resolver *platform.Resolver) (*toolPolicySource, error) {
	dir, err := resolver.StateDir()
	if err != nil {
		return nil, err
	}
	opts := integrity.Options{}
	approvals, err := integrity.OpenApprovalStore(dir, opts)
	if err != nil {
		return nil, err
	}
	quarantine, err := integrity.OpenQuarantineStore(dir, opts)
	if err != nil {
		return nil, err
	}
	files := make(map[string]bool, 2)
	for _, p := range integrity.PolicyFiles(dir) {
		files[p] = true
	}
	return &toolPolicySource{dir: dir, files: files, approvals: approvals, quarantine: quarantine}, nil
}

// watches reports whether a filesystem event names one of the two policy
// files, or the temp file an atomic write renames onto one ("<file>.tmp-*",
// which is what some platforms report instead of the rename).
//
// Fail direction: this is a LATENCY filter, not a security one — a missed
// event only defers the change to the next poll tick, and a false positive
// costs one redundant read.
func (s *toolPolicySource) watches(name string) bool {
	base := filepath.Base(name)
	for p := range s.files {
		if b := filepath.Base(p); base == b || strings.HasPrefix(base, b+".tmp-") {
			return true
		}
	}
	return false
}

// load projects both stores into a router.Policy.
//
// Fail direction: FAIL-CLOSED — any read error is returned with a zero
// Policy that the caller must NOT apply (a zero Policy denies nothing).
func (s *toolPolicySource) load(ctx context.Context) (router.Policy, error) {
	disabled, err := s.approvals.DisabledTools(ctx)
	if err != nil {
		return router.Policy{}, err
	}
	quarantined, err := s.quarantine.Snapshot(ctx)
	if err != nil {
		return router.Policy{}, err
	}
	pol := router.Policy{Disabled: disabled}
	if len(quarantined) > 0 {
		pol.Quarantined = make(map[string]bool, len(quarantined))
		for exposed := range quarantined {
			pol.Quarantined[exposed] = true
		}
	}
	return pol, nil
}

// toolPolicy returns the policy currently in force. ok=false means the
// policy is UNKNOWN and the caller must expose nothing (fail-closed).
func (g *gateway) toolPolicy() (router.Policy, bool) {
	pol := g.policy.Load()
	if pol == nil {
		return router.Policy{}, false
	}
	return *pol, true
}

// loadToolPolicyOnce performs the startup read. A failure leaves the policy
// unknown on purpose (fail-closed): the catalog stays empty until a later
// refresh succeeds.
func (g *gateway) loadToolPolicyOnce(ctx context.Context) {
	if g.policySrc == nil {
		return
	}
	pol, err := g.policySrc.load(ctx)
	if err != nil {
		g.log.Error("tool governance state unreadable; exposing no tools until it can be read", "error", err)
		return
	}
	g.policy.Store(&pol)
}

// startPolicyWatch attaches the state-store watcher so a `tool disable` or a
// quarantine entry takes effect on a RUNNING gateway. Without it "the kill
// switch needs a restart" would only postpone the lie it is fixing.
func (g *gateway) startPolicyWatch() {
	if g.policySrc == nil {
		return
	}
	g.policyWG.Add(1)
	go func() {
		defer g.policyWG.Done()
		g.watchToolPolicy(g.lifeCtx)
	}()
}

// watchToolPolicy is the fsnotify + poll loop over <data>/state. fsnotify is
// best-effort (fail-open toward the poll: a watcher that merely lags is
// strictly better than none); the poll ticker alone is a complete
// implementation.
func (g *gateway) watchToolPolicy(ctx context.Context) {
	var (
		fsnEvents chan fsnotify.Event
		fsnErrors chan error
	)
	if w, err := fsnotify.NewWatcher(); err == nil {
		if aerr := w.Add(g.policySrc.dir); aerr == nil {
			defer func() { _ = w.Close() }()
			fsnEvents, fsnErrors = w.Events, w.Errors
		} else {
			_ = w.Close()
			g.log.Debug("tool governance watch unavailable; polling only", "error", aerr)
		}
	}

	poll := time.NewTicker(toolPolicyPoll)
	defer poll.Stop()
	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-fsnEvents:
			if !ok {
				fsnEvents = nil
				continue
			}
			if !g.policySrc.watches(ev.Name) {
				continue
			}
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
			debounce.Reset(toolPolicyDebounce)
		case err, ok := <-fsnErrors:
			if !ok {
				fsnErrors = nil
				continue
			}
			// Losing the notification channel only costs latency: the poll
			// keeps running and will pick the change up.
			g.log.Debug("tool governance watch error; polling continues", "error", err)
		case <-debounce.C:
			g.refreshToolPolicy(ctx)
		case <-poll.C:
			g.refreshToolPolicy(ctx)
		}
	}
}

// refreshToolPolicy re-reads the stores and rebuilds the catalog when the
// policy actually moved.
func (g *gateway) refreshToolPolicy(ctx context.Context) {
	pol, err := g.policySrc.load(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down; not a governance failure
		}
		// Fail-closed: keep the deny sets already in force rather than
		// widening to "nothing denied" because a read failed.
		g.log.Warn("tool governance reload failed; keeping the policy in force", "error", err)
		return
	}
	if cur := g.policy.Load(); cur != nil && policyEqual(*cur, pol) {
		return
	}
	g.policy.Store(&pol)
	g.log.Info("tool governance applied",
		"disabled", countDisabled(pol), "quarantined", len(pol.Quarantined))

	// A policy change is a CATALOG change: rebuild whichever catalog this
	// gateway is currently serving. The cold path never marks the catalog
	// live (that is connectOne's decision alone).
	if _, ready, _ := g.catalog(); ready {
		g.rebuildAndNotify()
		return
	}
	g.rebuildColdAndNotify()
}

// policyEqual reports whether two policies deny exactly the same tools. It
// exists so an unchanged store never costs a router rebuild plus a spurious
// tools/list_changed (the poll fires every few seconds).
func policyEqual(a, b router.Policy) bool {
	if !maps.Equal(a.Quarantined, b.Quarantined) {
		return false
	}
	if len(a.Disabled) != len(b.Disabled) {
		return false
	}
	for server, tools := range a.Disabled {
		if !maps.Equal(tools, b.Disabled[server]) {
			return false
		}
	}
	return true
}

// countDisabled totals the kill-switch entries for the log line.
func countDisabled(pol router.Policy) int {
	n := 0
	for _, tools := range pol.Disabled {
		n += len(tools)
	}
	return n
}
