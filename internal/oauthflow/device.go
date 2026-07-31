package oauthflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// RFC 8628 timing constants.
const (
	// DefaultDeviceInterval is the poll interval when the device
	// authorization response omits `interval` (RFC 8628 §3.2 fixes the
	// default at 5 seconds).
	DefaultDeviceInterval = 5 * time.Second
	// SlowDownIncrement is added to the poll interval on every `slow_down`
	// (RFC 8628 §3.5 mandates "at least 5 seconds"). The increase is
	// PERMANENT for the rest of the poll loop, not a one-shot delay — an
	// AS sends slow_down because our rate is too high, and reverting would
	// walk straight back into it.
	SlowDownIncrement = 5 * time.Second
	// DefaultDeviceExpiry bounds the loop when the AS omits `expires_in`.
	DefaultDeviceExpiry = 15 * time.Minute
	// maxDeviceInterval caps interval growth so a misbehaving AS that
	// answers slow_down forever cannot push the next poll past the
	// device code's own lifetime.
	maxDeviceInterval = 60 * time.Second
)

// DeviceAuthorization is the RFC 8628 §3.2 device authorization response.
type DeviceAuthorization struct {
	// DeviceCode is the secret polled with. Never displayed.
	DeviceCode string
	// UserCode is what the human types. Displayed.
	UserCode string
	// VerificationURI is where the human types it.
	VerificationURI string
	// VerificationURIComplete embeds the user code; when present it is
	// what a QR code should encode.
	VerificationURIComplete string
	// ExpiresIn is the device code lifetime in seconds.
	ExpiresIn int64
	// Interval is the minimum poll interval in seconds (0 = use the
	// RFC default).
	Interval int64
}

// Expiry converts ExpiresIn into an absolute deadline.
func (d *DeviceAuthorization) Expiry(now time.Time) time.Time {
	if d.ExpiresIn <= 0 {
		return now.Add(DefaultDeviceExpiry)
	}
	return now.Add(time.Duration(d.ExpiresIn) * time.Second)
}

// PollInterval is the starting poll interval.
func (d *DeviceAuthorization) PollInterval() time.Duration {
	if d.Interval <= 0 {
		return DefaultDeviceInterval
	}
	return time.Duration(d.Interval) * time.Second
}

type rawDeviceAuthorization struct {
	DeviceCode              string        `json:"device_code"`
	UserCode                string        `json:"user_code"`
	VerificationURI         string        `json:"verification_uri"`
	VerificationURIAlt      string        `json:"verification_url"` // Google's pre-RFC spelling
	VerificationURIComplete string        `json:"verification_uri_complete"`
	ExpiresIn               lenientNumber `json:"expires_in"`
	Interval                lenientNumber `json:"interval"`
}

// DeviceRequest starts a device authorization.
type DeviceRequest struct {
	Metadata *AuthServerMetadata
	ClientID string
	Scopes   []string
	Resource string
}

// StartDevice performs the RFC 8628 §3.1 device authorization request.
//
// Failure direction: an AS with no device_authorization_endpoint is an
// error here rather than a silent fallback to another mode — mode selection
// happens once, in `auth login`, using AuthServerMetadata.SupportsDeviceFlow.
func (c *Client) StartDevice(ctx context.Context, req DeviceRequest) (*DeviceAuthorization, error) {
	if req.Metadata == nil || strings.TrimSpace(req.Metadata.DeviceAuthorizationEndpoint) == "" {
		e := newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("oauthflow: authorization server advertises no device_authorization_endpoint"))
		e.Suggestion = "this provider does not support the device flow; use the loopback or --manual mode"
		return nil, e
	}
	form := url.Values{}
	form.Set("client_id", req.ClientID)
	if s := strings.TrimSpace(strings.Join(req.Scopes, " ")); s != "" {
		form.Set("scope", s)
	}
	if req.Resource != "" {
		form.Set("resource", req.Resource)
	}
	body, err := c.postForm(ctx, req.Metadata.DeviceAuthorizationEndpoint, form, nil)
	if err != nil {
		return nil, wrapTokenError(ErrorTypeAuthorization, err)
	}
	var raw rawDeviceAuthorization
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("oauthflow: device authorization response is not JSON: %w", err))
	}
	if strings.TrimSpace(raw.DeviceCode) == "" || strings.TrimSpace(raw.UserCode) == "" {
		return nil, newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("oauthflow: device authorization response is missing device_code or user_code"))
	}
	uri := raw.VerificationURI
	if uri == "" {
		uri = raw.VerificationURIAlt
	}
	// Both of these are opened in the user's browser and both came out of
	// the AS's own response, so they are screened for the reason
	// AuthorizeURL gives: the browser carries the user's ambient cookies to
	// whatever this names. Fail-closed — a device flow that can only offer
	// a destination we refuse to open is a device flow that does not start.
	for _, u := range []string{uri, raw.VerificationURIComplete} {
		if strings.TrimSpace(u) == "" {
			continue
		}
		if err := c.screenBrowserURL(u); err != nil {
			return nil, err
		}
	}
	return &DeviceAuthorization{
		DeviceCode:              raw.DeviceCode,
		UserCode:                raw.UserCode,
		VerificationURI:         uri,
		VerificationURIComplete: raw.VerificationURIComplete,
		ExpiresIn:               int64(raw.ExpiresIn),
		Interval:                int64(raw.Interval),
	}, nil
}

// DevicePoller polls the token endpoint for a device authorization.
type DevicePoller struct {
	Client *Client
	// Now overrides time.Now (tests).
	Now func() time.Time
	// Sleep overrides the inter-poll wait (tests). It must honour ctx.
	Sleep func(ctx context.Context, d time.Duration) error
	// OnPending is called before each wait with the interval about to be
	// slept, so the CLI can emit an NDJSON progress event. Optional.
	OnPending func(interval time.Duration)
}

// PollDevice runs the RFC 8628 §3.4, docs/modules/oauth.md poll loop.
//
// Loop rules:
//
//   - authorization_pending → keep polling at the current interval.
//   - slow_down → permanently raise the interval by SlowDownIncrement,
//     then keep polling. (Capped at maxDeviceInterval.)
//   - access_denied / expired_token → terminal error.
//   - any other error → terminal. A transport failure mid-poll is NOT
//     retried here: the caller decides whether a whole login is worth
//     restarting, and a poll loop that swallows transport errors turns a
//     broken network into a silent 15-minute hang.
//   - the device code's own expiry bounds the loop regardless of interval,
//     so a hostile `interval` cannot extend it.
func (p *DevicePoller) PollDevice(ctx context.Context, md *AuthServerMetadata, clientID string, da *DeviceAuthorization, resource string) (*TokenResponse, error) {
	if da == nil || da.DeviceCode == "" {
		return nil, newFlowError(ErrorTypeAuthorization, fmt.Errorf("oauthflow: no device authorization to poll"))
	}
	now := p.now
	deadline := da.Expiry(now())
	interval := da.PollInterval()

	form := url.Values{}
	form.Set("grant_type", GrantDeviceCode)
	form.Set("device_code", da.DeviceCode)
	form.Set("client_id", clientID)
	if resource != "" {
		form.Set("resource", resource)
	}

	for {
		if now().After(deadline) {
			e := newFlowError(ErrorTypeAuthorization,
				fmt.Errorf("%w: device code expired before authorization completed", ErrTimeout))
			e.Suggestion = "re-run the login and enter the user code promptly"
			return nil, e
		}
		body, err := p.Client.postForm(ctx, md.TokenEndpoint, form, nil)
		if err == nil {
			return parseTokenResponse(body)
		}
		var te *TokenError
		if !errors.As(err, &te) {
			// Blocked, redirected, transport: terminal, typed.
			return nil, wrapTokenError(ErrorTypeAuthorization, err)
		}
		switch {
		case te.IsSlowDown():
			interval += SlowDownIncrement
			if interval > maxDeviceInterval {
				interval = maxDeviceInterval
			}
		case te.IsAuthorizationPending():
			// keep the current interval
		default:
			return nil, wrapTokenError(ErrorTypeAuthorization, err)
		}
		if p.OnPending != nil {
			p.OnPending(interval)
		}
		if err := p.sleep(ctx, interval); err != nil {
			return nil, newFlowError(ErrorTypeAuthorization, err)
		}
	}
}

func (p *DevicePoller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *DevicePoller) sleep(ctx context.Context, d time.Duration) error {
	if p.Sleep != nil {
		return p.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
