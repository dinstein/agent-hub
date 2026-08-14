# Protocol and transports

> **Answers** how a frame is parsed and written, how a downstream description becomes a live connection, and how every failure is classified.
> **Not here** what the hub does with a connection → [downstream.md](downstream.md); one revision's conformance → [../mcp-2026-07-28.md](../status/mcp-2026-07-28.md).
> **Kept true by** four fuzz targets (`FuzzParseMessage`, `FuzzSSEScanner`, `FuzzEncodeJSON`, `FuzzDecodeHeaderValue`) and the transport golden files.

`internal/mcp` is the only place in the repo that touches protocol implementation: wire format,
framing, domain types, version negotiation, all in-house and standard-library-only.
`internal/mcp/transport` turns a server description into a connection and classifies what goes wrong.

`.golangci.yml`'s `mcp-stdlib-only` rule restricts `internal/mcp/**` to `$gostd`, and
`no-third-party-mcp-libs` bans the known Go MCP SDKs repo-wide. The reason is not distrust: a handful
of invariants here need precise control — 16 MiB bounded reads, `notifications/cancelled` forwarding,
inline replies to reverse RPC, the stdio stderr tail — and a facade keeps the choice reversible inside
one package. One consequence: the SSRF screen cannot import `internal/guard/netguard` here, so the
caller injects a dialer instead.

## internal/mcp

`MaxFrameSize = 16 << 20` is a hard cap on read and write. Framing is newline-delimited JSON;
LSP-style `Content-Length` headers are deliberately unsupported.

**Two protocol generations coexist.**

| | `Version2026` (`2026-07-28`) | `Version2025` (`2025-11-25`) |
|---|---|---|
| Handshake | none — stateless | `initialize` + `notifications/initialized` |
| Session | none; `Mcp-Method` / `Mcp-Name` headers | `Mcp-Session-Id` |
| Negotiation | `server/discover` | the `initialize` result |
| Version carried by | per-request `_meta` | connection state |
| Notifications | `subscriptions/listen` | a long-lived GET stream |
| Reverse RPC | MRTR (`InputRequiredResult` / `requestState` / `inputResponses`) | in-band JSON-RPC requests |

`ProtocolVersion` stays at `Version2025` on purpose: every context that reads it is definitionally
pre-2026, since the legacy path runs only after `server/discover` failed. `SupportedVersions` is the
accepted set, newest first — `2026-07-28`, `2025-11-25`, `2025-06-18`, `2025-03-26`. `RequestState` is
echoed verbatim and never inspected; servers own its integrity.

### Invariants

**Reads are bounded, and exceeding the bound poisons the reader.** `readLine` checks the cap while
accumulating, so an oversized frame fails before being fully buffered. Once `ErrFrameTooLarge` is hit
the stream sits mid-frame, so the error is sticky and the connection must be considered unusable.

**Exceeding the bound on the write side is recoverable.** `WriteFrame` rejects an oversized frame
before writing any bytes, which is why the transport layer classifies an outbound over-limit as
`ClassFatal` rather than `ClassUnavailable`.

**Frame writes are atomic.** One `Write` per frame plus `FrameWriter`'s mutex keeps frames from
interleaving when the call goroutines and the read loop share a writer. `json.Marshal` escapes control
characters inside strings, so appending `'\n'` is safe.

**A blocked `WriteFrame` does not honour its call's context, deliberately.** A write abandoned partway
leaves a half-frame in the stream, and the atomicity guarantee above is what lets every other reader on
that connection trust it. What remains true is that a blocked write outlives its per-call context,
bounded to that server being unavailable until redial: `downstream`'s `enqueue` selects on the reply,
the call context and the server lifetime, `Server.Close` closes the transport before joining the owner
goroutine, and closing the child's stdin unwinds a blocked write.

**Malformed input yields decidable typed errors and never panics.** Any shape violation in
`ParseMessage` returns an error satisfying `errors.Is(err, ErrMalformedFrame)`. Whether to close the
connection over it is the caller's decision; this layer only makes the error decidable.

**`ID` preserves the original JSON text.** A peer's id is echoed back byte for byte, including numeric
spellings beyond float64 precision. `Key()` uses that text as a map key, and since string ids carry
their quotes they cannot collide with numeric ids. An unset ID serializes as `null`, used only for
protocol errors that cannot name a request.

**A message with a method but a null id is handled as a notification.** Blank lines are skipped, and a
final frame without a newline is still delivered, with EOF returned on the next call.

**Downstream JSON is always passed through raw.** Tool schemas, annotations, capabilities, arguments
and results are all `json.RawMessage`. This layer never reshapes downstream JSON.

**Cancellation is racy and receivers must tolerate late replies.** An unmatched response is discarded.

**`DEPRECATED-UPSTREAM` markers carry an earliest-removal date** ([conventions.md#mcp-protocol-scope](../conventions.md#mcp-protocol-scope)):
`roots` (the types and the `roots/list` reverse RPC — the gateway's `RootSource` seam absorbs its
removal), `ping`, and the `initialize` handshake pair, all 2027-07-28.

## internal/mcp/transport

`Transport` is the common denominator: `Call`, `Notify`, `OnPeerRequest`, `OnListChanged`, `Stderr`,
`Close`. Four constructors — `SpawnStdio` (handing off to `SpawnDocker` when `cfg.Docker != nil`),
`SpawnDocker`, `DialStreamableHTTP` (no connection until the first `Call`) and `DialHTTPSSE` (blocks
until the endpoint event arrives). `Kind` has three values, `stdio`/`http`/`sse`, because docker is a
variant of stdio and containerization is expressed by the registry's `runtime` field.

### The handshake

`Handshake` tries `server/discover` first. A 2026-07-28 server answers it and `NegotiateHighest` picks
the version; anything that negotiates ≤ 2025-11-25 falls through to `initialize`, because those
versions need the stateful handshake however the version was learned.

**`discoverFallback` is deliberately narrow**: only a JSON-RPC error reply, or a `ClassFatal` HTTP 4xx
(a 2025-11-25 streamable-http server rejects an unknown pre-session POST with 400 rather than a
JSON-RPC frame). Connection loss, 5xx, an oversized frame and cancellation all propagate unchanged —
falling back there would hide a real failure behind a second handshake attempt.

**An answered error is not by itself proof of an old server.** The codes 2026-07-28 reserves
(`mcp.IsSpecErrorCode`, −32020 to −32099) are answers only a modern server knows how to give, so
`discoverFallback` returns false for them and the caller sees the server's own error, which normally
says exactly what to correct. Falling back instead sends `initialize` — the one method a 2026-only
server does not implement — and turns a correctable request into a dead connection. Over HTTP the code
comes out of the body, since one 400 carries both a legacy server's opaque rejection and a modern
server's `HeaderMismatch`. The grandfathered −32000 to −32019 band still falls back: an SDK's private
code says nothing about the generation.

**A 404 is two different answers, and the body decides which.** 2026-07-28 makes it the required status
for an unimplemented method, so it stopped meaning "your session is gone". A JSON-RPC code in the body
means `ErrMethodNotFound` and `ClassFatal`; its absence keeps `ErrSessionExpired`.

**A transport that cannot carry the per-request `_meta` is refused, not degraded.** Once 2026-07-28 is
negotiated, `Handshake` requires `negotiatedSetter`; without it a strict server would reject bare
requests with −32602, so it fails `ClassFatal` instead. `injectMeta` splices `_meta` at the top level
only, so every existing value round-trips byte-identically.

**A failed handshake is always `ClassFatal`.** Retrying the same handshake cannot succeed, so it must
not consume circuit-breaker budget.

### The error model

`*Error{Class, StatusCode, RetryAfter, Err}`, with `Unwrap` exposing the cause so `errors.Is` works
against `mcp`'s sentinels. The criterion behind the three classes is that **`tools/call` is not
idempotent**:

| Class | Means | Breaker | May replay |
|---|---|---|---|
| `ClassFatal` | an ordinary error response, or a verdict on the configuration itself — a bad URL, a spawn-guard rejection, an over-limit outbound frame, a missing docker CLI | not counted: it says nothing about downstream health | no |
| `ClassUnavailable` | a connection-level failure | counted | no |
| `ClassRetry` | the request provably never reached the server (DNS or dial failure), or the server answered 429 | counted | yes |

`ErrDeadConnection` marks the pre-send half of `ClassUnavailable` — a call rejected before anything
went on the wire — which is what lets `internal/downstream` rebuild the connection and replay a
non-idempotent request once. It is attached by copying the stored terminal error, never by marking it
in place, because waiters whose requests may already have executed share that value. **Only the legacy
HTTP+SSE transport attaches it today**; stdio and streamable-http return the bare terminal error from
their pre-send checks, so a call they reject before sending is retried by nobody.

`StatusOf(err)` and `IsAuthStatus(err)` exist to keep callers from grepping error text. The message
includes a body snippet, so a check for `http 401` classifies a proxy's 502 whose body mentions an
upstream 401 as "your credentials were rejected", sending the operator to re-run `auth login` for a
failure no credential can fix.

### How the implementations differ

Only stdio and docker share a code base (`conn`).

| Dimension | stdio / docker | streamable-http | legacy HTTP+SSE |
|---|---|---|---|
| Underlying | child process stdin/stdout, newline-delimited | one POST per message, answered with JSON or an SSE stream | a long-lived GET to receive, POSTs to send |
| Failure model | **terminal**: any read-side or write-side I/O error sets `failErr` once, releases all pending calls, and every later call returns it | **non-terminal**: one bad request does not poison the transport; only `Close` and 410 are terminal | **terminal**, isomorphic to stdio |
| Reverse RPC | answered inline on the read loop, so a handler must not call back into the same transport | separate goroutine, reply POSTed back; a handler may call back in | separate goroutine, same `maxPeerWorkers` (8) cap |
| Session | none | `Mcp-Session-Id` on ≤ 2025-11-25; none under 2026-07-28 | none |
| Resumption | n/a | `Last-Event-ID`, best-effort, once, never sooner than a `retry` hint asked for | **none** — the legacy binding never defined it, and silently replaying non-idempotent calls is worse than reconnecting cleanly |
| `Stderr()` | the child's last 4 KiB | `""` | `""` |

### Invariants

**Bounded reads run through both wire formats.** stdio uses `mcp.FrameReader`; SSE uses `sseScanner`,
its analogue — an event whose accumulated data exceeds the cap yields `ErrFrameTooLarge` rather than
buffering first, the error is sticky, an unfinished event at EOF is discarded, and comments and unknown
fields are ignored. `readBounded` applies the same cap to HTTP bodies.

**That bound covers bytes read, not memory allocated, and on the batch path the gap is large — owed,
not fixed.** `decodeMessages` materializes the whole array as `[]json.RawMessage` and sizes its output
slice from `len(items)`, both before `ParseMessage` validates a single element. A body already inside
`readBounded`'s cap expands by a large constant: a 16 MiB answer of `[1,1,1,…]` is 8.4M elements,
measured at ~750 MiB live. The reach is one caller — `readJSONResponse`, an `application/json` POST
answer — so the party who can trigger it is a hostile downstream server, never an upstream client.
Deleting the batch path is not the fix, since 2025-03-26 servers may still answer with an array; an
element cap, or a streaming decoder that validates and releases each element, is.

**The dial screen a caller injects is defeated by an environment proxy — owed, not fixed.**
`newHTTPClient` sets `Proxy: http.ProxyFromEnvironment` on the same transport it gives the caller's
`DialContextFunc`, so with `HTTP_PROXY` set the screened dialer is handed the proxy's address and the
proxy resolves the real destination itself. This package cannot decide it: standard library only means
no netguard and no policy of its own, so the answer has to arrive from outside the way the dialer does.
The other half is in [downstream.md](downstream.md).

**This client never reconnects to an SSE stream sooner than the server asked.** MCP 2025-11-25 makes
waiting out a `retry` field a MUST. `sseScanner.retryHint` keeps the value — ASCII digits only, and an
unparseable one leaves the previous hint standing rather than clearing it, which would be a reconnect
storm dressed as conformance. The two reconnect sites spend it differently because only one has a
caller waiting: the per-call resume takes the wait only when it fits the remaining deadline and
otherwise abandons the resume, letting the call report the stream break it already had; the out-of-call
stream loop honours the full hint, `max`-ed with its own backoff.

**`sseScanner` dispatches an empty data buffer**, which the spec drops. A bare `id:` line and a blank
line — how a resumable stream advances `Last-Event-ID` without sending a message — surfaces as a
`message` event with no data. `lastID` has to advance either way, and one always-dispatch rule beats
two paths that both have to remember to update it. The three consumers pay for it by skipping an
empty-data event before parsing; delete one of those guards and `ParseMessage` is handed an empty
frame, returns `ErrMalformedFrame`, and the reader tears down the stream on a keep-alive.

**A reverse-RPC reply's id is forcibly overwritten with the request id**, whatever the handler set. With
no handler registered the answer is method-not-found; a handler error or nil answer is internal-error;
a reply over the frame limit is replaced by an in-band internal-error, so the stream stays intact and
the server is not left waiting.

**Cancellation is always forwarded, best-effort.** All three implementations send
`notifications/cancelled` before returning the ctx error. A failed write is swallowed, and on the HTTP
side it runs on the transport lifetime context rather than the dead call context, with a 5s timeout.

**A malformed frame closes the connection, and the process never crashes.** The stdio and HTTP+SSE read
loops `fail(ClassUnavailable)`: a peer emitting garbage cannot be trusted to still be maintaining frame
boundaries. The streamable-http out-of-call stream merely ends and reconnects next time.

**HTTP status classification:**

```
410 Gone       → ErrEndpointMoved, ClassUnavailable, never retried or resumed, endpoint permanently poisoned
404 Not Found  → ErrSessionExpired, ClassUnavailable, clears the session id
429 Too Many   → ClassRetry + a Retry-After hint (delta-seconds or HTTP-date; unparseable = use the caller's backoff)
5xx            → ClassUnavailable: the request did reach the server, so non-idempotent calls must not be replayed
other 4xx      → ClassFatal: our request was rejected on its own merits
```

**410 is terminal for the transport.** Once `noteTerminalStatus` sets `moved`, every later call fails
immediately, the stream loop exits, and `Close` does not even send the DELETE. The meaning is "a human
has to change the URL in the configuration".

**Every destination a server names fails closed on cross-origin.** A server can name one two ways: the
legacy endpoint event and a 3xx redirect. The caller's headers, `Authorization` among them, ride every
POST and every redirected request, so both are validated with `sameOrigin` — scheme plus host including
port. Relying on net/http's own stripping is not enough: it drops `Authorization` only when the
redirect leaves the domain, so a subdomain or an `https`→`http` downgrade keeps the header. A
caller-supplied `HTTPConfig.Client` is left alone, which is why `internal/downstream` sets the same
policy on the client it builds.

**The endpoint event is recorded, because nothing else in the system sees it.** The legacy binding's
POST address is chosen by the downstream: it is in no configuration file and survives no restart.
`setPostURL` logs it once per connection — `warn` when it resolves to the stream URL itself, `debug`
otherwise — through an injected logger, for the same reason the dialer is injected. The record drops
the query rather than redacting it: this binding routinely carries the session id there, and `logx`'s
scrubber matches key names it knows, so `sessionId` would pass straight through. The write happens
before `ready` is closed, because closing `ready` releases the dial.

**The SSRF screen is injected, and if it is not injected it is not there.** `HTTPConfig.DialContext` and
`HTTPConfig.Client` are mutually exclusive — both is rejected as `ClassFatal` — so a protective dialer
cannot be silently dropped. Supplying neither means no address screening at all, reserved for tests and
explicitly trusted loopback endpoints. When the injected dialer refuses, it manifests as a dial failure
classified `ClassRetry`: honest, since nothing was sent, and harmless, since the guard fails closed.

**Protocol headers always override the caller's**, and `MCP-Protocol-Version` comes from the body
rather than transport state. 2026-07-28 requires the header to equal the `_meta` protocol version of
the same POST, and a server seeing them disagree MUST answer 400 with −32020. `versionForHeader` reads
it back out of the encoded params so the two cannot drift; transport state fills in only when the body
declared nothing. `server/discover` is what makes this load-bearing: it declares 2026-07-28 before any
version is negotiated.

**`Mcp-Name` is encoded before it is sent.** A value outside the header-safe ASCII set travels as
`=?base64?…?=`, because `net/http` sends a non-ASCII value as raw UTF-8, trims a padded one silently,
and refuses outright to send one containing a newline. Receivers decode before comparing; a
sentinel-shaped value that does not decode is refused rather than compared as a literal.

**Body snippets in error messages are bounded and flattened to one line.** `drainBody` reads 1 KiB and
`bodySnippet` turns `\n\r\t` into spaces: error strings end up in JSON log lines and trace frames, where
an embedded newline amounts to permitting a forged record.

**Concurrent reverse RPC has backpressure rather than unbounded fan-out.** The `maxPeerWorkers = 8`
semaphore blocks stream reading when full, making a flooding peer slow itself down.

**stdio's process reaping follows a strict order.** `os/exec`'s pipe contract requires `cmd.Wait` not to
precede stdout being fully read, so reaping hangs off the end of the read loop. `Close` fails all
pending calls, closes stdin, waits `killGrace = 3s`, kills on timeout, then cleans up. The process is
always reaped. `Stderr()` waits for that reap once the transport is known dead: `exec` copies stderr on
its own goroutine and only `cmd.Wait` guarantees the copy finished, so reading the ring early yields an
empty tail exactly when it matters most. The trigger is `failedErr`, not `readDone`, because a child
that dies instantly breaks the pipe before the read loop reaches EOF.

**`StdioConfig.Command` is resolved against `StdioConfig.Env`'s PATH, not this process's.**
`exec.Command` resolves through the calling process's PATH while `cmd.Env` is only handed to the child.
Those are the same PATH often enough that the difference goes unnoticed — until a caller repairs the
child's PATH, which `internal/downstream` does because launchd hands a GUI-launched process a
four-entry PATH, and every spawn still fails naming a PATH the child was never going to run with. Three
narrowings, all toward doing nothing rather than something different: on Windows the command is
returned untouched, as is a command containing a path separator or one with a nil `Env`; and an empty
PATH entry is skipped, the one deviation from `exec.LookPath`, because POSIX reads it as the working
directory and a spawn must not let a directory nobody named decide which binary runs.

Resolution happens after the spawn guard and makes no difference to it — the guard matches on the
basename — and a command that cannot be resolved fails `ClassUnavailable`, so a missing binary stays
breaker-visible rather than becoming fatal.

### The docker spawner

The spawn guard is anti-smuggling, not a sandbox; this half is where resource and namespace isolation
lives. It drives the docker CLI with `os/exec` rather than an SDK, partly because this package may only
use the standard library, partly because shelling out makes `DOCKER_HOST`, docker contexts and
credential helpers work automatically.

- **Everything off by default**: `--network none`, only explicitly declared mounts, read-only unless
  `Mount.Write` is set, and never `--privileged`, host namespaces or capability grants.
- **`ExtraRunArgs` must not restate a flag this file emits itself.** Docker's last-wins semantics would
  let a stray `--network host` erase the isolation defaults. **The comparison covers every spelling
  docker accepts**, because docker takes a shorthand's value attached to the letter: `--user 0:0`,
  `--user=0:0`, `-u 0:0` and `-u0:0` are one flag, and comparing text up to `=` refuses three of them
  while letting `-u0:0` run the container as root under a config that said `user: 1000:1000`.
  `ownsRunFlag` is fail-closed for every flag it recognises and fail-open past an unrecognised
  shorthand, since guessing whether an unknown letter takes a value would refuse working configs as
  docker grows; `spawnguard` inspects the assembled command line without consulting that table, so the
  residue is not the only gate.
- **Secrets never go in argv.** Container environment is passed as `-e NAME` with no value, inherited
  from the docker CLI's own environment. `ps(1)` sees argv; it does not see the CLI's environment.
- **`StdioConfig.Cwd` becomes `--workdir`**, not the CLI process's directory: "the directory the server
  runs in" is a path inside the image. An explicit `DockerConfig.Workdir` is more specific and wins.
- **`BuildDockerRunArgs` is pure and totally ordered** — mounts sorted by (target, source), env by name
  — so one configuration always produces one argv, pinned by a golden file.
- **Configuration is validated before any process starts**: the image must not start with `-` or
  contain whitespace, the container name must start with `agenthub-`, mount paths must be absolute and
  free of `:` (or a second flag is smuggled out of the value position), and memory, CPU and network
  names each have their own pattern.
- **Container names are unique per spawn.** Several gateway processes legitimately run the same server
  at once, so a fixed name would collide — the one place mcpproxy's "idempotent pre-cleanup by fixed
  name" recipe does not apply.
- **Cleanup is belt and braces**: `--rm` covers normal exit, `removeContainer` covers "the CLI died
  first", and failures are always ignored.
- **Failure diagnostics are folded into the stderr tail.** `diagnoseDocker` appends an `agenthub: …`
  line, rescuing "image doesn't exist" and "daemon isn't running" from a bare deadline-exceeded. Daemon
  cases are matched first, because a dead daemon also emits wording that looks like an image problem.
- **`DockerBinary` has a fallback path table**, because a gateway launched by launchd or systemd has a
  truncated PATH and Docker Desktop hides its CLI inside an app bundle. Once found, its directory is
  prepended to the child's PATH, where the credential helpers sit.
- **The unit tests drive a shell stand-in for the docker CLI**, which pins argv exactly but can never
  prove a container ran. `TestDockerRuntimeDownstream` in `test/e2e` is the one that does: it mounts the
  downstream binary at a path that exists only inside the container, so a regression that spawns on the
  host cannot answer at all. It skips itself when docker is absent.
