package api

import (
	"context"
	"net/http"
	"net/url"
)

// SecretScopeGlobal is the default vault scope. The vault key is the
// composite (serverID, scopeName); "_global" is the scope a secret gets when
// the caller names none, and it is what a listing reports for one.
const SecretScopeGlobal = "_global"

// Secret backends, as reported by SecretRef.Backend. The label says where a
// resolution would find the value TODAY: the environment shadows both
// persistent backends, so an env-provided key reads as env even when a
// stored copy also exists.
const (
	// SecretBackendEnv: the process environment holds it.
	SecretBackendEnv = "env"
	// SecretBackendKeyring: the OS keychain holds it.
	SecretBackendKeyring = "keyring"
	// SecretBackendEncFile: the encrypted file store holds it.
	SecretBackendEncFile = "enc-file"
)

// Secret write actions reported by SecretChange.Action.
const (
	// SecretStored is the answer to a successful Set.
	SecretStored = "stored"
	// SecretRemoved is the answer to a successful Delete. Deleting an
	// absent credential also reports it: the vault's delete is idempotent
	// by contract.
	SecretRemoved = "removed"
)

// SecretRef identifies one stored secret and says where it lives.
//
// RED LINE (docs/subsystems/docs/subsystems/controlplane.md rule 5): there is no value field, and there never
// will be. Not "we do not populate it" — the type cannot carry one, so no
// listing, no log line and no frontend cache can ever contain a credential.
// A value is verified by making a REAL call (Servers.Test), never by reading
// it back.
type SecretRef struct {
	Server string `json:"server"`
	// Scope is the vault scope; a listing renders SecretScopeGlobal rather
	// than an empty string.
	Scope string `json:"scope"`
	Key   string `json:"key"`
	// Backend is one of the SecretBackend* constants (read side only).
	Backend string `json:"backend"`
	// Set is always true for a listed entry. It exists so a consumer can
	// join this list against a server's required keys without inferring
	// presence from list membership.
	Set bool `json:"set"`
}

// secretPutBody is the PUT /v1/secrets/{server}/{key} request body.
//
// It is UNEXPORTED on purpose: the plaintext travels as a function argument
// and lives only inside this request. No exported type in this package has a
// field a secret value could be assigned to, so a frontend cannot
// accidentally hold one in a model object it later renders, logs or caches.
type secretPutBody struct {
	Value string `json:"value"`
	Scope string `json:"scope,omitempty"`
}

// SecretChange is the answer to a secret write. Same rule as everywhere
// else: it names the reference and says what happened, and carries no value.
type SecretChange struct {
	// Action is SecretStored or SecretRemoved.
	Action string `json:"action"`
	Server string `json:"server"`
	Key    string `json:"key"`
	Scope  string `json:"scope"`
}

// SecretsService manages the credential vault's key names.
//
// Reads never return values and writes never echo them; even the failure
// message of a write is fixed daemon-side, because an error surfaced
// verbatim is one collaborator bug away from carrying the credential into a
// GUI, a clipboard and a screenshot. A frontend should use a password input,
// clear it on submit, and offer no reveal toggle: there is nothing on this
// API to reveal it with.
//
// These calls carry no expectedGeneration: the vault is not the registry, so
// there is no shared document to lose a compare-and-swap against.
type SecretsService struct{ c *Client }

// List returns every stored secret's identity and backend — names only.
// A non-empty server narrows the listing.
func (s *SecretsService) List(ctx context.Context, server string) ([]SecretRef, error) {
	var q url.Values
	if server != "" {
		q = url.Values{"server": {server}}
	}
	var out []SecretRef
	if err := s.c.do(ctx, http.MethodGet, "/secrets", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Set stores value under (server, scope, key); an empty scope selects
// SecretScopeGlobal. A blank value is refused rather than stored: the vault
// treats a blank as unset at every resolution level, so storing one would
// report success and leave the server exactly as broken as before.
func (s *SecretsService) Set(ctx context.Context, server, scope, key, value string) (SecretChange, error) {
	var out SecretChange
	err := s.c.do(ctx, http.MethodPut, secretPath(server, key), nil,
		secretPutBody{Value: value, Scope: scope}, &out)
	return out, err
}

// Delete removes one stored secret. It is idempotent: removing a credential
// that is not there succeeds, so a cleanup path never has to branch on state
// it cannot observe.
func (s *SecretsService) Delete(ctx context.Context, server, scope, key string) (SecretChange, error) {
	var q url.Values
	if scope != "" {
		q = url.Values{"scope": {scope}}
	}
	var out SecretChange
	err := s.c.do(ctx, http.MethodDelete, secretPath(server, key), q, nil, &out)
	return out, err
}

func secretPath(server, key string) string {
	return "/secrets/" + url.PathEscape(server) + "/" + url.PathEscape(key)
}
