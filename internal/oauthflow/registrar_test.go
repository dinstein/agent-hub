package oauthflow

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// TestDCRRegistersAsPublicClient: token_endpoint_auth_method is pinned to
// "none". agenthub runs on the user's machine; a "client secret" it held
// would be readable by anyone who can read the vault.
func TestDCRRegistersAsPublicClient(t *testing.T) {
	as := newFakeAS(t)
	reg := NewDCRRegistrar(as.client())
	if reg.Kind() != "dcr" {
		t.Fatalf("kind = %q", reg.Kind())
	}
	creds, err := reg.Register(context.Background(), as.metadata(), RegistrationRequest{
		ClientName:   "agenthub",
		RedirectURIs: []string{"http://127.0.0.1:5731/callback"},
		Scopes:       []string{"read"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if creds.ClientID != "client-abc" {
		t.Fatalf("client_id = %q", creds.ClientID)
	}
	as.mu.Lock()
	body := as.lastRegBody
	as.mu.Unlock()
	if body["token_endpoint_auth_method"] != TokenEndpointAuthNone {
		t.Fatalf("token_endpoint_auth_method = %v, want %q", body["token_endpoint_auth_method"], TokenEndpointAuthNone)
	}
	if got := body["redirect_uris"]; !reflect.DeepEqual(got, []any{"http://127.0.0.1:5731/callback"}) {
		t.Fatalf("redirect_uris = %v", got)
	}
	grants, _ := body["grant_types"].([]any)
	if !reflect.DeepEqual(grants, []any{GrantAuthorizationCode, GrantRefreshToken}) {
		t.Fatalf("grant_types = %v", grants)
	}
	if body["scope"] != "read" {
		t.Fatalf("scope = %v", body["scope"])
	}
}

func TestDCRRequiresRedirectURIForAuthorizationCode(t *testing.T) {
	as := newFakeAS(t)
	reg := NewDCRRegistrar(as.client())
	_, err := reg.Register(context.Background(), as.metadata(), RegistrationRequest{})
	if !errors.Is(err, ErrRegistration) {
		t.Fatalf("err = %v, want ErrRegistration", err)
	}
	// The device flow has no redirect leg, so demanding a redirect URI
	// there would force callers to invent one.
	if _, err := reg.Register(context.Background(), as.metadata(), RegistrationRequest{
		GrantTypes: []string{GrantDeviceCode, GrantRefreshToken},
	}); err != nil {
		t.Fatalf("device-only registration: %v", err)
	}
}

func TestDCRWithoutRegistrationEndpoint(t *testing.T) {
	as := newFakeAS(t)
	as.noRegistration = true
	reg := NewDCRRegistrar(as.client())
	_, err := reg.Register(context.Background(), as.metadata(), RegistrationRequest{
		RedirectURIs: []string{"http://127.0.0.1:1/callback"},
	})
	if !errors.Is(err, ErrRegistration) {
		t.Fatalf("err = %v", err)
	}
	var fe *FlowError
	if !errors.As(err, &fe) || fe.Registration != RegistrationFailed {
		t.Fatalf("registration status not recorded: %+v", fe)
	}
}

// TestDCRProviderRefusal reproduces the Figma-shaped failure of docs/status/oauth.md
// : a 403 on DCR must produce a precise suggestion, never a blank
// client_id that fails later with an unrelated message.
func TestDCRProviderRefusal(t *testing.T) {
	as := newFakeAS(t)
	as.registrationStatus = 403
	reg := NewDCRRegistrar(as.client())
	creds, err := reg.Register(context.Background(), as.metadata(), RegistrationRequest{
		RedirectURIs: []string{"http://127.0.0.1:1/callback"},
	})
	if err == nil {
		t.Fatalf("expected failure, got creds %+v", creds)
	}
	if creds != nil {
		t.Fatal("a failed registration must not return credentials")
	}
	var fe *FlowError
	if !errors.As(err, &fe) {
		t.Fatalf("not a FlowError: %v", err)
	}
	if fe.Type != ErrorTypeRegistration || fe.Registration != RegistrationFailed {
		t.Fatalf("classification: %+v", fe)
	}
	if fe.Suggestion == "" {
		t.Fatal("a provider refusal must carry a suggestion")
	}
}

// TestClientIDMetadataRegistrarIsAnExplicitSeam: the DCR successor exists
// as a wired seam and reports ErrNotImplemented rather than silently
// falling back to DCR — a silent fallback would hide the migration.
func TestClientIDMetadataRegistrarIsAnExplicitSeam(t *testing.T) {
	reg := NewClientIDMetadataRegistrar("https://agenthub.example/client.json")
	if reg.Kind() != "client_id_metadata_document" {
		t.Fatalf("kind = %q", reg.Kind())
	}
	_, err := reg.Register(context.Background(), &AuthServerMetadata{}, RegistrationRequest{})
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("err = %v, want ErrNotImplemented", err)
	}
}

func TestStaticRegistrar(t *testing.T) {
	reg := NewStaticRegistrar(ClientCredentials{ClientID: "preset"})
	if reg.Kind() != "preconfigured" {
		t.Fatalf("kind = %q", reg.Kind())
	}
	creds, err := reg.Register(context.Background(), nil, RegistrationRequest{})
	if err != nil || creds.ClientID != "preset" {
		t.Fatalf("creds = %+v err = %v", creds, err)
	}
	if _, err := NewStaticRegistrar(ClientCredentials{}).Register(context.Background(), nil, RegistrationRequest{}); err == nil {
		t.Fatal("an empty preconfigured client_id must be refused")
	}
}
