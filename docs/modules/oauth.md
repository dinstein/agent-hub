# OAuth / authorization interoperability

`internal/oauthflow` is agenthub's implementation of an **OAuth client** authenticating against
downstream MCP servers. This document answers two questions: **which specs we wrote against**, and
**which real-world deployment shapes work and which don't**.

The package's internal structure (invariants, failure directions, the division of labor between the
two `http.Client`s) is covered in the oauthflow section of [security.md](security.md) and is not
repeated here.

## Spec baseline

The MCP protocol version we advertise is `2025-11-25` (`mcpProtocolVersion` in
`internal/oauthflow/client.go`, canonical.md 5b). That revision's authorization chapter changed
substantially relative to `2025-06-18`, and the gap table below is aligned to it.

| Spec | Where it's used | Our status |
|---|---|---|
| OAuth 2.1 draft-13 | Overall flow, PKCE, HTTPS, redirect URI | ✅ |
| RFC 8414 AS Metadata | `MetadataCandidates`, 5 candidate shapes | ✅ The order is the contract, golden test |
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

Every row here is a provider behavior that actually exists in the wild, not a hypothetical
permutation.

### Authorization server discovery

| Shape | Supported | Mechanism |
|---|---|---|
| 401 `WWW-Authenticate` carries `resource_metadata` | ✅ | `ProbeChallenge` actively probes once and picks up `scope` at the same time; after passing the SSRF screen the pointer **takes precedence** over the candidate list |
| 401 without `resource_metadata` (only `realm` / `error`) | ✅ | Falls back to the RFC 9728 candidate list |
| PRM at the path-insertion position (`/.well-known/oauth-protected-resource/a/b`) | ✅ | Candidate 1 |
| PRM at the path-append position (`/a/b/.well-known/oauth-protected-resource`) | ✅ | Candidate 2 |
| PRM at the origin root (the resource identifier is a bare origin) | ✅ | Candidate 3, the regression fixed in `e8cbb28` |
| **No PRM at all, but AS metadata is hosted on the RS's own origin** | ✅ | `fetchResourceOriginMetadata`, `8d58c9f`. Seen for real: the per-resource `authorization_endpoint` is published only on the RS domain, while the copy on the issuer domain is a generic default |
| Issuer has a path, metadata at the insertion position | ✅ | `MetadataCandidates` candidates 1/2 |
| Issuer has a path, metadata at the append position (older providers that implement only OIDC) | ✅ | Candidate 3 |
| No metadata of any kind, but `/authorize` `/token` `/register` really do live under the issuer | ✅ | Synthesized fallback via `DefaultEndpoints`, recorded as `DiscoveryDefaults` |
| PRM lists multiple `authorization_servers` | ⚠️ | **Only the first is used.** Deliberate: trying them one by one would widen the set of hosts a malicious RS can make us contact |
| The `issuer` declared in AS metadata disagrees with where we fetched it from | ⚠️ | Not validated on **either** path. The resource-origin hop records `DiscoveryResourceOrigin`; the canonical `fetchMetadata` path carries no marker and no check — see the security boundaries below |

### Client identity

In the 2025-11-25 priority order (pre-registration → CIMD → DCR → manual entry):

| Shape | Supported | Mechanism |
|---|---|---|
| Operator pre-provisions a `client_id` (+ secret) | ✅ | `NewStaticRegistrar`, `--client-id` |
| CIMD (an https URL as the `client_id`) | ❌ | Seam exists, no implementation. Requires agenthub to first have a stable https hosting point |
| RFC 7591 DCR | ✅ | `NewDCRRegistrar`, the default path |
| Reuse of an already-registered `client_id` | ✅ | `State.ClientID`, avoids re-registering on every login |

**This is our biggest spec gap today**: 2025-11-25 demoted DCR from SHOULD to **MAY** ("included for
backwards compatibility") and promoted CIMD to **SHOULD**. Our default path happens to be the one
that got demoted. For an AS that implements only the new spec and doesn't enable DCR, our only
option right now is pre-provisioning via `--client-id`.

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

That inversion has a consequence worth naming, because it is invisible from the code: on the session
path `Paste` is nil, so `SelectMode` can never choose `ModeManual` and the automatic
loopback→manual downgrade in the last row above **cannot fire**. Manual mode reads a pasted callback
from a terminal, and there is none behind an HTTP API. A frontend on a host that truly cannot open a
browser has to fall back to the CLI, not to a mode that would block forever waiting for a paste.

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
scope discovery existed, so providers that work today keep working tomorrow.

Verified against real environments: grafana has PRM and automatically requests
`profile email openid`; server-a and server-b have no PRM, so no scope is sent, and they do **not**
end up with the `[... read write]` the AS advertises.

## Known gaps

Ordered by impact. These are genuine non-conformances, not "design choices".

### 1. CIMD is not implemented (spec SHOULD)

See above. We first need to decide where agenthub's client metadata document is hosted.

### 2. No step-up on 403 `insufficient_scope`

`ShouldRefreshOnStatus` treats 403 as "refresh once", but the spec requires parsing
`error="insufficient_scope"` + `scope=...` and then re-authorizing with the larger scope set.
Refreshing with the same scopes is guaranteed to fail again against `insufficient_scope`.

### 3. PKCE fails open when metadata is missing, contrary to 2025-11-25

`SupportsS256` ([pkce.go:61](../../internal/oauthflow/pkce.go#L61)) returns `true` when
`code_challenge_methods_supported` is absent. The comment spells out the reasoning (omission is very
common, and RFC 7636 requires servers to support S256). But 2025-11-25 says the exact opposite:

> If `code_challenge_methods_supported` is absent, the authorization server does not
> support PKCE and MCP clients **MUST** refuse to proceed.

This is a **deliberately retained deviation**, but it has to be recorded here rather than only in a
code comment: switching to fail-closed would lock out a set of existing providers, which makes it a
compatibility decision that needs its own evaluation.

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
read-modify-write against the same key; whichever finishes second overwrites what the other put
there. The window is narrow rather than absent: credentials are persisted precisely so that
re-registration does not happen on every start. Closing it means a third entry holding the
registration alone, which is additive and does not disturb the state-before-token write ordering
described below.

Deletion paths:

| Command | Effect |
|---|---|
| `auth logout <id>` | Deletes only `__oauth_state__` + `__http_auth__`; the registry entry stays |
| `server rm <id>` | Deletes the registry entry **and wipes all of that server's credentials** (across scopes, across keys), along with the rest of its footprint — see `confops.RemoveServer` |
| `server disable <id>` | Keeps the entry AND the credentials; the server just stops being used |

There is no `--keep-credentials`. Removing a server means removing what it was entitled to; wanting
the definition gone but the tokens kept is what `server disable` is for.

**Cleaning up by default is intentional**, and it fixes half of each of two problems:

1. Refresh tokens usually outlive access tokens by a long way. Deleting only the entry leaves one in
   the keychain while nothing in the registry hints that it exists any more.
2. The subtler one: because the index is by id, `server add foo` after `server rm foo` — even
   pointing at a completely different URL and a completely different provider — will **silently
   reuse** the old credentials instead of prompting a fresh login.

**Failure direction**: the registry deletion is committed first; a failed cleanup only emits a
warning and does not fail the whole operation. Doing it the other way round is worse in both
directions — cleaning up first would destroy credentials for a deletion that never happened when a
precondition fails, and promoting a keychain error into an operation failure would turn "the
keychain is locked" into "this server can't be deleted". The server is deleted either way, and the
warning names what was left behind and tells the operator to finish up with `auth logout`.

Cleanup is scope-blind (it goes through `List` and filters the whole set by `ServerID`) rather than
deleting just the two well-known keys — otherwise credentials under non-default scopes would be
missed, and those are exactly the fuel for the same-name resurrection path above.

**The one case that cannot be purged warns instead.** `secret set` writes to `secrets.enc` whenever
`AGENTHUB_SECRET_KEY` is set, but `List` can only see that file when the same key is present in the
process. Removing a server from a shell without it therefore enumerates nothing, deletes nothing,
and — until `secrets.Chain.HasUnreadableEnc` existed — reported a clean purge over a surviving
refresh token, feeding the resurrection path above. The predicate separates "nothing is stored" from
"something may be stored that I cannot see"; the purge now warns and names the fix (re-run with the
key set, or `auth logout <id>`). It answers TRUE on any doubt: a spurious warning costs nothing next
to a silently retained credential.

### Who refreshes, and what it logs

Two processes can renew a token, and both renew *early*. They differ in what they are looking at
when they decide:

| Process | Trigger | Serialization |
|---|---|---|
| daemon (`internal/daemon/oauth.go`) | proactive: a scan every ≤60s renews anything within the grace of `expires_at` | offline path (file lock) + an extra in-process singleflight |
| stdio gateway (`internal/gateway/authfresh.go`) | proactive: a connection asking for the credential renews it when `expires_at` is inside the grace | offline path — `<secrets>/<server>.refresh.lock`, then a re-read of `expires_at` |
| stdio gateway (`internal/gateway/auth.go`) | passive: a 401/403 from the downstream, refresh once and replay once | same |

The daemon renews on a timer because it is long-lived and owns every server at once. A stdio gateway
owns whatever its client dialed and has no timer, so it renews at the only moment it is guaranteed
to be looking — and its retry schedule **is** the deadline it reports to the round tripper, so a
failing provider cannot be asked once per request and there is no second timer to own. Both use the
same ladder, `oauthflow.RetryBackoff`, for the same reason the `trigger` values are shared
constants.

Servers that advertise no `expires_in` stay on the passive path permanently: "no expiry" means
"never expires", not "expired", so no deadline is reported for them at all. So do servers with no
stored OAuth state — a hand-pasted token has nothing to renew.

**Why the gateway does not simply rely on the 401.** Not every server issues one. A real downstream
answers `initialize`, `tools/list` *and* `tools/call` with `200` for a bogus or expired bearer,
returning `isError: true` and the text `Invalid token` inside an ordinary tool result; it answers
`401` only when the `Authorization` header is missing entirely. Against such a server the passive
path can never fire, and before the proactive one existed the credential stayed dead until the
client was restarted — while `agenthub auth refresh` on the same vault succeeded, because the
refresh token was fine and nothing had ever asked for it. Reading the downstream's *answer* to
detect this is not an option and never was: nothing in the chain inspects what a call carries back
(AGENTS.md). The gateway decides from its own vault instead.

Both processes log the same four messages, and every record carries
`trigger=expiry` (the daemon's scan) or `trigger=rejection` (a downstream 401/403) to say which one
produced it. One grep therefore covers either deployment, and the field — not the wording — is what
separates them:

| Message | Level | Meaning |
|---|---|---|
| `refreshing a downstream access token` | DEBUG | the attempt; separates "hung on the sibling lock" from "never attempted" |
| `access token refreshed` | INFO | `superseded=true` means another PROCESS got there first — the file lock working, not a double refresh. The daemon adds `shared`, the same statement about a caller inside its own process |
| `token cannot be refreshed without a new login` | WARN | dead end; only `agenthub auth login` fixes it |
| `access token refresh failed` | WARN | transient. `attempt` + `retry_in` appear only under `trigger=expiry`, on either process: a proactive refresher has a ladder to report, while under `trigger=rejection` the next try is simply whenever the downstream rejects again — their absence is the information |

The symmetry is deliberate and load-bearing. Which process renewed a token is a property of the
deployment, not of the event, and an operator reading a log usually does not know whether a daemon
was up — so it belongs in a field, not in prose that can only be grepped for on one of the two
sides. `internal/gateway/authlog_test.go` and `internal/daemon/oauthlog_test.go` pin the two halves;
renaming a message on one side alone is what they exist to catch.

The gateway's lines are load-bearing rather than decorative: `internal/downstream`'s round tripper
deliberately **discards** a refresh error and returns the downstream's original 401 (its
`WWW-Authenticate` is the better diagnostic), so without them a failed offline renewal would be
recorded nowhere at all.

## Security boundaries (don't loosen these in passing)

- Just like **`overlay` never persists to disk**, the `--authorization-endpoint` pin is
  **fail-closed**: if a pin is set but the URL is invalid or blocked by the SSRF screen, the login
  aborts rather than falling back to the discovered value. Silently authorizing against a different
  endpoint is the worse surprise.
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
- **Open: the canonical path has the same unvalidated `issuer`, and no status marker to say so.**
  `validateMetadata` (`discovery.go:449-458`) requires the endpoints to be present but never compares
  `md.Issuer` against the issuer the candidate URLs were built from, which RFC 8414 §3.3 requires; the
  callback then checks the returned `iss` against that same unvalidated value (`iss.go:41`). The
  tolerance was a deliberate position for the resource-origin hop and is documented as such above —
  but it was never argued for the canonical hop, where it silently applies too. The attack it leaves
  open is narrow and real: a hostile resource server whose `authorization_servers[0]` names a host
  that then declares itself a trusted issuer.

  **Not closed with an exact-equality check, because that breaks working providers**: a trailing
  slash, a host alias, or `http` vs `https` in a declared `issuer` is common real-world sloppiness,
  and exact equality turns each into a login that stops working. The shape that closes the attack
  without that cost is a **normalised** comparison — case-fold the host, drop a single trailing slash,
  then require scheme, host and path to match. Needs its own branch and its own argument; changing
  the resource-origin half is a separate decision from changing this one.

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
