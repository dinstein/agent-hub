# Platform, logging, profiling

> **Answers** where files go on this machine, how a record is written and scrubbed, and how the profiling endpoint is exposed.
> **Not here** what is written into those files → [records.md](records.md); the on-disk tree itself → [architecture.md#on-disk](../architecture.md#on-disk).
> **Kept true by** `internal/platform`'s cross-platform tests (they resolve Windows paths from macOS) and `TestMandatoryLogFieldsUseTheirConstants`.

Three packages with no business dependencies. `platform` resolves directories; `logx` writes records
into them; `diag` exposes the runtime's own profiles. `platform` and `logx` are locked to the standard
library by depguard, each with a failing case in `internal/depguardtest`.

## internal/platform

One place answers "which path on this machine holds agenthub's data, registry, logs, cache, state, run
directory and control endpoint", and one seam absorbs every Windows oddity — MSIX container
redirection, named pipes, SDDL.

| Function | Resolution order |
|---|---|
| `DataDir` | `AGENTHUB_DATA_DIR` (any platform, non-empty wins) → darwin `~/Library/Application Support/AgentHub` → linux `${XDG_DATA_HOME}/AgentHub` when absolute, else `~/.local/share/AgentHub` → windows `%APPDATA%\AgentHub` plus the MSIX escape → anything else `ErrUnsupportedPlatform` |
| `RegistryDir` | `AGENTHUB_REGISTRY` → `<data>/registry` |
| `LogsDir` / `CacheDir` / `StateDir` | `<data>/logs`, `<data>/cache`, `<data>/state` |
| `RunDir` | linux `${XDG_RUNTIME_DIR}/AgentHub` when absolute **and `AGENTHUB_DATA_DIR` is unset**, else `<data>/run`; darwin and windows always `<data>/run` |
| `CtlSocketPath` | `AGENTHUB_SOCKET` → the Windows pipe `\\.\pipe\agenthub-ctl-<sha8(SID)>` → `<run>/ctl.sock` |

`Resolver` makes the OS, environment, home directory and three Windows hooks (`PackageIdentity`,
`ProbePath`, `UserSID`) injectable, which is how a test resolves a complete Windows path on macOS. Its
zero value equals `Default()`.

### Invariants

**The directory name `AgentHub` and the `AGENTHUB_*` variable names are ABI.** Users' configuration and
other clients' launch scripts hardcode them ([canonical.md §1](../canonical.md#1-frozen-identifiers-abi-unchangeable-as-of-v1)).

**An explicit override always wins.** `AGENTHUB_DATA_DIR` is taken verbatim on every platform,
including inside an MSIX container: "the user named a path" needs no platform knowledge.

**Move the data directory and the socket moves with it.** `XDG_RUNTIME_DIR` is per-user, so pinning the
run directory under it would make every agenthub on the machine share one `ctl.sock` whatever data
directory each was pointed at — a dev build, an installed release and two sandboxed test runs all
binding the same endpoint, with everyone but the winner talking to a daemon and a registry that are not
theirs. `RunDir` takes that branch only while the data directory is at its platform default, which
makes the rule a property of the environment rather than of the binary: a release agenthub spawned by a
dev build computes the same run directory as its parent.

**An unsupported platform is a hard failure.** Anything outside darwin/linux/windows returns
`ErrUnsupportedPlatform`, tested with `errors.Is`, never string matching.

**`ProcessAlive(pid)` answers with two booleans, `alive` and `known`.** The question has three answers —
yes, no, and *this call may not look* — and folding the third into either of the others turns a probe
that merely lacked permission into a process that exited. `daemon stop` needs the unanswerable case to
read as "not mine to signal"; the daemon's owner watch needs it to read as "still alive".

**`EnsureDir` creates with `MkdirAll(0700)` and chmods an existing leaf back down**, because the run and
state directories hold sockets and credentials. On Windows it does neither, and returning early is the
honest answer: Go's permission bits do not map onto ACLs there, so a chmod would report success while
restricting nothing. What protects those directories on Windows is `%APPDATA%` being per-user and, for
the control endpoint, the pipe SDDL. An explicit owner-only ACL is owed — [windows.md](../windows.md).

### The Windows branch

**MSIX detection fails toward "assume packaged".** A gateway spawned by an MSIX-packaged client
inherits that client's app container, after which every write to `%APPDATA%` is redirected into the
package's private shadow directory and the user's configuration forks per client. Only
`APPMODEL_ERROR_NO_PACKAGE` (15700) from `GetCurrentPackageFamilyName` means "no package identity";
every other outcome is treated as packaged. Guessing "not packaged" inside a container is a silent data
fork; guessing "packaged" outside one costs one extra probe that succeeds anyway.

**The escape path is probed before it is adopted, and a failed probe is loud.** The redirection filter
keys on local paths, so the loopback-UNC twin (`\\127.0.0.1\C$\Users\...`) reaches the real directory —
but administrative shares can be disabled, so `defaultProbePath` `Stat`s it first, walking up to eight
parents because on first run the directory does not exist yet and what is being tested is the route. A
failed probe falls back to the local path and warns on **stderr**, never stdout: a stdio gateway's
stdout carries JSON-RPC frames.

**Windows paths are joined with explicit backslashes** (`winJoin`), not `filepath.Join`, because the
cross-platform tests resolve Windows paths from macOS. A path spelling that varies with the host is not
a path spelling.

**The control endpoint is not a file.** `CtlSocketPath` returns a pipe name, and callers must check
`IsPipePath` before creating a parent directory or changing permissions. The `sha8(SID)` is not
obfuscation: pipe names live in a machine-global namespace, so two users would contend and the loser
would connect to the winner's daemon. Access control is `D:P(A;;GA;;;<SID>)` from `CtlPipeSDDL` — owner
only, not Administrators, not SYSTEM — stricter than Windows convention, because the control plane
hands out every downstream credential.

**Nothing in this branch has run on real hardware.** It cross-compiles and is unit tested through the
injected hooks. Treat a mismatch as an expected unknown, not a regression.

## internal/logx

Repo-wide `log/slog` setup: a stderr text handler and a file JSON handler, with field conventions and
redaction that cannot be bypassed.

When both handlers exist, an internal `multiHandler` fans out and joins their errors — one sink failing
never silences the other. `Setup` wraps `ScrubHandler` outermost, so one redaction pass covers every
sink and every `WithAttrs`-bound attribute.

### Two sinks, two levels

The sinks have different owners. The JSON sink is a file this project names, rotates and prunes; the
text sink is stderr, and **a stdio gateway's stderr belongs to the MCP client that spawned it**. One
shared level meant buying a diagnosis by filling someone else's log with our prose.

| Setting | Reaches |
|---|---|
| the `info` default | both |
| `Config.Level` | both |
| `Config.TextLevel` / `Config.JSONLevel` | one each |
| `AGENTHUB_LOG_LEVEL` | both |
| `AGENTHUB_LOG_FILE_LEVEL` | the JSON file |
| `Config.Debug` / `AGENTHUB_DEBUG=1` | both, at `debug` |

Widest last: `Config` is the assembly's standing choice, the variables are the operator's, made in
front of a problem. The case this exists for is `AGENTHUB_LOG_LEVEL=warn AGENTHUB_LOG_FILE_LEVEL=debug`
— full detail on disk, a quiet stderr. All three are `AGENTHUB_*` and are stripped before a downstream
server is spawned: raising our own verbosity never reaches into someone else's process.

**An unreadable level is reported and ignored, never fatal.** Logging is not the operation the process
exists to perform. But it is reported, at `warn`, through the logger it failed to configure: a level
that did not apply and one that did are indistinguishable from inside.

### The field convention

Any record touching a downstream server, a derived instance, a tool call, a client, a session, a
registry generation or the writing process uses the constants in `fields.go` — `server`, `tool`,
`client`, `session`, `rev`, `pid`, `inst`. That is what makes the gateway's, daemon's and CLI's streams
joinable, and the list is complete.

**Do not invent synonyms.** Three ways that goes wrong:

- **A synonym splits a join.** One key spelled two ways means joining two streams requires knowing both
  names.
- **A key bound on the logger must never be passed again per record.** `slog`'s JSON handler does not
  deduplicate, so a record repeating a bound key emits the field twice on one line, and a reader taking
  the last — `encoding/json` included — reads the second value. The peer's self-reported name is
  therefore `client_name`, not `client`. A regression test greps the serialized line, because an
  assertion on a decoded record cannot see the bug at all: the decode is what discards the duplicate.
- **A field nothing sets is a convention that has already failed.** Where an assembly genuinely has
  none — a stdio gateway serves one terminal pipe and is keyed by client and pid — the constant says
  so, so the absence reads as the answer rather than an omission.

`TestMandatoryLogFieldsUseTheirConstants` walks every production file's AST and fails on a mandatory
key spelled as a string literal inside a `slog` call. Being an AST walk and not a grep is what makes it
usable: a cobra flag named `--client` and a table column named `server` are not log records. A key
assembled at runtime stays a review question.

**`pid` is mandatory on every gateway record and is attached at logger construction.** The log file is
named after the CLIENT — every `agenthub connect --client claude-code` appends to
`gateway-claude-code.log`, and a user normally has several running — so without it the interleaved
lines of two gateways read as one gateway doing impossible things.

### Redaction

**It cannot be turned off.** No switch, no environment variable. Every knob this package reads moves a
level filter and nothing else, and `ScrubString` reads no environment at all. A record that becomes
visible was always going to be scrubbed, so lowering a level can never be the thing that leaks.

**It fails closed.** Over-masking a harmless long random string is acceptable; leaking one credential is
not. Five pattern classes apply in order:

1. a sensitive key whose value opens with a known auth **scheme**, consumed to end of line;
2. the same key with a scheme-less value, kept whitespace-bounded;
3. bare bearer tokens loose in the body;
4. known credential shapes (`sk-`, `ghp_`/`gho_`/`github_pat_`, `xox[baprs]-`, `AKIA`, `ya29.`, JWTs);
5. generic `key=value` pairs whose value looks random — 32+ base64-ish characters, requiring both
   letters and digits so a long all-letter identifier is spared.

**Do not narrow these to make logs prettier.** The scheme list in class 1 is closed on purpose:
recognising only `Bearer` consumed the scheme NAME of every other RFC 7235 scheme and left the
credential in place, so `Authorization: Basic dXNlcjpwYXNz` logged as `[REDACTED] dXNlcjpwYXNz` — a
line that reads as though the secret had been removed, which is worse than an obvious leak. Matching
any leading word instead would let `SECRET_GITHUB=<value> loaded` swallow the rest of the message.

**Sensitive key names are masked wholesale, whatever the value type.** `SensitiveKey` lowercases,
strips `-` and `_`, and substring-matches `secret`, `token`, `password`, `passwd`, `authorization`,
`apikey`, `credential`, `accesskey`, `bearer`.

**`LogValuer` is resolved before redaction**, so redaction sees the final value; groups recurse, and
`string` and `error` inside a `KindAny` are scrubbed too — errors frequently wrap header dumps.
`WithAttrs` redacts eagerly, so bound attributes are clean whatever record they attach to.

**`ScrubString` never matches a quoted JSON key**, and that is not a leak today: in
`"authorization":"Basic …"` the key's closing quote sits between name and separator, so no pattern
fires. Structured attrs are `SensitiveKey`'s job. It becomes a gap the day a JSON document is
interpolated into a message string.

**This package does not open the log file.** It is standard-library-only, while a file several
processes append to needs the discipline `internal/jsonl` owns. `Config.JSON` takes an `io.Writer`, the
assembly opens `jsonl.NewLineWriter` and hands the sink over, and the assembly closes it.

## internal/diag

The Go runtime's own profiles — heap, goroutines, allocations, mutex, CPU — over a loopback-only HTTP
endpoint that does not exist unless asked for. `AGENTHUB_PPROF_ADDR` names the address; unset creates
no listener and logs nothing. Both long-lived processes assemble it: the gateway after its logger
exists, the daemon before the slow part of its startup, so a daemon that never finishes starting is
still one that can be profiled.

- **Loopback or nothing.** A non-loopback address is refused, never downgraded. These profiles *are*
  the process memory: a heap dump holds downstream credentials, tool arguments and results, and a
  goroutine dump names every downstream. The predicate is `netguard.AddrIsLoopback`, shared with the
  packages that bind, and it fails to false.
- **The bind address is proven once; every request is checked again.** Loopback binding stops the
  network, not a local browser under DNS rebinding: a page served from `evil.example:PORT` whose name
  is rebound to `127.0.0.1` is same-origin to the browser. `requestGuard` refuses any request carrying
  an `Origin` header at all — there is no browser client here to whitelist — and any request whose
  `Host` cannot itself be proven loopback.
- **A refused or unavailable address fails the process start.** An operator who asked for profiles and
  silently did not get them attaches to a port that never answers and reads a healthy process as
  wedged.
- **Port 0 is the intended spelling**, because one gateway runs per client and a fixed port in a shell
  profile breaks every start after the first. `Server.Addr` reads the listener rather than echoing the
  request, and the port is announced through the process logger.
- **A private mux, never `http.DefaultServeMux`.** Importing `net/http/pprof` registers handlers on the
  default mux as a side effect, and a package that decides its own exposure must not depend on which
  mux some other server in the process uses.
- **No `WriteTimeout`.** A CPU profile holds its request open for its whole duration.
  `ReadHeaderTimeout` still bounds a client that connects and says nothing.

## Two things that are easy to confuse

**There are two stderr tail windows, at two layers, with different sizes.** `mcp/transport` keeps a
4 KiB byte tail; `internal/downstream` has its own line-based ring. Finding one does not mean you have
changed the other.

**Windows cross-process locking is implemented and unverified.** Both halves live here —
`LockFileEx`/`UnlockFileEx` on Windows, `flock(2)` on darwin and linux — and each locking package owns
a `flock.go` tagged `darwin || linux || windows` that delegates, with `flock_stub.go` failing closed
elsewhere. `internal/ratelimit`, `internal/calllog` and `internal/secrets` set
`crossProcessLockSupported = true` there.
