# CANONICAL — the single source of truth for architectural conventions

This file registers **the things you don't get to change casually**: frozen identifiers, package
layout, dependency directions, command naming rules, engineering conventions, and every matter that
has ever been seriously decided, along with its rationale. Changing this file means changing an
architectural convention.

For how the system works see [architecture.md](architecture.md); for flow timing see
[flows.md](flows.md); for per-package detail see [modules/](modules/); for debts already pinned to a
line are recorded in the `modules/` doc of the package that owns them.

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

The repository name `agent-hub` deliberately differs from the product/binary name `agenthub` — the
repository name is **not** part of the frozen identifier set.

Data directory: macOS `~/Library/Application Support/AgentHub`, Linux
`${XDG_DATA_HOME:-~/.local/share}/AgentHub`, Windows `%APPDATA%\AgentHub` (via MSIX detection plus a
loopback-UNC twin path — implemented, but **never verified on a real Windows/MSIX environment**; see
[windows.md](windows.md)).

**Channel separation is a property of the binary, not of an environment variable.** A development
build resolves to `AgentHubDev`; only a build explicitly made for release resolves to the installed
location (`main.channel`, default `"dev"`). A build that forgot to declare its channel gets the
**dev** directory — guessing wrong in that direction costs you one extra sandbox, while guessing
wrong in the other direction burns the one-shot OAuth refresh token in the user's real installation.
An explicit `AGENTHUB_DATA_DIR` still takes precedence over both.

---

## 2. Package layout

```
github.com/dinstein/agent-hub
├── cmd/
│   ├── agenthub/            # the one required binary: cli / daemon / connect entry points
│   └── agenthub-gui/        # Wails3 (optional; the Go side sits behind //go:build wails)
├── api/                     # control-plane DTOs + Go client (sole entry point for the GUI and third parties; stdlib only)
├── internal/
│   ├── mcp/                 # ★ the one MCP protocol facade (stdlib only)
│   │   └── transport/       # stdio / streamablehttp / httpsse / docker
│   ├── platform/            # data/run directories, socket and npipe paths, Windows package-identity detection, channel separation
│   ├── tier/                # read|write|destructive vocabulary (leaf package, stdlib only)
│   ├── logx/                # slog initialization, field conventions, unbypassable scrubbing
│   ├── registry/            # multi-document Doc[T], atomic writes, generation, watch, self-write suppression, run markers
│   ├── scope/               # three-layer resolution chain, pure Merge, EffectiveScope (content-addressed)
│   ├── session/             # session identity, Overlay, tighten-only validation, TTL, SessionManager
│   ├── event/               # in-process event bus: coalescer and settled debounce
│   ├── router/              # namespace aggregation, RouteOf as sole provenance, Provider, Catalog
│   ├── downstream/          # connection lifecycle, serial queue, circuit breaker, retries, derived instance pool, per-server logs
│   ├── discovery/           # full/grouped/lazy, meta-tools, lexical ranker, SearchGuard, intent variants
│   │   └── toolsig/         # compact signature grammar
│   ├── shaping/             # result pagination/budget/fetch_result cursor (in-memory and file stores)
│   │   └── toonenc/         # TOON encoding (one-way display projection)
│   ├── pipeline/            # ★ the one execution pipeline: four gates + defend_and_shape + argument self-healing
│   ├── guard/
│   │   ├── injection/       # normalization + phrase/regex/base64/head-and-tail dual window
│   │   ├── spawnguard/      # wrapper/interpreter/env smuggling and container-escape interception
│   │   ├── netguard/        # bidirectional SSRF predicates + in-DialContext screening
│   │   └── leakguard/       # sensitive-data exfiltration detection
│   ├── integrity/           # fingerprint pinning, drift grading, quarantine, tool approval state machine
│   ├── approval/            # HITL broker + gateway-side asker + fingerprint allowlist
│   ├── audit/               # three streams: audit / security / savings + inspect ring buffer
│   ├── secrets/             # four-level resolution chain; vault key (serverID, scopeName), default "_global"
│   │   └── secureenv/       # child-process environment allowlist, login-shell PATH capture
│   ├── oauthflow/           # discovery/DCR/PKCE/three callback modes + refresh (singleflight and file locks)
│   ├── confops/             # ★ the one implementation of semantic registry writes (shared by CLI and control plane); Precondition optimistic locking
│   ├── catalog/             # curated server catalog + parsing of pasted client configs (produces proposals only, never writes to disk)
│   ├── ctlapi/              # control-plane server: REST + SSE over UDS, peer-cred authentication
│   ├── httpbridge/          # streamable-http exposure surface, ingress limits, agent tokens
│   ├── clients/             # config adaptation for 12 clients (grouped by config shape)
│   ├── skills/              # library/install two-tier model, targets table, OwnedDir/SentinelBlock, ApplyState
│   ├── gateway/             # stdio gateway assembly and lifecycle (the implementation behind connect)
│   ├── daemon/              # daemon assembly: HTTP surface + coordination plane + control plane
│   ├── cli/                 # the full command tree, depending only on the api client and the internal packages
│   │   └── output/          # human tables and --json envelopes rendered from the same source
│   ├── ratelimit/           # cooperative quotas (cross-process file lock + coalesced writes)
│   ├── depguardtest/        # failing cases proving the four dependency constraints actually bite
│   └── testutil/fakemcp/    # programmable fake downstream (the foundation for all tests)
├── test/e2e/                # end-to-end regression: real processes, real npx downstreams
├── test/concurrency/        # cross-process concurrency invariants
├── test/buildrules/         # proves the rules outside the Go build match the tree
└── go.mod
```

### Retired old names (they show up in early material; never use them)

| Old name | Canonical |
|---|---|
| `internal/controlapi`, `internal/control` | `internal/ctlapi` (DTOs and client live in the public `api` package) |
| `internal/vault` | `internal/secrets` |
| `internal/secure/{integrity,injection,ssrf,audit}` | `internal/guard/*` plus sibling `internal/audit`, `internal/integrity` |
| `internal/gatewaymode` | `internal/gateway` |
| `internal/downstream/transport` | `internal/mcp/transport` |
| `package skill` | `package skills` |
| the execute pipeline living inside `internal/gateway` | a standalone `internal/pipeline`; `gateway`/`daemon` only do assembly |
| `catalog.Snapshot` (tool catalog snapshot) | `router.Catalog`; `internal/catalog` refers specifically to the curated server catalog |
| `session.ScopeOverlay` | `*scope.Overlay` (fields follow `ScopeLayer` in `internal/scope`) |

### Hard dependency-direction constraints (enforced at compile time by depguard)

1. `cmd/agenthub-gui` and `api` **must not** import any `internal/*`.
2. `internal/mcp` **depends only on the standard library** — it must not import any third-party
   module (decision #32); no other `internal/*` package may import any third-party MCP library.
3. `internal/pipeline` must not import `internal/ctlapi` (the data plane does not depend on the
   control plane).
4. `internal/mcp`, `internal/platform`, `internal/logx`, and `internal/guard/*` are zero-business-dependency
   foundations.

These four are not review conventions; they are CI failure conditions, and each one must have a
failing case that proves it actually bites.

### On constraint #2: the MCP protocol facade is entirely first-party

`internal/mcp` (including the `transport` subpackage) is the only package in the repository allowed
to touch MCP protocol implementation, and **it itself uses only the standard library** — no
`go get` of the official `modelcontextprotocol/go-sdk`, `mark3labs/mcp-go`, or any other third-party
MCP library.

Rationale: bounded reads (16MB), `notifications/cancelled` forwarding, inline replies to reverse RPC,
and the trailing 4KB of stdio stderr are all protocol-layer invariants that need precise control,
while JSON-RPC encoding/decoding itself is not much work — not worth tying to an external project's
release cadence. The point of the facade is to keep that choice **reversible**: if we ever do swap
the implementation, the change is sealed inside one package, rather than borrowing one now.

---

## 3. Command naming rules

- **Resource groups are always singular as the canonical name, with the plural as a cobra alias**:
  `server` / `profile` / `client` / `session` / `tool` / `skill` / `secret` / `approval` / `grant`
  (`secrets`, `approvals`, `skills`, and so on remain as aliases)
- **Action/flow groups stay as they are**: `daemon`, `connect`, `auth`, `audit`, `activity`, `events`, `config`, `doctor`
- **There is no `scope` group.** Narrowing is what a profile *is*, so it lives on `profile`
  (`profile server` / `profile tools` / `profile discovery`), and handing a surface out lives on
  `client` (`client bind <client> <profile>` / `client unbind` / `client ls`). The retired group's
  commands map one-to-one: `scope set --client X --profile P` → `client bind X P`,
  `scope clear --client X` → `client unbind X`, `scope ls` → `client ls`
- The canonical name for the OAuth group is **`auth`**, not `oauth`
- Session-level flags are uniform: `--enable-server` / `--disable-server` / `--tools s:t1,t2` / `--discovery` / `--reset`.
  There is no `--persist`: a session overlay is volatile by construction, and the way to make a surface
  permanent is to edit the profile
- The `client` group: `ls | detect | inspect | connect | disconnect | bind | unbind`. `detect` stats and
  `inspect` reads, which is the whole distinction (macOS TCC — see internal/clients); `ls` gives the
  connect and the bind answers per client. There is no `import`: it was removed, and a client's existing
  servers are brought over by pasting the configuration instead
- The `skill` group: `add | ls | inspect | rm | enable | disable | install-to | sync | update | verify`
  (`install-to` = materialize one entry, `sync` = materialize in bulk by scope; both coexist)
- List subcommands are always `ls`
- Every command must support `--json`, with human and machine output rendered from the same data structure

### `add` and `enable` are separate primitives, and stay that way

`server add` writes the definition and **nothing else**: no connection, no probe, and the entry lands
**disabled**. `server enable` is what puts a server into service, and it is where the connection probe
lives.

They answer different questions. `add` records what a server IS — pure configuration, no network,
deterministic, safe to script against a downstream that happens to be unreachable right now. `enable`
declares the operator wants to USE it, which is the only point at which "can we actually reach it?" is
worth asking. Folding the two together makes `enabled` mean both *the user wants this* and *it answered
a probe*, and then a downstream that was merely mid-deploy at add time becomes indistinguishable from
one that was never added.

Two consequences that must not be "simplified" away later:

- **The probe reports; it never vetoes.** The enable is what was asked for and always happens. A server
  that needs a login is enabled and says so. Refusing would strand an entry the operator explicitly
  enabled, and would turn a transient outage into a configuration change.
- **Composition belongs to the caller.** `catalog add` and the GUI may offer one action over the two
  operations — `catalog add` does exactly that, and `auth login` enables the server it just authorized
  (which is what keeps the OAuth path two commands, not three). The primitives underneath stay separate.

---

## 4. Known capability boundaries

These aren't a to-do list; they're an **honest grading of the current implementation**. When you
touch the related code, know which tier you're standing on:

| Item | Status |
|---|---|
| Windows | The path and package-identity resolution in `internal/platform` is complete and has a `GOOS=windows` cross-compile gate, but **it doesn't run and has never been verified on real hardware**: the registry's cross-process lock and the control plane's named-pipe listener are both still stubs returning unsupported. See [windows.md](windows.md) |
| GUI | Functionally complete but **not part of the default build** (the webview needs GTK/WebKit, which CI runners lack); build it separately with `make gui` |
| skills materialization | Only at **client granularity**, not per-session — the files live outside agenthub's read path |
| skills from git sources | Records and pins a revision, but **never runs git and never touches the network**; update without a local checkout returns a typed unsupported error, and never lies that "you're already up to date" |
| TOON | A **one-way display projection with no decoder** (§7 item 4); anything requiring a round trip never enters this encoder |
| teams | Deliberately unimplemented; the `policy` layer reserves an `Effective()` (own OR forced, tighten-only) hook |
| Telemetry / update checker | Decided against (§7 item 6) — no data is collected |

### Three things that must never be retrofitted

Each of these was nearly left out, and each would be disproportionately expensive to add later. They
are in place; keep them that way.

1. **The composite vault key** `(serverID, scopeName)`, defaulting to `"_global"`
   — retrofitting this would touch every singleton in the token store, callback server, and refresh
   coordinator.
2. **Registry self-write suppression**: register the payload in a bounded TTL set before writing; the
   watcher ignores anything that hits it — without this, every self-write triggers a pointless
   reload cycle.
3. **X-Request-Id end to end**: the response header is written before the handler runs, error bodies
   carry it, and audit records carry it.

---

## 5. Reference-code policy

**Both reference implementations are read-only. All code is redesigned and reimplemented in this
project's own style.**

| Source | License | Usage |
|---|---|---|
| [smart-mcp-proxy/mcpproxy-go](https://github.com/smart-mcp-proxy/mcpproxy-go) | MIT / Go | **Reference only, never copy code.** What we inherit is its **list of problems** — which edge cases exist, what failure looks like, what the correct behavior is; every implementation is written from scratch |
| [tsouth89/toolport](https://github.com/tsouth89/toolport) | MIT / Rust | A different language; likewise a design reference only |

This project already has its own coherent structural conventions (the `internal/mcp` protocol facade,
the generic `Doc[T]` envelope, a per-server owner goroutine plus `calls chan` serialization,
content-addressed `EffectiveScope`, failure-direction comments), and pasting in foreign
implementations would tear at those conventions. The value of the reference implementations is the
problem list; the implementation has landed against it, and the code is now authoritative.

The root `NOTICE` records design-reference sources (academic honesty, not a license obligation).

---

## 5b. MCP protocol scope

- **Target version `2025-11-25`** (the current stable release); `initialize` declares that version
- **Negotiates downward** for downstreams that only support `2025-06-18` / `2025-03-26`
- Transport support is asymmetric by direction:
  - **Read side** (connecting to downstreams): `stdio` + `streamable-http` + **legacy HTTP+SSE**
  - **Exposure side** (daemon facing upstream clients): `streamable-http` only; no new SSE exposure surface

### Upstream deprecation tracking

Everything is implemented against the current state (the earliest removals are all after 2027-07-28),
but the migration seams are in place ahead of time. Every use site carries a
`// DEPRECATED-UPSTREAM(<feature>, earliest-removal: <date>)` comment so a single grep finds them all
later.

| Feature | Deprecated in | Dependency point | Migration seam |
|---|---|---|---|
| Roots | `2026-07-28` | `${ROOT}` and derived-instance keying (`internal/downstream`). The dependency **shrank** when the per-project scope layer was retired: longest-prefix root matching no longer selects anything, and the root has left the resolver's cache key | **In place since M0**: the `RootSource` interface, with one implementation for the roots protocol and one for an explicit root in `clients.json` |
| Sampling | `2026-07-28` | One of the isolation arguments in 1.1 | No seam needed (the conclusion is independently supported by credentials, connection parameters, and fault isolation) |
| DCR | `2026-07-28` | The OAuth discovery chain; DCR credentials persisted alongside tokens | **In place since M1**: the `ClientRegistrar` interface, with one implementation for DCR and one for Client ID Metadata Documents |
| Logging | `2026-07-28` | Downstream log forwarding | No seam needed (the logging surface is first-party anyway) |
| HTTP+SSE transport | `2025-03-26` | One of the three transports | Kept on the read side; no new exposure side |

---

## 5c. The config hot-reload path (two things not to get wrong)

GUI/CLI edits a profile → the corresponding gateway updates automatically; see [flows.md](flows.md)
§4 for the full path. Two points that must be right:

1. **Self-write suppression**: when the daemon writes `profiles.json` itself, fsnotify reports the
   event just the same, and without suppression it does a pointless reload cycle for its own write.
   Register the payload in a bounded TTL set before writing (multiple slots, 10s expiry, retracted on
   write failure, cleared on external change); the watcher ignores anything that hits it.
2. **The generation criterion**: the `Change{Kind, Rev}` pushed over the control connection is only a
   **notification** — it carries no snapshot, so the gateway must still re-read the file itself. The
   criterion is **adopt whenever the generation read is ≥ the generation applied**, not "equals the
   event's Rev"; otherwise several rapid successive writes leave you stuck on an old version, waiting
   for an event that will never come again.

---

## 5d. Collaboration conventions

**They live in [AGENTS.md](../AGENTS.md), not here** — worktree per feature, one commit per subtask,
`main` stays linear (rebase, never merge), `make ci-landing` **after** the rebase, `--ff-only` as the
enforcement.

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

- **Go 1.26+**
- **LICENSE: MIT** (`Copyright (c) 2026 dinstein`)
- `cobra` (CLI), `fsnotify` (watch), `zalando/go-keyring` (keyring), `log/slog`
- `golangci-lint` (**v2 config format**) + **depguard** (which pins the four dependency constraints from §2),
  plus `gofmt` and `goimports` — those two are declared under `formatters:`, a section separate from
  `linters:`, and are no less a CI failure condition for it
- CI: GitHub Actions, a macOS + Linux matrix running build / test / lint

### Conventional paths

| | |
|---|---|
| Fake downstream MCP server | `internal/testutil/fakemcp` |
| Where reference repositories are cloned | `~/Develop/_refs/` (**outside the repository**, to avoid polluting git history) |
| Per-server log file | `<data>/logs/server-<name>.log` |

### Every depguard constraint must have a failing case

The four dependency constraints can't just live in `.golangci.yml` — each needs a case proving it
actually bites (for example, a violating sample under `testdata/depguard/` plus a test that runs lint
and asserts it fails). A lint rule that is configured but not in effect is more dangerous than no
rule at all.

### Test infrastructure (an M0 deliverable)

A programmable **fake downstream MCP server** that can inject, by script: slow responses and
timeouts, half-written/malformed JSON-RPC frames, oversized payloads (hitting the 16MB bounded read),
crashes during the handshake, `tools/list_changed` storms, and protocol violations.

Three classes of test that have been in CI from day one:

1. **Golden tests** — the signature grammar, search ordering, error copy ("determinism is the contract")
2. **Cross-process concurrency tests** — single-line O_APPEND writes, file locks on pins/quarantine, monotonic generations
3. **Daemon `kill -9` injection tests** — the stdio data plane is unaffected, HITL really does fail closed

---

## 7. Decision records (the original six "to be decided" items, all now settled)

Registered here so none of them gets silently skipped. **All six are decided** (items 1 and 5 were
closed out at the end of M2):

1. ~~**Whether to pull lazy-connect forward into M1** (table 0.4 registered it as "M1 (TBC)")~~ →
   **Decided (M2): no.** Keep eager connect plus "answer from cache first" fast startup; the "M1
   (TBC)" entry in the `0.4` table is void.

   Rationale:
   - **The original motivation was solved by something else.** Lazy-connect was meant to fix the
     N×M process cost of "one process per client × one instance per server." What actually solves
     that is the daemon's **streamable-http shared pool** — clients that can speak HTTP share one set
     of downstream connections. The stdio gateway was always the fallback path for clients that only
     speak stdio, and building a separate lazy-connection state machine for it optimizes the side
     where the cost is smallest.
   - **The cost lands in exactly the wrong place.** A cold npx/uvx cache takes 10s to several minutes
     on first start (`DefaultConnectTimeout` is set to 120s for precisely this reason). Eager connect
     puts that wait in the gateway's startup window — `tools/list` is answered immediately from the
     tool cache and the agent never blocks. Lazy-connect moves it **into the middle of the first
     tools/call**, where it shows up inside an agent turn as an unexplainable long hang, and it has
     to race the client's own timeout. Trading visible startup time for invisible mid-call hangs is a
     net negative.
   - **It conflicts with fail-closed gating.** The criteria for the 7.5 approval state machine and
     integrity fingerprint pinning come from the **tools/list of a live connection**; lazy connection
     means that for some window the agent sees cached tools whose fingerprints were never validated
     this session. Either you validate synchronously on first call (which amplifies lazy-connect's
     latency again), or you loosen the gates (unacceptable).

   The escape hatch stays open: the seam is `downstream.Deps.Dial` plus the tool cache, so if we ever
   do build it, the change is sealed inside `gateway.connectAll` and a per-server "connect on first
   call" gate, with call sites untouched. The trigger should be measured process/memory cost, not a
   derivation.
2. ~~**Choice of on-disk cache for shaping** (bbolt vs. plain files)~~ → **Decided: plain files.**
   `<data>/cache/shaping/<sha256(owner)>/<cursor>.json`, atomic writes (temp file in the same
   directory → 0600 → fsync → rename) + TTL sweeping + a sweep at startup. Rationale: zero new
   dependencies throughout M0–M1 is an established style; the access pattern is single-key point
   lookup, with no queries, no transactions, and no cross-key consistency requirement; a corrupted
   entry costs exactly one cursor (skip that file), whereas a single-file database needs a recovery
   mechanism to offer the same property. See the `internal/shaping` package doc for details
3. ~~**Wails3 version and frontend stack** (v3 is still alpha/beta; we need a fallback plan)~~ →
   **Decided (M1-G)**: `wails/v3 v3.0.0-alpha2.118` + **vanilla TS + Vite** (the only frontend runtime
   dependency is `@wailsio/runtime`). The fallback plan isn't "switch frameworks"; it's **compressing
   the alpha dependency down to one file**:
   - All the GUI's Wails code carries the `//go:build wails` tag, and the default build is a
     placeholder main (`cmd/agenthub-gui/main.go`, `//go:build !wails`). CI's `go build ./...` and
     `golangci-lint run` therefore never touch webview dependencies — the ubuntu runner has no
     GTK/WebKit dev packages, and this isn't a workaround but a second piece of evidence that "the GUI
     is optional." Having wails in `go.mod` is harmless: downloading it needs no system libraries.
   - The **only** files depending on Wails are `cmd/agenthub-gui/gui_main.go` and
     `cmd/agenthub-gui/services/service_wails.go` (about 50 lines of assembly). The service body
     `services/hub.go` (all api calls plus the SSE→event bridge) carries **no tag**, so it compiles,
     vets, and unit-tests in CI. A breaking alpha change means editing those two files; page logic and
     the api layer are unaffected.
   - The frontend doesn't use `wails3 generate bindings`; it uses `Call.ByName(<Go FQN>)` plus
     `Events.On` (`frontend/src/bridge.ts`) — one fewer code-generation step and one fewer artifact
     drifting along with the alpha.
   - Build entry point: `make gui` (= `gui-frontend` + `gui-go`); not part of `make build`/`ci`.
   - The Health Level/AdminState/Action constants are generated by
     `go generate ./cmd/agenthub-gui/...`, parsed from the `api` package source into
     `frontend/src/generated/health.ts`; a golden test asserts "generated output == committed file ==
     Go constants," preventing the three-way drift described in 7.4.
4. ~~**TOON grammar scope and the golden case set** (no existing Go library; needs to be first-party)~~
   → **Decided (M1.5).** Both "determinism is the contract" grammars are frozen together, each with
   its own golden corpus:

   **(a) TOON (`internal/shaping/toonenc`) is a one-way projection: no round trip, no decoder.**
   The encoding exists solely for LLMs to read; anything that needs to make a round trip
   (`structuredContent`, tool arguments, cursors) stays in JSON and never enters this package.
   Rationale: a round trip would require a type marker on every scalar (otherwise bare `1` and `"1"`,
   or bare `true` and `"true"`, are indistinguishable), and those markers are exactly the tokens this
   encoding is trying to save. The contract is instead an **in-band declaration**: the first re-encoded
   text block is prefixed with a line reading
   `#toon/1 (display encoding; send tool arguments as JSON)`.

   Grammar scope: scalars pass through verbatim (numbers go through `json.Decoder.UseNumber` as
   literals and never touch float64); objects are `key: value` indented blocks with keys sorted
   bytewise (JSON member order is lost after Go decoding, so sorting is the only deterministic
   choice); arrays use a `- ` prefix; **homogeneous object arrays use a table**, `key[N]{c1,c2}:` plus
   comma-separated rows (criteria: ≥2 elements, all non-empty objects, identical key sets, all values
   scalar, ≤32 columns; failing any of these degrades to a `- ` list); empty object `{}`, empty array
   `[]`; strings are quoted (`strconv.Quote`) only when they are empty, have leading/trailing
   whitespace, contain `,` `:` `"` `\` `#` or control characters, start with `[` `{` `- `, or would be
   read as a number or true/false/null; keys are additionally quoted when they contain internal
   whitespace; no comments, no anchors, no references. Depth beyond 12 levels degrades to single-line
   compact JSON.

   Two constructive guarantees: **never-larger** (if `Consider` doesn't reach `MinSavingsPct`,
   default 10%, it returns the input unchanged, so callers need not compare sizes themselves) and
   **numeric fidelity**. Budget truncation cuts on whole lines, and the last line is the frozen
   `…truncated by agenthub: %d of %d lines`.

   Golden corpus: `internal/shaping/toonenc/testdata/*.toon` — scalars (including 2^53+1 and 30-digit
   integers), nesting, tables (inside objects, at the root, and the three rejected degradations),
   every quoting trigger, lists (including nested and mixed), root scalars, headers, budget
   truncation, and non-default indentation.

   Wiring: `shaping.Options.Format` (`shaping.ParseFormat` maps the governance `result_format:
   json|toon`, **defaulting to json**, with any unrecognized value falling back to json).
   `shaping.ShapeResult` is the new complete entry point: **re-encode first, then apply the budget**
   (the budget is spent on the cheaper representation, and the retained remainder uses the same
   notation the agent sees), with the trailer appended by the truncation step and therefore **always
   last**. Only text blocks are rewritten; `structuredContent` is never re-encoded. Ordering
   invariant: re-encoding sits on the delivery path, i.e. **after** the pipeline's injection/leak
   scans — which is what makes 7.6's "leakguard scans pre-encoding text" structurally true.

   **(b) Compact signature (`internal/discovery/toolsig`) grammar**:
   `name(p1:str, p2?:int=3, p3?~:obj{a,b}) -> str`. `?` = optional, `~` = lossy, `(~)` = schema
   unparseable. `?` marks **optional** parameters (optional parameters are the minority, so there are
   fewer marks and shorter lines); `~` marks lossy. Type abbreviations
   `str/int/num/bool/null/obj/arr/any`, `obj{k,k}` (top level expanded one level, anything deeper
   folded to `obj` and marked `~`), `arr<T>`, `enum{a|b}` (truncated past 6 and marked `~`). Parameter
   order = **the original order of the required array → the rest sorted bytewise**; over budget
   (default 200B) parameters are **dropped from the tail** (so required ones survive first), ending in
   `…+N more`, and the tool name is never truncated. `$ref` is not resolved (the router already
   inlines them); leftovers render as `any~`.
   Golden: `internal/discovery/toolsig/testdata/signatures.golden`.

   Two-stage describe: the new meta-tool `describe_tool{tool | tools:[≤5]}` sits alongside the
   existing four in lazy mode, making **five** (frozen order:
   `status, search_tools, describe_tool, call_tool, fetch_result`). Search hits no longer carry a
   schema; they carry `sig` + `lossy` instead. Describe's visibility predicate is exactly
   `Surface.byExposed` — the same set as search/tools\_list/call, so it's structurally impossible for
   it to be wider; **only one per-id error is emitted, `not_found`** (nonexistent / out of scope /
   quarantined / disabled all share the same copy, to prevent probing, matching the `fetch_result`
   rule).
5. ~~**A workable story for macOS keychain ACLs and unsigned development binaries** (does dev mode
   default to `secrets.enc`?)~~
   → **Decided (M1; implementation in the `internal/secrets` package doc): yes, dev mode falls back
   to `secrets.enc` automatically.**
   During development every `go build` produces a new unsigned binary, so the macOS keychain ACL
   prompts again each time. The decision: when **keyring availability detection fails**, or when
   `AGENTHUB_DEV_SECRETS=1` is set explicitly, writes automatically fall back to `secrets.enc`, with
   a 32-byte key generated automatically and persisted next to it (`secrets.enc.key`, 0600).

   The honest grading is written into the package comment: **putting the key next to the ciphertext is
   obfuscation, not encryption at rest** — anyone who can read both files has the plaintext. This holds
   only for the dev fallback; the production path uses `AGENTHUB_SECRET_KEY` or the OS keyring.
   The detection itself is hardened per 7.11: **read, don't write** (a `Set` probe triggers macOS's
   destructive confirmation dialog), results are cached per process, and every operation has a hard
   timeout (a stuck keychain dialog would otherwise hang the caller).
6. ~~**Whether to build telemetry and an update checker, and how to set the defaults**~~ → **Decided:
   neither.**
   AgentHub **collects no data**: no telemetry (no enum-only versioned reporting, and no opt-in
   switch) and no update checker (no channel probing, no network requests at startup). Both items in
   7.11's "optional extras" are removed from the M2 plan.

   Rationale:
   - This process holds every downstream credential, every tool-call argument and result, and the
     user's project paths. A reporting channel that promises "enums only, never free text" needs a
     `ScanForPII` test gate to keep that promise — the stronger the promise, the higher the
     maintenance cost, whereas **not opening the channel at all** costs nothing and cannot degrade.
   - "We collect no data" is a property you can state in one sentence and a user can verify on the
     spot (an empty packet capture); "we only collect anonymous enums" is not. For a security product,
     this is the best possible use of the trust budget.
   - An update checker either adds a network round trip to the startup path (with one process per
     client for the stdio gateway, the cost scales with process count) or requires a resident prober —
     both reinvent a wheel that package managers already solved. Distribution goes through Homebrew /
     package managers / GitHub Releases, and version comparison is their job.

   Implementation constraint (equivalent to a CI-checkable property): **there exists no** outbound
   request anywhere in `internal/*` pointing at an agenthub-owned domain or version manifest; network
   egress falls into exactly three categories — downstream MCP servers, OAuth authorization servers,
   and endpoints the user configured explicitly. Adding a fourth category violates this decision.
