# Security and governance layer

This layer answers two questions, and deliberately no longer answers a third.

It answers **how a process is started** (command smuggling) and **where an outbound connection may
go** (SSRF). Both are decided from the request itself, before anything is contacted, and both refuse
regardless of who asked — they are not permissions, and no configuration grants an exemption from
them. `internal/oauthflow` belongs here because it is the only component that deliberately sends
credentials to the public internet, and every one of its constraints is a security constraint rather
than a protocol one.

It no longer inspects **what a downstream returned**. Earlier versions scanned results for prompt
injection and for leaked credentials, fingerprinted tool definitions to grade drift, and queued calls
for a human decision. All of it was removed: what a client may reach is decided in advance by
configuration, and a stage that reads a result in order to relabel or withhold it is the opposite of
that model. Worth knowing when reading old commits; nothing in the tree does it today.

## The stdio-proxy escalation path, and why it does not close here

`security_best_practices.mdx` names an attack this architecture is the exact shape of: a local proxy
that spawns MCP servers as child processes and authenticates its clients with a token. The chain is
XSS in the client → steal the proxy token → call the proxy → **proxy spawns an arbitrary command** →
RCE.

It breaks at the fourth step, and structurally rather than by a check. **Nothing a client presents
can choose a command.** The set of servers is decided in advance by configuration, and the only
surface that can add one — `internal/ctlapi` — is not on the HTTP listener at all: the daemon's
`httpserve.go` mounts `httpbridge` with the MCP data plane as its only dispatcher, while the control
API listens on a Unix domain socket (0700 directory, 0600 socket, `SO_PEERCRED`/`LOCAL_PEERCRED`
same-uid verification) or a Windows named pipe whose SDDL admits the current user and nobody else.
It issues no tokens, because OS-level identity is the credential. A stolen bearer therefore buys
tool calls scoped by that token's profile and allowlist, and no way to name a new process.

**Do not mount a control route on the HTTP listener.** That single change would reconnect the chain,
and nothing else in this layer would notice: `spawnguard` grades a command's shape, not who asked
for it, and it is designed that way on purpose.

The tutorial's own mitigations are weaker than this and are also present where they apply:
`runtime: docker` sandboxes a spawned server, `spawnguard` refuses command shapes regardless of
caller, and the event log records server lifecycle.

`internal/guard/*` is the zero-business-dependency base (canonical.md §2 rule 4: standard library
plus `internal/guard` itself, enforced by depguard). These packages know nothing about servers,
sessions, or the pipeline; they make purely functional determinations and hand the result up for
someone else to act on. `internal/oauthflow` is the only credential-acquisition path, and it consumes
`netguard`'s predicates.

Every predicate and decision point here spells out `Failure direction:` in its doc comment; treat it
as part of the signature.

| Direction | Meaning | Typical examples |
|---|---|---|
| fail-open | If we can't decide, let it through | spawnguard's shape checks |
| fail-closed | If we can't decide, refuse | `netguard.HostIsPrivate` |
| fail-to-false | If we can't decide, don't grant trust | `netguard.HostIsDefinitelyPrivate` |

The distinction between the three is not a matter of style: the same "I'm not sure" has to produce
opposite answers depending on whether it's used to refuse or to grant trust, and netguard's paired
predicates encode exactly that into the shape of the API, outside the type system.

---

## internal/guard

**One-line responsibility**: provide the single decidable rejection sentinel for the whole guard
layer, so callers can recognize "was this a guard rejection?" without importing every subpackage.

It exports one `var ErrBlocked`. Every typed rejection in the subpackages — `*spawnguard.Blocked`,
`*netguard.BlockedError` — implements `Unwrap() error { return guard.ErrBlocked }`, so
`errors.Is(err, guard.ErrBlocked)` holds everywhere. The machine-readable code and the
human-readable reason stay on the subpackages' concrete types; the sentinel only answers "was this
blocked by a guard".

---

## internal/guard/spawnguard

**One-line responsibility**: scan the command line and environment before starting a downstream
process, and block the "smuggling shapes" that disguise arbitrary code execution as an ordinary
server entry point.

### Positioning: anti-smuggling, not a sandbox

Accept the framing before reading the code: `spawnguard` does pattern matching on command lines, not
execution boundaries. A server the user configured themselves, that was always going to run code,
still runs; `npx`, `uvx`, and `docker run` with ordinary project mounts pass through untouched. What
it blocks is the shape "an innocuous-looking entry point with `sh -c`, `LD_PRELOAD`, or
`--privileged` hiding inside it". The real isolation story is elsewhere and is shipped rather than
pending: the Docker spawner (`internal/mcp/transport/docker.go`), reached by `runtime: docker`, which
fails closed rather than degrading to host execution when the isolation an entry claims cannot be
delivered.

### Where it is attached

**Every spawn is screened, and the default is the guard rather than nil.** `downstream.Deps.Spawn`
resolves to the shared `spawnguard.Guard.Check` unless an assembly overrides it or sets
`SpawnUnscreened`, and `dialStdio` hands that to `transport.StdioConfig.Screen`, which both spawn
paths — plain stdio and the rewritten `docker run` line — consult on the **final** host command, after
secret expansion and after any docker rewriting.

It did not always work that way. `Screen` was implemented and called, `spawnguard` was implemented
and exhaustively tested, `Deps.Spawn` existed as `any` marked "reserved… nil today" — and nothing
joined them. The guard's only production caller was `confops`, which sees **docker** entries at
`server add` time and nothing else, so a host-runtime entry written straight into the registry was
never screened at all, and every test on both sides stayed green. Hence the default is the guard and
the opt-out is an explicit field: a screen each assembly must remember to attach is one that will be
missing from the next assembly.

### Invariants and failure directions

- **The check order is fixed**: environment variables (a deterministic check, always first and always
  applicable) → denylist → allowlist → wrapper unwrapping → inline eval → container escape.
- **The allowlist bypasses shape checks only, never the env check.** The distinction is deliberate:
  dangerous environment variables subvert precisely that trusted binary, so putting `LD_PRELOAD` on
  an allowlisted command is the most valuable attack, not the safest one.
- **Deterministic checks always block; shape checks fail open.** A denylist hit or a dangerous env
  name is always rejected, whereas a command line `Check` cannot parse is allowed through — turning
  every uncommon but legitimate launcher into a failure costs more than a missed detection.
- **Details of the dangerous-env decision**: an empty value is treated as inert (an explicit unset)
  and allowed; `AllowEnv` matches exactly and takes precedence over both the built-in table and the
  prefix table.
- **The env rejection names its variable in a field, and deliberately cannot say where it came
  from.** `Blocked.EnvVar` carries the name for `CodeEnvSmuggling` — empty for every other code, and
  empty for the shape-based env blocks (`env -S`, an unrecognized `env` flag), which are about no
  single variable. Provenance is not this package's answer to give: `Check` receives one flat
  `[]string`, while `downstream.dialStdio` built that slice by laying the entry's `env` block over
  the agenthub process's own environment, so only the caller can tell the two apart. It does, in
  `explainBlockedEnv`, and the distinction is the entire fix — a declared variable is edited out of
  the registry entry, an inherited one appears in no AgentHub file at all and has to be unset where
  the process was started, which for a gateway means restarting the client holding that environment.
  A message stopping at the name sent operators to grep a registry that never mentioned it.
- **Wrapper unwrapping goes at most 4 levels deep** (`maxWrapperDepth`); beyond that it stops and the
  remaining outer wrapper is passed through the subsequent checks as-is (fail-open). The supported
  wrappers are `env`, `busybox`, `nohup`, `setsid`, `nice`, `stdbuf`, `timeout`, `sudo`, `doas`.
- **Every wrapper registers two tables: flags that take a value and flags that don't; a flag in
  neither table causes unwrapping to be refused outright.** Registering only value-taking flags isn't
  enough — miss one and its value is taken for the command, and **the real command is never checked
  at all**:

  ```
  sudo --prompt x sh -c 'evil'     was once allowed
  timeout -d x 10 sh -c 'evil'     was once allowed
  stdbuf --input L sh -c 'evil'    was once allowed
  ```

  The handling here is deliberately different from `docker run`: the option sets of coreutils and
  sudo are **closed and documented**, so both tables can be complete, and whatever isn't listed is a
  shape this build doesn't understand and is rejected. We don't "assume it takes a value" the way the
  container side does, because at this layer **guessing wrong in either direction moves where the
  command is** — there is no safe default. The cost of forgetting to register a flag is a loud
  rejection of an uncommon but legitimate command line, which an operator can see and fix; a silent
  bypass is neither.
- **`env -S` is rejected outright rather than guessed at.** Split-string re-splits an opaque string
  into a command line, which is the very reason this package exists, so `-S`, `-S…`,
  `--split-string`, and `--split-string=…` all yield `CodeEnvSmuggling`. `NAME=VALUE` assignments
  inside `env` go through the same `checkEnvEntry` as a direct env slice. `env`'s own options get the
  same two tables, and anything unrecognized is rejected: GNU env and BSD env have different option
  sets (BSD has `-P`, GNU the signal options), so no single table can be complete for both, and
  rejection is the only answer that doesn't depend on which `env` is installed.
- **Docker's global flags (the ones before the subcommand) need two tables as well**, for a reason
  even harder than the run flags: misreading a global flag shifts the position of the **subcommand**,
  and when `sub` isn't `run|create|exec` the entire container check is skipped —
  `docker --tlscacert /tmp/ca run -v /:/host img` went completely unchecked for exactly this reason.
- **Inline-eval scanning stops at the first operand.** Flags after the script path belong to the
  script, and blocking them would be a false positive. `python -m` likewise terminates the scan
  (running by module name is not inline text). In the node family, besides `-e/--eval/-p/--print`,
  preload flags like `-r/--require/--import/--experimental-loader` also count as inline eval, because
  they load arbitrary modules before the script.
- **Every interpreter family has a table of "the value is the next argument" flags**, for **exactly
  the same** reason as the container case: a flag's value doesn't look like a flag either, and
  without skipping it the scan stops there treating it as an operand, so **the eval flag that follows
  is never seen**. This had only been reasoned through on the container branch, so all of the
  following sailed right past:

  ```
  bash --rcfile /tmp/x -c 'evil'     perl -I /tmp -e 'evil'
  node --title svc -e 'evil'         php  -d k=v  -r 'evil'
  ```

  The value-taking flags of shell / perl / ruby / php / python are a **closed set** and are listed in
  full; **node is not** — it adds options every release, so `node --somenewflag value -e ...` remains
  a residual possibility. Sealing it needs the scan to continue past the operand, at the cost of false
  positives on scripts carrying their own `-e`/`-c`: a policy change, not a bug fix.
- **The container check covers only `run|create|exec`** (including the second-level subcommand in
  `docker container run`), and the scan likewise stops at the first operand — the image name — to
  avoid false positives from the in-container command's arguments.
- **On the container side the value table is inverted: `containerBoolFlags` lists the flags that do
  NOT take a value.** This isn't a style choice. `docker run` has hundreds of flags and gains more
  every release, so a positive list of value-taking flags can never be complete, and
  **a single omission is a silent container escape** — the omitted flag's value is taken for the image
  name, the scan stops there, and none of the policy-bearing flags after it are ever judged:

  ```
  docker run --sysctl net.ipv4.ip_forward=1 -v /:/host img   was once allowed
  docker run --storage-opt size=1G --privileged img          was once allowed
  ```

  With the list inverted, **an unrecognized flag is assumed to take a value**: skip one argument and
  keep scanning, so the `-v` further along is still judged. Guessing wrong now costs at worst one
  false positive on an argument that never belonged to docker — loud and fixable, unlike a bypass.
- **Bind mounts are blocked for whole trees, matched on the cleaned source.** `-v`/`--volume` and
  `--mount`'s `source=`/`src=` are both judged. Denied: the roots `/`, `/etc`, `/root`, `/boot`,
  `/dev`, `/var`, `/usr`, `/home` **matched exactly**, anything at or under `/proc` or `/sys`, and any
  path ending in `docker.sock`/`podman.sock`/`containerd.sock`. A subdirectory such as `/etc/myapp` is
  therefore allowed — the target is whole-tree exposure, not an enumeration of every possible secret.
  Named and anonymous volumes are not host binds.
- **`--privileged` is judged by its value, not by its spelling, and the failure direction is ON.**
  docker parses the attached form with `strconv.ParseBool`, so `--privileged=true`, `=1` and `=T` all
  enable it; only an explicit parseable false is treated as off, and a value neither docker nor this
  build can parse (`--privileged=yes`) is not evidence that isolation is intact. Matching the bare
  token alone was a bypass. The `=false` branch is also the one place in the flag loop that continues
  **without** consuming a value — the shape `containerBoolFlags` was inverted to prevent — and
  `TestPrivilegedFalseDoesNotBlindTheScan` is the regression for it.
- **`--cap-add` and `--security-opt` are matched by meaning, not by spelling** — the same lesson as
  `--privileged`, in two more places. `--cap-add` folds case **before** stripping `CAP_`, the order
  docker normalizes in; doing it the other way round let `cap_sys_admin` through while refusing
  `CAP_SYS_ADMIN`, for a capability docker grants identically for both. `--security-opt` normalizes
  the separator before matching, because moby's `parseSecurityOpt` still accepts the legacy `:` form
  (the deprecation was never carried through to a removal) and reads a bare `disable` as SELinux
  label-disable — so `seccomp:unconfined`, `apparmor:unconfined` and `disable` each switched off a
  confinement layer this guard's own error message claims to protect. Adding confinement or naming a
  profile (`seccomp=/path/profile.json`, `apparmor=docker-default`, `no-new-privileges`, `--cap-drop`)
  still passes: a guard that refused those would push operators away from using them.
- **`--device` is a DENY list of block and memory devices**, matched on the cleaned host source — the
  first colon-separated field of `host-src[:container-dest[:permissions]]`. Refusing only the exact
  path `/dev` blocked the one form docker itself rejects, while `--device=/dev/sda` handed over the
  host filesystem and `/dev/mem` handed over its RAM. What is denied now: the `/dev` tree, `/dev/mem`,
  `/dev/kmem`, `/dev/kcore`, `/dev/port`, and the block-device families (`sd`, `hd`, `vd`, `xvd`,
  `nvme`, `dm-`, `md`, `loop`, plus the `mapper/` and `disk/` symlink trees).

  **This is the deliberate exception to "never a deny list", and the exception is what the rule is
  actually about.** That rule governs *tool selectors*, where the question is "what may this client
  reach" and an unknown answer must be no. Here the layer's own stated direction is the opposite —
  *deterministic checks always block; shape checks fail open* — because spawnguard judges a command
  line the **operator wrote**, and it is anti-smuggling rather than a sandbox. An allow list of device
  nodes cannot be completed: `/dev/dri`, `/dev/kvm`, `/dev/fuse`, `/dev/net/tun`, `/dev/snd`,
  `/dev/ttyUSB0` are all ordinary requests from a real server, and each new driver adds more. The
  denied set is the opposite shape — enumerable, stable across machines, and with no use inside an
  MCP server. The cost of a false positive also changed when the guard moved onto the spawn path: a
  refused device is now a server that dies at connect with `ClassFatal`, not a configuration rejected
  at `server add`, so a narrow allow list shipped after that move would have taken working GPU, VM,
  FUSE and serial-device servers down at their next connect.
- **The runtime never generates `--privileged` or `--device` itself** — both can only arrive through
  an operator's `extraArgs` — so neither rule can invalidate a configuration AgentHub produced.

---

## internal/guard/netguard

**One-line responsibility**: answer "is this destination private / not publicly routable", and screen
again at dial time against the IP actually resolved, closing the DNS-rebinding TOCTOU window. It also
holds the repository's one answer to "is this authority loopback", for the packages that bind.

### Why there are two predicates

This is the one design point to remember about the package: "is this host private?" is **two
questions with opposite failure directions** depending on the use, so there are two exported
predicates and they must never be substituted for each other.

- `HostIsPrivate(host) bool` is for **refusing** (an OAuth redirect target, a remote server URL).
  **fail-closed**: an empty host, a DNS failure or timeout (5s), and an empty answer all return
  `true`; if any record a hostname resolves to is a private address it returns `true` — one
  attacker-controlled A record has to be enough to trigger refusal.
- `HostIsDefinitelyPrivate(host) bool` is for **granting trust** (treating a target as local,
  relaxing localhost-only rules). **fail-to-false**: only a literal IP or a localhost name returns
  `true`, and it **never resolves DNS** — a DNS answer is a claim the zone owner can change at any
  time, so it can negate trust but must never grant it. Its scope is also narrower than
  `AddrIsPrivate`: only loopback, RFC1918/ULA, link-local unicast, and unspecified count, excluding
  CGNAT, the documentation ranges, and the benchmark range — those addresses aren't routable, but
  they aren't "locally private" either and shouldn't unlock local trust.

`AddrIsPrivate(netip.Addr) bool` is the address classifier both share. It returns `true` for an
invalid zero-value `Addr` (fail-closed) and calls `Unmap()` before deciding, so `::ffff:10.0.0.1` is
classified as `10.0.0.1`. Beyond the standard library's own loopback / private / unspecified /
link-local / multicast classifiers it adds a prefix table: `0.0.0.0/8`, CGNAT, the IETF protocol
assignments, the three TEST-NETs, benchmark `198.18.0.0/15`, `240.0.0.0/4`, the v6 discard and
documentation ranges, **deprecated IPv6 site-local `fec0::/10`**, and the four v4-embedding
prefixes.

Two shapes in that table are the ones to watch:

- `netip.Addr.IsPrivate` covers the ULA range `fc00::/7` that *replaced* site-local and stops there,
  so `fec0::1` was not private to `AddrIsPrivate` at all. Because the hostname-time screen and
  `DialControl` both consult that single function, one missing prefix opened both doors at once — a
  range being deprecated (RFC 3879) makes it *less* likely to be filtered elsewhere on the host, not
  more.
- `64:ff9b::/96` (NAT64 well-known, RFC 6052), `64:ff9b:1::/48` (NAT64 local-use, RFC 8215), `::/96`
  (IPv4-compatible, deprecated by RFC 4291) and `2002::/16` (6to4, deprecated by RFC 7526) are all ways
  of writing an IPv4 address as IPv6, so **judging them by their v6 form answers the wrong question**
  — `::127.0.0.1`, `2002:7f00:1::`, `64:ff9b::7f00:1` and `64:ff9b:1:7f00:1::` all mean 127.0.0.1, and
  none of them looks like loopback to `IsLoopback()`. (At one point only the NAT64 well-known prefix
  was covered; the deprecated forms got past even `DialControl`, and the NAT64 local-use prefix was
  the same gap, caught later by the 2026-07-31 sweep.) We reject the whole range rather than extracting
  and reclassifying the embedded v4: the deprecated forms exist only **to spell an IPv4 address**, and
  a NAT64 translator's whole job is to carry an arbitrary v4 destination — public or private — across
  the v6 side, so decoding and re-judging that address would only reintroduce the thing this predicate
  exists to catch. Rejecting the whole range costs nothing real and doesn't depend on a decoder being
  correct.

`AddrIsLoopback(addr) bool` is a **third question, not a third answer to that one**: the two above
judge a destination we are about to reach, this one judges an authority on *this* side — the address
a listener would bind, and the `Origin` a browser claims. **fail-to-false**, like
`HostIsDefinitelyPrivate` and for the same reason: it grants a weaker authorization, so an empty host
(which binds every interface), a name other than `localhost`, and an unparsable address all read as
not loopback. Unlike the destination predicates it has no rebinding problem to defend against — a
bind address is not resolved by anybody else — which is why it is not a special case of either.

It lives here rather than in the package that binds because its callers sit on **three different
layers**: `internal/httpbridge` binds the MCP face by it and screens `Origin` with it,
`internal/daemon` decides from it whether exposing the endpoint needs the operator's explicit
confirmation, and `internal/diag` refuses to publish profiles anywhere else — and, on top of the
bind-time refusal, screens every request's `Host` with it too, for the same reason the bridge screens
`Origin`: the bind address alone stops the network but not a browser rebound onto it. The lower ones
must not import the higher to ask, and a second copy would eventually disagree with the first — in the
direction that publishes tool execution, or a heap dump holding credentials, to a LAN.

`DialControl` is the package's real line of defense; install it on `net.Dialer.Control`. The address
it sees is the **already-resolved** address the socket is about to connect to, so whatever rebind
window a hostname-level pre-check leaves open is closed here. It is fail-closed too: an address it
cannot parse as an IP literal is refused rather than guessed at, and refusals return a `*BlockedError`
satisfying `errors.Is(err, guard.ErrBlocked)`.

### Invariants and failure directions

- "Private" in this package means uniformly "not publicly routable", not "RFC1918". When changing
  that table, think through the fact that it affects the refusal direction and the trust-granting
  direction at the same time.
- `lookupNetIP` is a package-level variable for test substitution only; the production path always
  goes through `net.DefaultResolver`.
- Hostname pre-checks are **necessarily insufficient**, and the docs state plainly that they must be
  paired with `DialControl`. `oauthflow.Client` uses them exactly that way: `checkURL` screens the
  URL's host before the request, and `dialControlFor` on the transport screens the actual IP at dial
  time.
- `HostIsDefinitelyPrivate` is wired, and **both of its callers use it to REFUSE rather than to grant
  trust** — the opposite of the role its own doc comment describes, deliberately.
  `confops.ValidateOAuthHint` and `ValidateEndpoint` screen a URL at the moment the operator types it,
  and there the fail-closed `HostIsPrivate` would reject a laptop with no network and a name that
  only resolves inside a VPN: honest edits refused for being unresolvable at edit time. Refusing with
  the fail-to-false predicate is sound *only* because this is not the boundary —
  `internal/downstream` screens the name before it connects and the resolved address at dial time,
  which is the pair DNS rebinding cannot walk around. **Reach for it to refuse only when you can name
  the fail-closed check that runs after you**; with no such check the doubt it waves through is the
  entire attack. The pairing is also why the function survived a period with no caller at all:
  refusing on doubt and withholding trust on doubt are different functions, and collapsing them is
  the mistake the pair exists to prevent.

---

## internal/oauthflow

**One-line responsibility**: implement a headless OAuth 2.1 client — the discovery chain, dynamic
registration, PKCE, three interaction modes, token exchange, writing into the credential vault, and
refresh coordination — while holding the line at every step on "credentials don't escape, don't get
downgraded, and don't get double-spent".

> This section covers **the package's internal structure**. For "which spec revision we wrote
> against / which provider deployment shapes work / known gaps", see [oauth.md](oauth.md). Read that
> one first when you can't connect to some downstream OAuth server.

A login is a pipeline, and each stage is a value usable on its own, so the CLI can emit NDJSON
progress events between stages and the daemon can re-enter at the "token exchange" stage to refresh:

```
discovery ──► registration ──► authorization ──► token exchange ──► persist
(RFC 8414/9728)  (RFC 7591)   (loopback|manual|device)   (PKCE)      (vault)
```

The two structural choices worth knowing before reading any of it: `Client` holds **two**
`http.Client`s with different redirect policies (see the invariants below), and `ClientRegistrar` is
a three-implementation seam (DCR, Client ID Metadata Documents, an operator-provisioned client_id) so
that replacing the deprecated mechanism is a constructor swap rather than a rewrite of the flow.

### Invariants and failure directions

- **Credentials are never sent to a private address; every outbound request is screened twice**: the
  URL's host is screened with `netguard.HostIsPrivate` before the request (fail-closed — unresolvable
  counts as private), and the actually-resolved IP is screened with `netguard.DialControl` at dial
  time (closing the DNS-rebind TOCTOU window). `AllowLoopback` is an explicit per-call switch, and
  even when on it allows **only** literal loopback addresses and RFC 6761's `localhost` name tree —
  `isLiteralLoopbackHost` is narrower than `netguard.HostIsDefinitelyPrivate`, RFC1918 and link-local
  are not exempt, and no hostname's DNS answer can unlock this exception. The connection pool's
  `IdleConnTimeout` is dropped to 30s to avoid reusing a connection that was still a public address
  when it was screened.

  **The carve-out is decided from the REQUESTED host, and needs the resolved address to agree.**
  `dialControlFor` is built per dial from the `host:port` the transport was asked for — before any
  resolution — and opens only when `isLiteralLoopbackHost` holds for that host **and** the address
  being dialed is itself a loopback literal. Deciding it from the resolved address alone (which is
  what it did) reopened the switch to precisely the DNS answer that cannot unlock it: a public-looking
  name answering `127.0.0.1` was dialed without netguard being consulted at all, delivering a
  discovery GET — or a `postForm` carrying a `code_verifier` or a refresh token — to whatever listens
  on this host's loopback interface. Requiring both halves keeps a poisoned `localhost` on the
  netguard branch.
- **A destination the USER'S BROWSER is sent to is screened by the same rules as one we would fetch
  ourselves.** `Client.AuthorizeURL` (the method every flow calls; `BuildAuthorizeURL` stays a pure
  renderer) and `StartDevice`'s two verification URIs all pass through `screenBrowserURL` →
  `checkURL`. The endpoints come out of a metadata document or a token-endpoint response that a remote
  authorization server wrote, and the browser carries the user's ambient cookies to whatever they
  name: an AS advertising `authorization_endpoint: https://10.0.0.5:8080/authorize` was otherwise an
  SSRF whose client is the human's session rather than ours (only the operator-PINNED endpoint used to
  be screened). Refusing an AS on a private address breaks nothing that worked: its token endpoint is
  already screened in `postForm`, so the flow died one step later — after the browser had been sent
  there. Two limits this cannot reach, and they are why the screen is not the whole defence: the
  browser resolves the name itself, and an injected `Flow.Open` can ignore the answer.
- **PKCE is never downgraded.** `ChallengeMethodS256` is the only method ever sent and there is no
  `plain` code path. `randRead` is a package-level variable rather than a config option — making the
  entropy source configurable would be manufacturing exactly that downgrade path — and
  `newRandomToken` uses `io.ReadFull` so a short read can't silently shorten the verifier, returning
  `ErrEntropy` rather than falling back to `math/rand`. `BuildAuthorizeURL` errors outright when
  `PKCE` is nil, the challenge is empty, or the method isn't S256; `Client.Exchange` refuses to
  exchange without a verifier. The one random value allowed to degrade is `correlationID()`: a
  diagnostic ID that can't be generated becomes a fixed placeholder rather than failing a login.
- **POSTs carrying credentials follow zero redirects.** The `credential` client's `CheckRedirect`
  returns `http.ErrUseLastResponse`, and `postForm`/`postJSON` then treat any 3xx as an `ErrRedirect`.
  A 302 on a request carrying a code_verifier, a refresh token, or a client secret is an exfiltration
  primitive, not a routing detail. Logged `Location` values keep only scheme+host+path
  (`redactLocation`). By contrast the `discovery` client allows up to 3 hops, **re-screening every hop
  through `checkURL`** — metadata documents are public, and providers really do move them.
- **Persistence write order: state first, then the access token** ([flows.md §5](../flows.md#5-headless-oauth-and-refresh)
  draws the two crash windows). `Store.Save` never parallelizes the two writes and never proceeds past
  a failed first write; `Clear` mirrors the order, deleting the token first. A corrupt `__oauth_state__`
  is a loud error rather than "no state": silently treating it as absent would trigger a fresh
  registration and orphan a working refresh token.
- **Two inherited rules in `SaveFromToken`**: a response without a refresh_token **does not clear** a
  stored one (non-rotating providers omit it on every refresh, and clearing it would turn the second
  refresh into a fresh login); and an omitted scope means "unchanged" per RFC 6749 §5.1.
- **Expiry semantics**: `ExpiresAt == 0` means **never expires**, not "already expired" (providers
  like Atlassian genuinely don't send `expires_in`). The 60s `RefreshGrace` is subtracted only from
  tokens with a lifetime over 5 minutes — subtracting it from a 60s token would make every token
  expire at birth and trap the gateway in an infinite refresh loop. `lenientNumber` never fails,
  returning 0 for anything unparseable, and negatives are zeroed in `parseTokenResponse` too —
  because an unparseable field shouldn't throw away a perfectly usable access token.
- **Refresh has a single writer, in two tiers.** Online (with the daemon present), all refreshes go
  through the in-process `Group` single-flight, because the daemon is the vault's sole writer.
  Offline, we take a `<server>.refresh.lock` sibling file lock, **and re-read the state after
  acquiring it**: the lock only serializes, it doesn't tell the second acquirer the work is already
  done. If the re-read shows `expires_at` has moved past the value observed before queueing, we
  abandon our own refresh and return `ErrRefreshSuperseded` along with the fresh credentials —
  continuing would burn the one-time refresh token the other party just stored.
  `CoordinatorConfig.Online` being nil defaults to **offline**: an extra lock acquisition costs one
  syscall, while failing to take one that was needed costs the user's refresh token.
- **`ErrNoToken` must be distinguished from `ErrNoState` and from an empty token.** A record with DCR
  credentials but no access token returns `ErrNoToken` and never `(state, "", nil)` — a caller handed
  an empty token would attach an empty Authorization header, get a 401, go "refresh", and loop
  forever. `EnsureFresh` relies on exactly this distinction to decide which errors are refreshable.
- **`ShouldRefreshOnStatus` treats 403 the same as 401**: several providers answer an expired token
  with 403. Treating 403 as "insufficient permission, don't refresh" would leave those servers
  permanently broken while still showing a Ready badge.
- **Scopes are sent verbatim, and `offline_access` is never added unilaterally.** Adding it looks like
  a convenience but is actually an escalation of consent scope: on some providers it turns a
  session-level grant into a long-lived one, and on others it makes the whole authorization fail
  outright. An operator-supplied set also wins outright over the discovered one and is never merged
  with it — `--scopes` is usually used to *narrow* a provider's default.
- **Registration hardcodes `token_endpoint_auth_method: "none"`** and never negotiates it from
  metadata: agenthub is a public client running on the user's machine, any "client secret" it holds
  is readable by anyone who can read the vault, and what actually protects the code exchange is PKCE.
- **The order in loopback mode cannot be rearranged**: bind → build (register) → **Serve** → open
  browser → wait, with a fresh random port every attempt
  ([flows.md §5](../flows.md#5-headless-oauth-and-refresh) says why). What is local to this package:
  only providers requiring an exactly pre-registered redirect_uri use `State.CallbackPort` +
  `ListenOnPort` to reuse a port, and when that port is taken the caller should discard the DCR
  credentials and re-register rather than silently switching ports. `Wait` always shuts down the
  server and releases the port before returning — a listener outliving its flow is the stale-
  interceptor bug random ports exist to prevent.
- **Callback acceptance rules**: a request with `error` fails with the AS's own error code; one with
  `code` and a matching `state` succeeds; one with `code` but a missing or mismatched `state`
  **fails loudly** (`ErrStateMismatch`) — under random ports there's no benign explanation; everything
  else (favicons, probes, a bare `GET /`) answers 204 and is ignored without ending the flow. The
  callback page is static, scriptless and echoes nothing from the query string, so it can't become
  reflected XSS or a token display surface.
- **`ParseManualCallback`'s state rules branch on input shape**: any input containing a query string
  **must** carry a state and it must match, with a missing state treated as a mismatch (every AS
  echoes state back when it receives one, so its absence means this isn't this flow's callback); a
  bare code can't be validated and is still accepted, because a user pasting one has usually trimmed
  the URL themselves and PKCE still stands — an intercepted code is useless without the verifier that
  never left this process. Manual mode's redirect_uri points at the **user's** loopback, which a
  headless host never binds.
- **Device flow loop rules**: `authorization_pending` keeps polling at the current interval;
  `slow_down` **permanently** increases the interval by `SlowDownIncrement` (5s, capped at 60s) rather
  than delaying once; `access_denied`/`expired_token` terminate; and any other error, transport errors
  included, terminates rather than retrying — a polling loop that swallows transport errors turns a
  network outage into a silent 15-minute hang. The device code's own expiry caps the whole loop
  independently of the interval, so a hostile `interval` can't extend it.
- **Abort conditions in the discovery chain**: a candidate returning non-2xx or unparseable JSON moves
  on to the next (providers 404 the forms they don't implement); but a candidate blocked by the SSRF
  screen or served over non-HTTPS **aborts the entire chain immediately** — a security decision, not a
  "try the next one" condition, since continuing would only probe more private URLs. A cancelled or
  expired context aborts too: nobody is waiting. A document that parses but lacks `token_endpoint`, or
  lacks both authorization endpoints, errors outright rather than silently trying the next candidate —
  that's a broken provider and the operator needs to see it. (The exception is the off-spec
  resource-origin hop, where an unusable document is an ordinary miss; see [oauth.md](oauth.md).) The
  `resource_metadata` in `WWW-Authenticate` is a **hint, not an instruction**: it comes from an
  unauthenticated 401, so it goes through `checkURL` all the same, and when it can't be fetched we
  still fall back to the candidates derived from the resource URL.
- **OAuth uses its own slow backoff ladder, `SlowBackoffLadder`** (5min/15min/1h/4h/24h): an OAuth
  failure during connection is waiting on a **human**, and ordinary exponential backoff retrying every
  few seconds would just keep popping browser windows or hammering the provider's authorization
  endpoint. `RetryBackoff` puts the first `FastRetries` failures on the flat `RefreshRetryBackoff`
  first, and lives beside the ladder because both proactive refreshers use it — a schedule
  reimplemented on each side is a schedule that drifts.
- **Every error is a sentinel or a `*FlowError` that unwraps to one**, so callers classify with
  `errors.Is` and never by string matching. `FlowError.Suggestion` is operator-facing and must never
  carry a secret.
- **Dependency budget**: standard library + `internal/secrets` + `internal/guard/netguard` +
  `internal/platform` (the file lock, and nothing else). It imports
  no control plane, no pipeline, and no logging package — it returns a structured `*FlowError` and
  lets the caller decide how to render it.
- The file lock is real on darwin/linux (`flock(2)`) and on Windows (`LockFileEx`), both through
  `internal/platform`; anywhere else it is an `errors.ErrUnsupported` stub, so **the offline refresh
  path would rather refuse to run than run unordered**: two processes racing for one single-use
  refresh token is worse than one "unsupported" refresh failure.
