# The test infrastructure

> **Answers** what the fake downstream can be made to do, how the dependency rules are proven, and what the e2e suite drives from outside.
> **Not here** which command to run → [AGENTS.md](../../AGENTS.md); the rules themselves → [canonical.md](../canonical.md).
> **Kept true by** itself: every claim below is a test, and the ones that could pass vacuously say so.

## internal/testutil/fakemcp

A programmable fake downstream MCP server. Every concurrency and security invariant in downstream, router,
pipeline and gateway was tested against it.

`Script` is **pure data** — it round-trips through `json.Marshal` exactly — so the same fault script can
reach a subprocess through one environment variable. Inbound messages match an ordered set of rules by
method name, optionally by Nth invocation, **first match wins**, and a matched rule's actions replace the
default handling.

Three drivers: `Serve` (the interpreter), `Connect` (in-process, using OS pipes rather than `io.Pipe`,
because kernel buffering preserves the non-blocking best-effort writes of a real transport), and
`MaybeServe()` + `StdioConfig()` (subprocess, re-execing the test binary); plus a standalone binary for
spawn tests wanting a dedicated executable.

**Fault injection primitives**: slow responses, never responding, half frames, malformed frames, frames
beyond 16 MiB, crashing mid-handshake, `list_changed` storms, protocol violations, stderr noise, and a
version mismatch from the script's declared protocol version. A half frame **suppresses all subsequent
scripted writes**, because the stream is already poisoned mid-frame.

**The interpreter executes strictly in order**: one message is fully handled, sleeps and storms included,
before the next frame is read, so scripted writes never interleave inside a frame.

**It never panics on hostile input**: malformed inbound frames are ignored, `Serve` returns nil on client
EOF or a scripted crash, the ctx error when cancelled mid-sleep, and a non-nil error only for interpreter
misuse or an unreadable input stream.

**The same script means the same thing under both drivers.** `Connect`'s transport deliberately mirrors the
internal stdio transport — which has no exported in-memory constructor — down to dispatch by id, the error
class on stream failure while preserving the sentinel for `errors.Is`, best-effort cancellation forwarding,
inline peer request replies, a 4 KiB stderr tail and an idempotent `Close`. `test/e2e` wraps the same
interpreter in a Streamable HTTP frontend, so a stdio fault script means the same thing there and there is
no second fake server to maintain.

**Like all non-`internal/mcp` code that speaks MCP, this package uses only the facade and the standard
library.**

## internal/depguardtest

Prove the four dependency constraints really do block, rather than merely being documented.
`TestDepguardRulesActuallyFire` injects a violating probe into each constrained package, runs
`golangci-lint` on that package alone, and asserts depguard reported a violation. Each rule also gets a
**control** — the same package without the probe — which must lint clean. Six cases, because each rule has
its own configuration and testing one would let the other rot.

**Probes are written into a disposable copy of the checkout, never into the checkout itself.** The real
tree is being *built* while this test runs — `go test ./...` runs package binaries in parallel and the e2e
suite shells out to `go build` — so a build that lists a constrained package between a probe's creation and
its removal dies with `no such file or directory`. Not hypothetical: it is how this proof turned the Linux
CI job red, and hammering `go build` alongside the old in-tree version fails 6 builds out of 25. The copy's
path is derived from the real root rather than random, because golangci-lint caches by absolute path.

**Inside the copy each probe is still removed by `t.Cleanup`** even when the test fails, which is what lets
each control case lint clean immediately afterwards. **The real tree being read-only is asserted, not
merely intended**: a walk afterwards fails on any leftover, naming the cause instead of resurfacing as an
unrelated flake in the e2e suite.

**Every package a probe imports is in `go.mod` and type-checks**, so a lint failure can only come from
depguard and never from the compiler; the assertion also requires the word "depguard" in the output.

**If `golangci-lint` cannot be found it skips with an actionable hint** rather than failing, and CI installs
it before `make test` so the proof really runs there — which is why CI greps its own log for `--- SKIP`.
The override variable is authoritative: a nonexistent path skips rather than falling back, which makes the
skip branch itself testable. A second line of defense requires the probe pattern in `.gitignore`, so a
crashed run's leftover is never committed.

## test/e2e

Pin the full chain with real processes: `TestMain` compiles the real `agenthub` and `fakemcp` binaries,
then drives them from the directions a user does.

**Four axes, and a file belongs to one of them.**

| Axis | Drives | Why it exists |
|---|---|---|
| client | spawns a gateway and speaks MCP to it | `gatewayClient` is a hand-written stdio client using only `encoding/json`, so the wire format is verified from the outside |
| operator | runs CLI verbs against a registry | where the verb's contract is about a running gateway it keeps one alive and asserts on the exposed surface, because a registry edit nothing propagates is the failure worth catching |
| agent | a real `daemon start --http-addr` and a bearer token | the only axis on which a credential exists at all, so the only one where the tier gate can fire |
| frontend | a real `api.Client` over the real control socket | the route the GUI writes by, and the only one carrying the optimistic-concurrency precondition |

The agent axis reserves and releases its port before the start rather than discovering it afterwards,
because **the daemon reports its bound data-plane address nowhere a caller can read it** — neither
`run/daemon.json` nor `daemon status` carries it, which is a real gap for an operator and not only for this
suite.

The frontend axis **may import `api`** — it is the published surface and imports nothing under `internal/`,
so the client here is the one a third-party embedder gets. Its assertions land on the *other* routes on
purpose: a live gateway must follow the write, and the CLI must read the same entry back out of the files
with no daemon in the path.

**The observability streams are read back by a process that did not write them**, which is the only way any
of their claims can be checked. Two conventions there are worth copying: a disclosure assertion is made
against the **raw** command output rather than a decoded struct — a payload leaking through a field the
test does not model would be invisible otherwise — and a selector is asserted to **exclude** the other
side's marker as well as to include its own, because a selector that was never applied satisfies a presence
check just as well as a working one.

**Credential paths are pinned to the encrypted file, never the OS keyring.** Without that, `secret set` on
a developer's macOS would write the real login keychain and prompt for it; the suite has no way to answer
that dialog and no business creating the entry.

**No test can touch a real user's registry.** `testEnv` strips every `AGENTHUB_*` variable and points
`AGENTHUB_DATA_DIR` at the test's own directory. `XDG_RUNTIME_DIR` is **deliberately inherited rather than
stripped**: it used to have to be stripped, because on Linux it alone determined the run directory and all
concurrent e2e daemons shared one control socket — but `AGENTHUB_DATA_DIR` now moves the run directory too
([platform.md](platform.md)), so passing the variable through is how that rule gets proven end to end on a
CI runner. Stripping it would hide the one environment shape where the rule bites.

**"The daemon really is dead" must be proven, not assumed.** `killDaemonStrict` requires `daemon.json` to be
readable and fails loudly otherwise — that ambiguity cost three rounds of CI — and `assertSocketRefuses`
further proves nothing is still serving the control socket, because a gated call may only be counted as
failing closed once that holds.

**Lazy mode's readiness signal is different**: tools never appear in `tools/list`, so the helper polls
`search_tools`. **Frozen ABI is written out here rather than imported**: the meta-tool list and order live
directly in the test, because this suite drives the gateway from the outside and that surface is exactly
the kind of ABI an external client depends on.

**Three cases self-skip, and they are the ones that need something this machine may not have**: the real-npx
case, the docker-runtime case, and the daemon-restart case, which skips under `-short` because it waits out
a 30s re-register ladder. Everything else always runs.

**An absence is asserted only after the same thing has been seen to arrive.** A test that expects a tool
*not* to be exposed polls for the positive case first and only then holds the negative for a fixed budget.
Asserting straight away would pass identically against a scope that was never applied, which is the failure
being looked for.

**Preconditions must fail hard, never silently return.** A silent skip disguises an environment difference
as some other component failing. For a hang, the suite's timeout SIGQUITs the process under test and folds
the goroutine stacks into the failure message.
