# The OAuth client

> **Answers** how a login is performed and a token refreshed, and where each step refuses rather than degrades.
> **Not here** which provider shapes work and what is missing → [../status/oauth.md](../status/oauth.md); the flow as a sequence → [../flows.md#oauth-login-and-refresh](../flows.md#oauth-login-and-refresh).
> **Kept true by** `internal/oauthflow`'s SSRF tests (`fakeAS`) and the refresh-coordination tests.

`internal/oauthflow` is a headless OAuth 2.1 client: the discovery chain, dynamic registration, PKCE,
three interaction modes, token exchange, writing into the vault, and refresh coordination. It is the
only component that deliberately sends credentials to the public internet, and every constraint below is
a security constraint rather than a protocol one.

A login is a pipeline, and each stage is a value usable on its own, so the CLI can emit progress events
between stages and the daemon can re-enter at token exchange to refresh:

```
discovery ──► registration ──► authorization ──► token exchange ──► persist
(RFC 8414/9728)  (RFC 7591)  (loopback|manual|device)   (PKCE)      (vault)
```

Two structural choices to know before reading any of it: `Client` holds **two** `http.Client`s with
different redirect policies, and `ClientRegistrar` is a three-implementation seam — DCR, Client ID
Metadata Documents, an operator-provisioned client id — so replacing the deprecated mechanism is a
constructor swap rather than a rewrite of the flow.

## Credentials never reach a private address

**Every outbound request is screened twice**: the URL's host with `netguard.HostIsPrivate` before the
request (fail-closed, so unresolvable counts as private), and the actually-resolved IP with
`netguard.DialControl` at dial time, which closes the rebinding window. The connection pool's
`IdleConnTimeout` drops to 30s to avoid reusing a connection that was still a public address when it was
screened.

`AllowLoopback` is an explicit per-call switch, and even when on it allows **only** literal loopback
addresses and RFC 6761's `localhost` name tree. `isLiteralLoopbackHost` is narrower than
`HostIsDefinitelyPrivate`: RFC1918 and link-local are not exempt, and no hostname's DNS answer can
unlock this exception.

**The carve-out is decided from the REQUESTED host, and needs the resolved address to agree.**
`dialControlFor` is built per dial from the `host:port` the transport was asked for — before any
resolution — and opens only when the requested host is a literal loopback **and** the address being
dialed is itself a loopback literal. Deciding it from the resolved address alone reopens the switch to
precisely the DNS answer that cannot unlock it: a public-looking name answering `127.0.0.1` would be
dialed without netguard being consulted, delivering a discovery GET — or a POST carrying a
`code_verifier` or a refresh token — to whatever listens on this host's loopback interface.

**A destination the USER'S BROWSER is sent to is screened by the same rules as one we would fetch
ourselves.** `Client.AuthorizeURL` and `StartDevice`'s two verification URIs all pass through
`screenBrowserURL`. The endpoints come out of a metadata document a remote authorization server wrote,
and the browser carries the user's ambient cookies to whatever they name: an AS advertising
`authorization_endpoint: https://10.0.0.5:8080/authorize` is otherwise an SSRF whose client is the
human's session rather than ours. Refusing an AS on a private address breaks nothing that worked — its
token endpoint is already screened, so the flow died one step later, after the browser had been sent
there. Two limits this cannot reach, and they are why the screen is not the whole defence: the browser
resolves the name itself, and an injected `Flow.Open` can ignore the answer.

**POSTs carrying credentials follow zero redirects.** The `credential` client returns
`http.ErrUseLastResponse` and any 3xx becomes `ErrRedirect`. A 302 on a request carrying a
`code_verifier`, a refresh token or a client secret is an exfiltration primitive, not a routing detail.
Logged `Location` values keep only scheme, host and path. By contrast the `discovery` client allows up
to three hops, **re-screening every hop**: metadata documents are public, and providers really do move
them.

## PKCE is never downgraded

`ChallengeMethodS256` is the only method ever sent and there is no `plain` code path. `randRead` is a
package-level variable rather than a config option — making the entropy source configurable would be
manufacturing exactly that downgrade path — and `newRandomToken` uses `io.ReadFull` so a short read
cannot silently shorten the verifier, returning `ErrEntropy` rather than falling back to `math/rand`.
`BuildAuthorizeURL` errors outright when PKCE is nil, the challenge is empty or the method is not S256,
and `Exchange` refuses to exchange without a verifier.

The one random value allowed to degrade is the correlation id: a diagnostic that cannot be generated
becomes a fixed placeholder rather than failing a login.

**Registration hardcodes `token_endpoint_auth_method: "none"`** and never negotiates it from metadata.
agenthub is a public client running on the user's machine, any "client secret" it holds is readable by
anyone who can read the vault, and what actually protects the code exchange is PKCE.

**Scopes are sent verbatim, and `offline_access` is never added unilaterally.** Adding it looks like a
convenience but is an escalation of consent scope: on some providers it turns a session-level grant into
a long-lived one, and on others it makes the whole authorization fail. An operator-supplied set wins
outright over the discovered one and is never merged with it — `--scopes` is usually used to *narrow* a
provider's default.

## Persistence

**Write order: state first, then the access token.** `Store.Save` never parallelizes the two writes and
never proceeds past a failed first write; `Clear` mirrors the order, deleting the token first. A corrupt
`__oauth_state__` is a loud error rather than "no state": silently treating it as absent would trigger a
fresh registration and orphan a working refresh token.

**Two inherited rules in `SaveFromToken`**: a response without a refresh token **does not clear** a
stored one, because non-rotating providers omit it on every refresh and clearing it would turn the
second refresh into a fresh login; and an omitted scope means "unchanged", per RFC 6749.

**Expiry semantics.** `ExpiresAt == 0` means **never expires**, not "already expired" — some providers
genuinely do not send `expires_in`. The 60s refresh grace is subtracted only from tokens with a lifetime
over five minutes, because subtracting it from a 60s token would make every token expire at birth and
trap the gateway in an infinite refresh loop. `lenientNumber` never fails, returning 0 for anything
unparseable, and negatives are zeroed: an unparseable field should not throw away a usable access token.

**`ErrNoToken` must be distinguished from `ErrNoState` and from an empty token.** A record with DCR
credentials but no access token returns `ErrNoToken` and never `(state, "", nil)` — a caller handed an
empty token would attach an empty Authorization header, get a 401, decide to refresh, and loop forever.

**`ShouldRefreshOnStatus` treats 403 the same as 401**: several providers answer an expired token with
403, and treating that as "insufficient permission, do not refresh" would leave those servers
permanently broken while still showing a Ready badge.

## Refresh has a single writer, in two tiers

Online, with the daemon present, all refreshes go through the in-process single-flight, because the
daemon is the vault's sole writer. Offline, we take a `<server>.refresh.lock` sibling file lock **and
re-read the state after acquiring it**: the lock only serializes, it does not tell the second acquirer
that the work is already done. If the re-read shows `expires_at` has moved past the value observed
before queueing, we abandon our own refresh and return `ErrRefreshSuperseded` along with the fresh
credentials — continuing would burn the one-time refresh token the other party just stored.

`CoordinatorConfig.Online` being nil defaults to **offline**: an extra lock acquisition costs one
syscall, while failing to take one that was needed costs the user's refresh token.

The file lock is real on darwin, linux and Windows through `internal/platform`; anywhere else it is an
`errors.ErrUnsupported` stub, so **the offline refresh path would rather refuse to run than run
unordered**. Two processes racing for one single-use refresh token is worse than one unsupported refresh
failure.

**OAuth uses its own slow backoff ladder** — 5min, 15min, 1h, 4h, 24h. An OAuth failure during
connection is waiting on a **human**, and ordinary exponential backoff retrying every few seconds would
keep popping browser windows or hammering the provider's authorization endpoint. `RetryBackoff` puts the
first few failures on a flat delay first, and lives beside the ladder because both proactive refreshers
use it — a schedule reimplemented on each side is a schedule that drifts.

## The three interaction modes

**Loopback's order cannot be rearranged**: bind → build (register) → **Serve** → open browser → wait,
with a fresh random port every attempt. Only providers requiring an exactly pre-registered redirect URI
reuse a port through `State.CallbackPort`, and when that port is taken the caller should discard the DCR
credentials and re-register rather than silently switching ports. `Wait` always shuts down the server
and releases the port before returning — a listener outliving its flow is the stale-interceptor bug
random ports exist to prevent.

**Callback acceptance rules.** A request with `error` fails with the AS's own error code; one with
`code` and a matching `state` succeeds; one with `code` but a missing or mismatched `state` **fails
loudly**, because under random ports there is no benign explanation; everything else — favicons, probes,
a bare `GET /` — answers 204 and is ignored without ending the flow. The callback page is static,
scriptless and echoes nothing from the query string, so it cannot become reflected XSS or a token
display surface.

**`ParseManualCallback`'s state rules branch on input shape.** Any input containing a query string
**must** carry a state and it must match, with a missing state treated as a mismatch — every AS echoes
state back when it receives one, so its absence means this is not this flow's callback. A bare code
cannot be validated and is still accepted, because a user pasting one has usually trimmed the URL
themselves and PKCE still stands: an intercepted code is useless without the verifier that never left
this process.

**Device flow loop rules.** `authorization_pending` keeps polling at the current interval; `slow_down`
**permanently** increases the interval by 5s, capped at 60s, rather than delaying once;
`access_denied` and `expired_token` terminate; and any other error, transport errors included,
terminates rather than retrying — a polling loop that swallows transport errors turns a network outage
into a silent 15-minute hang. The device code's own expiry caps the whole loop independently of the
interval, so a hostile `interval` cannot extend it.

## The discovery chain's abort conditions

A candidate returning non-2xx or unparseable JSON moves on to the next, because providers 404 the forms
they do not implement. But a candidate blocked by the SSRF screen or served over non-HTTPS **aborts the
entire chain immediately** — a security decision rather than a "try the next one" condition, since
continuing would only probe more private URLs. A cancelled or expired context aborts too: nobody is
waiting.

A document that parses but lacks `token_endpoint`, or lacks both authorization endpoints, errors
outright rather than silently trying the next candidate: that is a broken provider and the operator
needs to see it. The exception is the off-spec resource-origin hop, where an unusable document is an
ordinary miss.

**The `resource_metadata` in `WWW-Authenticate` is a hint, not an instruction.** It comes from an
unauthenticated 401, so it goes through the same screen, and when it cannot be fetched we still fall
back to the candidates derived from the resource URL.

## Conventions

**Every error is a sentinel or a `*FlowError` that unwraps to one**, so callers classify with
`errors.Is` and never by string matching. `FlowError.Suggestion` is operator-facing and must never carry
a secret.

**Dependency budget**: standard library plus `internal/secrets`, `internal/guard/netguard` and
`internal/platform` (the file lock, and nothing else). It imports no control plane, no pipeline and no
logging package — it returns a structured error and lets the caller decide how to render it.
