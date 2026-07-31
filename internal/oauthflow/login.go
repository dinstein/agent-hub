package oauthflow

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Mode is the interactive authorization mode (docs/modules/oauth.md).
type Mode string

const (
	// ModeAuto lets SelectMode choose.
	ModeAuto Mode = ""
	// ModeLoopback opens a browser on this host and catches the redirect
	// on a freshly bound loopback port.
	ModeLoopback Mode = "loopback"
	// ModeManual prints the URL and reads back the pasted callback.
	ModeManual Mode = "manual"
	// ModeDevice runs RFC 8628.
	ModeDevice Mode = "device"
)

// SelectMode implements the mode-selection rule of docs/modules/oauth.md:
//
//	explicit flag                             → that mode
//	otherwise, AS advertises device flow      → device
//	otherwise, this host can open a browser   → loopback
//	otherwise (no DISPLAY, SSH, --no-browser) → manual
//
// Device outranks loopback when available because it is the only mode that
// works identically on a laptop, over SSH and inside a container, and it
// never leaves an authorization code in a URL bar.
func SelectMode(explicit Mode, md *AuthServerMetadata, canOpenBrowser bool) Mode {
	if explicit != ModeAuto {
		return explicit
	}
	if md.SupportsDeviceFlow() {
		return ModeDevice
	}
	if canOpenBrowser {
		return ModeLoopback
	}
	return ModeManual
}

// PasteReader asks the user for the pasted callback URL or code (manual
// mode). Returning an error aborts the login.
type PasteReader func(ctx context.Context, instr ManualInstructions) (string, error)

// LoginRequest is one `agenthub auth login` invocation.
type LoginRequest struct {
	// ServerID keys the vault entries. Required.
	ServerID string
	// Issuer is the authorization server. Either Issuer or ResourceURL
	// must be set. A non-empty Issuer PINS the authorization server and
	// skips RFC 9728 discovery entirely; ResourceURL takes the RFC 9728
	// route and is only consulted when Issuer is empty. The pin wins
	// because it is an explicit operator override (`--issuer`, or
	// ServerEntry.OAuth.Issuer): it exists precisely for resource servers
	// that publish no usable protected-resource metadata, so letting
	// their URL send us back down the route that is already known to fail
	// would make the field unreachable for every http/sse server.
	Issuer string
	// ResourceURL is the MCP server URL being authorized against.
	ResourceURL string
	// ResourceMetadataURL is the pointer harvested from a 401's
	// WWW-Authenticate header, if any.
	ResourceMetadataURL string
	// Scopes is the requested scope set, sent verbatim. This package never
	// appends offline_access (see ScopeOfflineAccess).
	Scopes []string
	// AuthorizationEndpoint REPLACES the discovered authorization_endpoint.
	//
	// This is a deliberate deviation from RFC 8414, where that endpoint has
	// exactly one legal source: the metadata document. It exists for
	// providers that serve real authorization endpoints they never
	// advertise — e.g. a per-provider /oauth/authorize/<name> whose RFC 8414
	// candidates all 404 — so the endpoint is unreachable by any conforming
	// client. Empty (the default) keeps the standard behavior.
	//
	// The value decides where a user's authorization code is sent, so it is
	// SSRF-screened like any discovered URL and recorded as
	// DiscoveryPinnedAuthz. Prefer fixing the provider's metadata: that
	// fixes every standard client at once, this fixes only agenthub.
	AuthorizationEndpoint string
	// ClientName is shown on the consent screen.
	ClientName string
	// Mode selects the interactive mode; ModeAuto uses SelectMode.
	Mode Mode
	// Registrar obtains client credentials. nil uses DCR.
	Registrar ClientRegistrar
	// Open opens a browser (loopback mode). nil means this host cannot.
	Open BrowserOpener
	// Paste reads the pasted callback (manual mode).
	Paste PasteReader
	// OnDeviceCode is called once with the user code and verification URI
	// (device mode). Required for device mode — there is no point polling
	// for a code the user was never shown.
	OnDeviceCode func(DeviceAuthorization)
	// OnPollInterval reports each device-flow wait, for NDJSON progress.
	OnPollInterval func(time.Duration)
	// Timeout overrides LoopbackTimeout for the loopback wait.
	Timeout time.Duration
	// FixedCallbackPort re-binds a previously registered port.
	FixedCallbackPort int
	// RedirectURI pins the loopback callback URI verbatim (`--redirect-uri`).
	// Required by providers that authorize against a PRE-REGISTERED OAuth
	// client whose allowlist agenthub cannot influence: there the URI must
	// match byte for byte, and a random port never will. Empty means the
	// normal behaviour (fresh random port, 127.0.0.1, /callback).
	//
	// Only the host, port and path are taken from it; it must be a loopback
	// http URI (ParseLoopbackRedirectURI enforces that).
	RedirectURI string
	// ExtraAuthParams are provider-specific authorization parameters.
	ExtraAuthParams map[string][]string
}

// LoginResult is a completed authorization.
type LoginResult struct {
	// State is what was persisted.
	State *State
	// AccessToken is the freshly minted token (also persisted).
	AccessToken string
	// Mode is the mode actually used, which may differ from the requested
	// one when auto-selection or the browser-open fallback kicked in.
	Mode Mode
	// Discovery is the discovery outcome, for diagnostics.
	Discovery *DiscoveryResult
}

// Flow runs a complete login.
type Flow struct {
	Client     *Client
	Discoverer *Discoverer
	Store      *Store
	// Now overrides time.Now (tests).
	Now func() time.Time
	// Sleep overrides the device-flow wait (tests).
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewFlow assembles a Flow over one client and one vault.
func NewFlow(client *Client, store *Store) *Flow {
	return &Flow{Client: client, Discoverer: NewDiscoverer(client), Store: store}
}

func (f *Flow) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Login runs discovery → registration → authorization → token exchange →
// persistence.
//
// The step order is not negotiable and each step's output feeds the next:
// the callback port must exist before registration (the redirect URI
// carries it), registration must finish before the authorization URL (it
// carries the client_id), and persistence happens exactly once, at the end,
// under Store.Save's state-before-token ordering.
func (f *Flow) Login(ctx context.Context, req LoginRequest) (*LoginResult, error) {
	if strings.TrimSpace(req.ServerID) == "" {
		return nil, newFlowError(ErrorTypeUnknown, fmt.Errorf("oauthflow: login needs a server id"))
	}
	disc, err := f.discover(ctx, req)
	if err != nil {
		return nil, withServer(err, req.ServerID)
	}
	md := disc.Metadata
	if !SupportsS256(md) {
		e := newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("oauthflow: %s advertises no S256 code challenge support", md.Issuer))
		e.Suggestion = "agenthub requires PKCE S256 and will not fall back to the plain method"
		return nil, withServer(e, req.ServerID)
	}

	// Resolve the scope set ONCE, here, so all four login paths request the
	// same thing: req is passed by value to each of them.
	//
	// An operator-supplied set wins outright and is never merged with what
	// discovery found. `--scopes` is frequently used to NARROW a provider's
	// default (a read-only token against a server whose metadata also
	// advertises write), and quietly unioning the discovered set back in
	// would widen exactly the grant the operator sat down to restrict.
	if len(req.Scopes) == 0 {
		req.Scopes = disc.ScopeSet()
	}

	// Reuse a previous registration when there is one: dynamic
	// registration is per-installation, and re-registering on every login
	// both litters the provider's client list and trips DCR rate limits.
	prev, prevErr := f.Store.LoadState(ctx, req.ServerID)
	if prevErr != nil && !errors.Is(prevErr, ErrNoState) {
		return nil, withServer(prevErr, req.ServerID)
	}

	mode := SelectMode(req.Mode, md, req.Open != nil)
	switch mode {
	case ModeDevice:
		return f.loginDevice(ctx, req, disc, prev)
	case ModeManual:
		return f.loginManual(ctx, req, disc, prev)
	case ModeLoopback:
		res, err := f.loginLoopback(ctx, req, disc, prev)
		// Browser-open failure is the documented downgrade path: a host
		// with no DISPLAY only discovers that at open time.
		if err != nil && req.Mode == ModeAuto && req.Paste != nil && isBrowserOpenFailure(err) {
			return f.loginManual(ctx, req, disc, prev)
		}
		return res, err
	default:
		return nil, newFlowError(ErrorTypeUnknown, fmt.Errorf("oauthflow: unknown mode %q", mode))
	}
}

func (f *Flow) discover(ctx context.Context, req LoginRequest) (*DiscoveryResult, error) {
	res, err := f.discoverMetadata(ctx, req)
	if err != nil {
		return nil, err
	}
	return f.pinAuthorizationEndpoint(res, req.AuthorizationEndpoint)
}

// pinAuthorizationEndpoint applies LoginRequest.AuthorizationEndpoint over
// discovered metadata. Returns res unchanged when no override is set.
//
// Failure direction: FAIL-CLOSED. An unparsable or SSRF-screened URL aborts
// the login rather than silently falling back to the discovered endpoint —
// an operator who pinned an endpoint asked for THAT endpoint, and quietly
// authorizing against a different one would be the worse surprise.
func (f *Flow) pinAuthorizationEndpoint(res *DiscoveryResult, endpoint string) (*DiscoveryResult, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return res, nil
	}
	u, err := parseAbsoluteURL(endpoint)
	if err != nil {
		e := newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("oauthflow: bad pinned authorization endpoint %q: %w", endpoint, err))
		e.Discovery = DiscoveryPinnedAuthz
		return nil, e
	}
	// Same screen every fetched URL passes: this destination receives the
	// user's authorization code, so it is exactly as sensitive.
	if err := f.Client.checkURL(u); err != nil {
		return nil, err
	}
	if res == nil || res.Metadata == nil {
		e := newFlowError(ErrorTypeDiscovery,
			fmt.Errorf("%w: no metadata to pin an authorization endpoint onto", ErrDiscovery))
		e.Discovery = DiscoveryPinnedAuthz
		return nil, e
	}
	// Copy: the metadata may be shared with a cached discovery result, and
	// the pin is scoped to THIS login.
	md := *res.Metadata
	md.AuthorizationEndpoint = u.String()
	out := *res
	out.Metadata = &md
	out.Status = DiscoveryPinnedAuthz
	return &out, nil
}

func (f *Flow) discoverMetadata(ctx context.Context, req LoginRequest) (*DiscoveryResult, error) {
	switch {
	// An explicitly pinned issuer wins over the RFC 9728 route: it is the
	// documented escape hatch for resource servers that publish no
	// protected-resource metadata, and those servers always have a URL —
	// so consulting ResourceURL first would make the pin unreachable.
	case strings.TrimSpace(req.Issuer) != "":
		return f.Discoverer.DiscoverFromIssuer(ctx, req.Issuer)
	case strings.TrimSpace(req.ResourceURL) != "":
		// One unauthenticated request yields both 401 hints: the RFC 9728
		// pointer (guessing from a candidate list cannot reach a server that
		// publishes its document anywhere unconventional) and the RFC 6750
		// scope challenge. Fail-soft — empty answers leave the candidates and
		// the scope set exactly as they would have been.
		hint, challengeScope := f.Discoverer.ProbeChallenge(ctx, req.ResourceURL)
		if s := strings.TrimSpace(req.ResourceMetadataURL); s != "" {
			hint = s
		}
		res, err := f.Discoverer.DiscoverFromResource(ctx, req.ResourceURL, hint)
		if res != nil {
			res.ChallengeScope = challengeScope
		}
		return res, err
	default:
		return nil, newFlowError(ErrorTypeDiscovery,
			fmt.Errorf("%w: login needs an issuer or a resource url", ErrDiscovery))
	}
}

// register returns client credentials, reusing prev's when present.
func (f *Flow) register(ctx context.Context, req LoginRequest, md *AuthServerMetadata, prev *State, redirectURIs []string, grants []string) (*ClientCredentials, string, error) {
	if prev != nil && strings.TrimSpace(prev.ClientID) != "" {
		return &ClientCredentials{ClientID: prev.ClientID, ClientSecret: prev.ClientSecret},
			orDefault(prev.RegistrarKind, "reused"), nil
	}
	reg := req.Registrar
	if reg == nil {
		// DEPRECATED-UPSTREAM(dcr, earliest-removal: 2027-07-28)
		reg = NewDCRRegistrar(f.Client)
	}
	creds, err := reg.Register(ctx, md, RegistrationRequest{
		ClientName:   orDefault(req.ClientName, "agenthub"),
		RedirectURIs: redirectURIs,
		GrantTypes:   grants,
		Scopes:       req.Scopes,
	})
	if err != nil {
		return nil, reg.Kind(), err
	}
	return creds, reg.Kind(), nil
}

func (f *Flow) loginLoopback(ctx context.Context, req LoginRequest, disc *DiscoveryResult, prev *State) (*LoginResult, error) {
	state, err := NewState()
	if err != nil {
		return nil, err
	}
	pkce, err := NewPKCE()
	if err != nil {
		return nil, err
	}
	var creds *ClientCredentials
	var kind string
	lf := &LoopbackFlow{Open: req.Open, Timeout: req.Timeout, FixedPort: req.FixedCallbackPort}
	// An explicitly pinned redirect URI overrides the remembered port: the
	// operator is matching a provider allowlist by hand, and a port carried
	// over from an earlier registration would silently defeat that.
	if strings.TrimSpace(req.RedirectURI) != "" {
		host, port, path, perr := ParseLoopbackRedirectURI(req.RedirectURI)
		if perr != nil {
			return nil, newFlowError(ErrorTypeAuthorization, perr)
		}
		lf.FixedHost, lf.FixedPort, lf.FixedPath = host, port, path
	}
	// build runs AFTER the listener is bound and BEFORE the browser opens,
	// which is exactly where registration must happen: only now is the
	// redirect URI (with its real port) known.
	res, err := lf.Run(ctx, state, func(redirectURI string) (string, error) {
		var rerr error
		creds, kind, rerr = f.register(ctx, req, disc.Metadata, prev, []string{redirectURI},
			[]string{GrantAuthorizationCode, GrantRefreshToken})
		if rerr != nil {
			return "", rerr
		}
		return f.Client.AuthorizeURL(AuthorizeRequest{
			Metadata:    disc.Metadata,
			ClientID:    creds.ClientID,
			RedirectURI: redirectURI,
			Scopes:      req.Scopes,
			Resource:    disc.Resource,
			State:       state,
			PKCE:        pkce,
			Extra:       req.ExtraAuthParams,
		})
	})
	if err != nil {
		return nil, withServer(err, req.ServerID)
	}
	// RFC 9207: the callback arrived intact from the browser, so an AS that
	// advertises the iss parameter must have delivered it (fail closed).
	if err := validateIss(disc.Metadata, res.Iss, true); err != nil {
		return nil, withServer(err, req.ServerID)
	}
	tok, err := f.Client.Exchange(ctx, ExchangeRequest{
		TokenEndpoint: disc.Metadata.TokenEndpoint,
		ClientID:      creds.ClientID,
		ClientSecret:  creds.ClientSecret,
		Code:          res.Code,
		RedirectURI:   res.RedirectURI,
		CodeVerifier:  pkce.Verifier,
		Resource:      disc.Resource,
	})
	if err != nil {
		return nil, withServer(err, req.ServerID)
	}
	return f.persist(ctx, req, disc, creds, kind, prev, tok, ModeLoopback, res.RedirectURI, res.Port)
}

func (f *Flow) loginManual(ctx context.Context, req LoginRequest, disc *DiscoveryResult, prev *State) (*LoginResult, error) {
	if req.Paste == nil {
		return nil, newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("oauthflow: manual mode needs a paste reader"))
	}
	state, err := NewState()
	if err != nil {
		return nil, err
	}
	pkce, err := NewPKCE()
	if err != nil {
		return nil, err
	}
	redirect := ManualRedirectURI
	// A pinned redirect URI applies to manual mode too: the URI still has to
	// be the one the provider allows, and manual mode is the usual way to
	// drive exactly those providers (nothing is bound here, so any loopback
	// spelling is equally serviceable).
	if strings.TrimSpace(req.RedirectURI) != "" {
		host, port, path, perr := ParseLoopbackRedirectURI(req.RedirectURI)
		if perr != nil {
			return nil, newFlowError(ErrorTypeAuthorization, perr)
		}
		redirect = "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + path
	}
	if prev != nil && prev.RedirectURI != "" {
		redirect = prev.RedirectURI
	}
	creds, kind, err := f.register(ctx, req, disc.Metadata, prev, []string{redirect},
		[]string{GrantAuthorizationCode, GrantRefreshToken})
	if err != nil {
		return nil, withServer(err, req.ServerID)
	}
	authURL, err := f.Client.AuthorizeURL(AuthorizeRequest{
		Metadata:    disc.Metadata,
		ClientID:    creds.ClientID,
		RedirectURI: redirect,
		Scopes:      req.Scopes,
		Resource:    disc.Resource,
		State:       state,
		PKCE:        pkce,
		Extra:       req.ExtraAuthParams,
	})
	if err != nil {
		return nil, withServer(err, req.ServerID)
	}
	pasted, err := req.Paste(ctx, NewManualInstructions(authURL, redirect, state))
	if err != nil {
		return nil, withServer(newFlowError(ErrorTypeAuthorization, err), req.ServerID)
	}
	code, iss, err := ParseManualCallback(pasted, state)
	if err != nil {
		return nil, withServer(err, req.ServerID)
	}
	// RFC 9207, manual leniency: a paste may have been hand-trimmed to a
	// bare code, so only a PRESENT iss is checked (see validateIss).
	if err := validateIss(disc.Metadata, iss, false); err != nil {
		return nil, withServer(err, req.ServerID)
	}
	tok, err := f.Client.Exchange(ctx, ExchangeRequest{
		TokenEndpoint: disc.Metadata.TokenEndpoint,
		ClientID:      creds.ClientID,
		ClientSecret:  creds.ClientSecret,
		Code:          code,
		RedirectURI:   redirect,
		CodeVerifier:  pkce.Verifier,
		Resource:      disc.Resource,
	})
	if err != nil {
		return nil, withServer(err, req.ServerID)
	}
	return f.persist(ctx, req, disc, creds, kind, prev, tok, ModeManual, redirect, CallbackPortOf(redirect))
}

func (f *Flow) loginDevice(ctx context.Context, req LoginRequest, disc *DiscoveryResult, prev *State) (*LoginResult, error) {
	if req.OnDeviceCode == nil {
		return nil, newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("oauthflow: device mode needs a user-code display callback"))
	}
	// No redirect URI: the device flow has no redirect leg at all.
	creds, kind, err := f.register(ctx, req, disc.Metadata, prev, nil,
		[]string{GrantDeviceCode, GrantRefreshToken})
	if err != nil {
		return nil, withServer(err, req.ServerID)
	}
	da, err := f.Client.StartDevice(ctx, DeviceRequest{
		Metadata: disc.Metadata,
		ClientID: creds.ClientID,
		Scopes:   req.Scopes,
		Resource: disc.Resource,
	})
	if err != nil {
		return nil, withServer(err, req.ServerID)
	}
	req.OnDeviceCode(*da)
	poller := &DevicePoller{Client: f.Client, Now: f.Now, Sleep: f.Sleep, OnPending: req.OnPollInterval}
	tok, err := poller.PollDevice(ctx, disc.Metadata, creds.ClientID, da, disc.Resource)
	if err != nil {
		return nil, withServer(err, req.ServerID)
	}
	return f.persist(ctx, req, disc, creds, kind, prev, tok, ModeDevice, "", 0)
}

// persist writes the two vault entries and returns the result.
func (f *Flow) persist(ctx context.Context, req LoginRequest, disc *DiscoveryResult, creds *ClientCredentials, kind string, prev *State, tok *TokenResponse, mode Mode, redirectURI string, port int) (*LoginResult, error) {
	next := State{
		TokenEndpoint: disc.Metadata.TokenEndpoint,
		Issuer:        disc.Metadata.Issuer,
		ClientID:      creds.ClientID,
		ClientSecret:  creds.ClientSecret,
		RegistrarKind: kind,
		Resource:      disc.Resource,
		RedirectURI:   redirectURI,
		CallbackPort:  port,
	}
	saved, err := f.Store.SaveFromToken(ctx, req.ServerID, prev, next, tok, f.now())
	if err != nil {
		return nil, withServer(err, req.ServerID)
	}
	return &LoginResult{State: saved, AccessToken: tok.AccessToken, Mode: mode, Discovery: disc}, nil
}

// withServer stamps the server ID onto a FlowError that does not carry one.
func withServer(err error, serverID string) error {
	var fe *FlowError
	if errors.As(err, &fe) && fe.ServerID == "" {
		fe.ServerID = serverID
	}
	return err
}

// isBrowserOpenFailure reports the specific loopback failure that justifies
// downgrading to manual mode: the browser could not be launched. Every
// other loopback failure (timeout, state mismatch, denied) means the user
// DID see a browser, so retrying in manual mode would just ask them to do
// it again.
func isBrowserOpenFailure(err error) bool {
	var fe *FlowError
	if !errors.As(err, &fe) {
		return false
	}
	return fe.Type == ErrorTypeAuthorization && strings.Contains(fe.Error(), "open browser")
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
