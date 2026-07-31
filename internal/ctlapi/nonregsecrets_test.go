package ctlapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/dinstein/agent-hub/internal/secrets"
)

// nrSentinel is the value the leak assertions hunt for. It is deliberately
// distinctive: if it appears ANYWHERE in a response body or a log record,
// a credential escaped the daemon.
const nrSentinel = "S3NT1NEL-do-not-echo-me-9d41c2"

func TestSecretsList(t *testing.T) {
	vault := newNRVault(
		secrets.Ref{ServerID: "github", Key: "TOKEN"},
		secrets.Ref{ServerID: "acme", Scope: "team", Key: "API_KEY"},
	)
	env := nrStart(t, func(d *NonRegistryDeps) { d.Secrets = vault })

	status, body := nrDo(t, env.sock, http.MethodGet, "/v1/secrets", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out []SecretWire
	nrData(t, body, &out)
	if len(out) != 2 {
		t.Fatalf("got %d rows: %+v", len(out), out)
	}
	// Sorted by (server, scope, key): acme before github.
	if out[0].Server != "acme" || out[0].Scope != "team" || out[0].Key != "API_KEY" {
		t.Errorf("row 0 = %+v", out[0])
	}
	if out[1].Server != "github" || out[1].Scope != secrets.DefaultScope || !out[1].Set {
		t.Errorf("row 1 = %+v", out[1])
	}
	if out[1].Backend != "enc-file" {
		t.Errorf("backend = %q, want enc-file", out[1].Backend)
	}

	// The listing type must not even have a value-shaped key.
	var raw []map[string]any
	nrData(t, body, &raw)
	for _, row := range raw {
		for _, forbidden := range []string{"value", "secret", "plaintext"} {
			if _, ok := row[forbidden]; ok {
				t.Errorf("listing carries a %q key: %+v", forbidden, row)
			}
		}
	}
}

func TestSecretsListFilterAndBackends(t *testing.T) {
	dir := t.TempDir()
	keyringRef := secrets.Ref{ServerID: "github", Key: "KEYRING_KEY"}
	envRef := secrets.Ref{ServerID: "github", Key: "ENV_KEY"}
	fileRef := secrets.Ref{ServerID: "github", Key: "FILE_KEY"}
	other := secrets.Ref{ServerID: "gitlab", Key: "TOKEN"}
	reg := struct {
		Keys []string `json:"keys"`
	}{Keys: []string{keyringRef.StorageKey()}}
	raw, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, keyringRegistryFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The environment level shadows both persistent backends, so the label
	// must describe what a resolution would ACTUALLY use.
	t.Setenv(secrets.EnvName(envRef.Key), "present")

	vault := newNRVault(keyringRef, envRef, fileRef, other)
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.Secrets = vault
		d.SecretsDir = dir
	})

	status, body := nrDo(t, env.sock, http.MethodGet, "/v1/secrets?server=github", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out []SecretWire
	nrData(t, body, &out)
	if len(out) != 3 {
		t.Fatalf("filter leaked other servers: %+v", out)
	}
	got := map[string]string{}
	for _, r := range out {
		got[r.Key] = r.Backend
	}
	for key, want := range map[string]string{
		"KEYRING_KEY": "keyring", "ENV_KEY": "env", "FILE_KEY": "enc-file",
	} {
		if got[key] != want {
			t.Errorf("backend(%s) = %q, want %q", key, got[key], want)
		}
	}
}

func TestSecretsListVaultErrorIsNotAnEmptyList(t *testing.T) {
	vault := newNRVault()
	vault.listErr = errors.New("cannot decrypt secrets.enc")
	env := nrStart(t, func(d *NonRegistryDeps) { d.Secrets = vault })

	status, body := nrDo(t, env.sock, http.MethodGet, "/v1/secrets", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", status, body)
	}
	if code := nrErrCode(t, body); code != CodeInternal {
		t.Errorf("code = %s", code)
	}
}

// TestSecretPutNeverEchoesTheValue is THE test of this surface: a written
// value must not appear in the write response or in any later read — at any
// nesting depth, in any casing.
func TestSecretPutNeverEchoesTheValue(t *testing.T) {
	vault := newNRVault()
	env := nrStart(t, func(d *NonRegistryDeps) { d.Secrets = vault })

	status, put := nrDo(t, env.sock, http.MethodPut, "/v1/secrets/github/TOKEN",
		SecretPutRequest{Value: nrSentinel})
	if status != http.StatusOK {
		t.Fatalf("PUT status = %d: %s", status, put)
	}
	var change SecretChangeWire
	nrData(t, put, &change)
	if change.Action != "stored" || change.Server != "github" || change.Key != "TOKEN" {
		t.Errorf("change = %+v", change)
	}
	if change.Scope != secrets.DefaultScope {
		t.Errorf("scope = %q, want the default", change.Scope)
	}
	// The value did reach the vault — the endpoint is not a no-op.
	if vault.stored[(secrets.Ref{ServerID: "github", Key: "TOKEN"}).StorageKey()] != nrSentinel {
		t.Fatalf("the vault did not receive the value: %+v", vault.stored)
	}

	_, list := nrDo(t, env.sock, http.MethodGet, "/v1/secrets", nil)
	_, one := nrDo(t, env.sock, http.MethodGet, "/v1/secrets?server=github", nil)

	for name, blob := range map[string][]byte{
		"put response": put, "list response": list, "filtered list": one,
	} {
		if nrContains(blob, nrSentinel) {
			t.Errorf("%s leaked the credential: %s", name, blob)
		}
	}
}

// TestSecretPutFailureNeverEchoesTheValue covers the nastier direction: a
// collaborator whose ERROR text embeds the value must not be forwarded.
func TestSecretPutFailureNeverEchoesTheValue(t *testing.T) {
	vault := newNRVault()
	vault.setErr = errors.New("backend refused to store " + nrSentinel)
	env := nrStart(t, func(d *NonRegistryDeps) { d.Secrets = vault })

	status, body := nrDo(t, env.sock, http.MethodPut, "/v1/secrets/github/TOKEN",
		SecretPutRequest{Value: nrSentinel})
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", status, body)
	}
	if nrContains(body, nrSentinel) {
		t.Fatalf("the failure envelope leaked the credential: %s", body)
	}
}

// TestSecretPutMalformedBodyDoesNotEcho: json.Unmarshal quotes the offending
// input in its message, and that input is the credential.
func TestSecretPutMalformedBodyDoesNotEcho(t *testing.T) {
	env := nrStart(t, func(d *NonRegistryDeps) { d.Secrets = newNRVault() })

	// A body that is valid JSON but the wrong shape: the decoder's message
	// would quote the value.
	status, body := nrDo(t, env.sock, http.MethodPut, "/v1/secrets/github/TOKEN",
		map[string]any{"value": []string{nrSentinel}})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	if nrContains(body, nrSentinel) {
		t.Fatalf("the decode error leaked the credential: %s", body)
	}
}

func TestSecretPutRejectsEmptyValue(t *testing.T) {
	vault := newNRVault()
	env := nrStart(t, func(d *NonRegistryDeps) { d.Secrets = vault })

	for _, v := range []string{"", "   ", "\n\t"} {
		status, body := nrDo(t, env.sock, http.MethodPut, "/v1/secrets/github/TOKEN",
			SecretPutRequest{Value: v})
		if status != http.StatusBadRequest {
			t.Errorf("value %q: status = %d, want 400: %s", v, status, body)
		}
		if code := nrErrCode(t, body); code != CodeBadRequest {
			t.Errorf("value %q: code = %s", v, code)
		}
	}
	if len(vault.stored) != 0 {
		t.Errorf("a blank value reached the vault: %+v", vault.stored)
	}
}

func TestSecretPutScope(t *testing.T) {
	vault := newNRVault()
	env := nrStart(t, func(d *NonRegistryDeps) { d.Secrets = vault })

	status, body := nrDo(t, env.sock, http.MethodPut, "/v1/secrets/github/TOKEN",
		SecretPutRequest{Value: "v", Scope: "team"})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var change SecretChangeWire
	nrData(t, body, &change)
	if change.Scope != "team" {
		t.Errorf("scope = %q", change.Scope)
	}
	if _, ok := vault.stored[(secrets.Ref{ServerID: "github", Scope: "team", Key: "TOKEN"}).StorageKey()]; !ok {
		t.Errorf("the scope did not reach the vault: %+v", vault.stored)
	}
}

func TestSecretDelete(t *testing.T) {
	vault := newNRVault(secrets.Ref{ServerID: "github", Key: "TOKEN"})
	env := nrStart(t, func(d *NonRegistryDeps) { d.Secrets = vault })

	status, body := nrDo(t, env.sock, http.MethodDelete, "/v1/secrets/github/TOKEN?scope=team", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var change SecretChangeWire
	nrData(t, body, &change)
	if change.Action != "removed" || change.Scope != "team" {
		t.Errorf("change = %+v", change)
	}
	if len(vault.deleted) != 1 || vault.deleted[0].Scope != "team" {
		t.Fatalf("deleted = %+v", vault.deleted)
	}

	// Idempotent: the vault treats an absent entry as a no-op, and so does
	// the endpoint — a cleanup flow must not branch on unobservable state.
	status, _ = nrDo(t, env.sock, http.MethodDelete, "/v1/secrets/github/TOKEN", nil)
	if status != http.StatusOK {
		t.Errorf("second delete status = %d, want 200", status)
	}
}

func TestSecretDeleteFailure(t *testing.T) {
	vault := newNRVault()
	vault.delErr = errors.New("keyring is locked")
	env := nrStart(t, func(d *NonRegistryDeps) { d.Secrets = vault })

	status, body := nrDo(t, env.sock, http.MethodDelete, "/v1/secrets/github/TOKEN", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", status, body)
	}
}

func TestSecretsPathEscaping(t *testing.T) {
	vault := newNRVault()
	env := nrStart(t, func(d *NonRegistryDeps) { d.Secrets = vault })

	// A %2F inside a path parameter must not smuggle an extra segment past
	// the router: the request is refused, not re-interpreted.
	status, _ := nrDo(t, env.sock, http.MethodPut, "/v1/secrets/github/A%2FB",
		SecretPutRequest{Value: "v"})
	if status != http.StatusNotFound {
		t.Errorf("escaped slash: status = %d, want 404", status)
	}
	if len(vault.stored) != 0 {
		t.Errorf("a smuggled path wrote something: %+v", vault.stored)
	}
}
