// Package oauthflow implements agenthub's headless OAuth 2.1 client:
// the discovery chain, dynamic client registration, PKCE, the three
// interactive modes (loopback / manual / device), token exchange,
// persistence into the secrets vault and refresh coordination
// (docs/modules/oauth.md and 7.7).
//
// # Shape of a login
//
//	  discovery ──► registration ──► authorization ──► token exchange ──► persist
//	(RFC 8414/9728)   (RFC 7591)     (loopback|manual|device)  (PKCE)   (vault)
//
// Every step of that pipeline is an independently usable value so the CLI
// can stream NDJSON progress events between them (docs/modules/controlplane.md) and the
// daemon can re-enter at "token exchange" for a refresh.
//
// # Invariants this package exists to hold
//
//  1. No credential ever leaves the process to a private address. Every
//     outbound request is screened twice: the URL's host before the
//     request (internal/guard/netguard.HostIsPrivate, fail-closed) and the
//     actual resolved IP at dial time (netguard.DialControl, closing the
//     DNS-rebind TOCTOU window). Loopback is refused unless the caller
//     sets Config.AllowLoopback — an explicit, per-call opt-in for
//     self-hosted authorization servers and tests, and even then ONLY
//     literal loopback addresses and the RFC 6761 localhost name tree are
//     allowed, never RFC1918 or anything DNS claims is local.
//
//  2. PKCE never degrades. A crypto/rand failure is an error, never a
//     fallback to the "plain" challenge method and never a weaker source.
//     See newRandomToken and NewPKCE.
//
//  3. Credential POSTs follow zero redirects. The token/registration
//     client's CheckRedirect returns http.ErrUseLastResponse and any 3xx
//     is then treated as an error — a redirect on a request carrying a
//     code_verifier, a refresh token or a client secret is an exfiltration
//     primitive, not a routing detail.
//
//  4. Persistence writes state before the access token. Save writes
//     __oauth_state__ (which carries the ROTATED refresh token) first and
//     __http_auth__ (the access token) second. The failure direction is
//     deliberate: a crash between the two leaves a fresh refresh token
//     with a stale access token, which self-heals on the next refresh.
//     The reverse order leaves a fresh access token next to an already
//     invalidated refresh token — unrecoverable without a full re-login.
//
//  5. Refreshes are single-writer. Online (daemon present) all refreshes
//     funnel through the in-process singleflight Group; offline the caller
//     takes the <server>.refresh.lock sibling file lock, re-reads the
//     state after acquiring it, and abandons its own refresh if another
//     writer already advanced expires_at. A one-time refresh token spent
//     twice concurrently locks the user out.
//
// # Expiry semantics (docs/modules/oauth.md)
//
//   - ExpiresAt == 0 means "never expires", not "already expired": several
//     providers (Atlassian et al.) issue tokens without expires_in.
//   - The 60s refresh grace is only subtracted from tokens whose lifetime
//     exceeds 5 minutes; subtracting it from a 60s token would make every
//     token born already expired.
//   - A record holding only DCR credentials and no access token reports
//     ErrNoToken rather than an empty token, so reconnect loops terminate.
//
// # Dependency budget
//
// Standard library plus internal/secrets and internal/guard/netguard.
// Nothing here imports the control plane, the pipeline or a logging
// package: this is a leaf that returns structured *FlowError values and
// lets its callers decide how to render them.
package oauthflow
