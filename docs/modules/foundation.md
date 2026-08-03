# Foundation and protocol layer

Nine packages that everything depends on and that depend on nothing, answering six questions:
**where do files go** (`internal/platform`), **what was said and how is it recorded**
(`internal/logx`), **how does an append-only stream survive N processes writing it**
(`internal/jsonl`, plus the `internal/eventlog` state stream and the durable `internal/accesslog`
ledger), **who defines the words "read / write / destructive"** (`internal/tier`), **how do we talk
to downstreams** (`internal/mcp` and `internal/mcp/transport`), and **in what form does
configuration live on disk and get shared across processes** (`internal/registry`).

The collaboration is one-directional and flat: `platform` resolves directories, `registry` and
`logx` open files in them; `registry`'s `ServerEntry` describes a downstream, `mcp/transport` turns
that description into a live connection, and `mcp` supplies the grammar spoken over it.
`platform`, `logx` and `mcp` are locked by depguard to **`$gostd` only**; `registry` is the sole
exception (`fsnotify`, for watching), and each rule is backed by a failing case in
`internal/depguardtest`.

---

## internal/platform

Answer, in one place, "which path on this machine holds agenthub's data / registry / logs / cache /
state / run directory and control endpoint", and funnel every Windows-specific oddity (MSIX
container redirection, named pipes, SDDL) into the same seam.

`Resolver` makes the OS, environment, home directory and three Windows hooks (`PackageIdentity`,
`ProbePath`, `UserSID`) injectable, so a test can resolve a complete Windows path on macOS; its zero
value equals `Default()`, and code that needs testability should hold its own. `DataDir` is the only
function that branches on platform, everything else is a subdirectory of it, and only `RegistryDir`
and `CtlSocketPath` have an environment escape hatch of their own.

| Function | Resolution order |
|---|---|
| `DataDir` | `AGENTHUB_DATA_DIR` (any platform, non-empty wins) → darwin `~/Library/Application Support/AgentHub` → linux `${XDG_DATA_HOME}/AgentHub` (only when absolute), otherwise `~/.local/share/AgentHub` → windows `%APPDATA%\AgentHub` (plus the MSIX escape) → anything else `ErrUnsupportedPlatform` |
| `RegistryDir` | `AGENTHUB_REGISTRY` → `<data>/registry` |
| `LogsDir` / `CacheDir` / `StateDir` | `<data>/logs`, `<data>/cache`, `<data>/state` |
| `RunDir` | linux `${XDG_RUNTIME_DIR}/AgentHub` (only when absolute **and `AGENTHUB_DATA_DIR` is unset**), otherwise `<data>/run`; darwin/windows always `<data>/run` |
| `CtlSocketPath` | `AGENTHUB_SOCKET` → the Windows named pipe `\\.\pipe\agenthub-ctl-<sha8(SID)>` → `<run>/ctl.sock` |

`EnsureDir` / `EnsureDirs` create with `MkdirAll(0700)` and actively `chmod` an existing leaf back
down to 0700 — the run and state directories hold sockets and credentials.

`ProcessAlive(pid)` answers with **two** booleans, `alive` and `known`, and the second is why it
lives here: the question has three answers — yes, no, and *this call may not look* — and folding the
third into either of the others is how a probe that merely lacked permission comes to read as a
process that exited. `daemon stop` needs the unanswerable case to read as "not mine to signal"; the
daemon's owner watch needs it to read as "still alive, do not shut down".

### Invariants and failure directions

**Frozen identifiers.** The directory name `AgentHub` and the `AGENTHUB_*` variable names have been
ABI since v1: users' configuration and other clients' launch scripts hardcode them.

**An explicit override always wins.** `AGENTHUB_DATA_DIR` is taken verbatim on every platform,
including inside an MSIX container — "the user named a path" needs no platform knowledge.

**Move the data directory and the socket moves with it.** `XDG_RUNTIME_DIR` is a **per-user**
directory, so pinning the run directory under it makes every agenthub on the machine share one
`ctl.sock` whatever data directory each was pointed at — a dev build and an installed release, or
two sandboxed test runs, all bind the same endpoint and everyone but the winner talks to a daemon
and a registry that are not theirs. `RunDir` therefore takes that branch only while the data
directory is at its platform default (`dataDirRelocated`), which makes the rule a **property of the
environment, not of the binary**: a release agenthub spawned by a dev build (they share a PATH)
computes the same run directory as its parent, whereas keying on build channel would make them
diverge in exactly the case where one execs the other. `DevResolver` extends the channel split by
**answering the `AGENTHUB_DATA_DIR` lookup** rather than carrying a field — one determination, two
consumers. The Windows control pipe cannot inherit it (a pipe name derives from no directory), so
`DevResolver` sets an unexported `devChannel` flag for that one caller.

**An unsupported platform is a hard failure, not a guess.** Anything outside darwin/linux/windows
returns `ErrUnsupportedPlatform`, tested with `errors.Is`, never string matching.

**MSIX detection fails toward "assume packaged".** A gateway spawned by an MSIX-packaged client
inherits that client's app container, after which every write to `%APPDATA%` is silently redirected
into the package's private shadow directory and the user's configuration quietly forks per client.
Only `APPMODEL_ERROR_NO_PACKAGE`(15700) from `GetCurrentPackageFamilyName` means "no package
identity"; every other outcome is treated as packaged, because guessing "not packaged" inside a
container is a silent data fork while guessing "packaged" outside one costs one extra UNC probe that
succeeds anyway. The one exception is the export being absent (no app model before Windows 8).

**The escape path is probed before it is adopted, and a failed probe must be loud.** The redirection
filter keys on local paths, so the loopback-UNC twin (`\\127.0.0.1\C$\Users\...`) reaches the real
directory — but administrative shares can be disabled, so `defaultProbePath` `Stat`s it first,
walking up to 8 parents because on first run the directory does not exist yet and what is being
tested is the route. A failed probe **falls back to the local path and warns** — never silently —
and the warning goes to **stderr, not stdout**, because a stdio gateway's stdout carries JSON-RPC
frames. `defaultWarn` dedups by message.

**Windows paths are joined with explicit backslashes** (`winJoin`), not `filepath.Join`: the
cross-platform tests resolve Windows paths from macOS, where `filepath.Join` would use `/`. A path
spelling that varies with the host is not a path spelling.

**The control endpoint is not a file on Windows.** `CtlSocketPath` returns a pipe name; callers must
check `IsPipePath` before creating a parent directory or changing permissions. The `sha8(SID)` is
not obfuscation — pipe names live in a machine-global namespace, so two users would otherwise
contend and the loser would connect to the winner's daemon. Access control is the
`D:P(A;;GA;;;<SID>)` from `CtlPipeSDDL` — **owner only, not Administrators, not SYSTEM** — stricter
than Windows convention, because the control plane hands out every downstream credential.

**`EnsureDir` does not tighten permissions on Windows** (Go's 0700 and Windows ACLs are different
things; `os.Chmod` there only toggles the read-only attribute). `%APPDATA%` is already per-user and
the endpoint's protection is the pipe SDDL. Tightening the data directory's ACL explicitly is owed,
and tracked in `docs/windows.md`.

**The entire Windows branch has never run on real hardware.** It cross-compiles and is unit tested
through the injected hooks on macOS/Linux, but not one line has executed on Windows, let alone
inside an MSIX container. Treat a mismatch as an expected unknown, not a regression.

---

## internal/logx

Repo-wide logging setup on `log/slog` — a stderr text handler plus a file JSON handler — with field
conventions and secret redaction that **cannot be bypassed**.

When both handlers exist an internal `multiHandler` fans out and joins their errors with
`errors.Join`: **one sink failing never silences the other**. `Setup` wraps `ScrubHandler`
**outermost**, so one redaction pass covers every sink and every `WithAttrs`-bound attribute.

### The field convention

Any record touching a downstream server, a derived instance, a tool call, a client, a session, a
registry generation or the writing process must use the constants in `fields.go` — seven keys,
`server / tool / client / session / rev / pid / inst` — because that is what makes the gateway's,
daemon's and CLI's streams joinable. The constants are the whole list, and saying so is part of the
rule: an enumeration presenting itself as complete while it is not points the next writer at exactly
the field with no name reserved for it.
**Do not invent synonyms.** Three ways that has already gone wrong:

- **A synonym splits a join.** The derive key was spelled `derive_key` in four places and `inst` in
  the frame log, so joining the two streams meant knowing both names. Both are now `FieldInstance`.
- **A key bound on the logger must never be passed again per record.** `slog`'s JSON handler does
  not deduplicate, so a record repeating a bound key emits the field twice on one line and a reader
  taking the last — `encoding/json` included — reads the second value. That happened to `client`,
  which both handshake lines overwrote with the peer's self-reported name; the peer's name is now
  `client_name`. Testing it costs something: an assertion on a **decoded** record cannot see the bug
  at all, because the decode is what discards the duplicate — the regression test greps the
  serialized line.
- **A field nothing sets is a convention that has already failed.** `FieldSession` had no users
  while the two call sites holding a session id spelled `"session"` by hand. Where an assembly
  genuinely has none — a stdio gateway serves one terminal pipe and is keyed by client and pid
  instead — the constant now says so, so the absence reads as the answer rather than an omission.

**The convention is enforced, because writing it down twice was not enough.**
`TestMandatoryLogFieldsUseTheirConstants` (`test/buildrules`) walks every production file's AST and
fails on a mandatory key spelled as a string literal inside a `slog` call; it found eight, in five
files. Being an AST walk and not a grep is what makes it usable — a cobra flag named `--client` and
a table column named `server` are not log records. A key assembled at runtime or routed through a
caller's own helper stays a review question.

**`pid` is mandatory on every gateway record and is attached once at logger construction**, not per
call site. It exists because the log FILE is named after the CLIENT — every
`agenthub connect --client claude-code` appends to `gateway-claude-code.log`, and a user normally
has several running at once — so without it the interleaved lines of two gateways read as one
gateway doing impossible things. The per-server trace log (`internal/downstream`, `TraceFrame`)
carries it for the same reason one level down, alongside `inst`, because that file is named after
the SERVER.

### Invariants and failure directions

**Redaction cannot be turned off.** No configuration switch, no environment variable.
`AGENTHUB_DEBUG=1` (`EnvDebug`) only lowers the level; `ScrubString` reads no environment at all.

**Redaction fails closed.** Over-masking a harmless long random string is acceptable; leaking one
credential is not. Five pattern classes apply in order: a sensitive key whose value opens with a
known auth **scheme**, consumed to end of line; the same key with a scheme-less value, kept
whitespace-bounded; bare bearer tokens loose in the body; known credential shapes (`sk-`,
`ghp_`/`gho_`/`github_pat_`, `xox[baprs]-`, `AKIA`, `ya29.`, JWTs); and generic `key=value` pairs
whose value looks random (≥32 base64-ish characters, with `looksRandom` requiring both letters and
digits so a long all-letter identifier is spared). **Do not narrow these to make logs prettier.**

**The scheme half of that first class exists because recognising only `Bearer` produced the worst
possible output.** Every other RFC 7235 scheme had its NAME consumed and its credential left in
place, so `Authorization: Basic dXNlcjpwYXNz` logged as `Authorization: [REDACTED] dXNlcjpwYXNz` — a
line that reads as though the secret had been removed, which is worse than an obvious leak because
nobody goes looking; `Digest`, `Negotiate`, `NTLM` and `DPoP` all landed the same way and no later
class caught them. The scheme list is **closed** on purpose: matching any leading word would let
`SECRET_GITHUB=<value> loaded` swallow the rest of the message, and losing diagnostics is not the
same trade as harmless over-masking. Running to end of line once a scheme is present is what covers
`Digest`, whose credential spreads across comma-separated parameters.

**`ScrubString` never matches a quoted JSON key, and that is not a leak today.** In
`"authorization":"Basic …"` the key's own CLOSING quote sits between name and separator, so no
pattern fires. Structured attrs are `SensitiveKey`'s job and `ScrubString` works on message TEXT, so
the division of labour holds. Recorded because it looks like a gap every time someone reads these
patterns, and because it becomes one the day a JSON document is interpolated into a message string.

**Sensitive key names are masked wholesale, regardless of value type.** `SensitiveKey` lowercases,
strips `-`/`_`, and substring-matches
(`secret`/`token`/`password`/`passwd`/`authorization`/`apikey`/`credential`/`accesskey`/`bearer`);
a match is replaced with `[REDACTED]` whether it was a string, a number or a struct.

**`LogValuer` is resolved before redaction** (`scrubAttr` starts with `a.Value.Resolve()`), so
redaction sees the final value; groups recurse, and `string` and `error` inside a `KindAny` are
scrubbed too — errors frequently wrap request or header dumps.

**`WithAttrs` redacts eagerly**, so bound attributes are clean whatever record they later attach to.

**Log files are 0600, opened for append**, one JSON object per line.

---

## internal/jsonl

The append-only JSONL writer every on-disk stream goes through: the per-server wire trace
(`internal/downstream`) and the control-plane event stream (`internal/eventlog`).

It is the one primitive that survived `internal/audit`: the governance streams went and the write
discipline did not, because the discipline was never about audit — it is what any JSONL file written
by N processes at once needs.

### Invariants and failure directions

- **One record is one `write(2)` of one line, on a file opened `O_APPEND`.** That plus a line bound
  (`DefaultMaxLineBytes` = 4096 = PIPE_BUF) is the entire multi-writer story: N gateway processes
  and the daemon append to the same file and cannot tear each other's lines. `main_test.go` proves
  it by re-executing the test binary as several appending children; a single-process test cannot
  observe the property at all, because the in-process writer mutex hides its absence.
- **Rotation renames the active file; it never reads it back and truncates.** Truncation is what
  breaks the guarantee across processes that did not agree to rotate at the same instant.
- **Backpressure drops, and never blocks.** Appends funnel through one writer goroutine behind a
  buffered channel; overflow is counted (`Dropped`) and discarded. Fail-open, deliberately: a record
  on its way to disk must never slow down or fail the call that produced it.
- **An oversized line becomes an `OversizeMarker`, never a truncated one.** A reader can then tell
  "this record was too big" from "this file is corrupt"; half a JSON object says neither. The marker
  shares its `ts` field with a real record, so a reader that does not check `oversize` first decodes
  it into a zero value — a blank row asserting nothing happened, in place of the one record big
  enough to have been dropped. `DecodeOversize` is the check, and `server logs` runs it before
  decoding a frame.
- **A writer must fit the SERIALIZED line, not its raw payload.** Truncating a body to N raw bytes
  says nothing about the line: the body enters a JSON string where quotes and backslashes double and
  control bytes sextuple. `internal/downstream`'s trace log had both numbers set to 4096 and
  therefore dropped every frame over roughly 2 KB of quote-heavy JSON — a large `tools/list`, a big
  `tools/call` result, which is precisely what a trace is opened to see. It now marshals, measures
  and re-cuts until the line fits. Raising the bound instead would have traded dropped frames for
  torn ones.
- **Dependency budget**: standard library only.

---

## internal/eventlog

The control-plane event stream — one JSONL line per state change of a downstream server, a gateway
or the daemon, in a **closed vocabulary**.

**Everything it records was already being logged**, as slog records whose `msg` is prose — right for
a human reading `agenthub logs`, wrong for a UI timeline, a `--json` consumer or an alert, which
need values they may switch on. The two streams are the same facts in the two shapes their readers
need, and both call sites sit together so neither can change without the other being visible.

### Invariants and failure directions

- **Failure direction is OPEN, and that is the difference from `internal/accesslog`.** A record that
  cannot be written is dropped and counted; nothing is refused. `accesslog` is fail-closed because
  an unrecorded `tools/call` is a governance gap. A missed state change is not — the state itself is
  still observable — and refusing to serve a client because a note about it could not be filed would
  be worse than the gap it prevents.
- **A nil `*Stream` is usable and does nothing**, which makes "the switch is off" and "the file
  would not open" one code path at every call site rather than a nil check each can forget
  differently.
- **`PID` is stamped by the stream, never by the caller**, and carries no `omitempty`: N gateways
  plus the daemon share one file, and a record attributed to no process cannot be placed at all.
- **One file, three scopes.** Server, gateway and daemon events share `events.jsonl`. The question
  is a timeline — "the daemon restarted at 11:03 and six servers dropped two seconds later" — and
  splitting the file makes re-assembling it the reader's job.
- **`Detail` is fitted to the SERIALIZED line, not capped raw.** Same rule and reason as the trace
  log; what it prevents is a record replaced by an oversize marker, which reads back as a row
  claiming nothing happened.
- **The reader covers rotated segments.** `Read` walks `Segments(path)`, not just the active file.
  The retired savings projection opened only the active file, and the symptom was a report saying
  "nothing happened" rather than an error.
- **Retention runs on `Open`**, keeping the newest three segments. Rotation happens at 32 MiB of
  state-change records and is therefore rare, while gateway processes open this file constantly —
  one per `agenthub connect` — so the check is frequent in practice and costs one directory listing.
  A removal that fails is ignored: another process may have pruned it already, and a retention sweep
  able to fail an `Open` would turn "the disk is briefly busy" into "this gateway does not start".
- **Dependency budget**: standard library plus `internal/jsonl`.

### The closed vocabulary

Adding a kind means editing **three** places — the constant, `allKinds`, and this table — and then
**writing it somewhere**. `test/buildrules` fails until all four are true, and each direction hides
a different way of being wrong: a kind missing from the table is still written, `make ci` stays
green, and the consumer meant to recognize it silently does not; a kind nothing emits is still
offered as a `--kind` selector and answers "no events" — the same answer as "this has not happened",
which is the one confusion a closed set exists to prevent. Seven kinds shipped in that state before
the second check existed.

Kinds are checked as a **(scope, kind) pair**, never a bare kind: a gateway and the daemon both
`started`, and that spelling is meaningless at server scope.

<!-- event-kinds -->

| Scope | Kinds |
|---|---|
| `server` | `connected`, `connect_failed`, `disconnected`, `respawned`, `respawn_failed`, `circuit_open`, `circuit_half_open`, `circuit_closed`, `health_down`, `health_up`, `tools_changed`, `oauth_refresh_failed`, `secrets_missing` |
| `gateway` | `started`, `stopped`, `client_attached`, `registry_reload_failed` |
| `daemon` | `started`, `stopping`, `listener_bound`, `config_reloaded` |

### What `count` counts

A record carries one number and the **kind** decides what it is a number of. `CountNoun` holds the
whole mapping in code, because a meaning stated only in a doc comment is what let three writers
disagree: the field was called `attempt` while one writer put a respawn number in it, one a tool
count and one a failure streak, so a connect that listed thirteen tools rendered as a thirteenth try.

| Kinds | `count` is |
|---|---|
| `connected`, `tools_changed` | tools the server listed |
| `respawned`, `respawn_failed` | which respawn this was |
| `disconnected` | reconnects the connection had survived |
| `circuit_open`, `health_down`, `health_up` | consecutive failures behind the flip |

Every other kind leaves it zero, and `rev` is deliberately a separate field: a generation identifies
a revision rather than tallying anything. A renderer meeting a kind absent from this table prints
the bare number rather than hiding it — a frontend older than its daemon must not drop a value.

---

## internal/accesslog

A bounded, queryable metadata history for every `tools/call` lifecycle, plus the complete accepted
request bytes retained in encrypted payload packs.

Metadata and payload are deliberately separate. Every UTC day has one shared `access.jsonl`; each
gateway process owns payload packs named by a random boot id and pid. An event stays under 4096
bytes and is appended in one `O_APPEND` write, while a request may be as large as MCP's 16 MiB frame
bound without weakening that atomic-line contract. Payload entries are gzipped then sealed with
XChaCha20-Poly1305; metadata points at one by `(day, file, offset, length, key id)`. `NewCallID`
mints the cross-event join key — an upstream request id is not sufficient, because clients reuse it
across sessions.

### Invariants and failure directions

- **Complete means every byte of an accepted request.** `MaxPayloadBytes` equals the MCP facade's
  accepted frame bound; it is not a second, narrower truncation policy.
- **Metadata is bounded; payload is encrypted.** Full arguments never enter `slog`, a support
  bundle, or a shared JSONL line. An oversized metadata event is an error, never an oversize marker
  claiming the lifecycle was recorded.
- **Payload first, reference second.** Assemblies must make the pack entry durable before appending
  an event that names it. A crash may leave an orphan pack entry, which verification can reclaim; it
  must never leave a committed event pointing at bytes that were never written.
- **Each payload pack has one process writer.** Large writes therefore need no cross-process
  atomicity; only the bounded event stream is shared, and a cross-process acceptance test proves its
  lines arrive whole and in the expected count.
- **Storage pressure is a serialized decision.** When retention or capacity limits are configured,
  every process holds the same root lock across pruning, directory-size inspection, free-space
  inspection and the write itself. Complete UTC partitions older than the retention cutoff are the
  only automatic deletion targets. `maxBytes` is a hard ledger limit; `minFreeBytes` reserves room
  on the containing filesystem. Crossing either is an error, never an unbounded queue or a silent
  drop, and a multi-process test proves concurrent writers cannot multiply the cap. A platform with
  no cross-process lock refuses the bounded configuration outright rather than serving it unbounded.
- **Durability is explicit.** `sync` acknowledges only after the pack/event file has been synced;
  `write` acknowledges the kernel write. Neither mode queues without bound and neither silently
  drops on backpressure — intentionally unlike `internal/jsonl`, whose streams must never slow the
  data plane.
- **The package decides no permission.** An assembly may make a durable `received` record a
  prerequisite for execution, but that wrapper does not enter or reorder the frozen pipeline gates
  and it never inspects or modifies the arguments.
- **Integrity has a stated boundary.** Each metadata event carries HMAC-SHA256 and each payload
  entry is bound by AEAD to its call id, kind, sizes and codec, so `audit verify` detects edits,
  corruption and reference substitution. Independent records cannot prove an attacker deleted a
  whole day or the entire directory; deletion evidence needs an external immutable anchor, which
  this local-only design does not claim.
- **Key rotation never orphans retained history.** The current key id is public governance metadata;
  each 32-byte key is stored under an immutable key-id vault ref. Rotation writes the new ref before
  switching governance and retains old refs while retained data still needs them.

---

## internal/tier

The vocabulary of the three operation tiers `read | write | destructive` — the one ladder in the
repo. Standard library only.

It is a leaf on purpose — five packages need these three words (`pipeline` gates on them,
`httpbridge` stores one on an agent token, `ctlapi` mints those tokens, `discovery` names intent
variants after them, `cli` parses them from user input) and none should import another to say
"read". Why it is a leaf, and what its old home in `internal/pipeline` cost, is canonical.md §2.

### Invariants and failure directions

**`Covers(caller, tool)` decides by rank, not equality**: a write credential can call read tools,
and destructive can call anything.

**The empty string means "no tier privilege", not "the lowest tier".** stdio callers are the human's
own session and carry no agent token, so the tier gate has nothing to enforce against them. That is
a different thing from an *unrecognized* tier, which ranks 0 and **covers nothing** — fail-closed,
because a typo in a stored token should be refused, not escalated.

**The first and last rows of `ToolTier` are different cases; don't merge them.**

| annotations | Tier | Why |
|---|---|---|
| Absent entirely / null / unparseable | `destructive` | The server **said nothing at all**. Fail-closed: an unannotated tool must never be reachable by a read-only credential |
| `readOnlyHint == true` | `read` | |
| `destructiveHint == true` | `destructive` | |
| `destructiveHint == false` | `write` | |
| An annotations object exists, but neither hint is set | `write` | The server **did describe itself**, it just stayed silent on this one item |

**An annotated but silent tool is `write`, not `destructive`.** The MCP spec's default for a missing
`destructiveHint` is destructive, and this ladder deliberately departs from it when an annotations
object exists: `ToolTier` feeds coarse credential separation and intent variants, and treating every
annotated-but-silent tool as destructive would collapse the ladder into one rung. A **missing or
unparseable** annotations value is still destructive — the fail-closed case the tier gate relies on.

**Intent variants use equality, not coverage.** `call_tool_read` accepts only read tools, because a
variant expresses "what I intend to do" while a credential expresses "how far I'm permitted to go".

---

## internal/mcp

The **only** place in the repo that touches the MCP/JSON-RPC protocol implementation: wire format,
framing, domain types, version negotiation — all in-house, standard library only.

`.golangci.yml`'s `mcp-stdlib-only` rule restricts `internal/mcp/**` to `$gostd` plus itself, and
`no-third-party-mcp-libs` bans the known Go MCP SDKs **repo-wide**. The reason is not distrust: a
handful of invariants here need precise control (16 MiB bounded reads, `notifications/cancelled`
forwarding, inline replies to reverse RPC, the stdio stderr tail window), and the façade's point is
to keep the choice **reversible**, sealed inside one package. One direct consequence: the SSRF
screen cannot import `internal/guard/netguard` here, so the caller injects a dialer instead.

`MaxFrameSize = 16 << 20` is a hard cap on both read and write. Framing is newline-delimited JSON —
LSP-style `Content-Length` headers are deliberately unsupported.

**Two protocol generations coexist.** `Version2026` (`2026-07-28`) is stateless: no initialize
handshake, no `Mcp-Session-Id`, per-request `_meta` carrying the protocol version and client
capabilities, `server/discover` for negotiation, `subscriptions/listen` in place of the GET
notification stream, and MRTR (`InputRequiredResult` / `requestState` / `inputResponses`) in place
of in-band reverse RPC. `Version2025` (`2025-11-25`) is the stateful predecessor.
**`ProtocolVersion` stays at `Version2025` on purpose** — every context that reads it is
definitionally pre-2026 (the legacy path runs only after `server/discover` failed), and the
2026-07-28 declaration travels per-request instead. `SupportedVersions` is the accepted set, newest
first: `2026-07-28`, `2025-11-25`, `2025-06-18`, `2025-03-26`. `NegotiateVersion` validates the one
version a legacy `initialize` returned; `NegotiateHighest` picks from a `server/discover` list.
`RequestState` is echoed **verbatim** and never inspected — servers own its integrity.

### Invariants and failure directions

**Reads are bounded, and exceeding the bound poisons the reader.** `readLine` checks the cap while
accumulating, so an oversized frame fails before being fully buffered. Once `ErrFrameTooLarge` is
hit the stream position sits mid-frame and the semantics are undefined, so **the error is sticky**
and the connection must be considered unusable.

**Exceeding the bound on the write side is recoverable.** `WriteFrame` rejects an oversized frame
**before** writing any bytes, so the stream is not contaminated — which is why the transport layer
classifies an outbound over-limit as `ClassFatal` rather than `ClassUnavailable`.

**Frame writes are atomic.** One `Write` call per frame plus `FrameWriter`'s mutex keeps frames from
interleaving when the Call/Notify goroutines and the read loop share a writer. `json.Marshal`
escapes control characters inside strings, so appending `'\n'` is safe.

**A blocked `WriteFrame` does not honour its call's context, and that is deliberate.** A security
sweep raised it as a shutdown deadlock and it was **not reproduced**: `downstream`'s `enqueue`
selects on the reply, the call context and the server lifetime; `Server.Close` closes the transport
*before* joining the owner goroutine; and closing the child's stdin unwinds a blocked `Write`. What
remains true is only that a blocked write outlives its per-call context, bounded to that server
being unavailable until redial. **Making `WriteFrame` context-aware would be worse than the
finding**: a write abandoned partway leaves a half-frame in the stream, and the atomicity guarantee
above is what lets every other reader on that connection trust it.

**Blank lines are skipped, and a final frame without a newline is still delivered** (with EOF
returned on the next call).

**Malformed input yields decidable typed errors and never panics.** Any shape violation in
`ParseMessage` returns an error satisfying `errors.Is(err, ErrMalformedFrame)`. **Whether to close
the connection over it is the caller's decision**; this layer only makes the error decidable — one
garbage frame must not crash the process.

**`ID` preserves the original JSON text.** It stores a raw string, so a peer's id is echoed back
byte for byte including numeric spellings beyond float64 precision. `Key()` uses that text directly
as a map key, and since string ids carry their quotes they can never collide with numeric ids; an
unset ID serializes as `null`, used only for protocol errors that cannot name a request.

**A message with a method but a null id is handled as a notification** — an explicit trade-off.

**Downstream JSON is always passed through raw.** Tool schemas, annotations, server capabilities,
call arguments and results are all `json.RawMessage`. This layer **never reshapes downstream JSON**.

**Cancellation is racy and receivers must tolerate late replies**; an unmatched response is
discarded.

**`DEPRECATED-UPSTREAM` markers carry an earliest-removal date** and are retained per canonical.md
§5b: `roots` (the types and the `roots/list` reverse RPC — the gateway's `RootSource` seam absorbs
its removal), `ping`, and the `initialize`-handshake pair, all 2027-07-28.

---

## internal/mcp/transport

Turn "a description of a downstream MCP server" into a live connection, and **classify** every
failure into one of the three categories the upstream circuit breaker can use.

`Transport` is the common denominator: `Call` / `Notify` / `OnPeerRequest` / `OnListChanged` /
`Stderr` / `Close`. Four constructors: `SpawnStdio` (a child-process stdio connection, handing off
to `SpawnDocker` when `cfg.Docker != nil`), `SpawnDocker` (the same with `docker run -i --rm ...` as
the host process), `DialStreamableHTTP` (no connection made until the first `Call`), and
`DialHTTPSSE` (blocks until the endpoint event arrives or ctx expires). `Kind` has only three values
— `stdio`/`http`/`sse` — because docker is a variant of stdio and containerization is expressed by
the registry's `runtime` field.

### The handshake

`Handshake` tries **`server/discover` first**. A 2026-07-28 server answers it and
`mcp.NegotiateHighest` picks the version; anything that negotiates ≤ 2025-11-25 falls through to the
legacy `initialize` + `notifications/initialized` path, because those versions need the stateful
handshake however the version was learned.

`discoverFallback` decides which failures mean "alive but pre-2026", and it is deliberately narrow:
only a JSON-RPC error reply (a real 2026 server MUST implement discover, so an error object proves
an old one) or a `ClassFatal` HTTP 4xx (a 2025-11-25 streamable-http server rejects an unknown
pre-session POST with 400 rather than a JSON-RPC frame). **Everything else — connection loss, 5xx,
oversized frame, cancellation — propagates unchanged**; falling back there would hide a real failure
from the circuit breaker behind a second handshake attempt.

**A transport that cannot carry the per-request `_meta` is refused, not degraded.** Once 2026-07-28
is negotiated, `Handshake` requires the transport to implement `negotiatedSetter`; without it,
sending bare requests would be rejected by a strict server with -32602, so it fails `ClassFatal`
instead. `injectMeta` splices `_meta` at the top level only, so every existing value round-trips
through `json.RawMessage` byte-identically and params that already carry `_meta` are left alone.

**A failed handshake is always `ClassFatal`** — retrying the same handshake cannot succeed, so it
must not consume circuit-breaker budget.

### The error model

`*Error{Class, StatusCode, RetryAfter, Err}`, with `Unwrap` exposing the cause so `errors.Is` works
against `mcp`'s sentinels. The criterion behind the three classes is that **`tools/call` is not
idempotent**:

- `ClassFatal`: an ordinary error response, or a verdict on the configuration itself (a bad URL, a
  spawn-guard rejection, an over-limit outbound frame, a missing docker CLI). **Not counted by the
  breaker**, because it says nothing about downstream health.
- `ClassUnavailable`: a connection-level failure. **Counted by the breaker.**
- `ClassRetry`: the request **provably never reached the server** (DNS or dial failure), or the
  server explicitly answered 429. Only these may replay a non-idempotent call.

`ErrDeadConnection` marks the pre-send half of `ClassUnavailable` — a call rejected *before*
anything went on the wire — which is what lets `internal/downstream` rebuild the connection and
replay a non-idempotent request once. It is attached by **copying** the stored terminal error
(`presend`), never by marking it in place: waiters whose requests may already have executed share
that value. **Only the legacy HTTP+SSE transport attaches it today**; the stdio `conn` and
streamable-http return the bare terminal error from their pre-send checks, so a call they reject
before sending is retried by nobody — correct where used, simply absent elsewhere.

`StatusOf(err)` and `IsAuthStatus(err)` exist to **keep callers from grepping error text**. The
message includes a body snippet, so a check for `http 401` in `Error()` classifies a proxy's 502
whose body mentions an upstream 401 as "your credentials were rejected", sending the operator to
re-run `auth login` for a failure no credential can fix. `internal/cli` and `internal/ctlapi` had
each independently written such a substring match; both now decide by status code.

### How the implementations differ

Only stdio and docker share a code base (`conn`).

| Dimension | stdio / docker | streamable-http | Legacy HTTP+SSE |
|---|---|---|---|
| Underlying | Child process stdin/stdout, newline-delimited frames | One POST per message, the answer being JSON or an SSE stream | A long-lived GET to receive, POSTs to send |
| Failure model | **Terminal**: any read-side error or write-side I/O error sets `failErr` (written once), releases all pending calls, and every later call returns it immediately | **Non-terminal**: one bad request does not poison the transport; only `Close` and 410 are terminal | **Terminal**, isomorphic to stdio: one long-lived stream, and if it breaks everything breaks |
| Reverse RPC | **Answered inline on the read loop**, so a handler must not call back into the same transport and must return promptly | On a separate goroutine, reply POSTed back; a handler **may** call back in | Separate goroutine + POST, same `maxPeerWorkers`(8) cap |
| Session | None | `Mcp-Session-Id` on ≤ 2025-11-25 (echoed after initialization, DELETEd on `Close`); none under 2026-07-28, which sends `Mcp-Method`/`Mcp-Name` headers instead | None |
| Out-of-call stream | N/A | optional GET (≤ 2025-11-25) or `subscriptions/listen` (2026-07-28), same reconnect loop | the one GET stream |
| Resumption | N/A | `Last-Event-ID` best-effort resume once (off with `DisableResume`; never after a 410) | **None**: the legacy binding never defined it, and silently replaying non-idempotent calls is worse than letting `internal/downstream` reconnect cleanly |
| `Stderr()` | The child's last 4 KiB of stderr (docker also appends diagnostic lines) | `""` | `""` |

### Invariants and failure directions

**Bounded reads run through both wire formats.** stdio uses `mcp.FrameReader`; SSE uses
`sseScanner`, its analogue — an event whose accumulated data exceeds the cap yields
`mcp.ErrFrameTooLarge` rather than buffering first, the error is sticky, an unfinished event at EOF
is discarded per the dispatch rules, and comments (`:` heartbeats) and unknown fields are ignored.
`readBounded` / `encodeMessage` apply the same cap to HTTP bodies.

**That bound covers bytes read, not memory allocated, and on the batch path the gap is large — owed,
not fixed.** `decodeMessages` materializes the whole array as `[]json.RawMessage` and then sizes its
output slice from `len(items)`, both **before `ParseMessage` validates a single element**. Each
element becomes its own heap allocation with its own size class, so a body already inside
`readBounded`'s cap expands by a large constant: a 16 MiB answer of `[1,1,1,…]` is 8.4M elements,
measured at ~750 MiB live and ~1.3 GiB allocated. The reach is one caller — `readJSONResponse`, an
`application/json` POST answer — so the party who can trigger it is a hostile or compromised
*downstream* server, never an upstream client, and the SSE path is unaffected because `sseScanner`
hands `ParseMessage` one event at a time. Deleting the batch path is not the fix: it exists for
2025-03-26 servers, which may still answer with an array. An element cap, or decoding through a
streaming `json.Decoder` so each element is validated and released as it arrives, is.

**The one place `sseScanner` deviates from the dispatch rules is an empty data buffer**, which the
spec drops and this scanner dispatches — so a bare `id:` line and a blank line, the way a resumable
stream advances `Last-Event-ID` without sending a message, surfaces as a `message` event with no
data. `lastID` has to advance either way, and one always-dispatch rule beats two paths that both
have to remember to update it. **The three consumers pay for it** by skipping an empty-data event
before parsing; delete one of those guards and `ParseMessage` is handed an empty frame, returns
`ErrMalformedFrame`, and the reader tears down the stream on a keep-alive.

**A reverse-RPC reply's id is forcibly overwritten with the request id**, whatever the handler set.
With no handler registered the answer is method-not-found; a handler error or nil answer is
internal-error; a reply over the frame limit is replaced by an in-band internal-error, so the stream
stays intact and the server is not left waiting.

**Cancellation is always forwarded, best-effort.** All three implementations send
`notifications/cancelled` before returning the ctx error. A failed write is swallowed (a dead pipe
is about to be reported by the read loop), and on the HTTP side it runs on the **transport lifetime
ctx** rather than the already-dead call ctx, with a 5s timeout.

**Unmatched responses are discarded and unknown notifications are ignored.** Neither is fatal.

**A malformed frame closes the connection, but the process never crashes.** The stdio and HTTP+SSE
read loops `fail(ClassUnavailable)`: a peer emitting garbage cannot be trusted to still be
maintaining frame boundaries. The streamable-http out-of-call stream merely ends that stream and
reconnects next time.

**The HTTP status classification is deliberate** (`httpError`):

```
410 Gone         → ErrEndpointMoved, ClassUnavailable, never retried, never resumed, endpoint permanently poisoned
404 Not Found    → ErrSessionExpired, ClassUnavailable, clears the session id (re-initializing is decided upstream)
429 Too Many     → ClassRetry + a Retry-After hint (delta-seconds or HTTP-date; unparseable = 0 = use the caller's own backoff)
5xx              → ClassUnavailable: the request did reach the server, non-idempotent calls must not be replayed, counted by the breaker
other 4xx        → ClassFatal: our request was rejected on its own merits, which says nothing about server health
```

**410 is terminal for the transport.** Once `noteTerminalStatus` sets `moved`, `stateErr` makes
every later `Call`/`Notify` fail immediately, the out-of-call stream loop exits, and `Close` does
not even send the DELETE. The meaning is "a human has to change the URL in the configuration".

**Every destination a server names fails closed on cross-origin**, by one predicate. A server can
name one two ways: legacy HTTP+SSE's endpoint event (`setPostURL`) and a `3xx` redirect
(`newHTTPClient`'s `CheckRedirect`). The caller's headers, `Authorization` among them, ride every
POST and every redirected request, so both are validated with `sameOrigin` — scheme plus host
including port — and refused otherwise; a destination pointing elsewhere is credential exfiltration
dressed up as a protocol event. Relying on net/http's own stripping is not enough: it drops
`Authorization` only when the redirect leaves the *domain*, so a subdomain or an `https`→`http`
downgrade keeps the header. A caller-supplied `HTTPConfig.Client` is left alone — that caller owns
its redirect policy — which is why `internal/downstream` sets the same policy on the client it
builds. A stream answering with a non-SSE content type is likewise given up on permanently.

**The SSRF screen is injected, and if it is not injected it is not there.** `HTTPConfig.DialContext`
and `HTTPConfig.Client` are **mutually exclusive** (both is rejected as `ClassFatal`), so a
protective dialer cannot be silently dropped. Supplying neither means **no address screening at
all** — reserved for tests and explicitly trusted loopback endpoints. When the injected dialer
refuses it manifests as a dial failure and is classified `ClassRetry`; that is honest (nothing was
sent) and harmless (the guard fails closed, so every bounded retry is rejected the same way).

**Protocol headers always override the caller's.** `newRequest` lays down caller headers first, and
each call site sets `Accept`/`Content-Type`/`Mcp-Session-Id`/`MCP-Protocol-Version` afterwards. Note
that `net/http` canonicalizes header names, so what goes out is `Mcp-Protocol-Version`; header names
are case-insensitive per RFC 9110 §5.1 and the golden files pin the canonical form.

**Body snippets in error messages are bounded and flattened to one line.** `drainSnippet` reads 1
KiB, turns `\n\r\t` into spaces and strips other control characters — error strings end up in JSON
log lines and trace frames, where an embedded newline amounts to permitting a forged record.

**Concurrent reverse RPC has backpressure rather than unbounded fan-out.** The `maxPeerWorkers = 8`
semaphore **blocks stream reading** when full, making a flooding peer slow itself down. `wg.Add`
always happens under the same lock that publishes `closed`, so `Close`'s `Wait` cannot end up
waiting on a counter about to be incremented.

**stdio's process reaping follows a strict order.** `os/exec`'s pipe contract requires `cmd.Wait`
not to precede stdout being fully read, so reaping hangs off the end of the read loop. `Close` first
fails all pending calls, closes stdin (an EOF to a well-behaved child), waits `killGrace = 3s`,
`Kill`s on timeout, then runs cleanup. **The process is always reaped.** For the same reason
`Stderr()` waits for the reap once — and only once — the transport is known dead: `exec` copies
stderr on its own goroutine and only `cmd.Wait` guarantees the copy finished, so reading the ring
early yields an empty tail exactly when it matters most. The trigger is `failedErr`, not `readDone`,
because a child that dies instantly (`docker run` on a missing image) breaks the pipe before the
read loop reaches EOF.

**`StdioConfig.Command` is resolved against `StdioConfig.Env`'s PATH, not this process's**
(`transport.LookPath`). `exec.Command` resolves through `exec.LookPath`, which reads the *calling*
process's PATH; `cmd.Env` is only ever handed to the child. Those are the same PATH often enough
that the difference goes unnoticed — and then a caller repairs the child's PATH, which
`internal/downstream` does because launchd hands a GUI-launched process a four-entry PATH, and every
spawn still fails with `executable file not found in $PATH` naming a PATH the child was never going
to run with. Three deliberate narrowings, all toward doing nothing rather than something different:
**on Windows the command is returned untouched** (resolution there means reproducing PATHEXT
exactly, and no gate here runs on a real Windows machine — `docs/windows.md`), as is a command
containing a path separator or one with a nil `Env` (the child inherits ours, so the two PATHs are
one). **An empty PATH entry is skipped**, the one deviation from `exec.LookPath`: POSIX reads it as
the working directory, and a spawn must not let a directory nobody named decide which binary runs.

Resolution happens **after** the screen and makes no difference to it — `spawnguard` matches on the
basename, so `npx` and `/opt/homebrew/bin/npx` reach the same verdict — and a command that cannot be
resolved fails `ClassUnavailable`, exactly as the failed `exec.Start` did before, so a missing
binary stays breaker-visible rather than becoming fatal.

### The docker spawner's additional rules

The positioning is explicit: the spawn guard is **anti-smuggling**, not a sandbox; this half is
where resource and namespace isolation lives. It drives the docker CLI with `os/exec` rather than an
SDK — partly because `internal/mcp` may only use the standard library, partly because shelling out
makes `DOCKER_HOST`, docker contexts and credential helpers work automatically.

- **Everything off by default**: `--network none`, only explicitly declared mounts and read-only
  unless `Mount.Write` is set, and never `--privileged`, host namespaces or capability grants.
- **`ExtraRunArgs` must not restate a flag this file emits itself** (`ownedRunFlags`). Docker's
  last-wins semantics would let a stray `--network host` quietly erase the isolation defaults; a
  self-contradictory configuration is a bug, not an override. **The comparison covers every spelling
  docker accepts, not the token**, because docker takes a shorthand's value attached to the letter:
  `--user 0:0`, `--user=0:0`, `-u 0:0` and `-u0:0` are one flag, and only the first three begin with
  a token equal to `-u`. Comparing text up to `=` refused three of them and let `-u0:0` run the
  container as root under a config that said `user: 1000:1000` — isolation silently degrading rather
  than being refused, the highest-severity shape in this tree. `ownsRunFlag` is fail-closed for
  every flag it recognises and fail-open past an unrecognised shorthand, since guessing whether an
  unknown letter takes a value would refuse working configs as docker grows; `spawnguard` inspects
  the assembled command line without consulting that table, so the residue is not the only gate.
- **Secrets never go in argv**: container environment is passed as `-e NAME` (no value), inherited
  from the docker CLI's own environment. `ps(1)` sees argv; it does not see the CLI's environment.
- **`StdioConfig.Cwd` becomes `--workdir`, not the CLI process's directory.** "The directory the
  server runs in" is a path inside the image, so applying it to the docker CLI would be a silent
  no-op for the workload — the entry asks for a working directory and the container gets none, with
  nothing to notice. An explicit `DockerConfig.Workdir` is the more specific statement and wins.
- **`BuildDockerRunArgs` is pure and totally ordered**: mounts sorted by (target, source), env by
  name, so one configuration always produces one argv — pinned by a golden file.
- **Configuration is validated before any process starts**: the image must not start with `-` or
  contain whitespace, the container name must start with `agenthub-` and be docker-legal, mount
  paths must be absolute and free of `:` (otherwise a second flag is smuggled out of the value
  position), and memory/CPU/network names each have their own regexes.
- **Container names are unique per spawn**: several gateway processes legitimately run the same
  server at once, so a fixed name would collide. This is the one place mcpproxy's "idempotent
  pre-cleanup by fixed name" recipe **does not apply**.
- **Cleanup is belt and braces**: `--rm` covers normal exit, `removeContainer` (by cidfile id,
  falling back to the name) covers "the CLI died first"; failures are always ignored — the container
  may already be gone, and shutdown must not fail over it.
- **Failure diagnostics are folded into the stderr tail**: `diagnoseDocker` appends an
  `agenthub: ...` line, rescuing "image doesn't exist" and "daemon isn't running" from a bare
  deadline-exceeded. Daemon cases are matched first, because a dead daemon also emits wording that
  looks like an image problem.
- **`DockerBinary` has a fallback path table**, because a gateway launched by launchd/systemd has a
  truncated PATH and Docker Desktop hides its CLI inside an app bundle. Once found, its directory is
  prepended to the child's PATH (the credential helpers sit there). `DockerVersion` and
  `StrayContainers` are the doctor-facing probes: the daemon answers, and nothing carrying
  `agenthub.managed=true` was left behind by a `kill -9`.
- **The unit tests drive a shell stand-in for the docker CLI**, which pins argv exactly but can
  never prove a container ran. The one test that does is `TestDockerRuntimeDownstream` in
  `test/e2e`: it mounts the downstream binary at a path that exists ONLY inside the container, so a
  regression that loses the runtime dimension and spawns on the host cannot answer at all. Verified
  falsifiable, and it skips itself when docker is absent.

---

## internal/registry

The **on-disk source of truth for configuration** shared by the CLI, each gateway process and the
daemon: multi-document, unknown-field-preserving, cross-process atomic writes, a monotonic
generation, change awareness, and self-write suppression.

Five documents named by their `DocKind` (`meta`, `servers`, `profiles`, `clients`, `governance`),
a sibling `.lock` guarding all of them, `backups/` holding 5 rolling generations per document, and
`.runstate.json` — the crash marker, whose dot prefix deliberately keeps it out of the
`<kind>.json` namespace.

### Key types

**`Doc[T]`** is the persistence envelope: a typed view plus verbatim passthrough of unknown fields,
with **known fields winning on collision**, so fields written by a newer agenthub (or by hand)
survive an older version's load-modify-save. The preservation is **per level** —
`ServersDoc.Servers` holds `Doc[ServerEntry]`, so unknown fields inside one server entry survive
too.

**Passthrough is exactly what makes a RETIRED field dangerous, which is why `HasUnknownField(name)`
exists.** A field the type system dropped keeps round-tripping verbatim, so a rule an operator wrote
while it worked still *looks* applied long after it stopped applying — and when the retired rule was
a narrowing one, "stopped applying" means widening. `agenthub doctor`'s `scope:projects` check uses
it for the retired `projects` block in `clients.json`. Reading the key is deliberately **all** it
exposes: a caller may ask whether a name is present, never reach in and act on its contents.

**`Store`** is a handle on one directory. `Open`/`OpenOptions` **still return a usable `*Store` when
a document had to be quarantined**, alongside a non-nil error joining an `*UnreadableError` —
whether that is fatal is the caller's call. `Update` is the full
`lock → load → modify → commit → bump` transaction; **`Tx`**, the mutable view its callback sees, is
valid only for that callback's duration and **does not expose `meta.json`** — the generation is the
store's business. **`Snapshot`** is the immutable view, deep-copied by JSON round-trip so it is
independent of maps the callback may still hold.

Two domain shapes worth knowing:

- **`ClientEntry` holds `{Profile, ProfileRef}` and nothing else**: a client selects a profile and
  never narrows on top of one. It used to also carry `Discovery`, `Servers`, `Tools`,
  `ResultBudget`, `Approval` and a `Projects` map of per-root overrides; all are gone, along with
  the `BindingInherit` kind that existed only so a project could inherit its client's binding. A
  `ProfileBinding` is now exactly `named` or `followActive`, and `Binding()` hardcodes the priority
  "explicit `ProfileRef` > the `profile` shorthand > the layer default". `Profile` gained the
  `Discovery` field the client lost, because discovery describes the tool set it is attached to.
- **`GovernanceDoc.RateLimits` exists only at the global layer** and never enters the three-layer
  scope chain: the rule patterns already carry the (client, server, tool) dimensions, and
  cross-process counting buckets are keyed by rule pattern — so the same pattern at several layers
  would either split one quota into one bucket per layer (a multiplied limit, the opposite of "only
  tighten") or need a per-pattern min-merge that exists nowhere else here. The registry stores it
  **verbatim** (`window` is a duration string and is not parsed); parsing, validation and
  enforcement live in `internal/ratelimit`, which errors on the whole rule set rather than silently
  dropping a rule it cannot understand.

### Invariants and failure directions

**One directory lock, not one per document**, because `meta.json`'s generation must be written
atomically with the batch of documents it covers. It is a flock on `<dir>/.lock`: non-blocking
attempts plus 5ms polling until timeout, then a `*LockTimeoutError` satisfying
`errors.Is(err, ErrLockTimeout)` (the CLI maps it to exit code 7); the polling gaps respect ctx
cancellation.

**`Update` never trusts the in-memory snapshot.** It reloads from disk under the lock every time, so
a stale `Snapshot` can never overwrite what another process just wrote.

**The generation is incremented only when something was actually written, and only under the lock.**
The no-op guard compares **parsed JSON values** (`canonicalize` keeps numbers verbatim via
`json.Number`, sorts object keys, strips whitespace), so key-order jitter causes no phantom bumps.
Input that `canonicalize` fails on is judged "not equal" — forcing a rewrite, the safe direction for
a persistence layer.

**An unparseable file is never destroyed by reading it.** A parse failure is retried
`readRetries = 4` times at `readRetryDelay = 75ms` to ride over a non-atomic external writer; only
then is the file renamed to `<name>.json.unreadable-<timestamp>` (**quarantine, never destroy**), a
default written, and an `*UnreadableError` reported. **One quarantined document does not block
updates to the others** — the error is joined into the return value and the transaction commits.

**A missing file gets a default written, but that is not a change.** First contact persists it so
the file exists from then on; it does not trigger a bump.

**Rolling backups rotate only on a real write**, so the five slots always hold five **distinct**
generations.

**`atomicWrite` never leaves half a target file behind**: temp file in the same directory →
`chmod 0600` → write → `fsync` → `rename` over the target → `fsync` the parent directory. The parent
fsync is tolerated to fail (EINVAL/ENOTSUP) on filesystems that do not support it — rename is still
atomic there.

**Registry documents never store credentials.** `${SECRET_X}` placeholders in `ServerEntry.Env` /
`Headers` are **persisted verbatim**, and resolution against the vault happens at connect time in
`internal/downstream`. In the same spirit `OAuthHint` deliberately **lacks** a `needsAuth` field:
whether a server currently needs authorization is runtime state, and persisting it would create a
second source of truth — a stale `"needsAuth": false` keeps a Ready badge on a server that 401s on
every call.

**`omitzero` on tri-state fields is load-bearing.** For `ToolSelector.Allow`, `ServerEntry.Tools`
and `Profile.Servers`, nil (don't intervene) and `[]` (block everything) mean different things, and
`omitempty` would erase an empty list from disk — silently turning "block everything" into "allow
everything". `omitzero` lets the empty list round-trip, so block-all stays closed.

**An unknown runtime name is rejected, not treated as host.** A typo like `"dcoker"` must error out
in `ValidateRuntime` rather than quietly discarding the isolation the operator asked for. The docker
runtime likewise applies only to the stdio transport and must carry an image.

### The generation criterion, self-write suppression, and the two watch channels

These three answer three different questions: the generation answers "**did anything change**",
self-write suppression answers "**did I write this myself**", and the Applier answers "**should this
state I just read be adopted**".

**The Applier's criterion is "the generation read ≥ the generation applied", not "equal to the
event's Rev"** (canonical.md §5c #2). A push is only a notification and carries no snapshot, so the
consumer re-reads the files itself; under several writes in quick succession the generation read
**exceeds** the Rev of the event in hand, so an equality test rejects it and the consumer waits
forever for an event that will never come. `>=` adopts any state that is not older, and re-applying
the same generation is idempotent by construction. `MarkApplied` only ever increases, so a late
out-of-order apply cannot push the criterion backwards; `Apply(gen, fn)` holds one lock across both
check and update so two concurrent reloads cannot finish on the older state; and **a failed apply is
not recorded**, so the next trigger retries.

**Self-write suppression fails open (toward reloading).** `selfWriteSet` is a bounded TTL set: 64
slots, 10s expiry, registered before the write, withdrawn if the write fails, cleared wholesale when
an external change is observed. A TTL expiring, a slot being evicted or a fingerprint not matching
costs at worst **one redundant empty reload**, and it **cannot mask an external change** — content
whose fingerprint is not in the set is always treated as external. The withdrawal and clearing steps
are both necessary: a fingerprint for content that never reached disk must not suppress a future
**external write that happens to be identical**, and once someone else has touched the registry the
pending fingerprints no longer describe the on-disk lineage. The fingerprint is a SHA-256 taken
after canonicalization, so formatting differences between what was written and what is read back do
not affect matching; if canonicalization fails it hashes the raw bytes, again costing one reload.

**Watching runs on two channels, both always on.** `fsnotify` plus a 200ms debounce is the primary
signal, 2s polling is the safety net — fsnotify is unreliable on SMB and network mounts and may not
even initialize. Any fsnotify initialization failure **merely degrades to pure polling** rather than
failing `Watch` (fail-open: a slightly laggy watcher beats no watcher); the channel closing at
runtime is handled the same way, and fsnotify's error channel is non-fatal.

`scan()` is where the decisions happen, and it **holds no cross-process lock**: our own writes are
atomic renames, and the torn state a non-atomic external writer produces fails canonicalization, so
the next trigger retries. Invariant by invariant:

- read `meta.json` first and **abandon the whole round if it cannot be read** (something is probably
  being written), advancing nothing;
- compare each content document's canonical content against **this Watcher's last applied baseline**
  — which is why events carry a precise `DocKind` rather than a vague "something changed";
- a failed read or canonicalization always `continue`s: **a failed load never advances the
  baseline**, so a half-written file is never mistaken for new state;
- a self-write fingerprint hit: **advance the baseline silently, emit no event**;
- judged an external change: clear the self-write set first, then advance and emit;
- event delivery **never blocks the scan loop**: when the channel is full the event is parked by
  kind (keeping the latest Rev) and redelivered on the next trigger. Merging by kind is safe because
  consumers were always going to re-read and do not trust the Rev.

A Watcher **seeds its baseline** from the Store's current snapshot at creation, so state this
process has already applied is not reported again. `meta.json` only supplies `Change.Rev`; it is
never itself a `Kind`.

### The crash marker

`ArmRunMarker(dir)` atomically reads out the **previous** run's outcome and arms a new marker;
`Resolve()` marks it clean as the last step of a graceful shutdown. A process that is SIGKILLed,
panics or loses power never resolves, and **that failure to resolve is the signal**. `daemon.Run` is
the writer: it arms **after** `ctlapi.Listen` succeeds, so a second daemon that lost the socket race
cannot clobber the winner's marker, and resolves only on the graceful-stop branch. `agenthub doctor`
reports the result through the read-only `PreviousShutdown`, which never arms.

Failing to write the marker only costs the **next** startup its diagnostic capability, so it
degrades to a warning: refusing to serve for the sake of a diagnostic is the worse trade.

Two design trade-offs. **Resolve rewrites rather than deletes** — "no marker" must stay
distinguishable from "a resolved marker", and deleting would make "first run" and "clean shutdown"
the same observation, so a first run would be reported as clean with no evidence. **Every ambiguity
falls toward `ShutdownUnknown`**: a marker that cannot be read or parsed, or that carries an
unrecognized version, is unknown, because a diagnostic must not issue a clean bill of health out of
thin air. The pid and timestamp are purely diagnostic, and the verdict **does not depend on whether
that pid still exists** (pids are reused, and the check is meaningless across machines or reboots).

---

## Appendix: two things that are easy to confuse

**There is one stderr tail window at each of two layers, with different sizes.** This layer's
`transport` keeps a **4 KiB byte** tail (`stderrTailSize`); the layer above, `internal/downstream`,
has its own **line-based** ring buffer. They serve different presentations — finding one does not
mean you have changed the other.

**Windows cross-process locking is implemented but unverified.** The registry lock uses
`LockFileEx`/`UnlockFileEx` delegating to `internal/platform`, the stub build tag was narrowed to
`!darwin && !linux && !windows`, and `internal/ratelimit` and `internal/accesslog` both set
`crossProcessLockSupported = true` there. Nothing has run on a real Windows machine — see
[../windows.md](../windows.md).
