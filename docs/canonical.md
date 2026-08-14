# CANONICAL — the single source of truth for architectural conventions

This file registers **the things you don't get to change casually**: frozen identifiers, the
singletons, dependency directions, command naming rules, engineering conventions, and every matter
that has been seriously decided. Changing this file means changing an architectural convention.

It carries nothing you can read off the tree. For how the system works see
[architecture.md](architecture.md); for flow timing [flows.md](flows.md); for per-package invariants
and debt already pinned to a line, [subsystems/](subsystems/).

**The section numbers are an interface.** Code comments cite them by number — `§2 rule 3`, `§5c #2`,
`§7 #4` — in well over a hundred places. Add sections and rewrite their contents, but do not
renumber: a citation that lands on the wrong ruling is worse than one that lands on nothing.

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

The repository name `agent-hub` deliberately differs from the product and binary name `agenthub`, and
is **not** part of the frozen set.

Data directory: macOS `~/Library/Application Support/AgentHub`, Linux
`${XDG_DATA_HOME:-~/.local/share}/AgentHub`, Windows `%APPDATA%\AgentHub` — the Windows resolution is
implemented but never verified on real hardware ([windows.md](windows.md)).

**Channel separation is a property of the binary, not of an environment variable.** `main.channel`
defaults to `"dev"`; only a build made explicitly for release resolves to the installed location.
**Failure direction: a build that forgot to declare its channel gets the dev directory** — wrong that
way costs one sandbox, wrong the other burns the one-shot OAuth refresh token in the user's real
installation. `AGENTHUB_DATA_DIR` still overrides both.

---

## 2. Package layout

Which package holds what: [architecture.md#the-packages](architecture.md#the-packages) and
[subsystems/](subsystems/). What follows is only what you *cannot* read off `ls internal/`.

### There is exactly one of each

| The one … | Lives in | What "one" forbids |
|---|---|---|
| MCP protocol facade | `internal/mcp` (+ `transport`) | Any other package touching protocol implementation, and any third-party MCP library anywhere (rule 2 below) |
| execution pipeline | `internal/pipeline` | A second call path. Direct calls and `call_tool` both go through `pipeline.Execute`, and tests assert their gate counters match; a new path must carry the same assertion |
| semantic-write implementation | `internal/confops` | The CLI and the control plane owning separate copies of "what it means to add a server" |
| provenance for an exposed name | `router.RouteOf` | Splitting on `__` — a server id or a tool name may itself contain `__` |
| governance surface | the scope chain (§7 of architecture.md) | A second place that decides what a caller may see |

Four packages are **leaves on purpose**: `internal/tier` (the read/write/destructive vocabulary),
`internal/platform`, `internal/logx`, `internal/guard/*`. `tier` is the instructive one — seven
packages need to say the word "read", and none should import another to do it. It used to live in
`pipeline`, which turned rule 3's failing case into an **import cycle rather than a lint error**.

### Retired old names (they show up in early material; never use them)

| Old name | Canonical |
|---|---|
| `internal/controlapi`, `internal/control` | `internal/ctlapi` (DTOs and client live in the public `api` package) |
| `internal/vault` | `internal/secrets` |
| `internal/secure/{integrity,injection,ssrf,audit}` | `internal/guard/*` — but only `ssrf` survived, as `netguard` |
| `internal/accesslog` | `internal/calllog`. The package was named for a ledger of `tools/call` evidence; it records every interaction with a downstream now, and `audit` named the use rather than the content. `<data>/audit` becomes `<data>/calls` (renamed once, on the first resolution), `access.jsonl` becomes `calls.jsonl` (the old name stays READABLE forever — an authenticated ledger does not restate its own history), the CLI group `audit` becomes `calls` with the `audit.*` settings kept as aliases, `/v1/audit/*` becomes `/v1/calls/*`, and the governance key `audit` becomes `calls` (read from both, written to one). The vault key name `__audit_encryption__` does NOT move: it is what every existing installation's key is filed under, and renaming it would lose the key that decrypts their days |
| `internal/integrity`, `internal/approval`, `internal/audit` | **Nothing.** Removed with the old runtime governance surface. `internal/calllog` is a new local ledger with no permission role, not a forwarding address; `internal/audit`'s one surviving primitive, the multi-writer JSONL append, is `internal/jsonl` |
| `internal/savings` | **Nothing.** Removed rather than renamed (§7 #9); `agenthub activity`, its only reader, went with it |
| `internal/gatewaymode` | `internal/gateway` |
| `internal/downstream/transport` | `internal/mcp/transport` |
| `package skill` | `package skills` |
| `agenthub tool ls` / `tool inspect` / `tool allow` | `agenthub server tool …` (the shim is gone; the old spelling is a usage error) |
| `agenthub tool allow <server> a b` (positional) | `agenthub server tool allow <server> --only a,b` |
| `agenthub tool allow <server>` (bare = block all) | `agenthub server tool allow <server> --none` |
| `agenthub tool allow <server> --clear` | `agenthub server tool allow <server> --all` |
| `agenthub profile tools <profile> <server>` | `agenthub profile tool allow …` (the shim is gone; `tools` now aliases the group, per §3) |
| `agenthub server tool ls --rules` | `agenthub server ls` (a TOOLS column) / `agenthub server inspect <server>`. `--rules` is still accepted as a hidden, deprecated spelling; deleting it is outstanding (§7 #8) |
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

These are CI failure conditions, not review conventions, and each must have a failing case proving it
actually bites (§6).

On rule 2: no `go get` of any MCP library. Bounded reads (16MB), `notifications/cancelled` forwarding,
inline replies to reverse RPC and the trailing 4KB of stdio stderr are protocol-layer invariants
needing precise control, while JSON-RPC encoding itself is not much work. The facade keeps the choice
**reversible** — swapping the implementation stays sealed inside one package.

---

## 3. Command naming rules

- **Resource groups are singular as the canonical name, with the plural as a cobra alias**:
  `server` / `profile` / `client` / `session` / `skill` / `secret` / `token`. One alias per group;
  `grep -n 'Aliases' internal/cli/*.go` is the whole list
- **Action/flow groups take no plural alias**: `daemon`, `connect`, `auth`, `calls`, `logs`, `events`,
  `config`, `doctor`, `catalog`. The plural marks a group you accumulate entries in, not every noun.
  `logs` reads **across every process log at once**, which is what separates it from `daemon logs`
  (one process) and `server logs` (one downstream connection's frames); the name may not move between
  the three. `events` reads the event log (`internal/eventlog`), not the daemon SSE stream it
  originally subscribed to. `activity` is **gone, not renamed** (§7 #9). The OAuth group is **`auth`**
- List subcommands are always `ls`, and **every command supports `--json`**, with human and machine
  output rendered from the same data structure
- **There is no top-level `tool` group.** The rule deciding which tools a server contributes lives on
  the server entry beside `enabled`, so the group hangs off `server` (§7 #7)
- **The two narrowing layers are one command at two altitudes**: `server tool` and `profile tool`,
  each `ls | inspect | allow`, the writes taking `--only a,b | --all | --none` and the listings
  `--search | --all`. They intersect and neither can widen. A second spelling invites a second mental
  model, and `--all` (drop the rule) against `--none` (store the empty list) is exactly the pair a
  second model gets backwards
- **A rule is read where it is stored; a listing reads its effect** (§7 #8)
- **There is no `scope` group.** Narrowing is what a profile *is* (`profile server` / `profile tool` /
  `profile discovery`); handing a surface out lives on `client` (`bind` / `unbind` / `ls`). The
  retired group maps one-to-one: `scope set --client X --profile P` → `client bind X P`,
  `scope clear --client X` → `client unbind X`, `scope ls` → `client ls`
- **A live session cannot be renarrowed.** What a client sees is decided before it connects, so
  `session` is read-and-terminate (`ls` / `show` / `kill`); edit the profile instead
- The `client` group is `ls | detect | inspect | connect | disconnect | bind | unbind` — `detect`
  stats and `inspect` reads (macOS TCC, see `internal/clients`). There is no `import`: a client's
  existing servers are brought over by pasting its configuration
- The `skill` group is `add | ls | inspect | rm | enable | disable | install-to | sync | update |
  verify` (`install-to` materializes one entry, `sync` in bulk by scope; both coexist)
- **`manual` prints the binary's own SKILL.md, and it is neither `skill doc` nor a flag.** A leaf
  command, no subcommands, no plural. Not under `skill`, because every invariant that group has is
  built for text the operator imported from elsewhere while this document is the binary describing
  itself, and because the group is withheld on the release page that most needs to answer "what is
  this" (docs/subsystems/cli.md); not the root `--skill` flag it shipped as in 0.42.0, because
  a flag that prints a document and exits is a verb, and being one made the root `RunE` hand-write a
  precedence check against unknown subcommands

### `add` and `enable` are separate primitives, and stay that way

`server add` writes the definition and **nothing else**: no connection, no probe, and the entry lands
**disabled**. `server enable` puts a server into service, and that is where the connection probe lives.
Folded together, `enabled` would mean both *the user wants this* and *it answered a probe*. Two
consequences that must not be "simplified" away: **the probe reports, it never vetoes**, so a
transient outage never becomes a configuration change; and **composition belongs to the caller** —
`catalog add` and `auth login` each offer one action over the two, and the primitives stay separate.

---

## 4. Known capability boundaries

Not a to-do list — an **honest grading of the current implementation**.

| Item | Status |
|---|---|
| Windows | Platform layer filled in; CI gates on `GOOS=windows` build + vet, but it has **never run on a real Windows machine**. [windows.md](windows.md) tracks what works and what is missing |
| GUI | Functionally complete, **not part of the default build** (the webview needs GTK/WebKit, which CI runners lack); `make gui` |
| skills materialization | **Client granularity only**, not per-session — the files live outside agenthub's read path |
| skills from git sources | Records and pins a revision, but **never runs git and never touches the network**; update without a local checkout returns a typed unsupported error rather than claiming you are up to date |
| TOON | A **one-way display projection with no decoder** (§7 #4); anything requiring a round trip never enters the encoder |
| teams | Deliberately unimplemented, and **nothing in the tree reserves a seam for it** — a hook nobody can reach reads as a decision already half-made |
| Telemetry / update checker | Decided against (§7 #6) — no data is collected |

### Three things that must never be retrofitted

1. **The composite vault key** `(serverID, scopeName)`, defaulting to `"_global"` — retrofitting it
   would touch every singleton in the token store, callback server and refresh coordinator.
2. **Registry self-write suppression** (§5c #1) — without it every self-write costs a reload cycle.
3. **X-Request-Id end to end**: the response header is written before the handler runs, error bodies
   carry it, and the log line for the failure carries the same value.

---

## 5. Reference-code policy

| Source | License | Usage |
|---|---|---|
| [smart-mcp-proxy/mcpproxy-go](https://github.com/smart-mcp-proxy/mcpproxy-go) | MIT / Go | **Reference only, never copy code** |
| [tsouth89/toolport](https://github.com/tsouth89/toolport) | MIT / Rust | A different language; likewise a design reference only |

The rule and its reasoning live in [AGENTS.md](../AGENTS.md) and are not restated here. What this file
adds is the line the tree cites §5 for: **a bound learned from them is not copied code.** Two constants
come across verbatim — `internal/httpbridge`'s ingress limits and `clients.MaxConfigSize` — and both
are what the policy permits: a number is a finding *about* the problem, and rederiving a different one
would be worse, not more independent. What may never cross is the implementation around it.

The root `NOTICE` records the design references (academic honesty, not a license obligation).

---

## 5b. MCP protocol scope

**Two protocol generations are spoken, and on both faces.** `mcp.SupportedVersions` is the list,
newest first — `2026-07-28`, `2025-11-25`, `2025-06-18`, `2025-03-26` — and it is the only place
either direction reads what it accepts.

- The **read side** (connecting to downstreams) probes with `server/discover`, and falls back to the
  `initialize` handshake only on proof the server answered *and is old*. An error code the
  specification reserves for itself (-32020 to -32099) is proof of the opposite: only a modern server
  produces one, so it propagates rather than triggering a fallback that would send `initialize` to a
  server that does not implement it. An older downstream negotiates downward from the same list
- The **exposure side** answers `server/discover` with that same list, and a request carrying the
  per-request `_meta` puts the session in stateless mode. `initialize` negotiates the **stateful
  family only**: a client declaring `2026-07-28` *there* gets the default instead, because echoing it
  would promise per-request `_meta` semantics on a session that used the handshake 2026 removed
- Transport support is asymmetric by direction: the read side speaks `stdio` + `streamable-http` +
  **legacy HTTP+SSE**; the exposure side speaks `streamable-http` only — no new SSE exposure surface
  (ruling #29). **That ruling is about the 2024-11-05 binding**, the two-endpoint one whose `endpoint`
  event names where to POST, and it still holds. It never covered streamable HTTP's own server→client
  stream, which is part of the transport the exposure side already speaks
- **Both generations are offered the out-of-band notification stream on the HTTP face.** This
  **retires** the stance recorded here that neither would be; it carries no ruling number because it
  was decided in this repository rather than in the design document §8 indexes. `GET` opens it on
  ≤ 2025-11-25 and
  `subscriptions/listen` on 2026-07-28, both in `internal/httpbridge/stream.go`; the stdio face pushes
  notifications inline, as it always has. The retired stance was inherited rather than argued — the GET
  stream was refused first and `subscriptions/listen` was refused for consistency with it — and what it
  cost was stated plainly in the same section: `tools/list_changed` is the ONLY trigger for catalog
  refresh, so a client on this face was served a tool set that could go stale forever while
  `initialize` handed it `{"tools":{"listChanged":true}}`. The declaration was the giveaway: this face
  was promising a channel it had decided not to build
- **The two directions are still not symmetric, and the reasons still differ.** AgentHub *asks* every
  streamable-http downstream for the server→client stream — the GET on ≤ 2025-11-25, the
  `subscriptions/listen` POST on 2026-07-28 — because it is the only channel by which such a downstream
  can report a tool-set change. Declining to *open* one leaves this hub's catalog wrong with nothing
  saying so; declining to *serve* one leaves a client's catalog wrong the same way, which is why that
  half is no longer declined. What remains asymmetric is the vocabulary: the read side accepts whatever
  a downstream sends, while the exposure side acknowledges only what it can actually produce
  (`carriedNotifications`, one method today), because 2026-07-28's acknowledgement is the only place a
  client learns that a type it subscribed to will never arrive

**`mcp.ProtocolVersion` does not name the version this tree targets, and flipping it to 2026 is
wrong.** It stays at `2025-11-25` because every context that reads it is definitionally pre-2026: the
legacy handshake, the exposure side's default answer, and the HTTP header on a request whose body
declared no version of its own. The 2026-07-28 declaration travels per-request in `_meta`, built
from `Version2026` directly — and `MCP-Protocol-Version` is read back out of that `_meta` rather
than from this constant, because the header and the body MUST agree and `server/discover` declares
2026 before anything is negotiated.
[mcp-2026-07-28.md](mcp-2026-07-28.md) §6.1 records why "flip the constant" was dropped.

### Upstream deprecation tracking

No removal is scheduled earlier than 2027-07-28 and every seam is in place. Each use site carries a
`// DEPRECATED-UPSTREAM(<feature>, earliest-removal: <date>)` comment, so one grep finds them all.

**`earliest-removal` is a removal date or the literal `none`, and never the date a feature was
deprecated.** The two are different facts and the `Deprecated in` column below carries the other one.
HTTP+SSE is the case that needs the word: it is deprecated with no removal planned, because ruling
#29 keeps it on the read side for servers that expose nothing else. Its three markers once read
`earliest-removal: deprecated 2025-03-26`, which puts a date already past in the field a sweep looks
at to decide what may be deleted — and it was the one entry that must not be acted on.

| Feature | Deprecated in | Dependency point | Migration seam |
|---|---|---|---|
| The `initialize` handshake | `2026-07-28` | The stateful session path, both faces | **Landed**: `server/discover` plus per-request `_meta`. The handshake stays for downstreams on the older generation |
| `ping` | `2026-07-28` | Liveness on the stateful path | None needed — a stateless request carries its own context |
| Roots | `2026-07-28` | `${ROOT}` and derived-instance keying (`internal/downstream`) | **In place**: `RootSource`, one implementation for the roots protocol, one for an explicit root in `clients.json` |
| Sampling | `2026-07-28` | One of the isolation arguments | None needed — the conclusion is independently supported |
| DCR | `2026-07-28` | The OAuth discovery chain; credentials persisted alongside tokens | **In place**: `ClientRegistrar`, one implementation for DCR, one for Client ID Metadata Documents |
| Logging | `2026-07-28` | Downstream log forwarding | None needed — the logging surface is first-party |
| HTTP+SSE transport | `2025-03-26` | One of the three transports | Kept on the read side; no new exposure side |

---

## 5c. The config hot-reload path (two things not to get wrong)

GUI/CLI edits a profile → the corresponding gateway updates automatically. The path is in
[flows.md#config-hot-reload](flows.md#config-hot-reload) and the mechanism in
[subsystems/registry.md](subsystems/registry.md); these two
rulings are what the mechanism exists to satisfy.

1. **Self-write suppression.** When the daemon writes `profiles.json` itself, fsnotify reports the
   event just the same, so the payload is registered in a bounded TTL set *before* writing and the
   watcher ignores anything that hits it. Without it, every self-write costs a reload cycle.
2. **The generation criterion is "the generation read ≥ the generation applied"**, never "equals the
   event's `Rev`". `Change{Kind, Rev}` is a **notification** carrying no snapshot, so the gateway
   re-reads the file itself; an equality test leaves rapid successive writes stuck on an old version,
   waiting for an event that will never come again.

---

## 5d. Collaboration conventions

**They live in [AGENTS.md](../AGENTS.md), not here** — worktree per feature, a draft PR per branch
kept current and closed by the landing, one commit per subtask, `main` stays linear (rebase, never
merge), `make ci-landing` **after** a rebase that moved the branch, `--ff-only` as the enforcement.

Development workflow is not an architectural convention, and `AGENTS.md` is the file every coding
agent reads first. This section once carried its own copy, and the copy had gone stale in the
dangerous direction: commit straight to `main`.

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
| Where reference repositories are cloned | `~/Develop/_refs/` (**outside the repository**) |
| Per-process frame stream | `<data>/calls/<day>/frames-<bootid>-p<pid>.jsonl` |

### Every depguard constraint must have a failing case

A violating sample plus a test that runs lint and asserts it fails (`internal/depguardtest`, which
plants its probes in a disposable copy of the checkout). **A lint rule that is configured but not in
effect is more dangerous than no rule at all** — and the same standard applies to the proof: when
golangci-lint is absent it calls `t.Skip` and `make test` counts that as success, so the CI gate greps
the verbose output for `--- SKIP` and fails on it. **A check that can quietly not run is a check you
do not have.**

**Current assembly status — one of the seven rules has no failing case, and cannot get one from this
harness.** `.golangci.yml` configures seven: `gui-and-api-no-internal` (two probes, one per half),
`mcp-stdlib-only`, `no-third-party-mcp-libs`, `pipeline-no-ctlapi`, `platform-zero-dep`,
`logx-zero-dep` and `guard-zero-dep`. Six are proven — `guard-zero-dep` closed the last of the
rule-4 trio with a probe shaped like the `platform-zero-dep` / `logx-zero-dep` pair, planting a
business import under `internal/guard/**` and asserting golangci-lint rejects it. The remaining gap
is recorded here because it cannot be closed the same way:

- **`no-third-party-mcp-libs`.** This one the harness as designed *cannot* prove. The rule denies
  three named third-party MCP SDKs repo-wide, none of which is in `go.mod`, while every probe must
  type-check — that is what makes a lint failure attributable to depguard and nothing else. A probe
  importing one of those packages would fail to build instead, so proving it needs a fake module
  planted in the disposable tree: a change to the harness, not one more case. `mcp-stdlib-only`'s
  probe does not cover it, since that probe plants `github.com/spf13/cobra` in `internal/mcp`, which
  the stdlib-only rule rejects on its own.

It is recorded rather than fixed because the harness change it needs is deliberately out of scope
for a tidy pass.

### Test infrastructure

A programmable **fake downstream MCP server** (`internal/testutil/fakemcp`) injects by script: slow
responses, half-written and malformed JSON-RPC frames, oversized payloads (hitting the 16MB bounded
read), handshake crashes, `tools/list_changed` storms, protocol violations.

Which **protocol generation** it speaks is scripted too, by `Script.SupportedVersions`: empty (the
default) means no `server/discover`, hence the pre-2026 handshake every script written before that
field relies on; a list containing `2026-07-28` makes it a stateless server that then *requires* the
per-request `_meta` on everything after the handshake. Only the subprocess driver reaches 2026 —
`transport`'s `negotiatedSetter` has an unexported method, so the in-process pipe cannot implement it
and `Handshake` fails closed rather than sending bare requests a strict server would reject.

Three classes of test have been in CI from day one:

1. **Golden tests** — the signature grammar, search ordering, error copy. **Determinism is the
   contract** (ruling #27): agents key retry logic and prompts off exact wording, so error text and
   ordering are frozen artifacts. Fix the code, never the golden.
2. **Cross-process concurrency tests** — single-line `O_APPEND` writes (`internal/jsonl` re-executes
   the test binary as N appending children and asserts no line was torn), monotonic generations.
3. **Daemon `kill -9` injection tests** — the stdio data plane is unaffected.

---

## 7. Decision records

The items that were once "to be decided". **All are decided**; each is registered so none gets
silently reopened, and the numbering is cited from code.

1. ~~Whether to pull lazy-connect forward~~ → **Decided: no.** Keep eager connect plus "answer from
   cache first" fast startup. The N×M process cost that motivated it is solved by the daemon's shared
   streamable-http pool instead, and a cold npx/uvx cache (10s to minutes; hence
   `DefaultConnectTimeout` at 120s) is better spent in the startup window, where `tools/list` answers
   from cache, than inside the first `tools/call`, where it reads as an unexplainable hang. The escape
   hatch stays open — `downstream.Deps.Dial` plus the tool cache — and the trigger should be measured
   cost, not a derivation.
2. ~~On-disk cache for shaping: bbolt or plain files~~ → **Decided: plain files.**
   `<data>/cache/shaping/<sha256(owner)>/<cursor>.json`, atomic writes (temp file in the same
   directory → 0600 → fsync → rename) + TTL sweeping + a sweep at startup. The access pattern is
   single-key point lookup with no transactions, and a corrupted entry costs exactly one cursor —
   where a single-file database would need a recovery mechanism to match that.
3. ~~Wails3 version and frontend stack~~ → **Decided:** `wails/v3 v3.0.0-alpha2.118` + vanilla TS +
   Vite (`@wailsio/runtime` is the only frontend runtime dependency). The fallback plan is not
   "switch frameworks", it is **compressing the alpha dependency down to three files**: only
   `cmd/agenthub-gui/gui_main.go`, `cmd/agenthub-gui/services/service_wails.go` and
   `cmd/agenthub-gui/tray_wails.go` are behind `//go:build wails`, while the bodies they assemble
   carry no tag and so compile, vet and unit-test in CI. The frontend also skips
   `wails3 generate bindings` in favour of `Call.ByName` plus `Events.On`.
   See [subsystems/gui.md](subsystems/gui.md).
4. ~~TOON grammar scope and the golden case set~~ → **Decided.** Both "determinism is the contract"
   grammars are frozen, with golden corpora in `internal/shaping/toonenc` and
   `internal/discovery/toolsig`:

   **(a) TOON is a one-way projection: no round trip, no decoder** — a round trip would need a type
   marker on every scalar (bare `1` vs `"1"`), exactly the tokens the encoding exists to save. The
   contract is stated in-band by a first line reading
   `#toon/1 (display encoding; send tool arguments as JSON)`, and anything that must survive a round
   trip (`structuredContent`, tool arguments, cursors) stays JSON. Two constructive guarantees:
   **never-larger** (below `MinSavingsPct`, default 10%, the input is returned unchanged) and
   **numeric fidelity** (no value is routed through float64).

   **(b) The compact signature grammar** — `name(p1:str, p2?:int=3, p3?~:obj{a,b}) -> str`, `?` for
   optional and `~` for lossy — is what search hits return *instead of* a schema.

   Two ordering invariants are not local to those packages and so live here: `shaping.ShapeResult`
   **re-encodes first and applies the budget second**, so the truncation trailer is always last, and
   re-encoding sits at the end of the delivery path, so nothing downstream can invalidate the budget
   that trailer describes.

   Two-stage describe is part of the same ruling: the meta-tools are **five, in a frozen order**
   (`status, search_tools, describe_tool, call_tool, fetch_result`); `describe_tool`'s visibility
   predicate is exactly `Surface.byExposed`, so it cannot be wider than search/tools_list/call; and it
   emits **one per-id error only, `not_found`** — differentiated errors would make it an enumeration
   oracle. Same rule as `fetch_result`.
5. ~~macOS keychain ACLs and unsigned development binaries~~ → **Decided: dev mode falls back to
   `secrets.enc` automatically.** Every `go build` produces a new unsigned binary, so the keychain ACL
   prompts again each time; when keyring detection fails, or `AGENTHUB_DEV_SECRETS=1` is set, writes
   fall back to `secrets.enc` with a 32-byte key persisted beside it (`secrets.enc.key`, 0600).
   **A key next to the ciphertext is obfuscation, not encryption at rest** — true of the dev fallback
   only; production uses `AGENTHUB_SECRET_KEY` or the OS keyring. Detection must **read, never write**
   (a `Set` probe triggers macOS's destructive confirmation dialog), caches per process, and gives
   every operation a hard timeout.
6. ~~Whether to build telemetry and an update checker~~ → **Decided: neither.** No telemetry (not even
   enum-only reporting, and no opt-in switch) and no update checker. This process holds every
   downstream credential, argument and result; "enums only" would need a `ScanForPII` gate to keep the
   promise, while not opening the channel costs nothing and is verifiable with an empty packet capture.

   Implementation constraint, equivalent to a CI-checkable property: **there exists no** outbound
   request anywhere in `internal/*` to an agenthub-owned domain or version manifest. Network egress
   falls into exactly three categories — downstream MCP servers, OAuth authorization servers, and
   endpoints the user configured explicitly. **Adding a fourth violates this decision.**
7. ~~Where the tool commands live, and how the two narrowing layers are spelled~~ → **Decided: under
   `server`, and identically.** `server tool ls | inspect | allow`, with `server tool allow <server>`
   and `profile tool allow <profile> <server>` taking the same `--only | --all | --none`.

   The cause of the spelling it replaced was that the command tree disagreed with where the rule is
   stored: a top-level `tool` sat in the withheld group, `tool ls` applied no allow list at all, and a
   bare `tool allow <server>` meant "expose nothing". The two layers stay ONE vocabulary, and there is
   still no `deny` verb at either altitude — allow and deny answer the arrival of a tool the
   downstream adds tomorrow in opposite directions.
8. ~~Where the tool RULES are read, now that they are written identically at both altitudes~~ →
   **Decided: a rule is reported by the resource that stores it; a listing reports the effect.**
   `server ls` / `server inspect` carry the global allow list, `profile ls` carries a profile's
   selectors, and `server tool ls` / `profile tool ls` list the tools each layer leaves offered.

   **`server tool ls --rules` is hidden and deprecated but still accepted.** It was meant to go one
   release later and has outlived that by several; deleting it is outstanding work.

   Two consequences worth keeping: the rule appears in `server ls` **only when some server carries
   one** (a column that never varies is a column readers learn to skip), and `profile tool ls --all`
   names WHICH layer took each tool. Two things that must not be re-simplified: the blocking layer is
   derived from the same `scope.Merge` as the verdict, never from a second reading of the rules; and
   `inspect` stays ONE implementation — `profile tool inspect` is that report narrowed after it is
   computed, machine-wide verdict kept, because hiding it would claim a profile allows something no
   client can reach.
9. ~~Whether to repair the token-savings ledger or remove it~~ → **Decided: removed.**
   `internal/savings`, `shaping`'s token estimator, the discovery-side projection and the
   `agenthub activity` command are gone. Its one writer fired only when `resultBudget` cut a result
   and `resultBudget` has no built-in default, so on an untouched install nothing was ever written.

   Three reasons not to repair it instead. **The unit was wrong** — four bytes per token is ±20% on
   English JSON and understates CJK, least accurate for the payloads most worth shrinking. **The claim
   was wrong** — it measured how much a configured budget truncated, a consequence of the operator's
   own setting. **The absence was worse than silence** — 0 because nothing was recorded is
   indistinguishable from 0 because it is broken.

   What must NOT be reintroduced under another name: an estimator with a fixed bytes-per-token divisor.
   Per-mechanism accounting, if ever wanted again, measures **bytes** — a fact this process observes,
   not a third party's tokenizer. `MinSavingsPct` is not an exception: it compares byte lengths to
   decide whether TOON is used at all, a data-path decision with no accounting in it.
10. ~~Whether anything but Homebrew installs the CLI, and what may read what it needs~~ →
    **Decided: a shell installer, and nothing inside the binary.** `brew` runs `git`, so it requires
    Xcode Command Line Tools; on a Mac without them the tap is not a slower path, it is no path.
    `scripts/install.sh` covers macOS and Linux for the **CLI only**, using nothing but base-system
    tools — sh, curl or wget, tar, awk, sed, one of shasum / sha256sum / openssl. The macOS app stays
    a cask, because unpacking a DMG and owning `/Applications` is what a cask already does correctly.

    It is driven by `manifest.json`, one stable-named asset per Release rendered by
    `scripts/release-manifest.sh` from the same `checksums-cli.txt` the formula reads. The manifest
    exists because the artifacts carry a build id in their names, which is exactly what GitHub's
    `releases/latest/download/<name>` redirect cannot serve. Three properties are not decoration:
    asset names and hashes are **read back** from the checksums file rather than recomposed (the rule
    the formula and cask already follow); the download is verified before anything is unpacked or
    moved and there is **no `--skip-verify`**; and the unpacked binary must identify itself as the
    manifest's version and **not** as `(dev)` — the one failure a checksum cannot catch, whose only
    other symptom is a user's servers appearing to vanish.

    **The manifest's bytes are verified; its strings are not trusted.** Nothing checks the manifest
    itself — it is the root — and `AGENTHUB_INSTALL_MANIFEST_URL` exists so a mirror may serve it.
    Its strings then become a local path, a file name, and the syntax of the install receipt, so each
    is constrained where it enters: the asset must be a plain file name (its download destination is
    written **before** the checksum, which is what the checksum is computed over); `version` and
    `commit` must not carry what would leave the hand-written receipt unparseable; the unpacked
    `agenthub` must not be a symlink, which `[ -f ]` follows and `chmod` then acts on. Allow lists in
    every case, per §6's rule for selectors — these must also hold for whatever a manifest names next
    year.

    **Every action lives in a function, and the last line calls `main`.** The documented invocation
    pipes the file into a shell, which runs what has arrived; a truncated download must define an
    installer and run none of it rather than run the half it received. `test/installer` asserts that
    over every line-boundary prefix.

    **What it does not buy.** The manifest ships from the release it describes, so it cannot vouch
    for those bytes independently. This is the cask's chain of trust, not a stronger one; signing the
    artifacts is what would change that, and there is none.

    **Decision 6 is not reopened by this.** The manifest is fetched by a script the user ran on
    purpose; no code under `internal/*` may request it, and `agenthub self-update` — the obvious next
    step — would be precisely the fourth egress category decision 6 forbids. Shipping one means
    amending decision 6 first, here, on the record.

11. ~~Whether a `tools/call` that cannot be recorded may run~~ → **Decided: it runs.** Every
    observability stream in the tree now fails OPEN — `logs`, `events`, the wire trace, and both
    tiers of the call ledger. A write failure costs the history a line, is logged at Error
    (`ledger record dropped; the call is unaffected`), and costs the call nothing.

    The evidence tier used to refuse the governed method, on the rule that an unrecorded call is a
    governance gap. Three things were wrong with it. **It protected nothing**: the record it was
    defending was already lost at the moment the write failed, so refusing afterwards only added a
    second failure. **It put availability in the wrong place**: a full disk or an unreadable vault
    stopped every tool a client had, and this ledger has no permission role (§7 #9's neighbour,
    the `internal/audit` retirement, says so — it is a local record, not a gate). **One of its three
    sites could not even be safe**: the finish is written after the downstream has run, so replacing
    that response reported a failure that had not happened and invited a client to repeat a side
    effect.

    What did not change: metadata is still always on, evidence is still opt-in behind
    `calls.enabled`, recording still happens before the gate chain, and the capacity, retention and
    free-space bounds are still hard — nothing is written past them. Fail-open is about the CALL,
    never about the bound. The behaviour lives in `subsystems/records.md` (`internal/calllog`, the
    two-tier table) and `flows.md`.

---

## 8. Historical ruling ids

Around sixty comments cite a ruling by a number from the original design document — `ruling #8`,
`A.1 #8`, `A.6 #5`. **That document is not in this repository**, so without this table those citations
resolve to nothing. The ids are kept because they are *stable* while the rules are *live*: a ruling
number does not move when a section is renumbered, which is exactly what a citation wants.

**`A.6 #N` is `§7 #N`.** The appendix's open questions are the decision records above, in the same
order. Prefer the `§7` spelling in new comments.

Two conventions, so this table cannot quietly rot: the bare `#N` and the `A.x #N` spellings of one
ruling are one row, and **a number not listed here may not be cited** —
`TestHistoricalRulingIdsResolve` fails on an unregistered id. Milestone *task* numbers (`M0-7`,
`M1-3`) are not rulings and are not citable at all.

| Cited as | What it ruled | Where the rule lives now |
|---|---|---|
| `#7`, `A.1 #7` | Two id shapes on purpose: the human `client:seq` for the CLI, a random token for the protocol — CLI ids are for typing, protocol ids are for not guessing | subsystems/scope.md |
| `#18` | Lazy mode's `call_tool` may split into read/write/destructive **intent variants**, and compatibility mode stays byte-identical to the pre-variant surface | docs/model.md#how-the-surface-is-presented; subsystems/exposure.md |
| `#27` | **Determinism is the contract**: goldens pin the wire shape; fix the code, never the golden | §6 |
| `#29` | Legacy HTTP+SSE is a **read-side** transport only, never offered on the exposure side | §5b |
| `#32` | `internal/mcp` is standard-library only — one first-party protocol facade | §2 rule 2 |
| `A.2 #9` | The manual paste loop, for providers that cannot reach a loopback redirect | status/oauth.md |
| `A.2 #10` | Refresh is serialized: daemon singleflight online, a file lock offline | status/oauth.md |
| `A.3 #1` | Cross-process shared state takes a **file lock** or an atomic rename, proven by an N-process acceptance test. Now governs the rate-limit counters, the registry and the credential vault | §6; subsystems/registry.md; subsystems/credentials.md |
| `A.3 #2` | `kill -9` on the daemon: the stdio data plane is untouched and gateways re-register | §6; flows.md |
| `A.3 #4` | A daemon restart makes the session overlay vanish on **both** sides. Retired by its own logic: a session now carries no scope of its own at all | subsystems/scope.md |
| `A.3 #5` | skills materialization is **client-granular**, never per-session | §4 |
| `A.5 #23` | Windows is confined to a seam inside `internal/platform`; nothing outside it branches on the platform | §4; windows.md |
| `A.5 #26` | The **composite vault key** `(serverID, scopeName)` from day one | §4 ("never retrofitted", item 1) |
| `A.5 #30` | The roots **migration seam** (`RootSource`), in place before upstream deprecation | §5b (deprecation table) |
