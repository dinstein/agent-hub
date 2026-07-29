package ctlapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/dinstein/agent-hub/internal/catalog"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
)

// The curated catalog on the wire (docs/modules/controlplane.md): GET /v1/catalog[?q=],
// GET /v1/catalog/{id}, POST /v1/catalog/{id}/add.
//
// The directory is EMBEDDED, so unlike the non-registry surface there is no
// collaborator to be missing and no "unavailable on this daemon" case: a
// daemon that answers /v1/ping answers these. Nothing here reaches the
// network — a GUI that talked to a remote index itself would break the
// "GUI only through the control plane" constraint, and a daemon that did it
// behind the user's back would be worse.
//
// The add path goes through internal/confops like every other write: same
// validation, same optimistic-concurrency guard, same audit line. A curated
// entry gets no shortcut through the rules; the catalog only saves the
// typing.

// catalogEntryWire is one catalog entry on the wire.
//
// catalog.Entry is EMBEDDED rather than mirrored, so a field added to the
// package appears here without a second definition drifting from the first.
// The two derived fields are computed server-side because they encode a
// judgement (docs/modules/controlplane.md "skip whatever can be skipped") that every frontend must make identically:
// a GUI and the CLI disagreeing about whether an entry is one-click would be
// two different products.
type catalogEntryWire struct {
	catalog.Entry
	// NeedsConfig is false when the entry can be added with a single click.
	NeedsConfig bool `json:"needs_config"`
	// RequiredKeys are the credentials that must be stored afterwards
	// (optional ones excluded).
	RequiredKeys []string `json:"required_keys,omitempty"`
}

func catalogWire(e catalog.Entry) catalogEntryWire {
	return catalogEntryWire{Entry: e, NeedsConfig: e.NeedsConfig(), RequiredKeys: e.RequiredKeys()}
}

// catalogListWire is the answer to GET /v1/catalog. An object rather than a
// bare array so the query that produced it travels with the result — a
// frontend rendering a stale response must be able to tell.
type catalogListWire struct {
	Query   string             `json:"query,omitempty"`
	Entries []catalogEntryWire `json:"entries"`
}

// catalogAddRequest is the POST /v1/catalog/{id}/add body.
type catalogAddRequest struct {
	preconditionWire
	// Name overrides the registry id ("" = the catalog id).
	Name string `json:"name,omitempty"`
	// Params supplies the entry's declared {{placeholders}}. A missing or
	// unknown one is refused, never guessed.
	Params map[string]string `json:"params,omitempty"`
}

// catalogAddResult is the answer to a successful add.
type catalogAddResult struct {
	writeResultWire
	// ID is the server id as stored; CatalogID is where it came from.
	ID        string                `json:"id"`
	CatalogID string                `json:"catalog_id"`
	Entry     *registry.ServerEntry `json:"entry,omitempty"`
	// NextSteps are the commands that finish the job (storing a credential,
	// logging in). They are rendered here rather than in each frontend so
	// the GUI and the CLI tell the user the same thing — and because
	// showing the equivalent command IS the GUI's teaching surface (docs/modules/controlplane.md).
	NextSteps []string `json:"next_steps,omitempty"`
}

// handleCatalogList implements GET /v1/catalog (optional ?q=).
func (s *Server) handleCatalogList(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	entries := catalog.Search(query)
	out := catalogListWire{Query: query, Entries: make([]catalogEntryWire, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, catalogWire(e))
	}
	writeOK(w, http.StatusOK, out)
}

// handleCatalogGet implements GET /v1/catalog/{id}.
func (s *Server) handleCatalogGet(w http.ResponseWriter, r *http.Request, id string) {
	entry, ok := catalog.Get(id)
	if !ok {
		writeNotFound(w, r) // unknown id reads exactly like an unknown route
		return
	}
	writeOK(w, http.StatusOK, catalogWire(entry))
}

// handleCatalogAdd implements POST /v1/catalog/{id}/add.
//
// Failure direction: an entry whose parameters are incomplete is REFUSED
// with every missing name listed, never added with a literal "{{directory}}"
// in its argv. The refusal happens before the registry is opened, so a bad
// request leaves nothing behind.
func (s *Server) handleCatalogAdd(w http.ResponseWriter, r *http.Request, catalogID string) {
	reqID := requestIDFrom(r.Context())
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	var req catalogAddRequest
	if len(body) > 0 && !decodeAdminBody(w, r, body, &req) {
		return
	}
	pre, ok := adminPrecondition(w, r, body)
	if !ok {
		return
	}
	source, found := catalog.Get(catalogID)
	if !found {
		writeNotFound(w, r)
		return
	}
	name := req.Name
	if name == "" {
		name = source.ID
	}
	entry, err := source.Render(req.Params)
	if err != nil {
		var perr *catalog.ParamError
		if errors.As(err, &perr) {
			writeErr(w, http.StatusBadRequest, CodeBadRequest, perr.Error(),
				catalogParamHint(source), reqID)
			return
		}
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", reqID)
		return
	}

	start := time.Now()
	res, err := confops.AddServer(r.Context(), s.opts.Registry,
		confops.ServerSpec{ID: name, Entry: entry}, pre)
	s.auditAdmin(r, adminAudit{
		action: "catalog/add:" + catalogID, server: name, body: body, err: err, dur: time.Since(start),
	})
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	s.publishRegistryChange(registry.DocServers, res.Generation)
	writeOK(w, http.StatusOK, catalogAddResult{
		writeResultWire: resultWire(res.Result),
		ID:              name,
		CatalogID:       catalogID,
		Entry:           &entry,
		NextSteps:       catalogNextSteps(source, name),
	})
}

// catalogParamHint lists what the entry actually declares, so a rejected
// request carries its own fix.
func catalogParamHint(e catalog.Entry) string {
	if len(e.Params) == 0 {
		return "this catalog entry takes no parameters"
	}
	hint := "this entry needs: "
	for i, p := range e.Params {
		if i > 0 {
			hint += ", "
		}
		hint += p.Name
		if p.Example != "" {
			hint += " (e.g. " + p.Example + ")"
		}
	}
	return hint
}

// catalogNextSteps renders what is left to do after the entry is stored.
//
// Adding the definition is not the same as making the server work: a
// credential still has to be stored, an OAuth server still has to be logged
// into. Saying so here — in the exact command form — is what keeps a
// one-click add from looking finished when it is not.
func catalogNextSteps(e catalog.Entry, serverID string) []string {
	var out []string
	for _, key := range e.RequiredKeys() {
		out = append(out, "agenthub secret set "+serverID+" "+key)
	}
	if e.Auth == catalog.AuthOAuth {
		out = append(out, "agenthub auth login "+serverID)
	}
	return out
}
