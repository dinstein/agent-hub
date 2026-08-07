# OAuth / authorization interoperability

`internal/oauthflow` is agenthub's implementation of an **OAuth client** authenticating against
downstream MCP servers. This document answers two questions: **which specs we wrote against**, and
**which real-world deployment shapes work and which don't**.

The package's internal structure (invariants, failure directions, the division of labor between the
two `http.Client`s) is covered in the oauthflow section of [security.md](security.md) and is not
repeated here.

## Spec baseline

The authorization baseline is MCP `2025-11-25`, which is also the `MCP-Protocol-Version` header this
package sends on metadata requests (`mcpProtocolVersion` in `internal/oauthflow/client.go`; the tree
also speaks `2026-07-28` elsewhere — canonical.md §5b). That revision's authorization chapter changed
substantially relative to `2025-06-18`, and the gap table below is aligned to it.

| Spec | Where it's used | Our status |
|---|---|---|
| OAuth 2.1 draft-13 | Overall flow, PKCE, HTTPS, redirect URI | ✅ |
| RFC 8414 AS Metadata | `MetadataCandidates` — 3 candidates when the issuer has a path, 2 when it doesn't | ✅ The order is the contract, golden test |
| OIDC Discovery 1.0 | Both `openid-configuration` shapes | ✅ Same candidate chain as 8414 |
| RFC 9728 Protected Resource Metadata | `ProtectedResourceCandidates`, `WWW-Authenticate` | ✅ Includes the origin-root fallback |
| RFC 8707 Resource Indicators | `resource` parameter, sent on both authorize and token | ✅ `canonicalResource` |
| RFC 7636 PKCE | S256 mandatory | ✅ S256 only, `plain` is rejected outright |
| RFC 7591 DCR | `NewDCRRegistrar` | ✅ But upstream has downgraded it to MAY; `application_type` pinned to `"native"` (required by MCP 2026-07-28) |
| RFC 9207 `iss` response parameter | `validateIss`, before every code redemption | ✅ Fail closed: a mismatch always refuses; a MISSING `iss` refuses on the loopback path when the AS advertises support (manual paste is lenient — a hand-trimmed bare code loses the parameter) |
| RFC 8628 Device Flow | `StartDevice` / `DevicePoller` | ✅ Not required by MCP; we support it anyway |
| RFC 6750 §3 `scope` challenge | The `scope` parameter in a 401 | ✅ Priority level 1 |
| draft-ietf-oauth-client-id-metadata-document-00 (CIMD) | `NewClientIDMetadataRegistrar` | ⚠️ Seam only, `Register` returns `ErrNotImplemented` |

## Supported deployment shapes

Every row is a provider behavior that exists in the wild, not a hypothetical permutation.

### Authorization server discovery

| Shape | Supported | Mechanism |
|---|---|---|
| 401 `WWW-Authenticate` carries `resource_metadata` | ✅ | `ProbeChallenge` actively probes once and picks up `scope` at the same time; after passing the SSRF screen the pointer **takes precedence** over the candidate list |
| 401 without `resource_metadata` (only `realm` / `error`) | ✅ | Falls back to the RFC 9728 candidate list |
| PRM at the path-insertion position (`/.well-known/oauth-protected-resource/a/b`) | ✅ | Candidate 1 |
| PRM at the path-append position (`/a/b/.well-known/oauth-protected-resource`) | ✅ | Candidate 2 |
| PRM at the origin root (the resource identifier is a bare origin) | ✅ | Candidate 3, the regression fixed in `e8cbb28` |
| **No PRM at all, but AS metadata is hosted on the RS's own origin** | ✅ | `fetchResourceOriginMetadata`. Seen for real: the per-resource `authorization_endpoint` is published only on the RS domain, while the copy on the issuer domain is a generic default. Off-spec, hence bounded — see the security boundaries below |
| Issuer has a path, metadata at the insertion position | ✅ | `MetadataCandidates` candidates 1/2 |
| Issuer has a path, metadata at the append position (older providers that implement only OIDC) | ✅ | Candidate 3 |
| No metadata of any kind, but `/authorize` `/token` `/register` really do live under the issuer | ✅ | Synthesized fallback via `DefaultEndpoints`, recorded as `DiscoveryDefaults` |
| PRM lists multiple `authorization_servers` | ⚠️ | **Only the first is used.** Deliberate: trying them one by one would widen the set of hosts a malicious RS can make us contact |
| The `issuer` declared in AS metadata disagrees with where we fetched it from | ✅ / ⚠️ | **Refused** on the canonical `fetchMetadata` path (`validateIssuerMatch`, RFC 8414 §3.3). Still unvalidated on the resource-origin hop, which is why that one records `DiscoveryResourceOrigin` and never `DiscoveryOK` — see the security boundaries below |

#### What the chain records about itself

Every candidate is kept as an `Attempt` — the URL **plus what it answered** — on
`DiscoveryResult.Attempted` and, when the chain fails, on `FlowError.Attempted`. The URL alone
diagnosed nothing: four candidates and a failure read identically whether

- every one **404'd** (`no document` — the provider publishes nothing, so either the issuer is wrong or
  the synthesized fallback is what is wanted),
- one answered with a document that cannot drive a flow (`unusable document` — the provider is broken,
  and this is the URL to show them), or
- the first was **refused by the SSRF screen** (`refused`), in which case every later candidate was
  never tried and the list is evidence of a search that did not happen.

**A failed discovery is the case that needs the trace most**, which is why it rides the error too. The
candidate list did reach the message string of some branches, but a sentence cannot be rendered,
filtered or counted, and a reader wanting the third candidate's outcome had to parse English.

**This package writes no logs, and that is the arrangement, not an omission.** Its dependency budget
excludes any logging package, so it reports what happened as data and whoever holds a logger renders
it — `oauthlogin.Manager.logDiscovery` does, at Debug: one line for the status, one per candidate. Same
division of labour as `scope.Converge`. It covers **both** outcomes, not only failures: a login that
succeeded through `DiscoveryDefaults` is one candidate away from one that failed, and it is the case
that goes wrong later — a 403 from a synthesized `/register` means something entirely different from a
403 from an advertised `registration_endpoint`. A failure that never reached discovery logs nothing
rather than an empty chain, which would read as "every candidate was fine".

URLs only. Metadata endpoints are the class `FlowError.Error()` already interpolates, and no token,
code or client secret passes through a `DiscoveryResult`.

### Client identity

In the 2025-11-25 priority order (pre-registration → CIMD → DCR → manual entry):

| Shape | Supported | Mechanism |
|---|---|---|
| Operator pre-provisions a `client_id` (+ secret) | ⚠️ | `NewStaticRegistrar` implements it and `LoginRequest.Registrar` is the seam, but **nothing sets that field and there is no flag**. Not reachable by a user today |
| CIMD (an https URL as the `client_id`) | ❌ | Seam exists, no implementation. Requires agenthub to first have a stable https hosting point |
| RFC 7591 DCR | ✅ | `NewDCRRegistrar`, and — since `req.Registrar` is always nil — the only path |
| Reuse of an already-registered `client_id` | ✅ | `State.ClientID`, avoids re-registering on every login |

**This is our biggest spec gap today, and it is one row wider than it looks**: 2025-11-25 demoted DCR
from SHOULD to **MAY** ("included for backwards compatibility") and promoted CIMD to **SHOULD**. Our
only path happens to be the one that got demoted. This table used to mark the first row ✅ and cite a
`--client-id` flag as the way out; that flag has never existed, and `NewStaticRegistrar` has no
caller outside its own test — so an AS that implements only the new spec and does not enable DCR
currently has **no** route through `agenthub auth login` at all. Wiring the existing registrar to a
flag is the small half of closing this; CIMD is the large one.

### Interaction modes

| Shape | Supported |
|---|---|
| Loopback + random port | ✅ Default |
| Loopback + fixed redirect URI (pre-registered clients whose allowlist requires a byte-for-byte match) | ✅ `--redirect-uri` |
| Manually pasted callback (hosts with no browser) | ✅ `ModeManual` |
| Device flow (RFC 8628) | ✅ Chosen automatically: used whenever the AS advertises `device_authorization_endpoint` |
| Automatic downgrade to manual when the browser fails to open | ✅ `ModeAuto` only |

**Two callers drive this flow, and there is only one implementation of it.** `agenthub auth login`
runs it in the foreground; the control plane runs it as a session for graphical frontends
(`internal/oauthlogin`, and
[controlplane.md](controlplane.md#the-one-long-running-exchange-an-interactive-login)). The single
behavioural difference is `LoginRequest.Open`: the CLI opens a browser there, the session **records
the URL and returns success** so the caller can open it — the daemon may be headless and may not be
the machine the user is at.

That inversion excludes manual mode from the session path entirely, and both halves are load-bearing.
`SelectMode` reads `Open != nil` as "this host can open a browser", so the recording opener means it
can never choose `ModeManual`; and `Paste` is left nil, which disables the loopback→manual downgrade
in the last row above. There is no terminal behind an HTTP API to paste a callback into, so a
frontend on a host that truly cannot open a browser has to fall back to the CLI, not to a mode that
would block forever waiting.

### Token handling

| Shape | Supported |
|---|---|
| `resource` parameter binding the audience | ✅ Sent on both authorize and token, regardless of whether the AS declares support |
| Refresh token rotation | ✅ |
| Single-flight concurrent refresh | ✅ `Coordinator` + `Group[T]` |
| `expires_in` arriving as a string (non-conforming but common) | ✅ Tolerated |
| An omitted `scope` meaning "unchanged" (RFC 6749 §5.1) | ✅ Not misread as a downgrade |
| 401/403 triggering a single refresh | ✅ 403 counts too; several providers use 403 to signal an expired token |

## Scope selection

Which scopes a login requests follows the spec's Scope Selection Strategy — three levels, first hit
wins:

| Priority | Source | Notes |
|---|---|---|
| 0 | **The operator's `--scopes` / `oauth.scopes`** | If non-empty, sent verbatim and **completely overrides** the three levels below |
| 1 | The `scope` parameter in a 401 `WWW-Authenticate` | The spec calls this "authoritative" for the current request |
| 2 | **PRM**'s `scopes_supported` | The minimal set the resource server declares for itself |
| 3 | None of the above → **send no `scope` parameter at all** | |

Two deliberate rulings, both nailed down by mutation tests:

**Explicit configuration is not merged with discovery results.** The most common use of `--scopes`
is to **narrow** a provider's defaults (getting a read-only token against a server whose metadata
also advertises write). Merging the discovered set back in would amplify exactly the grant the
operator sat down to restrict.

**Level 3 does not fall back to AS metadata's `scopes_supported`.** The two documents answer
different questions: PRM says "what does accessing **me** require", AS metadata says "what can I
issue **in total**". Falling back to the AS copy when there's no PRM means asking, on behalf of a
resource server that never requested any of it, for everything the provider offers — write and admin
included. Sending nothing is the fail-closed direction, and it matches the behavior from before
scope discovery existed, so providers that work today keep working tomorrow. Verified against real
providers: one with PRM requests `profile email openid` automatically; ones without send no scope
and do **not** end up with the `[... read write]` their AS advertises.

## Known gaps

Ordered by impact. These are genuine non-conformances, not "design choices".

### 1. CIMD is not implemented (spec SHOULD)

See above. We first need to decide where agenthub's client metadata document is hosted.

### 2. No step-up on 403 `insufficient_scope`

`ShouldRefreshOnStatus` treats 403 as "refresh once", but the spec requires parsing
`error="insufficient_scope"` + `scope=...` and then re-authorizing with the larger scope set.
Refreshing with the same scopes is guaranteed to fail again against `insufficient_scope`.

### 3. ~~PKCE fails open when metadata is missing~~ — closed, and how

`SupportsS256` accepted an absent `code_challenge_methods_supported`, against both revisions:

> If `code_challenge_methods_supported` is absent, the authorization server does not
> support PKCE and MCP clients **MUST** refuse to proceed.

It was recorded here as a *deliberately retained deviation*, because failing closed "would lock out a
set of existing providers". **That claim was never true, and it is worth knowing how it survived.**
It was written once, at the initial public release, into two places at once — a code comment and this
entry — and thereafter each read as corroboration of the other. It named no provider, shipped no
fixture, and linked no issue.

Measured: every OAuth provider in this repository's own seed catalog, reached through the full
RFC 9728 → RFC 8414 chain to the authorization server's metadata document. **Zero omitted the
field.** Failing closed locks out nobody it was said to protect, and the check is reproducible from
the catalog alone.

Absent is now a refusal. Synthesized metadata is not: when no document exists, `DefaultEndpoints`
invents one and marks it `DiscoveryDefaults`, and nothing there omitted anything — proceeding
against a provider that publishes no metadata is the `AllowDefaultEndpoints` decision, taken
earlier.

**The same measurement found the risk that is real**, and it is not this one: several providers list
`plain` *before* `S256`, so a client selecting element zero selects `plain`. This package has always
used membership rather than an index, and `AuthorizeURL` refuses to build a URL for any method but
S256 — three layers, and `TestSupportsS256` now pins the middle one.

It also found a gap worth its own investigation: **some providers publish no `resource_metadata` in
their challenge and no PRM at the default locations at all.** A conformant client's discovery breaks
there long before PKCE is reached. Whether `fetchResourceOriginMetadata` rescues them is untested —
that hop probes the RS origin for *AS* metadata, which the measurement did not cover.

### 4. Only the first of multiple `authorization_servers` is used

RFC 9728 §7.6 says selection is the client's responsibility. Our policy is "take the first", on the
grounds that it limits a malicious RS's lateral probing surface. Deployments listing more than one
are rare in the wild; if you hit one where the first is unavailable and a later one works, the only
option today is specifying `--issuer` by hand.

## Credential lifecycle

The vault is indexed by **server id** (`agenthub/v1/<serverID>/<scope>/<key>`), and one server may
have:

| Entry | Contents |
|---|---|
| `__oauth_state__` | OAuth state JSON: refresh token, client_id/secret, token_endpoint |
| `__http_auth__` | Access token (shared slot for OAuth tokens and manually pasted tokens) |
| Any custom key | Whatever `secret set <server> <KEY>` stored, possibly under a non-default scope |

**Gap: the registration and the refresh token share `__oauth_state__`, so they cannot be updated
independently.** `Store.Save` marshals the whole `State` — client_id/secret and refresh token
together — and writes it as one value, which makes a re-registration and a token refresh the same
read-modify-write against the same key; whichever finishes second overwrites the other. The window is
narrow rather than absent, since credentials are persisted precisely so re-registration does not
happen on every start. Closing it means a third entry holding the registration alone, which is
additive and does not disturb the state-before-token write ordering.

Deletion paths:

| Command | Effect |
|---|---|
| `auth logout <id>` | Deletes only `__oauth_state__` + `__http_auth__`; the registry entry stays |
| `server rm <id>` | Deletes the registry entry **and wipes all of that server's credentials** (across scopes, across keys), along with the rest of its footprint — see `confops.RemoveServer` |
| `server disable <id>` | Keeps the entry AND the credentials; the server just stops being used |

There is no `--keep-credentials`. Removing a server means removing what it was entitled to; wanting
the definition gone but the tokens kept is what `server disable` is for.

**Cleaning up by default is intentional**, for two reasons. Refresh tokens usually outlive access
tokens by a long way, so deleting only the entry leaves one in the keychain while nothing in the
registry hints it exists. And the subtler one: because the index is by id, `server add foo` after
`server rm foo` — even pointing at a completely different URL and provider — would **silently reuse**
the old credentials instead of prompting a fresh login.

**Failure direction**: the registry deletion is committed first; a failed cleanup only emits a
warning and does not fail the whole operation. Both alternatives are worse — cleaning up first would
destroy credentials for a deletion that then fails its precondition, and promoting a keychain error
into an operation failure would turn "the keychain is locked" into "this server can't be deleted".
The server is deleted either way, and each warning names what survived and how to finish it by hand.

Cleanup is scope-blind (it goes through `List` and filters the whole set by `ServerID`) rather than
deleting just the two well-known keys — otherwise credentials under non-default scopes would be
missed, and those are exactly the fuel for the same-name resurrection path above.

**The one case that cannot be purged warns instead.** `secret set` writes to `secrets.enc` whenever
`AGENTHUB_SECRET_KEY` is set, but `List` can only see that file when the same key is present in the
process. Removing a server from a shell without it therefore enumerates nothing, deletes nothing,
and — until `secrets.Chain.HasUnreadableEnc` existed — reported a clean purge over a surviving
refresh token. The predicate separates "nothing is stored" from "something may be stored that I
cannot see", and answers TRUE on any doubt: a spurious warning costs nothing next to a silently
retained credential.

### Who refreshes, and what it logs

Three renewers, all of them early. They are **components, not processes**, and the distinction is
load-bearing rather than pedantic — two of the three can be running inside one process:

| Component | Trigger | Serialization |
|---|---|---|
| daemon refresher (`internal/daemon/oauth.go`) | proactive: a scan every ≤60s renews anything within the grace of `expires_at` | offline path (file lock) + an extra in-process singleflight |
| gateway, proactive (`internal/gateway/authfresh.go`) | a connection asking for the credential renews it when `expires_at` is inside the grace | offline path — `<secrets>/<server>.refresh.lock`, then a re-read of `expires_at` |
| gateway, passive (`internal/gateway/auth.go`) | a 401/403 from the downstream: refresh once, replay once | same |

**"Gateway" here does not mean "stdio gateway".** The daemon's HTTP data plane assembles one gateway
per credential inside its own process (`internal/daemon/httpdata.go`), and those reach both gateway
rows by the same route an `agenthub connect` process does: `Config.Auth` is left nil, so `newGateway`
builds the chain (`controlplane.md`, `internal/httpbridge` → "Who assembles it"). A daemon serving that
plane therefore hosts the refresher **and** N gateway coordinators at once, over one vault.

That is safe, and it is safe for the reason the offline path was chosen to begin with — which survives
the move indoors: each acquisition opens its own descriptor, and `flock(2)` is held per open file
description, so two coordinators in one process exclude each other exactly as two processes do. What
would **not** survive is switching a gateway to the online path on the grounds that "it is inside the
daemon now": `agenthub auth login/refresh` still writes the same vault from a third process, and no
in-process singleflight can see it.

The daemon refresher renews on a timer because it is long-lived and owns every server at once. A
gateway owns whatever its client dialed and has no timer, so it renews at the only moment it is
guaranteed to be looking — and its retry schedule **is** the deadline it reports to the round tripper,
so a failing provider cannot be asked once per request and there is no second timer to own. All three
use the same ladder, `oauthflow.RetryBackoff`, for the same reason the `trigger` values are shared
constants.

Servers that advertise no `expires_in` stay on the passive path permanently ("no expiry" means never
expires, not expired, so no deadline is reported at all), as do servers with no stored OAuth state —
a hand-pasted token has nothing to renew.

**Why the gateway does not simply rely on the 401.** Not every server issues one. A real downstream
answers `initialize`, `tools/list` *and* `tools/call` with `200` for an expired bearer, returning
`isError: true` and the text `Invalid token` inside an ordinary tool result; it answers `401` only
when the `Authorization` header is missing entirely. Against such a server the passive path can never
fire, and before the proactive one existed the credential stayed dead until the client was restarted
— while `agenthub auth refresh` on the same vault succeeded. Detecting it by reading the downstream's
*answer* is not an option and never was: nothing in the chain inspects what a call carries back
(AGENTS.md). The gateway decides from its own vault instead.

Refresher and gateway log the same four messages, and every record carries
`trigger=expiry` (a proactive refresher) or `trigger=rejection` (a downstream 401/403) to say which
one produced it. One grep therefore covers either deployment, and the field — not the wording — is
what separates them:

| Message | Level | Meaning |
|---|---|---|
| `refreshing a downstream access token` | DEBUG | the attempt; separates "hung on the sibling lock" from "never attempted". Emitted by the daemon's scan and the gateway's passive path, not by the gateway's proactive source |
| `access token refreshed` | INFO | `superseded=true` means another PROCESS got there first — the file lock working, not a double refresh. The daemon adds `shared`, the same statement about a caller inside its own process |
| `token cannot be refreshed without a new login` | WARN | dead end; only `agenthub auth login` fixes it |
| `access token refresh failed` | WARN | transient. `attempt` + `retry_in` appear only under `trigger=expiry`, on either process: a proactive refresher has a ladder to report, while under `trigger=rejection` the next try is simply whenever the downstream rejects again — their absence is the information |

The symmetry is deliberate. Which component renewed a token is a property of the deployment, not of
the event, and an operator reading a log usually does not know whether a daemon was up — so it belongs
in a field, not in prose greppable on only one of the two sides.
`internal/gateway/authlog_test.go` and `internal/daemon/oauthlog_test.go` pin the two halves;
renaming a message on one side alone is what they exist to catch.

**Where those lines land is not symmetric, and only the field keeps them apart.** A stdio gateway
writes to `<data>/logs/gateway-<client>.log`, one file per client. A data-plane gateway is handed the
daemon's own logger, so its renewals and the refresher's arrive interleaved in `daemon.log` — the
per-client file that `agenthub logs` is built around does not exist for it. Two fields separate them
there: `trigger`, as above, and `client`, which every gateway line carries (`newGateway` binds it) and
the refresher's lines never do.

The gateway's lines are load-bearing rather than decorative: `internal/downstream`'s round tripper
deliberately **discards** a refresh error and returns the downstream's original 401 (its
`WWW-Authenticate` is the better diagnostic), so without them a failed offline renewal would be
recorded nowhere at all.

## Security boundaries (don't loosen these in passing)

- The `--authorization-endpoint` pin is **fail-closed**: if a pin is set but the URL is invalid or
  blocked by the SSRF screen, the login aborts rather than falling back to the discovered value.
  Silently authorizing against a different endpoint is the worse surprise.
- **A callback is state-checked before anything else, error responses included.** RFC 6749 §4.1.2.1
  obliges an AS to echo `state` on an error response exactly as on a success, so a callback failing
  that check is not this flow's outcome whichever member it carries. Checking the error branch first
  — which both handlers used to do — meant nothing had to be guessed: anything reaching the loopback
  port during the flow could end a pending login and choose the text explaining why, since
  `TokenError.Error()` puts `error_description` in the terminal and the event log.
- **RFC 9207 applies to a failed callback too.** On an issuer mismatch a client MUST NOT act on or
  display `error` / `error_description` / `error_uri`, so both handlers carry the `iss` out with the
  failure and `issThenCallback` validates it before the AS's error is surfaced. It fires only for a
  failure carrying an actual AS error response (`*TokenError` in the chain): a login that died with
  no browser, a refused endpoint or a deadline never received an `iss`, and validating one anyway
  replaces the real cause with a fabricated issuer mismatch.
- **Every URL obtained from the network goes through `checkURL`**, including the
  `resource_metadata` pointer in a 401 (it comes from an unauthenticated response, so it's an
  attacker-influenceable hint, not an instruction).
- **`DiscoveryResourceOrigin` is not `DiscoveryOK`**: AS metadata fetched from an RS origin has an
  `issuer` that was never validated against where it came from. That's precisely the shape of a
  mix-up attack, so it gets its own status value — the diagnosis of a failure reads completely
  differently under it.
- **The resource-origin fallback does not synthesize endpoints**: that hop deliberately does not
  call `DefaultEndpoints`. Guessing `/authorize` on the RS's own origin would send the user's
  browser to a URL no provider ever declared. This one is nailed down by a mutation test
  (`TestDiscoverFromResourceOriginDoesNotSynthesize`).
- **Closed on the canonical path: `validateIssuerMatch` refuses metadata that names another
  issuer.** RFC 8414 §3.3 requires the declared `issuer` to match the identifier the well-known URL
  was built from, and until it did, the check the callback performs afterwards meant nothing:
  `validateIss` compares the response's `iss` against `md.Issuer`, so a host serving both the
  document and the redirect was comparing itself to itself. The attack that closes is the narrow
  real one — a hostile resource server whose `authorization_servers[0]` names a host that then
  declares itself a trusted issuer. A mismatch is fatal rather than "try the next candidate": the
  remaining candidates are on the same host.

  The comparison is **normalised, as this file argued before the check existed**: host lower-cased
  (DNS is case-insensitive, so it hands an attacker nothing), one trailing slash dropped, then
  scheme, host and path compared exactly. Exact equality would have turned ordinary provider
  sloppiness into logins that stop working. Scheme is deliberately not normalised — `http` where
  `https` was expected is a downgrade, not sloppiness — and neither is the path, which names the
  tenant.

- **Open: the resource-origin hop still has an unvalidated `issuer`.** Applying the same check there
  would defeat the purpose of the hop, which exists precisely for deployments publishing an AS's
  metadata on the resource server's own origin. It stays marked instead: `DiscoveryResourceOrigin`,
  never `DiscoveryOK`, and RFC 9207 offers no mix-up protection on that path. Changing that half is
  a separate decision from the one above.

## Troubleshooting: start with `DiscoveryStatus`

The first thing to look at on failure is `FlowError.Discovery`; it determines how to read every
error that follows:

| Status | Meaning | What "DCR 403" means under this status |
|---|---|---|
| `ok` | Metadata came from a canonical location under the issuer | The provider really did reject DCR |
| `protected_resource_metadata` | The 9728 hop succeeded, the 8414 hop failed | — |
| `resource_origin_metadata` | Metadata came from the RS's own origin; `issuer` was not location-validated | The endpoints may belong to a different deployment |
| `fell_back_to_default_endpoints` | The endpoints are guesses; there was no metadata document | `/register` may not exist at all |
| `pinned_authorization_endpoint` | The authorize address came from the operator, not from the provider's advertisement | For a 400 on the consent page, suspect the pin first |
| `failed` | Nothing was discovered | — |

## References

- [MCP 2025-11-25 Authorization](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization)
- [RFC 8414](https://datatracker.ietf.org/doc/html/rfc8414) /
  [RFC 9728](https://datatracker.ietf.org/doc/html/rfc9728) /
  [RFC 8707](https://www.rfc-editor.org/rfc/rfc8707.html)
- [CIMD draft-00](https://datatracker.ietf.org/doc/html/draft-ietf-oauth-client-id-metadata-document-00)
