package httpbridge_test

import (
	"errors"
	"testing"

	"github.com/dinstein/agent-hub/internal/httpbridge"
)

// Binding the endpoint is itself an authorization decision (docs/architecture.md §2).
// The matrix below is the whole rule, including the two cases that are easy
// to get wrong: a registered client authorizes ONLY loopback, and
// --insecure-loopback never authorizes a non-loopback bind.
func TestAuthorizeBindMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		cfg        httpbridge.BindConfig
		wantErr    bool
		wantReason string
	}{
		{
			name:    "loopback, nothing configured",
			cfg:     httpbridge.BindConfig{Addr: "127.0.0.1:7777"},
			wantErr: true,
		},
		{
			name:       "loopback with an admin token",
			cfg:        httpbridge.BindConfig{Addr: "127.0.0.1:7777", HasAdminToken: true},
			wantReason: "admin-token",
		},
		{
			name:       "loopback with an agent token",
			cfg:        httpbridge.BindConfig{Addr: "127.0.0.1:7777", ActiveAgentTokens: 1},
			wantReason: "agent-token",
		},
		{
			name:       "loopback with a registered client",
			cfg:        httpbridge.BindConfig{Addr: "localhost:7777", RegisteredClients: 2},
			wantReason: "registered-client",
		},
		{
			name:       "loopback with the escape hatch",
			cfg:        httpbridge.BindConfig{Addr: "[::1]:7777", InsecureLoopback: true},
			wantReason: "insecure-loopback",
		},
		{
			name:    "non-loopback with only a registered client",
			cfg:     httpbridge.BindConfig{Addr: "192.168.1.5:7777", RegisteredClients: 5},
			wantErr: true,
		},
		{
			name:    "non-loopback with the escape hatch",
			cfg:     httpbridge.BindConfig{Addr: "0.0.0.0:7777", InsecureLoopback: true},
			wantErr: true,
		},
		{
			name:    "every interface with the escape hatch",
			cfg:     httpbridge.BindConfig{Addr: ":7777", InsecureLoopback: true, RegisteredClients: 3},
			wantErr: true,
		},
		{
			name:       "non-loopback with a token",
			cfg:        httpbridge.BindConfig{Addr: "0.0.0.0:7777", HasAdminToken: true},
			wantReason: "admin-token",
		},
		{
			name:    "a hostname is not provably loopback",
			cfg:     httpbridge.BindConfig{Addr: "my-laptop.local:7777", InsecureLoopback: true},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		dec, err := httpbridge.AuthorizeBind(tc.cfg)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: bind was authorized (%+v), want refusal", tc.name, dec)
				continue
			}
			if !errors.Is(err, httpbridge.ErrBindUnauthorized) {
				t.Errorf("%s: err = %v, want ErrBindUnauthorized", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: err = %v, want an authorized bind", tc.name, err)
			continue
		}
		if dec.Reason != tc.wantReason {
			t.Errorf("%s: reason = %q, want %q", tc.name, dec.Reason, tc.wantReason)
		}
	}
}

// The refusal has to tell the operator how to proceed — a fail-closed default
// nobody can get past is a bug report, not a security property.
func TestBindRefusalNamesTheWayOut(t *testing.T) {
	t.Parallel()
	_, err := httpbridge.AuthorizeBind(httpbridge.BindConfig{Addr: "127.0.0.1:7777"})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"agenthub token create", "--insecure-loopback"} {
		if !contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
