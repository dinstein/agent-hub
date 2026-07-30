package ctlapi

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"github.com/dinstein/agent-hub/internal/integrity"
)

// The quarantine set (docs/modules/controlplane.md): tools the integrity
// subsystem isolated after a definition drift, and the human release that
// ends the isolation.
//
// Entries are keyed by the CLIENT-VISIBLE exposed name, computed after
// per-scope overrides — quarantine tracks what an agent can actually see and
// call, so a rename cannot move a tool out from under it.

// quarantineWire is one isolated tool.
type quarantineWire struct {
	Exposed     string `json:"exposed"`
	Server      string `json:"server"`
	Tool        string `json:"tool"`
	Reason      string `json:"reason,omitempty"`
	PinnedHash  string `json:"pinned_hash,omitempty"`
	CurrentHash string `json:"current_hash,omitempty"`
	At          string `json:"at"`
}

// quarantineListWire is the GET /v1/quarantine body.
type quarantineListWire struct {
	Generation uint64           `json:"generation"`
	Entries    []quarantineWire `json:"entries"`
}

// quarantineReleaseWire is the DELETE /v1/quarantine/{exposed} response.
type quarantineReleaseWire struct {
	writeResultWire
	Exposed  string         `json:"exposed"`
	Entry    quarantineWire `json:"entry"`
	Released bool           `json:"released"`
}

func quarantineWireOf(exposed string, e integrity.QuarantineEntry) quarantineWire {
	return quarantineWire{
		Exposed: exposed, Server: e.Server, Tool: e.Tool, Reason: e.Reason,
		PinnedHash: e.PinnedHash, CurrentHash: e.CurrentHash,
		At: e.At.UTC().Format(time.RFC3339Nano),
	}
}

// handleQuarantineList implements GET /v1/quarantine.
//
// Failure direction: a corrupt store is an ERROR, never an empty list. "No
// tools are quarantined" and "the quarantine file is unreadable" must never
// render the same, or an operator would read an isolation that is still in
// force as one that was lifted.
func (s *Server) handleQuarantineList(w http.ResponseWriter, r *http.Request) {
	store, ok := s.quarantineStore(w, r)
	if !ok {
		return
	}
	snap, err := store.Snapshot(r.Context())
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	out := quarantineListWire{Generation: s.generation(), Entries: []quarantineWire{}}
	for exposed, e := range snap {
		out.Entries = append(out.Entries, quarantineWireOf(exposed, e))
	}
	slices.SortFunc(out.Entries, func(a, b quarantineWire) int { return cmp.Compare(a.Exposed, b.Exposed) })
	writeOK(w, http.StatusOK, out)
}

// handleQuarantineRelease implements DELETE /v1/quarantine/{exposed}: the
// human re-approve step.
//
// An entry that is not in the set answers the uniform 404 rather than a
// cheerful success: the store's Release is idempotent, so without this check
// a typo'd exposed name would report "released" and leave the real
// quarantine in place.
func (s *Server) handleQuarantineRelease(w http.ResponseWriter, r *http.Request, exposed string) {
	store, ok := s.quarantineStore(w, r)
	if !ok {
		return
	}
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	pre, ok := adminPrecondition(w, r, body)
	if !ok {
		return
	}
	// Weak (snapshot) precondition: the quarantine store is not the
	// registry, so this catches a stale operator view, not a concurrent
	// write — the store's own lock serializes those.
	if err := s.checkSnapshotPrecondition(pre); err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	entry, found, err := store.Release(r.Context(), exposed)
	if err == nil && !found {
		writeNotFound(w, r)
		return
	}
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	writeOK(w, http.StatusOK, quarantineReleaseWire{
		writeResultWire: writeResultWire{Generation: s.generation(), Changed: true},
		Exposed:         exposed,
		Entry:           quarantineWireOf(exposed, entry),
		Released:        true,
	})
}

// quarantineStore opens the quarantine store, answering the uniform 404 when
// no state directory was injected.
func (s *Server) quarantineStore(w http.ResponseWriter, r *http.Request) (*integrity.QuarantineStore, bool) {
	opt, ok := s.stateOptions()
	if !ok {
		writeNotFound(w, r)
		return nil, false
	}
	store, err := integrity.OpenQuarantineStore(opt.Dir, integrity.Options{LockTimeout: opt.LockTimeout})
	if err != nil {
		s.writeOpsError(w, r, err)
		return nil, false
	}
	return store, true
}
