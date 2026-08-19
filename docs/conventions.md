# Conventions

> **Answers** whether a name, a dependency direction, a command spelling or an engineering rule may move.
> **Not here** why a settled question was settled that way → [decisions/](decisions/); how the system works → [architecture.md](architecture.md).
> **Kept true by** `internal/depguardtest` (the dependency directions), `internal/cli/tree_test.go` (the command rules), and `test/buildrules` (the rest).

This file registers the things you do not get to change casually. Changing it means changing an
architectural convention. It carries nothing you can read off the tree.

**Sections are cited by anchor, not by number.** A heading's slug travels with it when the file is
reordered, and renaming one breaks the citation loudly — `TestDocReferencesResolve` resolves every
`docs/*.md#anchor` in the tree, and refuses a `§N` aimed at a document that does not number its
headings. The three that still number are `guide.md`, `status/*` and `subsystems/gui.md`; there the
numbers are an interface other files cite, and renumbering silently redirects them.

## Frozen identifiers

ABI as of v1: users' configuration and other clients' launch scripts hardcode them.

| Item | Value |
|---|---|
| Go module | `github.com/dinstein/agent-hub` |
| Remote repository | `git@github.com:dinstein/agent-hub.git` |
| Required binary | `agenthub` |
| Optional GUI binary | `agenthub-gui` |
| Data directory name | `AgentHub` (release) / `AgentHubDev` (dev, a sibling directory) |
| env prefix | `AGENTHUB_*`, stripped wholesale when spawning a downstream |
| Control socket | `<run>/ctl.sock`; on Windows `\\.\pipe\agenthub-ctl-<sha8(SID)>` |

The repository name `agent-hub` deliberately differs from the product and binary name `agenthub`, and is
**not** part of the frozen set.

**Channel separation is a property of the binary, not of an environment variable.** `main.channel`
defaults to `"dev"`, and only a build made explicitly for release resolves to the installed location.
**Failure direction: a build that forgot to declare its channel gets the dev directory** — wrong that way
costs one sandbox, wrong the other burns the one-shot OAuth refresh token in the user's real
installation. `AGENTHUB_DATA_DIR` still overrides both.

## Package layout

Which package holds what is [architecture.md#the-packages](architecture.md#the-packages). What follows is
only what you cannot read off `ls internal/`.

### Exactly one of each

| The one … | Lives in | What "one" forbids |
|---|---|---|
| MCP protocol facade | `internal/mcp` (+ `transport`) | any other package touching protocol implementation, and any third-party MCP library anywhere |
| execution pipeline | `internal/pipeline` | a second call path. Direct calls and `call_tool` both go through `pipeline.Execute`, and tests assert their gate counters match; a new path must carry the same assertion |
| semantic-write implementation | `internal/confops` | the CLI and the control plane owning separate copies of "what it means to add a server" |
| provenance for an exposed name | `router.RouteOf` | splitting on `__` — a server id or a tool name may itself contain `__` |
| governance surface | the scope chain | a second place that decides what a caller may see |

Four packages are **leaves on purpose**: `internal/tier`, `internal/platform`, `internal/logx`,
`internal/guard/*`. `tier` is the instructive one — seven packages need to say the word "read", and none
should import another to do it. It used to live in `pipeline`, which turned rule 3's failing case into an
**import cycle rather than a lint error**.

### Dependency directions

Four CI failure conditions, not review conventions. Each must have a failing case proving it actually
bites.

1. `cmd/agenthub-gui` and `api` **must not** import any `internal/*`.
2. `internal/mcp` **depends only on the standard library** — no third-party module at all — and no other
   `internal/*` package may import any third-party MCP library.
3. `internal/pipeline` must not import `internal/ctlapi`: the data plane does not depend on the control
   plane.
4. `internal/mcp`, `internal/platform`, `internal/logx` and `internal/guard/*` are
   zero-business-dependency foundations.

On rule 2: no `go get` of any MCP library. Bounded reads, `notifications/cancelled` forwarding, inline
replies to reverse RPC and the trailing stdio stderr window are protocol-layer invariants needing precise
control, while JSON-RPC encoding itself is not much work. The facade keeps the choice **reversible** —
swapping the implementation stays sealed inside one package.

### Retired names

They show up in early material; never use them.

| Old name | Canonical |
|---|---|
| `internal/controlapi`, `internal/control` | `internal/ctlapi` (DTOs and client live in the public `api` package) |
| `internal/vault` | `internal/secrets` |
| `internal/secure/{integrity,injection,ssrf,audit}` | `internal/guard/*` — but only `ssrf` survived, as `netguard` |
| `internal/accesslog` | `internal/calllog`. `<data>/audit` became `<data>/calls`, `access.jsonl` became `calls.jsonl` (the old name stays READABLE forever — an authenticated ledger does not restate its own history), the CLI group `audit` became `calls` with the old settings kept as aliases, and `/v1/audit/*` became `/v1/calls/*`. The vault key `__audit_encryption__` does **not** move: it is what every existing installation's key is filed under |
| `internal/integrity`, `internal/approval`, `internal/audit` | **Nothing.** Removed with the old runtime governance surface. `internal/calllog` is a new local ledger with no permission role, not a forwarding address; `internal/audit`'s one surviving primitive, the multi-writer JSONL append, is `internal/jsonl` |
| `internal/savings` | **Nothing** ([decision 0009](decisions/0009-savings-ledger-removed.md)); `agenthub activity`, its only reader, went with it |
| `internal/gatewaymode` | `internal/gateway` |
| `internal/downstream/transport` | `internal/mcp/transport` |
| `package skill` | `package skills` |
| `agenthub tool ls / inspect / allow` | `agenthub server tool …`; the shim is gone and the old spelling is a usage error |
| `agenthub tool allow <server> a b` | `agenthub server tool allow <server> --only a,b` |
| `agenthub tool allow <server>` (bare = block all) | `agenthub server tool allow <server> --none` |
| `agenthub tool allow <server> --clear` | `agenthub server tool allow <server> --all` |
| `agenthub profile tools <profile> <server>` | `agenthub profile tool allow …` |
| `agenthub server tool ls --rules` | `agenthub server ls` / `server inspect`. `--rules` is still accepted as a hidden, deprecated spelling; deleting it is outstanding |
| the execute pipeline inside `internal/gateway` | a standalone `internal/pipeline` |
| `catalog.Snapshot` | `router.Catalog`; `internal/catalog` is the curated *server* catalogue |

## Command naming

- **Resource groups are singular as the canonical name, with the plural as a cobra alias**: `server`,
  `profile`, `client`, `session`, `skill`, `secret`, `token`. One alias per group.
- **Action and flow groups take no plural alias**: `daemon`, `connect`, `auth`, `calls`, `logs`, `events`,
  `config`, `doctor`, `catalog`. The plural marks a group you accumulate entries in, not every noun.
  `logs` reads **across every process log at once**, which is what separates it from `daemon logs` (one
  process) and `server logs` (one downstream connection's frames); the name may not move between the
  three. `events` reads the event log, not the daemon's SSE stream.
- List subcommands are always `ls`, and **every command supports `--json`**, with human and machine
  output rendered from the same data structure.
- **There is no top-level `tool` group.** The rule deciding which tools a server contributes lives on the
  server entry beside `enabled`, so the group hangs off `server`.
- **The two narrowing layers are one command at two altitudes**: `server tool` and `profile tool`, each
  `ls | inspect | allow`, the writes taking `--only a,b | --all | --none` and the listings
  `--search | --all`. A second spelling invites a second mental model, and `--all` (drop the rule) against
  `--none` (store the empty list) is exactly the pair a second model gets backwards.
- **A rule is read where it is stored; a listing reads its effect**
  ([decision 0008](decisions/0008-rules-are-read-where-they-are-stored.md)).
- **There is no `scope` group.** Narrowing is what a profile *is*, and handing a surface out lives on
  `client`. The retired group maps one to one: `scope set --client X --profile P` → `client bind X P`,
  `scope clear --client X` → `client unbind X`, `scope ls` → `client ls`.
- **A live session cannot be renarrowed.** What a client sees is decided before it connects, so `session`
  is read-and-terminate; edit the profile instead.
- The `client` group is `ls | detect | inspect | connect | disconnect | bind | unbind`. There is no
  `import`: a client's existing servers are brought over by pasting its configuration.
- **`manual` prints the binary's own SKILL.md, and it is neither `skill doc` nor a flag.** Not under
  `skill`, because every invariant that group has is built for text the operator imported from elsewhere
  while this document is the binary describing itself; not a flag, because a flag that prints a document
  and exits is a verb.

### `add` and `enable` are separate primitives, and stay that way

`server add` writes the definition and **nothing else**: no connection, no probe, and the entry lands
disabled. `server enable` puts a server into service, and that is where the connection probe lives.
Folded together, `enabled` would mean both *the user wants this* and *it answered a probe*. Two
consequences that must not be simplified away: **the probe reports, it never vetoes**, so a transient
outage never becomes a configuration change; and **composition belongs to the caller** — `catalog add` and
`auth login` each offer one action over the two, and the primitives stay separate.

## Capability boundaries

Not a to-do list — an honest grading of the current implementation.

| Item | Status |
|---|---|
| Windows | platform layer filled in; CI gates on `GOOS=windows` build and vet, but it has **never run on a real Windows machine** ([status/windows.md](status/windows.md)) |
| GUI | functionally complete, **not part of the default build**; `make gui` |
| skills materialization | **client granularity only**, not per-session — the files live outside agenthub's read path |
| skills from git sources | records and pins a revision, but **never runs git and never touches the network** |
| TOON | a **one-way display projection with no decoder** ([decision 0004](decisions/0004-toon-and-signature-grammars.md)) |
| teams | deliberately unimplemented, and **nothing in the tree reserves a seam for it** — a hook nobody can reach reads as a decision already half-made |
| telemetry, update checker | decided against ([decision 0006](decisions/0006-no-telemetry-and-no-update-checker.md)) |

### Three things that must never be retrofitted

1. **The composite vault key** `(serverID, scopeName)`, defaulting to `_global` — retrofitting it would
   touch every singleton in the token store, callback server and refresh coordinator.
2. **Registry self-write suppression** — without it every self-write costs a reload cycle.
3. **X-Request-Id end to end**: the response header is written before the handler runs, error bodies carry
   it, and the log line for the failure carries the same value.

## Reference-code policy

| Source | License | Usage |
|---|---|---|
| [mcpproxy-go](https://github.com/smart-mcp-proxy/mcpproxy-go) | MIT / Go | **reference only, never copy code** |
| [toolport](https://github.com/tsouth89/toolport) | MIT / Rust | a different language; likewise a design reference only |

**A bound learned from them is not copied code.** Two constants come across verbatim —
`internal/httpbridge`'s ingress limits and `clients.MaxConfigSize` — and both are what the policy permits:
a number is a finding *about* the problem, and rederiving a different one would be worse, not more
independent. What may never cross is the implementation around it.

The root `NOTICE` records the design references: academic honesty, not a license obligation.

## MCP protocol scope

**Two protocol generations are spoken, on both faces.** `mcp.SupportedVersions` is the list, newest first,
and it is the only place either direction reads what it accepts.

- The **read side** probes with `server/discover` and falls back to the `initialize` handshake only on
  proof the server answered *and is old*. An error code the specification reserves for itself is proof of
  the opposite: only a modern server produces one, so it propagates rather than triggering a fallback that
  would send `initialize` to a server that does not implement it.
- The **exposure side** answers `server/discover` with that same list, and a request carrying the
  per-request `_meta` puts the session in stateless mode. `initialize` negotiates the **stateful family
  only**: a client declaring `2026-07-28` there gets the default instead, because echoing it would promise
  per-request `_meta` semantics on a session that used the handshake 2026 removed.
- **Transport support is asymmetric by direction**: the read side speaks stdio, streamable-http and legacy
  HTTP+SSE; the exposure side speaks streamable-http only. That rule is about the 2024-11-05 two-endpoint
  binding, and it never covered streamable HTTP's own server→client stream.
- **Both generations are offered the out-of-band notification stream on the HTTP face** — `GET` on
  ≤ 2025-11-25, `subscriptions/listen` on 2026-07-28. Refusing it left a client served a tool set that
  could go stale forever while `initialize` handed it `{"tools":{"listChanged":true}}`: this face was
  promising a channel it had decided not to build.
- **The two directions are still not symmetric, and the reasons differ.** AgentHub *asks* every
  streamable-http downstream for the server→client stream, because it is the only channel by which such a
  downstream can report a tool-set change. What remains asymmetric is the vocabulary: the read side
  accepts whatever a downstream sends, while the exposure side acknowledges only what it can actually
  produce, because 2026-07-28's acknowledgement is the only place a client learns that a type it
  subscribed to will never arrive.

**`mcp.ProtocolVersion` does not name the version this tree targets, and flipping it to 2026 is wrong.**
It stays at `2025-11-25` because every context that reads it is definitionally pre-2026. The 2026-07-28
declaration travels per-request in `_meta`, and `MCP-Protocol-Version` is read back out of that `_meta`
rather than from this constant, because the header and the body MUST agree and `server/discover` declares
2026 before anything is negotiated.

### Upstream deprecation tracking

No removal is scheduled earlier than 2027-07-28 and every seam is in place. Each use site carries a
`// DEPRECATED-UPSTREAM(<feature>, earliest-removal: <date>)` comment, so one grep finds them all.

**`earliest-removal` is a removal date or the literal `none`, and never the date a feature was
deprecated.** HTTP+SSE is the case that needs the word: it is deprecated with no removal planned, and its
markers once carried a date already past in the field a sweep looks at to decide what may be deleted.

| Feature | Deprecated in | Dependency point | Migration seam |
|---|---|---|---|
| the `initialize` handshake | 2026-07-28 | the stateful session path, both faces | **landed**: `server/discover` plus per-request `_meta` |
| `ping` | 2026-07-28 | liveness on the stateful path | none needed — a stateless request carries its own context |
| roots | 2026-07-28 | `${ROOT}` and derived-instance keying | **in place**: `RootSource` |
| sampling | 2026-07-28 | one of the isolation arguments | none needed — the conclusion is independently supported |
| DCR | 2026-07-28 | the OAuth discovery chain | **in place**: `ClientRegistrar` |
| logging | 2026-07-28 | downstream log forwarding | none needed — the logging surface is first-party |
| HTTP+SSE transport | 2025-03-26 | one of the three transports | kept on the read side; no new exposure side |

## The hot-reload path

Two things not to get wrong. The path is
[flows.md#config-hot-reload](flows.md#config-hot-reload) and the mechanism is
[subsystems/registry.md](subsystems/registry.md); these two rulings are what the mechanism exists to
satisfy.

1. **Self-write suppression.** When a process writes `profiles.json` itself, fsnotify reports the event
   just the same, so the payload is registered in a bounded TTL set *before* writing and the watcher
   ignores anything that hits it.
2. **The generation criterion is "the generation read ≥ the generation applied"**, never "equals the
   event's `Rev`". The change notification carries no snapshot, so the reader re-reads the file itself; an
   equality test leaves rapid successive writes stuck on an old version, waiting for an event that will
   never come again.

## Collaboration conventions

**They live in [AGENTS.md](../AGENTS.md), not here** — worktree per feature, a draft PR per branch kept
current and closed by the landing, one commit per subtask, `main` stays linear, `make ci-landing` after a
rebase that moved the branch, `--ff-only` as the enforcement.

Development workflow is not an architectural convention, and AGENTS.md is the file every coding agent
reads first. This section once carried its own copy, and the copy had gone stale in the dangerous
direction: commit straight to `main`.

## Engineering conventions

- **Go 1.26+**; **LICENSE: MIT**.
- `cobra` (CLI), `fsnotify` (watch), `zalando/go-keyring` (keyring), `log/slog`.
- `golangci-lint` v2 plus depguard, which pins the four dependency directions, and `gofmt`/`goimports`
  declared under `formatters:` — a section separate from `linters:`, and no less a CI failure for it.
- CI: GitHub Actions, a macOS and Linux matrix running build, test and lint.

| | |
|---|---|
| fake downstream MCP server | `internal/testutil/fakemcp` |
| where reference repositories are cloned | `~/Develop/_refs/`, **outside the repository** |
| per-process frame stream | `<data>/calls/<day>/frames-<bootid>-p<pid>.jsonl` |

### Every depguard constraint must have a failing case

A violating sample plus a test that runs lint and asserts it fails. **A lint rule that is configured but
not in effect is more dangerous than no rule at all** — and the same standard applies to the proof: when
golangci-lint is absent it calls `t.Skip` and `make test` counts that as success, so the CI gate greps the
verbose output for `--- SKIP` and fails on it. **A check that can quietly not run is a check you do not
have.**

**Owed: one of the seven configured rules has no failing case, and cannot get one from this harness.**
`no-third-party-mcp-libs` denies three named third-party MCP SDKs repo-wide, none of which is in `go.mod`,
while every probe must type-check — that is what makes a lint failure attributable to depguard and nothing
else. A probe importing one of those packages would fail to build instead, so proving it needs a fake
module planted in the disposable tree: a change to the harness, not one more case.

### Three classes of test have been in CI from day one

1. **Golden tests** — the signature grammar, search ordering, error copy. **Determinism is the contract**:
   agents key retry logic and prompts off exact wording, so error text and ordering are frozen artifacts.
   Fix the code, never the golden.
2. **Cross-process concurrency tests** — single-line `O_APPEND` writes, monotonic generations.
3. **Daemon `kill -9` injection tests** — the stdio data plane is unaffected.

The fake downstream's **protocol generation** is scripted too: empty means no `server/discover`, hence the
pre-2026 handshake every older script relies on; a list containing `2026-07-28` makes it a stateless server
that then *requires* the per-request `_meta`. Only the subprocess driver reaches 2026 — the in-process pipe
cannot implement the transport's unexported negotiation seam, so `Handshake` fails closed rather than
sending bare requests a strict server would reject.
