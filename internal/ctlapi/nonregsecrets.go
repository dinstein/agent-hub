package ctlapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/secrets"
)

// GET /v1/secrets, PUT|DELETE /v1/secrets/{server}/{key} — the credential
// surface (docs/modules/controlplane.md).
//
// THE INVARIANT, restated because it is the whole reason this file is
// separate: a stored value never leaves the daemon through this API. The
// listing type below has no value field — not an omitted one, none at all —
// and the write path uses the value it was given and forgets it. Even the
// failure envelope of a write is a FIXED string: an error surfaced verbatim
// is one bug in a collaborator away from carrying the credential into a GUI,
// a clipboard and a screenshot.
//
// Verifying that a credential is correct is POST /v1/servers/{id}/test —
// prove it works by using it, never by printing it back (docs/modules/controlplane.md #5).

// keyringRegistryFileName mirrors internal/secrets' unexported keyring key
// registry. It is READ ONLY, and only to label a row's backend: enumerating
// the OS keyring is impossible, and probing it would raise a keychain prompt
// just to draw a table. Kept in sync by hand with secrets/keyring.go — a
// stale name only mislabels a backend, it never fails a listing.
const keyringRegistryFileName = "keyring-keys.json"

// SecretWire is one stored credential reference. THERE IS NO VALUE FIELD;
// adding one would break the invariant this whole file exists to hold.
type SecretWire struct {
	Server string `json:"server"`
	Scope  string `json:"scope"`
	Key    string `json:"key"`
	// Backend is where a resolution would find the value today: env,
	// keyring or enc-file.
	Backend string `json:"backend"`
	// Set is always true for a listed reference. It exists so a frontend
	// can join this list against a server's required keys without inferring
	// presence from list membership.
	Set bool `json:"set"`
}

// SecretPutRequest is the body of PUT /v1/secrets/{server}/{key}.
//
// Value is INPUT ONLY: it is handed to the vault and never echoed, logged
// or hashed into an audit record.
type SecretPutRequest struct {
	Value string `json:"value"`
	// Scope is the vault scope ("" = secrets.DefaultScope).
	Scope string `json:"scope,omitempty"`
}

// SecretChangeWire is the answer to a write. Same rule: no value.
type SecretChangeWire struct {
	// Action is "stored" or "removed".
	Action string `json:"action"`
	Server string `json:"server"`
	Key    string `json:"key"`
	Scope  string `json:"scope"`
}

// handleSecretsList implements GET /v1/secrets (optional ?server= filter).
func (s *Server) handleSecretsList(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDFrom(r.Context())
	refs, err := s.opts.NonRegistry.Secrets.List(r.Context())
	if err != nil {
		// A vault that cannot be enumerated is reported, never rendered as
		// an empty list: "no secrets stored" and "the vault is unreadable"
		// lead an operator to opposite conclusions.
		writeErr(w, http.StatusInternalServerError, CodeInternal,
			"listing stored credentials failed: "+err.Error(),
			"check AGENTHUB_SECRET_KEY and the secrets directory", reqID)
		return
	}
	filter := r.URL.Query().Get("server")
	inKeyring := loadKeyringRegistry(s.opts.NonRegistry.SecretsDir)
	out := make([]SecretWire, 0, len(refs))
	for _, ref := range refs {
		if filter != "" && ref.ServerID != filter {
			continue
		}
		out = append(out, SecretWire{
			Server:  ref.ServerID,
			Scope:   scopeOrDefault(ref.Scope),
			Key:     ref.Key,
			Backend: secretBackend(ref, inKeyring),
			Set:     true,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Key < out[j].Key
	})
	writeOK(w, http.StatusOK, out)
}

// handleSecretPut implements PUT /v1/secrets/{server}/{key}.
func (s *Server) handleSecretPut(w http.ResponseWriter, r *http.Request, server, key string) {
	reqID := requestIDFrom(r.Context())
	var req SecretPutRequest
	if err := decodeBody(r, &req); err != nil {
		// The body carries the credential, so the decoder's message is NOT
		// forwarded: json.Unmarshal quotes the offending input on syntax
		// errors, and that input is the secret.
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"malformed request body", `expected {"value":"…"}`, reqID)
		return
	}
	if strings.TrimSpace(req.Value) == "" {
		// Fail direction: refuse. The vault treats a blank value as unset at
		// every resolution level, so storing one would report success and
		// leave the server exactly as broken as before.
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"refusing to store an empty value", "send a non-blank value", reqID)
		return
	}
	ref := secrets.Ref{ServerID: server, Scope: req.Scope, Key: key}
	if err := ref.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", reqID)
		return
	}

	start := time.Now()
	err := s.opts.NonRegistry.Secrets.Set(r.Context(), ref, req.Value)
	s.auditNonReg(r, server, "secrets/set", "", err == nil, time.Since(start))
	if err != nil {
		// Fixed message on purpose (see the file header): the underlying
		// error is diagnostics for the daemon log, never response bytes.
		s.log.Error("ctlapi: storing a credential failed",
			"server", server, "key", key, "err", err, "requestId", reqID)
		writeErr(w, http.StatusInternalServerError, CodeInternal,
			"storing the credential failed",
			"see the daemon log for the reason; the value was not recorded anywhere", reqID)
		return
	}
	writeOK(w, http.StatusOK, SecretChangeWire{
		Action: "stored", Server: server, Key: key, Scope: scopeOrDefault(req.Scope),
	})
}

// handleSecretDelete implements DELETE /v1/secrets/{server}/{key}
// (optional ?scope=). Deleting an absent credential succeeds: the vault's
// Delete is idempotent by contract, and a cleanup script must not have to
// branch on state it cannot observe.
func (s *Server) handleSecretDelete(w http.ResponseWriter, r *http.Request, server, key string) {
	reqID := requestIDFrom(r.Context())
	scope := r.URL.Query().Get("scope")
	ref := secrets.Ref{ServerID: server, Scope: scope, Key: key}
	if err := ref.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", reqID)
		return
	}
	start := time.Now()
	err := s.opts.NonRegistry.Secrets.Delete(r.Context(), ref)
	s.auditNonReg(r, server, "secrets/rm", "", err == nil, time.Since(start))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeInternal,
			"removing the credential failed: "+err.Error(), "", reqID)
		return
	}
	writeOK(w, http.StatusOK, SecretChangeWire{
		Action: "removed", Server: server, Key: key, Scope: scopeOrDefault(scope),
	})
}

// secretBackend labels where a resolution would find this ref's value.
// The environment levels shadow both persistent backends, so an env-provided
// key is reported as "env" even when a stored copy also exists — the label
// must describe what the daemon would actually use.
func secretBackend(ref secrets.Ref, inKeyring map[string]bool) string {
	if name := secrets.EnvName(ref.Key); name != secrets.EnvEncKey {
		if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
			return "env"
		}
	}
	if inKeyring[ref.StorageKey()] {
		return "keyring"
	}
	return "enc-file"
}

// loadKeyringRegistry reads the keyring key registry. A missing, unreadable
// or malformed file yields an empty set: the classification is cosmetic and
// mislabeling a backend must never fail a listing.
func loadKeyringRegistry(dir string) map[string]bool {
	if dir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, keyringRegistryFileName))
	if err != nil {
		return nil
	}
	var doc struct {
		Keys []string `json:"keys"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	out := make(map[string]bool, len(doc.Keys))
	for _, k := range doc.Keys {
		out[k] = true
	}
	return out
}

// scopeOrDefault renders an empty vault scope as its effective value.
func scopeOrDefault(s string) string {
	if s == "" {
		return secrets.DefaultScope
	}
	return s
}
