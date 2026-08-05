package httpbridge_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/tier"
)

// TestAuthenticateDoesNotRetainTheCredential pins the property that keeps
// agenthub out of the token-passthrough anti-pattern MCP 2026-07-28 names a
// MUST NOT: "the MCP server MUST NOT pass through the token it received from
// the MCP client".
//
// The property holds structurally rather than by a check somewhere — Caller
// carries the agent token's NAME, and the presented value is compared and
// dropped — which is exactly the kind of property that survives until
// someone adds a field for a good reason. A raw credential on Caller is
// reachable by everything downstream of authentication, including the code
// that builds outbound requests to third-party servers, and that is the
// whole distance between this design and the anti-pattern.
//
// It walks the struct rather than naming fields, so a field added later is
// covered without this test being updated to match it.
func TestAuthenticateDoesNotRetainTheCredential(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "agent-1", Tier: tier.Read})

	a := &httpbridge.Authenticator{Tokens: store, Now: func() time.Time { return time.Now() }}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "127.0.0.1:51234"
	r.Header.Set("Authorization", "Bearer "+value)

	caller, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if caller.Kind != httpbridge.CallerAgent {
		t.Fatalf("caller kind = %q, want the agent kind", caller.Kind)
	}
	if caller.Token != "agent-1" {
		t.Fatalf("Caller.Token = %q, want the token's NAME", caller.Token)
	}
	assertNoSecret(t, reflect.ValueOf(*caller), "Caller", value)
}

// TestAdminAuthenticateDoesNotRetainTheCredential: the same for the
// operator's bearer, which is the one an attacker would most want relayed.
func TestAdminAuthenticateDoesNotRetainTheCredential(t *testing.T) {
	t.Parallel()
	const admin = "super-secret-operator-bearer"
	a := &httpbridge.Authenticator{AdminToken: admin}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "127.0.0.1:51234"
	r.Header.Set("Authorization", "Bearer "+admin)

	caller, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	assertNoSecret(t, reflect.ValueOf(*caller), "Caller", admin)
}

// assertNoSecret fails if secret appears anywhere reachable in v.
func assertNoSecret(t *testing.T, v reflect.Value, path, secret string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		if strings.Contains(v.String(), secret) {
			t.Fatalf("%s holds the presented credential; authentication must compare it and "+
				"drop it, or every caller of this struct becomes able to relay it", path)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			assertNoSecret(t, v.Field(i), path+"."+v.Type().Field(i).Name, secret)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			assertNoSecret(t, v.Index(i), path, secret)
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			assertNoSecret(t, v.Elem(), path, secret)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			assertNoSecret(t, k, path, secret)
			assertNoSecret(t, v.MapIndex(k), path, secret)
		}
	}
}
