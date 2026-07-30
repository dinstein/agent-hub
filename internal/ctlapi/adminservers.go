package ctlapi

import (
	"encoding/json"
	"net/http"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
)

// Server CRUD (docs/modules/controlplane.md).
//
// GET /v1/servers keeps its historical shape — the runtime list with
// embedded Health, byte-identical to the `servers` SSE payload. Editing
// needs the STORED entry instead (command, env, docker, …), which is what
// GET /v1/servers/{id} serves: the read half of the read-modify-write that
// PATCH completes.

// serverEntryWire is the stored definition of one server on the wire.
// registry.ServerEntry is used verbatim rather than mirrored, so a field
// added to the registry becomes settable here without a second definition
// drifting from the first.
//
// Unknown fields written by a NEWER agenthub are not lost by an edit: they
// live in the Doc envelope, and confops replaces the typed value only.
type serverDetailWire struct {
	Generation uint64               `json:"generation"`
	ID         string               `json:"id"`
	Entry      registry.ServerEntry `json:"entry"`
}

// serverCreateWire is the POST /v1/servers body.
type serverCreateWire struct {
	preconditionWire
	ID    string               `json:"id"`
	Entry registry.ServerEntry `json:"entry"`
}

// serverPatchWire is the PATCH /v1/servers/{id} body: a PARTIAL entry.
//
// Merge rule: a key PRESENT in `entry` replaces that field wholesale; an
// absent key keeps the stored value. There is no deep merge — otherwise an
// env map could only ever grow, and removing a leaked variable would be
// impossible through this endpoint.
type serverPatchWire struct {
	preconditionWire
	Entry json.RawMessage `json:"entry"`
}

// serverWriteWire is the response of every server write.
type serverWriteWire struct {
	writeResultWire
	ID string `json:"id"`
	// Entry is the definition as it now stands; absent after a delete.
	Entry   *registry.ServerEntry `json:"entry,omitempty"`
	Deleted bool                  `json:"deleted,omitempty"`
}

// handleServerGet implements GET /v1/servers/{id}: the stored entry plus the
// generation it was read at, which the caller sends back as
// expected_generation so its edit cannot silently overwrite someone else's.
func (s *Server) handleServerGet(w http.ResponseWriter, r *http.Request, id string) {
	snap := s.opts.Registry.Snapshot()
	doc, ok := snap.Servers.V.Servers[id]
	if !ok {
		writeNotFound(w, r) // unknown id reads exactly like an unknown route
		return
	}
	writeOK(w, http.StatusOK, serverDetailWire{Generation: snap.Generation, ID: id, Entry: doc.V})
}

// handleServerCreate implements POST /v1/servers.
func (s *Server) handleServerCreate(w http.ResponseWriter, r *http.Request) {
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	var req serverCreateWire
	if !decodeAdminBody(w, r, body, &req) {
		return
	}
	pre, ok := adminPrecondition(w, r, body)
	if !ok {
		return
	}
	res, err := confops.AddServer(r.Context(), s.opts.Registry,
		confops.ServerSpec{ID: req.ID, Entry: req.Entry}, pre)
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	s.publishRegistryChange(registry.DocServers, res.Generation)
	entry := req.Entry
	writeOK(w, http.StatusOK, serverWriteWire{
		writeResultWire: resultWire(res.Result), ID: req.ID, Entry: &entry,
	})
}

// handleServerPatch implements PATCH /v1/servers/{id}.
//
// The merge happens here, but the RESULT is validated and written by
// confops.UpdateServer — a whole entry, because an entry's fields are not
// independent (the transport decides which half is meaningful) and a
// per-field write could build a shape neither transport accepts.
//
// Concurrency: the patch is applied to an entry read OUTSIDE the lock, so a
// precondition is mandatory here. When the caller supplies none, the
// generation the entry was read at is used — a concurrent write then answers
// 409 rather than losing itself under a merge computed from stale bytes.
// This is the one place a write is guarded without being asked to be.
func (s *Server) handleServerPatch(w http.ResponseWriter, r *http.Request, id string) {
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	var req serverPatchWire
	if !decodeAdminBody(w, r, body, &req) {
		return
	}
	pre, ok := adminPrecondition(w, r, body)
	if !ok {
		return
	}
	reqID := requestIDFrom(r.Context())
	if len(req.Entry) == 0 {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "entry must not be empty",
			`send the fields to change, e.g. {"entry":{"enabled":false}}`, reqID)
		return
	}
	snap := s.opts.Registry.Snapshot()
	doc, found := snap.Servers.V.Servers[id]
	if !found {
		writeNotFound(w, r)
		return
	}
	if pre.Generation == 0 {
		pre.Generation = snap.Generation
	}
	next, err := mergeServerEntry(doc.V, req.Entry)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "decoding entry patch: "+err.Error(), "", reqID)
		return
	}
	res, err := confops.UpdateServer(r.Context(), s.opts.Registry,
		confops.ServerSpec{ID: id, Entry: next}, pre)
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	s.publishRegistryChange(registry.DocServers, res.Generation)
	writeOK(w, http.StatusOK, serverWriteWire{
		writeResultWire: resultWire(res.Result), ID: id, Entry: &next,
	})
}

// mergeServerEntry applies a partial entry onto the stored one.
//
// The container fields are CLEARED before decoding when the patch mentions
// them, because encoding/json merges into an existing map (and into an
// existing pointed-to struct) instead of replacing it. Without this, a patch
// could add an env var but never remove one — a silently one-way door on the
// exact fields that carry secrets and mounts.
func mergeServerEntry(cur registry.ServerEntry, patch json.RawMessage) (registry.ServerEntry, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(patch, &keys); err != nil {
		return registry.ServerEntry{}, err
	}
	next := cur
	if _, ok := keys["env"]; ok {
		next.Env = nil
	}
	if _, ok := keys["headers"]; ok {
		next.Headers = nil
	}
	if _, ok := keys["docker"]; ok {
		next.Docker = nil
	}
	if _, ok := keys["oauth"]; ok {
		next.OAuth = nil
	}
	if err := json.Unmarshal(patch, &next); err != nil {
		return registry.ServerEntry{}, err
	}
	return next, nil
}

// handleServerDelete implements DELETE /v1/servers/{id}.
//
// Deleting a server removes its whole footprint — registry entry, references
// to it in the other registry documents, credentials, and the out-of-registry
// state keyed by its id. What that means is decided ONCE, in
// confops.RemoveServer; this handler only supplies the collaborators, because
// the daemon and `agenthub server rm` must not disagree about what a delete
// leaves behind. A collaborator this daemon was assembled without is skipped
// there, and whatever survives comes back as a warning in the response.
func (s *Server) handleServerDelete(w http.ResponseWriter, r *http.Request, id string) {
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	pre, ok := adminPrecondition(w, r, body)
	if !ok {
		return
	}
	opts := confops.RemoveOptions{State: s.opts.ServerStateForgetters}
	if s.opts.NonRegistry.Secrets != nil {
		opts.Credentials = s.opts.NonRegistry.Secrets
	}
	res, err := confops.RemoveServer(r.Context(), s.opts.Registry, id, pre, opts)
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	s.publishRegistryChange(registry.DocServers, res.Generation)
	writeOK(w, http.StatusOK, serverWriteWire{
		writeResultWire: resultWire(res.Result), ID: id, Deleted: true,
	})
}
