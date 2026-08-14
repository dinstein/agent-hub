# The two guards

> **Answers** which command shapes are refused before a process starts, and which destinations are refused before a connection is made.
> **Not here** who is allowed to call what → [../model.md](../model.md); the docker isolation itself → [protocol.md](protocol.md).
> **Kept true by** `internal/guard/spawnguard`'s bypass regression tests and `internal/depguardtest` (these packages carry zero business dependencies).

Two questions, decided from the request itself before anything is contacted, and refused regardless of
who asked. They are not permissions: no configuration grants an exemption.

Nothing here inspects **what a downstream returned**. Result scanning for prompt injection and leaked
credentials, tool-definition drift grading and a human approval queue all existed and were removed —
what a client may reach is decided in advance by configuration, and a stage that reads a result in order
to relabel or withhold it is the opposite of that model.

**Every predicate spells out its failure direction in its doc comment; treat it as part of the
signature.**

| Direction | Meaning | Example |
|---|---|---|
| fail-open | cannot decide → let it through | spawnguard's shape checks |
| fail-closed | cannot decide → refuse | `netguard.HostIsPrivate` |
| fail-to-false | cannot decide → do not grant trust | `netguard.HostIsDefinitelyPrivate` |

The distinction is not style. The same "I am not sure" has to produce opposite answers depending on
whether it is used to refuse or to grant trust, and netguard's paired predicates encode that into the
shape of the API, outside the type system.

`internal/guard` itself exports one thing: `ErrBlocked`. Every typed rejection in the subpackages
unwraps to it, so `errors.Is(err, guard.ErrBlocked)` holds everywhere without importing every
subpackage.

## The escalation path this architecture is shaped like

MCP's own security guidance names an attack this design matches exactly: a local proxy that spawns MCP
servers as child processes and authenticates its clients with a token. The chain is XSS in the client →
steal the proxy token → call the proxy → **proxy spawns an arbitrary command** → RCE.

It breaks at the fourth step, structurally rather than by a check. **Nothing a client presents can
choose a command.** The set of servers is decided in advance by configuration, and the only surface that
can add one — `internal/ctlapi` — is not on the HTTP listener at all: the daemon mounts `httpbridge`
with the MCP data plane as its only dispatcher, while the control API listens on a unix socket (0700
directory, 0600 socket, same-uid peer verification) or a Windows named pipe whose SDDL admits the
current user and nobody else. It issues no tokens, because OS-level identity is the credential. A stolen
bearer therefore buys tool calls scoped by that token's profile and allowlist, and no way to name a new
process.

**Do not mount a control route on the HTTP listener.** That single change would reconnect the chain, and
nothing else in this layer would notice: `spawnguard` grades a command's shape, not who asked for it.

## internal/guard/spawnguard

Scan the command line and environment before starting a downstream process, and block the smuggling
shapes that disguise arbitrary code execution as an ordinary server entry point.

**Anti-smuggling, not a sandbox.** Accept the framing before reading the code: this is pattern matching
on command lines, not an execution boundary. A server the user configured themselves, that was always
going to run code, still runs; `npx`, `uvx` and `docker run` with ordinary project mounts pass through
untouched. What it blocks is the shape "an innocuous-looking entry point with `sh -c`, `LD_PRELOAD` or
`--privileged` hiding inside it". The real isolation story is `runtime: docker`, which fails closed
rather than degrading to host execution.

**Every spawn is screened, and the default is the guard rather than nil.** `downstream.Deps.Spawn`
resolves to the shared `Guard.Check` unless an assembly overrides it or sets `SpawnUnscreened`, and
`dialStdio` hands that to `transport.StdioConfig.Screen`, which both spawn paths — plain stdio and the
rewritten `docker run` line — consult on the **final** host command, after secret expansion and after
any docker rewriting. The default is the guard and the opt-out is an explicit field because a screen
each assembly must remember to attach is one that will be missing from the next assembly.

### Rules

- **The check order is fixed**: environment variables (deterministic, always applicable) → denylist →
  allowlist → wrapper unwrapping → inline eval → container escape.
- **The allowlist bypasses shape checks only, never the env check.** Dangerous environment variables
  subvert precisely that trusted binary, so putting `LD_PRELOAD` on an allowlisted command is the most
  valuable attack, not the safest one.
- **Deterministic checks always block; shape checks fail open.** A denylist hit or a dangerous env name
  is always rejected, whereas a command line `Check` cannot parse is allowed through — turning every
  uncommon but legitimate launcher into a failure costs more than a missed detection.
- **An empty env value is treated as inert** (an explicit unset) and allowed; `AllowEnv` matches exactly
  and takes precedence over both the built-in table and the prefix table.
- **The env rejection names its variable and deliberately cannot say where it came from.** `Check`
  receives one flat `[]string`, while `downstream.dialStdio` built that slice by laying the entry's
  `env` block over the agenthub process's own environment, so only the caller can tell the two apart. It
  does, in `explainBlockedEnv`, and the distinction is the entire fix: a declared variable is edited out
  of the registry entry, an inherited one appears in no AgentHub file at all and has to be unset where
  the process was started.

### Wrappers

Unwrapping goes at most four levels deep; beyond that the remaining outer wrapper is passed through the
later checks as-is. The supported wrappers are `env`, `busybox`, `nohup`, `setsid`, `nice`, `stdbuf`,
`timeout`, `sudo`, `doas`.

**Every wrapper registers two tables: flags that take a value and flags that do not; a flag in neither
table causes unwrapping to be refused outright.** Registering only value-taking flags is not enough —
miss one and its value is taken for the command, and **the real command is never checked at all**:

```
sudo --prompt x sh -c 'evil'     was once allowed
timeout -d x 10 sh -c 'evil'     was once allowed
stdbuf --input L sh -c 'evil'    was once allowed
```

The handling is deliberately different from `docker run`: the option sets of coreutils and sudo are
**closed and documented**, so both tables can be complete and whatever is not listed is a shape this
build does not understand. We do not assume an unknown flag takes a value the way the container side
does, because here **guessing wrong in either direction moves where the command is** — there is no safe
default. The cost of forgetting to register a flag is a loud rejection an operator can see and fix.

**`env -S` is rejected outright rather than guessed at.** Split-string re-splits an opaque string into a
command line, which is the very reason this package exists. `NAME=VALUE` assignments inside `env` go
through the same check as a direct env slice. `env`'s own options get the same two tables and anything
unrecognized is rejected: GNU env and BSD env have different option sets, so no single table can be
complete for both.

### Inline eval

**Scanning stops at the first operand.** Flags after the script path belong to the script, and blocking
them would be a false positive. `python -m` likewise terminates the scan. In the node family, preload
flags like `-r`, `--require`, `--import` and `--experimental-loader` count as inline eval, because they
load arbitrary modules before the script.

**Every interpreter family has a table of "the value is the next argument" flags**, for the same reason
as the container case: a flag's value does not look like a flag either, so without skipping it the scan
stops there treating it as an operand and **the eval flag that follows is never seen**:

```
bash --rcfile /tmp/x -c 'evil'     perl -I /tmp -e 'evil'
node --title svc -e 'evil'         php  -d k=v  -r 'evil'
```

The value-taking flags of shell, perl, ruby, php and python are a closed set and are listed in full.
**node is not** — it adds options every release, so `node --somenewflag value -e …` remains a residual
possibility. Sealing it needs the scan to continue past the operand, at the cost of false positives on
scripts carrying their own `-e`/`-c`: a policy change, not a bug fix.

### The container check

It covers only `run`, `create` and `exec`, including the second-level subcommand in
`docker container run`, and stops at the first operand — the image name.

**Docker's global flags need two tables as well**, for a harder reason than the run flags: misreading a
global flag shifts the position of the **subcommand**, and when the subcommand is not one of the three
the entire container check is skipped. `docker --tlscacert /tmp/ca run -v /:/host img` went completely
unchecked for exactly this reason.

**On the container side the value table is inverted: `containerBoolFlags` lists the flags that do NOT
take a value.** `docker run` has hundreds of flags and gains more every release, so a positive list of
value-taking flags can never be complete, and **a single omission is a silent container escape** — the
omitted flag's value is taken for the image name, the scan stops there, and none of the policy-bearing
flags after it are judged:

```
docker run --sysctl net.ipv4.ip_forward=1 -v /:/host img   was once allowed
docker run --storage-opt size=1G --privileged img          was once allowed
```

With the list inverted, an unrecognized flag is assumed to take a value: skip one argument and keep
scanning, so the `-v` further along is still judged. Guessing wrong now costs at worst one false
positive, which is loud and fixable.

**Bind mounts are blocked for whole trees, matched on the cleaned source.** `-v`/`--volume` and
`--mount`'s `source=`/`src=` are both judged. Denied: the roots `/`, `/etc`, `/root`, `/boot`, `/dev`,
`/var`, `/usr`, `/home` **matched exactly**, anything at or under `/proc` or `/sys`, and any path ending
in `docker.sock`, `podman.sock` or `containerd.sock`. A subdirectory such as `/etc/myapp` is allowed —
the target is whole-tree exposure, not an enumeration of every possible secret.

**`--privileged` is judged by its value, not by its spelling, and the failure direction is ON.** Docker
parses the attached form with `ParseBool`, so `--privileged=true`, `=1` and `=T` all enable it; only an
explicit parseable false is treated as off, and a value neither docker nor this build can parse
(`--privileged=yes`) is not evidence that isolation is intact. The `=false` branch is also the one place
in the flag loop that continues **without** consuming a value — the shape `containerBoolFlags` was
inverted to prevent — and `TestPrivilegedFalseDoesNotBlindTheScan` is the regression for it.

**`--cap-add` and `--security-opt` are matched by meaning, not by spelling.** `--cap-add` folds case
**before** stripping `CAP_`, the order docker normalizes in; the other way round let `cap_sys_admin`
through while refusing `CAP_SYS_ADMIN`, for a capability docker grants identically. `--security-opt`
normalizes the separator before matching, because moby still accepts the legacy `:` form and reads a
bare `disable` as SELinux label-disable — so `seccomp:unconfined`, `apparmor:unconfined` and `disable`
each switched off a confinement layer this guard's own error message claims to protect. Adding
confinement or naming a profile still passes: a guard that refused those would push operators away from
using them.

**`--device` is a deny list of block and memory devices**, matched on the cleaned host source. Denied:
the `/dev` tree, `/dev/mem`, `/dev/kmem`, `/dev/kcore`, `/dev/port`, and the block-device families
(`sd`, `hd`, `vd`, `xvd`, `nvme`, `dm-`, `md`, `loop`, plus the `mapper/` and `disk/` symlink trees).

**This is the deliberate exception to "never a deny list", and the exception is what the rule is about.**
That rule governs *tool selectors*, where the question is "what may this client reach" and an unknown
answer must be no. Here the layer's own direction is the opposite — deterministic checks block, shape
checks fail open — because spawnguard judges a command line the **operator wrote**. An allow list of
device nodes cannot be completed: `/dev/dri`, `/dev/kvm`, `/dev/fuse`, `/dev/net/tun`, `/dev/snd`,
`/dev/ttyUSB0` are all ordinary requests from a real server. The denied set is the opposite shape —
enumerable, stable across machines, and with no use inside an MCP server. The cost of a false positive
also changed when the guard moved onto the spawn path: a refused device is now a server that dies at
connect, not a configuration rejected at `server add`.

**The runtime never generates `--privileged` or `--device` itself** — both can only arrive through an
operator's `extraArgs` — so neither rule can invalidate a configuration AgentHub produced.

## internal/guard/netguard

Answer "is this destination private", screen again at dial time against the IP actually resolved, and
hold the repository's one answer to "is this authority loopback".

### Two predicates, opposite failure directions

This is the one design point to remember: "is this host private?" is two questions with opposite failure
directions depending on the use, so there are two exported predicates and they must never be substituted
for each other.

| Predicate | For | Direction | Behaviour |
|---|---|---|---|
| `HostIsPrivate` | **refusing** — an OAuth redirect target, a remote server URL | fail-closed | an empty host, a DNS failure or timeout, and an empty answer all return true; if any record a hostname resolves to is private it returns true, because one attacker-controlled A record has to be enough |
| `HostIsDefinitelyPrivate` | **granting trust** — treating a target as local | fail-to-false | only a literal IP or a localhost name returns true, and it **never resolves DNS**: a DNS answer is a claim the zone owner can change at any time, so it can negate trust but must never grant it |

`HostIsDefinitelyPrivate`'s scope is also narrower than `AddrIsPrivate`: only loopback, RFC1918/ULA,
link-local unicast and unspecified count. CGNAT, the documentation ranges and the benchmark range are
not routable, but they are not "locally private" either and should not unlock local trust.

`AddrIsPrivate` is the address classifier both share. It returns true for an invalid zero-value `Addr`
and calls `Unmap()` before deciding, so `::ffff:10.0.0.1` is classified as `10.0.0.1`. Beyond the
standard library's classifiers it adds a prefix table: `0.0.0.0/8`, CGNAT, the IETF protocol
assignments, the three TEST-NETs, benchmark `198.18.0.0/15`, `240.0.0.0/4`, the v6 discard and
documentation ranges, **deprecated IPv6 site-local `fec0::/10`**, and the four v4-embedding prefixes.

Two shapes in that table are the ones to watch:

- `netip.Addr.IsPrivate` covers the ULA range that *replaced* site-local and stops there, so `fec0::1`
  was not private at all. Because the hostname-time screen and `DialControl` both consult that one
  function, a missing prefix opens both doors at once — and a range being deprecated makes it *less*
  likely to be filtered elsewhere on the host, not more.
- `64:ff9b::/96` (NAT64 well-known), `64:ff9b:1::/48` (NAT64 local-use), `::/96` (IPv4-compatible) and
  `2002::/16` (6to4) are all ways of writing an IPv4 address as IPv6, so **judging them by their v6 form
  answers the wrong question**: `::127.0.0.1`, `2002:7f00:1::`, `64:ff9b::7f00:1` and
  `64:ff9b:1:7f00:1::` all mean 127.0.0.1, and none looks like loopback to `IsLoopback()`. We reject the
  whole range rather than extracting and reclassifying the embedded v4: the deprecated forms exist only
  to spell an IPv4 address, and a NAT64 translator's whole job is to carry an arbitrary v4 destination
  across the v6 side, so decoding and re-judging would reintroduce the thing this predicate exists to
  catch.

`AddrIsLoopback` is a **third question, not a third answer to that one**: the two above judge a
destination we are about to reach, this one judges an authority on *this* side — the address a listener
would bind, and the `Origin` a browser claims. Fail-to-false, so an empty host (which binds every
interface), a name other than `localhost`, and an unparsable address all read as not loopback. It has no
rebinding problem to defend against, because a bind address is not resolved by anybody else.

It lives here rather than in the package that binds because its callers sit on three different layers:
`httpbridge` binds the MCP face by it and screens `Origin` with it, `daemon` decides from it whether
exposing the endpoint needs explicit confirmation, and `diag` refuses to publish profiles anywhere else.
The lower ones must not import the higher to ask, and a second copy would eventually disagree with the
first — in the direction that publishes tool execution, or a heap dump holding credentials, to a LAN.

### DialControl is the real line

Install it on `net.Dialer.Control`. The address it sees is the **already-resolved** address the socket is
about to connect to, so whatever rebind window a hostname-level pre-check leaves open is closed here. It
is fail-closed: an address it cannot parse as an IP literal is refused rather than guessed at.

**Hostname pre-checks are necessarily insufficient** and must be paired with `DialControl`.
`oauthflow.Client` uses them exactly that way: `checkURL` screens the URL's host before the request, and
`dialControlFor` screens the actual IP at dial time.

**`HostIsDefinitelyPrivate` is wired, and both of its callers use it to REFUSE rather than to grant
trust** — the opposite of the role its own doc comment describes, deliberately.
`confops.ValidateOAuthHint` and `ValidateEndpoint` screen a URL at the moment the operator types it, and
there the fail-closed predicate would reject a laptop with no network and a name that only resolves
inside a VPN: honest edits refused for being unresolvable at edit time. Refusing with the fail-to-false
predicate is sound *only* because this is not the boundary — `internal/downstream` screens the name
before it connects and the resolved address at dial time, which is the pair DNS rebinding cannot walk
around. **Reach for it to refuse only when you can name the fail-closed check that runs after you.**

**One thing defeats this screen, and it is recorded rather than fixed: an environment proxy.** With
`HTTP_PROXY` set, the transports that carry downstream traffic hand `DialControl` the *proxy's* address
and the proxy then resolves and connects to the real destination. It is a self-inflicted precondition —
the operator sets the daemon's environment; no client or downstream can — which is why it is owed a
decision rather than an emergency patch. The shape of that decision is in
[protocol.md](protocol.md) and [downstream.md](downstream.md); this note exists so an audit starting from
the SSRF boundary does not conclude the screen is unconditional.

**"Private" here means uniformly "not publicly routable", not "RFC1918".** When changing that table,
think through the fact that it affects the refusal direction and the trust-granting direction at the
same time.
