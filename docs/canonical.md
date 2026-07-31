# CANONICAL — the single source of truth for architectural conventions

This file registers **the things you don't get to change casually**: frozen identifiers, the
singletons, dependency directions, command naming rules, engineering conventions, and every matter
that has ever been seriously decided, along with its rationale. Changing this file means changing an
architectural convention.

It deliberately carries nothing you can read off the tree or the code. For how the system works see
[architecture.md](architecture.md); for flow timing [flows.md](flows.md); for per-package invariants
[modules/](modules/) — which is also where a debt already pinned to a line is recorded, in the doc of
the package that owns it.

**The section numbers are an interface.** Code comments cite them by number — `§2 rule 3`, `§5c #2`,
`§7 #4` — in well over a hundred places. Add sections, rewrite their contents, but do not renumber:
a citation that lands on the wrong ruling is worse than one that lands on nothing.

---

## 1. Frozen identifiers (ABI, unchangeable as of v1)

| Item | Value |
|---|---|
| Go module | `github.com/dinstein/agent-hub` |
| Remote repository | `git@github.com:dinstein/agent-hub.git` |
| Required binary | `agenthub` |
| Optional GUI binary | `agenthub-gui` |
| Data directory name | `AgentHub` (release) / `AgentHubDev` (dev, a sibling directory) |
| env prefix | `AGENTHUB_*` (stripped wholesale when spawning downstream) |
| Control socket | `<run>/ctl.sock`; on Windows `\\.\pipe\agenthub-ctl-<sha8(SID)>` |

The repository name `agent-hub` deliberately differs from the product and binary name `agenthub`; the
repository name is **not** part of the frozen set.

Data directory: macOS `~/Library/Application Support/AgentHub`, Linux
`${XDG_DATA_HOME:-~/.local/share}/AgentHub`, Windows `%APPDATA%\AgentHub` — the Windows resolution
(MSIX detection plus a loopback-UNC twin path) is implemented but **never verified on real hardware**;
[windows.md](windows.md) is the one place that tracks what does and doesn't work there.

**Channel separation is a property of the binary, not of an environment variable.** `main.channel`
defaults to `"dev"` and resolves to the `AgentHubDev` sibling; only a build made explicitly for release
resolves to the installed location. **Failure direction: a build that forgot to declare its channel
gets the dev directory.** Wrong in that direction costs one extra sandbox; wrong in the other burns the
one-shot OAuth refresh token in the user's real installation. An explicit `AGENTHUB_DATA_DIR` still
takes precedence over both.

---

## 2. Package layout

Which package holds what, and which layer it belongs to: [architecture.md §3](architecture.md#3-core-module-map)
and [modules/](modules/). The tree itself is `ls internal/`, which is faster and never goes stale.
What follows is only what you *cannot* read off the tree.

### There is exactly one of each

| The one … | Lives in | What "one" forbids |
|---|---|---|
| MCP protocol facade | `internal/mcp` (+ `transport`) | Any other package touching protocol implementation, and any third-party MCP library anywhere (rule 2 below) |
| execution pipeline | `internal/pipeline` | A second call path. Both direct calls and `call_tool` go through `pipeline.Execute`, and tests assert their gate counters match exactly; a new path must carry the same assertion |
| semantic-write implementation | `internal/confops` | The CLI and the control plane owning separate copies of "what it means to add a server". They are two frontends over one rule set |
| provenance for an exposed name | `router.RouteOf` | Splitting on `__` — a server id or a tool name may itself contain `__` |
| governance surface | the scope chain (§7 of architecture.md) | A second place that decides what a caller may see |

Four packages are **leaves on purpose**: `internal/tier` (the read/write/destructive vocabulary),
`internal/platform`, `internal/logx`, `internal/guard/*`. `tier` is the instructive one — five
packages need to say the word "read", and none of them should import another to do it. It used to live
in `pipeline`, which made the control plane import the data plane's execution package for a noun, and
that import turned rule 3's failing case into an **import cycle rather than a lint error**, leaving
the rule unprovable.

### Retired old names (they show up in early material; never use them)

| Old name | Canonical |
|---|---|
| `internal/controlapi`, `internal/control` | `internal/ctlapi` (DTOs and client live in the public `api` package) |
| `internal/vault` | `internal/secrets` |
| `internal/secure/{integrity,injection,ssrf,audit}` | `internal/guard/*` — but only `ssrf` survived, as `netguard` |
| `internal/integrity`, `internal/approval`, `internal/audit` | **Nothing.** Removed with the runtime governance surface rather than renamed, so there is no forwarding address: fingerprint pinning, the approval queue and the per-call ledger are gone, not relocated. `internal/audit`'s one surviving primitive — the multi-writer JSONL append — was extracted first and is `internal/jsonl` |
| `internal/gatewaymode` | `internal/gateway` |
| `internal/downstream/transport` | `internal/mcp/transport` |
| `package skill` | `package skills` |
| `agenthub tool ls` / `tool inspect` / `tool allow` | `agenthub server tool …`. The hidden shim (`newToolShim`) shipped in v0.14.0, and v0.14.0 **is** the one release it was granted — it goes before the next one is cut. "One release" dates nothing on its own; the sibling row below is what it should read like |
| `agenthub tool allow <server> a b` (positional) | `agenthub server tool allow <server> --only a,b` |
| `agenthub tool allow <server>` (bare = block all) | `agenthub server tool allow <server> --none` |
| `agenthub tool allow <server> --clear` | `agenthub server tool allow <server> --all` |
| `agenthub profile tools <profile> <server>` | `agenthub profile tool allow …` (the one-release shim shipped in v0.14.0 and is gone; `tools` now aliases the group, per §3) |
| the execute pipeline living inside `internal/gateway` | a standalone `internal/pipeline`; `gateway`/`daemon` only do assembly |
| `catalog.Snapshot` (tool catalog snapshot) | `router.Catalog`; `internal/catalog` is the curated *server* catalog |

### Hard dependency-direction constraints (enforced at compile time by depguard)

1. `cmd/agenthub-gui` and `api` **must not** import any `internal/*`.
2. `internal/mcp` **depends only on the standard library** — no third-party module at all (ruling #32);
   no other `internal/*` package may import any third-party MCP library.
3. `internal/pipeline` must not import `internal/ctlapi` (the data plane does not depend on the
   control plane).
4. `internal/mcp`, `internal/platform`, `internal/logx` and `internal/guard/*` are
   zero-business-dependency foundations.

These are CI failure conditions, not review conventions, and each one must have a failing case that
proves it actually bites (§6).

### On constraint #2: the MCP protocol facade is entirely first-party

No `go get` of `modelcontextprotocol/go-sdk`, `mark3labs/mcp-go`, or any other MCP library.

Rationale: bounded reads (16MB), `notifications/cancelled` forwarding, inline replies to reverse RPC
and the trailing 4KB of stdio stderr are protocol-layer invariants that need precise control, while
JSON-RPC encoding itself is not much work — not worth tying to an external project's release cadence.
The point of the facade is to keep that choice **reversible**: if the implementation is ever swapped,
the change is sealed inside one package, rather than borrowing one now.

---

## 3. Command naming rules

- **Resource groups are singular as the canonical name, with the plural as a cobra alias**:
  `server` / `profile` / `client` / `session` / `skill` / `secret`
- **Action/flow groups stay as they are**: `daemon`, `connect`, `auth`, `activity`, `events`,
  `config`, `doctor`. The OAuth group is **`auth`**, not `oauth`
- List subcommands are always `ls`, and **every command supports `--json`**, with human and machine
  output rendered from the same data structure
- **There is no top-level `tool` group.** A tool is something a server contributes, and the rule
  deciding which of them it contributes lives on the server entry beside `enabled` — so the group
  hangs off `server`: `server tool ls` / `server tool inspect` / `server tool allow`. While it was at
  the top level it was also in the withheld group, which shipped a global allow list with no
  advertised way to read or write it
- **The two narrowing layers are one command at two altitudes**: `server tool allow <server>` and
  `profile tool allow <profile> <server>`, both taking `--only a,b | --all | --none`. They
  intersect and neither can widen. Spelling them differently — which they were, `tool allow` with a
  positional list against `profile tools --only` — invites a second mental model of one mechanism,
  and the pair that must not be confused (`--all` drops the rule, `--none` stores the empty list) is
  exactly the pair a second model gets backwards
- **There is no `scope` group.** Narrowing is what a profile *is*, so it lives on `profile`
  (`profile server` / `profile tool allow` / `profile discovery`), and handing a surface out lives on
  `client` (`client bind <client> <profile>` / `client unbind` / `client ls`). The retired group maps
  one-to-one: `scope set --client X --profile P` → `client bind X P`, `scope clear --client X` →
  `client unbind X`, `scope ls` → `client ls`
- **A live session cannot be renarrowed.** What a client sees is decided before it connects, so
  `session` is a read-and-terminate group (`ls` / `show` / `kill`) and the way to change a surface is
  to edit the profile
- The `client` group is `ls | detect | inspect | connect | disconnect | bind | unbind`. `detect` stats
  and `inspect` reads — that is the whole distinction (macOS TCC, see `internal/clients`); `ls` gives
  the connect and the bind answers per client. There is no `import`: a client's existing servers are
  brought over by pasting its configuration
- The `skill` group is `add | ls | inspect | rm | enable | disable | install-to | sync | update |
  verify` (`install-to` materializes one entry, `sync` materializes in bulk by scope; both coexist)

### `add` and `enable` are separate primitives, and stay that way

`server add` writes the definition and **nothing else**: no connection, no probe, and the entry lands
**disabled**. `server enable` puts a server into service, and that is where the connection probe lives.

They answer different questions. `add` records what a server IS — pure configuration, no network,
deterministic, safe to script against a downstream that is unreachable right now. `enable` declares
that the operator wants to USE it, the only point at which "can we reach it?" is worth asking. Folded
together, `enabled` would mean both *the user wants this* and *it answered a probe*, and a downstream
that was merely mid-deploy at add time becomes indistinguishable from one that was never added.

Two consequences that must not be "simplified" away:

- **The probe reports; it never vetoes.** The enable always happens. A server that needs a login is
  enabled and says so. Refusing would strand an entry the operator explicitly enabled and turn a
  transient outage into a configuration change.
- **Composition belongs to the caller.** `catalog add` offers one action over the two operations, and
  `auth login` enables the server it just authorized (which is what keeps the OAuth path two commands
  rather than three). The primitives underneath stay separate.

---

## 4. Known capability boundaries

Not a to-do list — an **honest grading of the current implementation**. When you touch the related
code, know which tier you are standing on.

| Item | Status |
|---|---|
| Windows | Platform layer filled in: `LockFileEx` cross-process locks in every flock package, named-pipe listener with SDDL (owner-only), api dialing, GUI channel wiring, and portable zip packaging. CI gates on `GOOS=windows` build + vet. Two gaps remain above it — `daemon stop` has no process control, and no client has a user-level config path. **Never run on a real Windows machine.** [windows.md](windows.md) tracks all of it |
| GUI | Functionally complete, **not part of the default build** (the webview needs GTK/WebKit, which CI runners lack); `make gui` |
| skills materialization | **Client granularity only**, not per-session — the files live outside agenthub's read path |
| skills from git sources | Records and pins a revision, but **never runs git and never touches the network**; update without a local checkout returns a typed unsupported error rather than claiming you are up to date |
| TOON | A **one-way display projection with no decoder** (§7 #4); anything requiring a round trip never enters the encoder |
| teams | Deliberately unimplemented, and **nothing in the tree reserves a seam for it**. There used to be a `policy` layer holding an `Effective()` hook for a forced team policy; it went with the rest of the runtime governance surface, and a hook nobody can reach is worse than none — it reads as a decision already half-made |
| Telemetry / update checker | Decided against (§7 #6) — no data is collected |

### Three things that must never be retrofitted

Each was nearly left out, and each would be disproportionately expensive to add later. They are in
place; keep them that way.

1. **The composite vault key** `(serverID, scopeName)`, defaulting to `"_global"` — retrofitting it
   would touch every singleton in the token store, callback server and refresh coordinator.
2. **Registry self-write suppression** (§5c #1) — without it every self-write triggers a pointless
   reload cycle.
3. **X-Request-Id end to end**: the response header is written before the handler runs, error bodies
   carry it, and the log line for the failure carries the same value.

---

## 5. Reference-code policy

| Source | License | Usage |
|---|---|---|
| [smart-mcp-proxy/mcpproxy-go](https://github.com/smart-mcp-proxy/mcpproxy-go) | MIT / Go | **Reference only, never copy code** |
| [tsouth89/toolport](https://github.com/tsouth89/toolport) | MIT / Rust | A different language; likewise a design reference only |

What is inherited is the **list of problems**: which edge cases exist, what failure looks like, what
the correct behavior is. Every implementation is written from scratch, because this project has its own
coherent conventions — the `internal/mcp` facade, the generic `Doc[T]` envelope, a per-server owner
goroutine plus `calls chan` serialization, content-addressed `EffectiveScope`, failure-direction
comments — and pasted-in foreign implementations tear at them. The root `NOTICE` records the design
references (academic honesty, not a license obligation).

---

## 5b. MCP protocol scope

**Two protocol generations are spoken, and on both faces.** `mcp.SupportedVersions` is the list,
newest first — `2026-07-28`, `2025-11-25`, `2025-06-18`, `2025-03-26` — and it is the only place
either direction reads what it accepts.

- The **read side** (connecting to downstreams) probes with `server/discover`, and falls back to the
  `initialize` handshake only on proof the server answered. A downstream speaking an older version
  negotiates downward from the same list
- The **exposure side** (the gateway facing upstream clients) answers `server/discover` with that
  same list, and a request carrying the per-request `_meta` puts the session in stateless mode.
  `initialize` negotiates the **stateful family only**: a client declaring `2026-07-28` *there* is
  answered with the default instead, because echoing it would promise per-request `_meta` semantics
  on a session that just used the handshake 2026 removed
- Transport support stays asymmetric by direction: the read side speaks `stdio` +
  `streamable-http` + **legacy HTTP+SSE**; the exposure side speaks `streamable-http` only — no new
  SSE exposure surface
- **Neither generation is offered an out-of-band notification stream on the HTTP face.**
  `subscriptions/listen` — 2026's replacement for the GET stream — lands in the dispatch default, so
  a conforming client reads it as "this server offers no stream", which is the stance already frozen
  for the stream it replaces. The stdio face pushes notifications inline, as it always has

**`mcp.ProtocolVersion` does not name the version this tree targets, and flipping it to 2026 is
wrong.** It stays at `2025-11-25` because every context that reads it is definitionally pre-2026 —
the legacy handshake, the exposure side's default answer, the HTTP header sent before negotiation.
The 2026-07-28 declaration travels per-request in `_meta`, built from `Version2026` directly. The
constant carries this warning too; [mcp-2026-07-28.md](mcp-2026-07-28.md) §6.1 records why the
original "flip the constant" plan was dropped, and its §7 what is deliberately still absent.

### Upstream deprecation tracking

The removals themselves are all no earlier than 2027-07-28, and every seam is already in place —
which is what makes the 2026-07-28 column read as history rather than as a plan. Every use site
carries a `// DEPRECATED-UPSTREAM(<feature>, earliest-removal: <date>)` comment, so one grep finds
them all.

| Feature | Deprecated in | Dependency point | Migration seam |
|---|---|---|---|
| The `initialize` handshake | `2026-07-28` | The stateful session path, on both faces | **Landed**: `server/discover` plus per-request `_meta`. The handshake stays, because a downstream that speaks only the older generation still needs it |
| `ping` | `2026-07-28` | Liveness on the stateful path | None needed — a stateless request carries its own context, so there is no session to keep alive |
| Roots | `2026-07-28` | `${ROOT}` and derived-instance keying (`internal/downstream`). The dependency **shrank** when the per-project scope layer was retired: the root no longer selects anything and has left the resolver's cache key | **In place since M0**: `RootSource`, with one implementation for the roots protocol and one for an explicit root in `clients.json` |
| Sampling | `2026-07-28` | One of the isolation arguments | None needed (the conclusion is independently supported by credentials, connection parameters and fault isolation) |
| DCR | `2026-07-28` | The OAuth discovery chain; DCR credentials persisted alongside tokens | **In place since M1**: `ClientRegistrar`, with one implementation for DCR and one for Client ID Metadata Documents |
| Logging | `2026-07-28` | Downstream log forwarding | None needed (the logging surface is first-party anyway) |
| HTTP+SSE transport | `2025-03-26` | One of the three transports | Kept on the read side; no new exposure side |

---

## 5c. The config hot-reload path (two things not to get wrong)

GUI/CLI edits a profile → the corresponding gateway updates automatically. The path is in
[flows.md](flows.md) §4 and the mechanism in
[modules/foundation.md](modules/foundation.md#the-generation-criterion-self-write-suppression-and-the-two-watch-channels);
these two rulings are what the mechanism exists to satisfy.

1. **Self-write suppression.** When the daemon writes `profiles.json` itself, fsnotify reports the
   event just the same, so the payload is registered in a bounded TTL set *before* writing and the
   watcher ignores anything that hits it. Without it, every self-write costs a reload cycle.
2. **The generation criterion is "the generation read ≥ the generation applied"**, never "equals the
   event's `Rev`". `Change{Kind, Rev}` is a **notification** carrying no snapshot, so the gateway
   re-reads the file itself; an equality test leaves several rapid successive writes stuck on an old
   version, waiting for an event that will never come again.

---

## 5d. Collaboration conventions

**They live in [AGENTS.md](../AGENTS.md), not here** — worktree per feature, a draft PR per branch
kept current and closed by the landing, one commit per subtask, `main` stays linear (rebase, never
merge), `make ci-landing` **after** the rebase, `--ff-only` as the enforcement.

This section used to carry its own copy, and the copy was the older rule: *commit directly to `main`,
push as soon as each commit is done*. That is the dangerous direction for a contradiction to fall in.
This file advertises itself as the authority on whether a convention may change, so an agent that
starts here and stops reading commits straight into the main work tree — which is what the worktree
rule exists to prevent, and no review after the fact puts the history back.

Development workflow is not an architectural convention: it says nothing about the shape of the code,
it changes for reasons the code never sees, and the file every coding agent reads first is `AGENTS.md`.
One home, and that is the one.

---

## 6. Toolchain and engineering conventions

- **Go 1.26+**; **LICENSE: MIT** (`Copyright (c) 2026 dinstein`)
- `cobra` (CLI), `fsnotify` (watch), `zalando/go-keyring` (keyring), `log/slog`
- `golangci-lint` (**v2 config format**) + **depguard** (which pins §2's four constraints), plus
  `gofmt` and `goimports` — declared under `formatters:`, a section separate from `linters:`, and no
  less a CI failure for it
- CI: GitHub Actions, a macOS + Linux matrix running build / test / lint

### Conventional paths

| | |
|---|---|
| Fake downstream MCP server | `internal/testutil/fakemcp` |
| Where reference repositories are cloned | `~/Develop/_refs/` (**outside the repository**, to keep git history clean) |
| Per-server log file | `<data>/logs/server-<name>.log` |

### Every depguard constraint must have a failing case

A violating sample plus a test that runs lint and asserts it fails (`internal/depguardtest`, which
plants its probes in a disposable copy of the checkout). **A lint rule that is configured but not in
effect is more dangerous than no rule at all.**

The same standard applies to the proof itself: when golangci-lint is absent the proof calls `t.Skip`,
`make test` reports that skip as success, and **a skipped proof is not a proof** — so the CI gate greps
the verbose output for `--- SKIP` and fails on it. The rule generalizes beyond depguard: a check that
can quietly not run is a check you do not have.

### Test infrastructure

A programmable **fake downstream MCP server** (`internal/testutil/fakemcp`) that injects by script:
slow responses and timeouts, half-written and malformed JSON-RPC frames, oversized payloads (hitting
the 16MB bounded read), crashes during the handshake, `tools/list_changed` storms, protocol violations.

Three classes of test have been in CI from day one:

1. **Golden tests** — the signature grammar, search ordering, error copy. **Determinism is the
   contract**: agents key retry logic and prompts off exact wording, so error text and ordering are
   frozen artifacts, not cosmetics.
2. **Cross-process concurrency tests** — single-line `O_APPEND` writes (`internal/jsonl`, which
   re-executes the test binary as N appending children and asserts no line was torn), monotonic
   generations.
3. **Daemon `kill -9` injection tests** — the stdio data plane is unaffected.

---

## 7. Decision records

The items that were once "to be decided". **All are decided**; each is registered here so none gets
silently reopened, and the numbering is cited from code.

1. ~~Whether to pull lazy-connect forward into M1~~ → **Decided (M2): no.** Keep eager connect plus
   "answer from cache first" fast startup.

   Three reasons, in order of weight. **The original motivation was solved by something else**: lazy
   connect was meant to fix the N×M process cost of one process per client × one instance per server,
   and what actually solves that is the daemon's streamable-http shared pool. **The cost lands in
   exactly the wrong place**: a cold npx/uvx cache takes 10s to minutes on first start (hence
   `DefaultConnectTimeout` at 120s), and eager connect spends that in the gateway's startup window
   where `tools/list` is already answered from cache and the agent never blocks — lazy connect moves it
   into the middle of the first `tools/call`, where it reads as an unexplainable hang inside an agent
   turn and races the client's own timeout.

   The escape hatch stays open — the seam is `downstream.Deps.Dial` plus the tool cache — and the
   trigger should be measured process/memory cost, not a derivation.
2. ~~On-disk cache for shaping: bbolt or plain files~~ → **Decided: plain files.**
   `<data>/cache/shaping/<sha256(owner)>/<cursor>.json`, atomic writes (temp file in the same
   directory → 0600 → fsync → rename) + TTL sweeping + a sweep at startup. Zero new dependencies is
   an established style here; the access pattern is single-key point lookup with no queries, no
   transactions and no cross-key consistency; and a corrupted entry costs exactly one cursor, whereas
   a single-file database would need a recovery mechanism to offer the same property.
3. ~~Wails3 version and frontend stack (v3 is still alpha; we need a fallback)~~ → **Decided (M1-G):**
   `wails/v3 v3.0.0-alpha2.118` + vanilla TS + Vite (`@wailsio/runtime` is the only frontend runtime
   dependency). The fallback plan is not "switch frameworks", it is **compressing the alpha dependency
   down to two files**: only `cmd/agenthub-gui/gui_main.go` and
   `cmd/agenthub-gui/services/service_wails.go` (~50 lines of assembly) depend on Wails, both behind
   `//go:build wails`, while the service body `services/hub.go` carries no tag and so compiles, vets
   and unit-tests in CI. A breaking alpha change edits those two files; page logic and the `api` layer
   are untouched. The frontend also skips `wails3 generate bindings` in favour of `Call.ByName` plus
   `Events.On` — one fewer generated artifact to drift. Details in [modules/gui.md](modules/gui.md).
4. ~~TOON grammar scope and the golden case set~~ → **Decided (M1.5).** Both "determinism is the
   contract" grammars are frozen, each with its own golden corpus:

   **(a) TOON is a one-way projection: no round trip, no decoder.** A round trip would need a type
   marker on every scalar (bare `1` vs `"1"`, bare `true` vs `"true"`) — exactly the tokens the
   encoding exists to save. The contract is stated in-band instead, a first line reading
   `#toon/1 (display encoding; send tool arguments as JSON)`. Anything that must survive a round trip
   (`structuredContent`, tool arguments, cursors) stays JSON and never enters the encoder. Two
   constructive guarantees: **never-larger** (below `MinSavingsPct`, default 10%, the input is returned
   unchanged, so callers never compare sizes themselves) and **numeric fidelity** (no value is routed
   through float64).

   **(b) The compact signature grammar** — `name(p1:str, p2?:int=3, p3?~:obj{a,b}) -> str` — carries
   `?` for optional and `~` for lossy, and is what search hits return *instead of* a schema; the agent
   calls `describe_tool` for detail.

   The grammars themselves are frozen in the package docs, next to the golden corpora that actually
   pin them: `internal/shaping/toonenc` (`testdata/*.toon`) and `internal/discovery/toolsig`
   (`testdata/signatures.golden`). Two ordering invariants that are *not* local to those packages, and
   so live here: `shaping.ShapeResult` **re-encodes first and applies the budget second** (the budget
   is spent on the cheaper representation, and the truncation trailer is therefore always last), and
   re-encoding sits at the very end of the delivery path, so nothing downstream of it can invalidate
   the budget the truncation trailer describes.

   Two-stage describe is part of the same ruling: the meta-tools are **five, in a frozen order**
   (`status, search_tools, describe_tool, call_tool, fetch_result`); `describe_tool`'s visibility
   predicate is exactly `Surface.byExposed`, so it is structurally impossible for it to be wider than
   search/tools_list/call; and it emits **one per-id error only, `not_found`** — nonexistent, out of
   scope, and left out of an allow list all share the copy, because differentiated errors would make
   it an enumeration oracle. Same rule as `fetch_result`.
5. ~~A workable story for macOS keychain ACLs and unsigned development binaries~~ → **Decided (M1):
   yes, dev mode falls back to `secrets.enc` automatically.** Every `go build` produces a new unsigned
   binary, so the keychain ACL prompts again each time; when keyring availability detection fails, or
   `AGENTHUB_DEV_SECRETS=1` is set, writes fall back to `secrets.enc` with a 32-byte key generated and
   persisted beside it (`secrets.enc.key`, 0600).

   The honest grading is in the package comment: **a key next to the ciphertext is obfuscation, not
   encryption at rest.** That holds for the dev fallback only; production uses `AGENTHUB_SECRET_KEY` or
   the OS keyring. The detection itself must **read, never write** (a `Set` probe triggers macOS's
   destructive confirmation dialog), caches per process, and gives every operation a hard timeout — a
   stuck keychain dialog would otherwise hang the caller.
6. ~~Whether to build telemetry and an update checker~~ → **Decided: neither.** AgentHub collects no
   data: no telemetry (not even enum-only reporting, and no opt-in switch) and no update checker (no
   channel probing, no network request at startup).

   This process holds every downstream credential, every tool-call argument and result, and the user's
   project paths. A channel promising "enums only" needs a `ScanForPII` gate to keep the promise, and
   the stronger the promise the higher the maintenance cost — whereas not opening the channel costs
   nothing and cannot degrade. "We collect no data" is also verifiable on the spot with an empty packet
   capture, which "we only collect anonymous enums" is not; for a security product that is the best
   available use of the trust budget. An update checker, meanwhile, either adds a round trip to a
   startup path that runs once per client process, or needs a resident prober — and version comparison
   is the package manager's job.

   Implementation constraint, equivalent to a CI-checkable property: **there exists no** outbound
   request anywhere in `internal/*` to an agenthub-owned domain or version manifest. Network egress
   falls into exactly three categories — downstream MCP servers, OAuth authorization servers, and
   endpoints the user configured explicitly. **Adding a fourth violates this decision.**
7. ~~Where the tool commands live, and how the two narrowing layers are spelled~~ → **Decided: under
   `server`, and identically.** `server tool ls | inspect | allow`, with `server tool allow <server>`
   and `profile tool allow <profile> <server>` taking the same `--only | --all | --none`.

   Three faults, one cause — the command tree disagreed with where the rule is stored. The rule is a
   field on the server entry beside `enabled`, so **`tool` at the top level was in the withheld
   group**: a shipped build carried a global allow list it never advertised a way to reach.
   **`tool ls` applied no allow list at all**, so the rule's only possible reader disagreed with the
   rule — and it had no other reader, since `server ls` does not carry the field and `server inspect`
   does not print it. And **a bare `tool allow <server>` meant "expose nothing"**, one forgotten
   argument from the opposite of the intent, silent afterwards, and spelled unlike the same edit one
   layer up.

   What must not be re-simplified: the two layers stay ONE vocabulary. They are an intersection, and
   the pair that decides whether it fails open (`--all` drops the rule, `--none` stores the empty
   list) is precisely what a second spelling gets backwards. There is still no `deny` verb at either
   altitude — allow and deny answer the arrival of a tool the downstream adds tomorrow in opposite
   directions.

---

## 8. Historical ruling ids

Around sixty comments cite a ruling by a number from the original design document — `ruling #8`,
`A.1 #8`, `A.6 #5`. **That document is not in this repository**, so until this table existed those
citations resolved to nothing: a reader met an id that looked authoritative, had no way to look it up,
and could only guess whether the rule still held.

They are kept rather than deleted because the ids are *stable* and the rules are *live*. A ruling
number does not move when a section is renumbered or a paragraph rewritten, which is exactly the
property a citation wants — it just needs somewhere to resolve. This table is that somewhere, and it is
the reason each id may still be written.

**`A.6 #N` is `§7 #N`.** The appendix's six open questions are the six decision records above, in the
same order, so `A.6 #3` and "§7 item 3" name one ruling. Prefer the `§7` spelling in new comments.

Two conventions, so this table cannot quietly rot: the bare `#N` and the `A.x #N` spellings of one
ruling are one row, and **a number not listed here may not be cited** —
`TestHistoricalRulingIdsResolve` fails on an unregistered id. Milestone *task* numbers (`M0-7`,
`M1-3`) are not rulings and are not citable at all: they named a unit of work in a plan that is also
absent, they carry no rule, and the module doc for the package says everything the task number was
standing in for.

| Cited as | What it ruled | Where the rule lives now |
|---|---|---|
| `#7`, `A.1 #7` | Two id shapes on purpose: the human `client:seq` for the CLI, a random token for the protocol — CLI ids are for typing, protocol ids are for not guessing | modules/config.md |
| `#18` | Lazy mode's `call_tool` may split into read/write/destructive **intent variants**, and compatibility mode stays byte-identical to the pre-variant surface | architecture.md §8; modules/dataplane.md |
| `#27` | **Determinism is the contract**: goldens pin the wire shape; fix the code, never the golden | §6 |
| `#29` | Legacy HTTP+SSE is a **read-side** transport only, never offered on the exposure side | §5b |
| `#32` | `internal/mcp` is standard-library only — one first-party protocol facade | §2 rule 2 |
| `A.2 #9` | The manual paste loop, for providers that cannot reach a loopback redirect | modules/oauth.md |
| `A.2 #10` | Refresh is serialized: daemon singleflight online, a file lock offline | modules/oauth.md |
| `A.3 #1` | Cross-process shared state takes a **file lock** or an atomic rename, proven by an N-process acceptance test. Its original subject — quarantine and pin writes — was removed with `internal/integrity`; the rule outlived it and now governs the rate-limit counters and the registry | §6; modules/foundation.md |
| `A.3 #2` | `kill -9` on the daemon: the stdio data plane is untouched and gateways re-register | §6; flows.md |
| `A.3 #4` | A daemon restart makes the session overlay vanish on **both** sides. Retired by its own logic taken further: a session now carries no scope of its own at all, so there is nothing left to survive a restart or to fail to | modules/config.md |
| `A.3 #5` | skills materialization is **client-granular**, never per-session | §4 |
| `A.5 #23` | Windows is confined to a seam inside `internal/platform`; nothing outside it branches on the platform | §4; windows.md |
| `A.5 #26` | The **composite vault key** `(serverID, scopeName)` from day one | §4 ("never retrofitted", item 1) |
| `A.5 #30` | The roots **migration seam** (`RootSource`), in place before upstream deprecation | §5b (deprecation table) |
