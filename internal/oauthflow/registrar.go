package oauthflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// TokenEndpointAuthNone is the only token endpoint authentication method
// agenthub registers with. agenthub is a public client: it runs on the
// user's machine, so any "client secret" it held would be readable by
// whoever can read the vault, and PKCE — not a shared secret — is what
// actually protects the code exchange.
const TokenEndpointAuthNone = "none"

// RegistrationRequest is what the caller wants registered. It is
// mechanism-independent on purpose: dynamic client registration and Client
// ID Metadata Documents consume the same input.
type RegistrationRequest struct {
	// ClientName is the human-facing application name shown on the consent
	// screen.
	ClientName string
	// RedirectURIs are the callback URIs. For the loopback mode this is
	// http://127.0.0.1:<port>/callback with the port actually bound.
	RedirectURIs []string
	// GrantTypes defaults to ["authorization_code", "refresh_token"].
	GrantTypes []string
	// ResponseTypes defaults to ["code"].
	ResponseTypes []string
	// Scopes is the requested scope set. Empty means "send no scope
	// member" — see BuildAuthorizeURL for why offline_access is never
	// added here.
	Scopes []string
	// SoftwareID / SoftwareVersion are optional RFC 7591 members.
	SoftwareID      string
	SoftwareVersion string
	// ClientURI is an optional informational URL.
	ClientURI string
}

// ClientCredentials is the result of registration (or of a preconfigured
// client). ClientSecret is empty for public clients, which is the norm
// here; it exists because a handful of providers issue one anyway and
// refuse the token request without it.
type ClientCredentials struct {
	ClientID              string `json:"client_id"`
	ClientSecret          string `json:"client_secret,omitempty"`
	ClientIDIssuedAt      int64  `json:"client_id_issued_at,omitempty"`
	ClientSecretExpiresAt int64  `json:"client_secret_expires_at,omitempty"`
	// RegistrationAccessToken and RegistrationClientURI let a future
	// implementation update or delete the registration (RFC 7592). Stored
	// but unused today.
	RegistrationAccessToken string `json:"registration_access_token,omitempty"`
	RegistrationClientURI   string `json:"registration_client_uri,omitempty"`
}

// ClientRegistrar obtains client credentials for an authorization server.
//
// This is the migration seam of canonical.md §5b: dynamic client
// registration is deprecated upstream (deprecated 2026-07-28, earliest
// removal 2027-07-28) and its declared successor is Client ID Metadata
// Documents. Both are implementations of this one interface so switching is
// a wiring change, not a rewrite of the flow.
type ClientRegistrar interface {
	// Kind names the mechanism, for diagnostics and for the persisted
	// state's provenance.
	Kind() string
	// Register returns credentials usable at md's token endpoint.
	Register(ctx context.Context, md *AuthServerMetadata, req RegistrationRequest) (*ClientCredentials, error)
}

// --- implementation 1: RFC 7591 dynamic client registration ---------------

// dcrRegistrar implements ClientRegistrar via RFC 7591.
//
// DEPRECATED-UPSTREAM(dcr, earliest-removal: 2027-07-28)
type dcrRegistrar struct{ client *Client }

// NewDCRRegistrar returns the RFC 7591 dynamic-client-registration
// implementation of ClientRegistrar.
//
// DEPRECATED-UPSTREAM(dcr, earliest-removal: 2027-07-28)
func NewDCRRegistrar(client *Client) ClientRegistrar { return &dcrRegistrar{client: client} }

// Kind implements ClientRegistrar.
func (r *dcrRegistrar) Kind() string { return "dcr" }

// dcrPayload is the RFC 7591 client metadata document. Field order here is
// irrelevant to the protocol but the *content* is not: token_endpoint_auth_
// method is pinned to "none" and never taken from metadata.
type dcrPayload struct {
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope,omitempty"`
	SoftwareID              string   `json:"software_id,omitempty"`
	SoftwareVersion         string   `json:"software_version,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
}

// Register performs the RFC 7591 POST.
//
// DEPRECATED-UPSTREAM(dcr, earliest-removal: 2027-07-28)
//
// Failure direction: a missing registration_endpoint, a non-2xx status and
// a response without a client_id are all errors carrying
// RegistrationFailed, never a zero-valued credential. Several providers
// (Figma among them) answer 403 to DCR and then reject an empty client_id
// with an unrelated message — returning a blank client_id here would move
// the failure one step away from its cause.
func (r *dcrRegistrar) Register(ctx context.Context, md *AuthServerMetadata, req RegistrationRequest) (*ClientCredentials, error) {
	if md == nil || strings.TrimSpace(md.RegistrationEndpoint) == "" {
		e := newFlowError(ErrorTypeRegistration,
			fmt.Errorf("%w: authorization server advertises no registration_endpoint", ErrRegistration))
		e.Registration = RegistrationFailed
		e.Suggestion = "this provider does not support dynamic client registration; register a client manually and configure its client_id"
		if md != nil {
			e.Issuer = md.Issuer
		}
		return nil, e
	}
	grants := defaultStrings(req.GrantTypes, []string{GrantAuthorizationCode, GrantRefreshToken})
	// redirect_uris are required only for grant types that redirect. The
	// device flow has no redirect at all, so demanding one there would
	// force callers to invent a fake URI and register it.
	if len(req.RedirectURIs) == 0 && containsString(grants, GrantAuthorizationCode) {
		e := newFlowError(ErrorTypeRegistration,
			fmt.Errorf("%w: authorization_code registration needs at least one redirect_uri", ErrRegistration))
		e.Registration = RegistrationFailed
		return nil, e
	}
	payload := dcrPayload{
		ClientName:    req.ClientName,
		RedirectURIs:  req.RedirectURIs,
		GrantTypes:    grants,
		ResponseTypes: defaultStrings(req.ResponseTypes, []string{"code"}),
		// Pinned, never negotiated: agenthub is a public client.
		TokenEndpointAuthMethod: TokenEndpointAuthNone,
		Scope:                   strings.Join(req.Scopes, " "),
		SoftwareID:              req.SoftwareID,
		SoftwareVersion:         req.SoftwareVersion,
		ClientURI:               req.ClientURI,
	}
	body, err := r.client.postJSON(ctx, md.RegistrationEndpoint, payload, nil)
	if err != nil {
		return nil, r.wrap(md, err)
	}
	var creds ClientCredentials
	if err := json.Unmarshal(body, &creds); err != nil {
		return nil, r.wrap(md, fmt.Errorf("%w: registration response is not JSON: %v", ErrRegistration, err))
	}
	if strings.TrimSpace(creds.ClientID) == "" {
		return nil, r.wrap(md, fmt.Errorf("%w: registration response carries no client_id", ErrRegistration))
	}
	return &creds, nil
}

// wrap converts a transport/status error into a registration FlowError,
// preserving an already-typed FlowError's classification (e.g. an SSRF
// refusal must stay ErrorTypeBlocked).
func (r *dcrRegistrar) wrap(md *AuthServerMetadata, err error) error {
	var fe *FlowError
	if errors.As(err, &fe) {
		if fe.Registration == "" {
			fe.Registration = RegistrationFailed
		}
		if fe.Issuer == "" && md != nil {
			fe.Issuer = md.Issuer
		}
		return fe
	}
	e := newFlowError(ErrorTypeRegistration, err)
	e.Registration = RegistrationFailed
	if md != nil {
		e.Issuer = md.Issuer
	}
	var se *statusError
	if errors.As(err, &se) {
		e.Err = fmt.Errorf("%w: %s answered HTTP %d", ErrRegistration, se.URL, se.Status)
		if se.Status == 403 || se.Status == 401 {
			e.Suggestion = "the provider refused dynamic client registration (HTTP " +
				fmt.Sprint(se.Status) +
				"); register a client in its developer console and configure client_id explicitly"
		}
	}
	return e
}

// --- implementation 2: Client ID Metadata Documents (seam only) -----------

// clientIDMetadataRegistrar is the successor mechanism to DCR: instead of
// registering, the client publishes a metadata document at an https URL and
// uses that URL as its client_id.
//
// It is deliberately unimplemented in M1. The seam exists now (canonical.md
// §5b: "in place by M1") so that when DCR is removed upstream the change is a
// constructor swap. Implementing it requires a hosting story for the
// document, which agenthub does not have yet.
type clientIDMetadataRegistrar struct {
	// DocumentURL is the https URL that would serve the client metadata
	// document and simultaneously be the client_id.
	DocumentURL string
}

// NewClientIDMetadataRegistrar returns the Client ID Metadata Document
// implementation of ClientRegistrar.
//
// TODO(M2, oauth): implement once agenthub can publish a client metadata
// document at a stable https URL. Until then Register returns
// ErrNotImplemented rather than silently falling back to DCR — a silent
// fallback would hide the very migration this seam exists to make visible.
func NewClientIDMetadataRegistrar(documentURL string) ClientRegistrar {
	return &clientIDMetadataRegistrar{DocumentURL: documentURL}
}

// Kind implements ClientRegistrar.
func (r *clientIDMetadataRegistrar) Kind() string { return "client_id_metadata_document" }

// Register implements ClientRegistrar.
func (r *clientIDMetadataRegistrar) Register(context.Context, *AuthServerMetadata, RegistrationRequest) (*ClientCredentials, error) {
	e := newFlowError(ErrorTypeRegistration,
		fmt.Errorf("%w: client ID metadata documents are not implemented yet", ErrNotImplemented))
	e.Registration = RegistrationNotAttempted
	e.Suggestion = "use dynamic client registration or a preconfigured client_id for now"
	return nil, e
}

// --- implementation 3: preconfigured client -------------------------------

// staticRegistrar hands back a client_id the operator configured by hand.
// It is not a fallback for a failed DCR — it is selected explicitly, so
// that "we registered dynamically" and "the operator supplied this" are
// distinguishable in the persisted state and in every error message.
type staticRegistrar struct{ creds ClientCredentials }

// NewStaticRegistrar returns a ClientRegistrar over preconfigured
// credentials.
func NewStaticRegistrar(creds ClientCredentials) ClientRegistrar {
	return &staticRegistrar{creds: creds}
}

// Kind implements ClientRegistrar.
func (r *staticRegistrar) Kind() string { return "preconfigured" }

// Register implements ClientRegistrar.
func (r *staticRegistrar) Register(context.Context, *AuthServerMetadata, RegistrationRequest) (*ClientCredentials, error) {
	if strings.TrimSpace(r.creds.ClientID) == "" {
		e := newFlowError(ErrorTypeRegistration,
			fmt.Errorf("%w: preconfigured client_id is empty", ErrRegistration))
		e.Registration = RegistrationFailed
		return nil, e
	}
	c := r.creds
	return &c, nil
}

func defaultStrings(v, def []string) []string {
	if len(v) == 0 {
		return def
	}
	return v
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
