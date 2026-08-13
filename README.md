<div align="center">

# AgentHub

**Configure an MCP server once. Every AI client gets it — with the credentials, and with only the
tools you allowed.**

Claude Code · Cursor · Codex · Zed · Open WebUI · 7 more — one binary, no account, no telemetry

[![CI](https://github.com/dinstein/agent-hub/actions/workflows/ci.yml/badge.svg)](https://github.com/dinstein/agent-hub/actions/workflows/ci.yml)
[![Go 1.26+](https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)
[![Version](https://img.shields.io/github/v/release/dinstein/agent-hub?label=version&color=blue)](https://github.com/dinstein/agent-hub/releases/latest)
[![Platforms](https://img.shields.io/badge/platforms-macOS%20%7C%20Linux%20%7C%20Windows%20(exp.)-lightgrey.svg)](#platforms)
[![Telemetry: none](https://img.shields.io/badge/telemetry-none-brightgreen.svg)](#privacy-no-data-collection)

**[中文文档](README.zh-CN.md)** · [Architecture](docs/architecture.md) · [User guide](docs/guide.md) · [Flows](docs/flows.md)

</div>

---

## The problem it removes

You use more than one AI client. So today:

- The same MCP server is written down in four config files, in four formats, and the fourth one is
  out of date.
- The same API key sits in those same four files, and rotating it is a search-and-replace across
  your home directory.
- Signing in is per client — a separate browser round trip each time, for the clients that can do
  OAuth at all.
- Every tool you enabled is in every client's context on every turn, whether or not that client was
  ever meant to use it.

AgentHub is one local process holding the servers, the credentials and the rules, handing each
client exactly the surface you decided it should see.

```
   Claude Code ──┐                                   ┌── linear
   Cursor ───────┤      ┌──────────────────┐         ├── github
   Codex ────────┼─────►│     AgentHub     │────────►┼── postgres
   Open WebUI ───┤      │  one config      │         ├── filesystem
   … 8 more ─────┘      │  one credential  │         └── …
                        │  one pipeline    │
                        └──────────────────┘
```

|  | Without AgentHub | With AgentHub |
|---|---|---|
| Add a server | edit every client's config, in its own format | `agenthub server add` — once |
| Rotate a key | find every copy of it first | one vault, keyed `(server, scope)` |
| Sign in | per client, if it supports OAuth at all | `agenthub auth login`, headless, shared |
| Context cost | every tool of every server, every turn | five meta-tools, the rest searched on demand |
| Withdraw a server | one edit per client, then restart each | `agenthub server disable`, unconditional |
| A call went wrong | whatever the client happened to log | `calls` · `logs` · `events` · per-server wire trace |

## Install

**The CLI, macOS and Linux, no package manager** — the same command installs and updates:

```bash
curl -fsSL https://raw.githubusercontent.com/dinstein/agent-hub/main/scripts/install.sh | sh
```

`brew` runs `git`, so it needs Xcode Command Line Tools; this needs nothing the system does not
already have. It puts `agenthub` in `~/.local/bin` (`--prefix` moves it), checks the download
against the sha256 its release pins before anything is unpacked, and prints the `PATH` line to add
rather than editing a shell profile. `--uninstall` reverses it, `--help` lists the rest, and
[the script](scripts/install.sh) is worth reading first if piping one into a shell is not your
habit.

**With Homebrew** — and the only way to get the macOS app:

```bash
brew tap dinstein/agenthub

brew install agenthub                 # the CLI, which is all you need
brew install --cask agenthub-gui      # the macOS app, and the CLI with it
```

Take one path or the other: both put a binary at a path they each believe they own, and neither
notices the other afterwards. The cask installs `AgentHub.app` and depends on the formula, so the
`agenthub` on your `$PATH` has exactly one owner that way too. The app is ad-hoc signed but **not
notarized**: the cask clears the quarantine flag macOS puts on downloads, and its pinned sha256 is
what vouches for the bytes instead of Gatekeeper — the same thing the script's checksum does, and no
stronger, since both ship from the release they describe. Windows has no package — take the `.zip`
from [Releases](https://github.com/dinstein/agent-hub/releases).

## Quickstart

```bash
# 1. register a downstream MCP server (written down, still switched off)
agenthub server add linear --url https://mcp.linear.app/mcp

# 2. switch it on — this connects first, and says what it still needs
agenthub server enable linear

# 3. sign in, if step 2 asked for it (this enables the server too)
agenthub auth login linear

# 4. connect a client — once, ever
agenthub client connect claude-code --dry-run   # look first
agenthub client connect claude-code
```

Step 4 happens **once per client**: the entry it writes runs `agenthub connect --client
claude-code`, so every server you add later is picked up without touching the client's config
again. To see that it took, open Claude Code and run `/mcp` — an `agenthub` entry is listed,
carrying every server you enabled. Full walkthrough — profiles, narrowing, the whole model — in
[docs/guide.md](docs/guide.md).

To let an agent drive the CLI rather than you, hand it the skill the binary carries:

```bash
mkdir -p ~/.claude/skills/agenthub && agenthub manual > ~/.claude/skills/agenthub/SKILL.md
```

`agenthub manual` prints the copy compiled into the binary you just installed, so the document and the CLI
it describes are the same release. The Homebrew tap publishes that same file; nothing needs a
checkout to get it.

## What makes it different

### It is a CLI, and the GUI has no capability it lacks

One Go binary, `agenthub`: `connect` is the stdio gateway (one process per client), `daemon` is the
shared HTTP pool carrying the control and coordination planes, and everything else is management
subcommands. Nothing else is required — no runtime, no account, no background updater. The optional
`agenthub-gui` is a view onto the same control-plane API, so anything you can click you can script,
and a headless box never needs it.

### Sign in once, not once per client

Credentials resolve through four levels — environment, explicit bare environment, the encrypted
`secrets.enc` vault, the OS keyring — under the composite key `(serverID, scopeName)`, so two
servers may hold two different tokens for the same provider. OAuth runs headless, with three
callback modes and cross-process refresh coordination, so four clients pointed at one server do not
race for one token. No key is ever copied into a client's config file.

### Hundreds of tools, five names in the context

`lazy` discovery is the default: a client is handed `status`, `search_tools`, `describe_tool`,
`call_tool` and `fetch_result`, and finds everything else on demand through a compact signature
grammar and a two-stage describe. `grouped` and `full` are there for the surfaces small enough not
to care.

### What a client may reach is decided before it connects

A server offers all of its tools or a named subset; a profile takes a subset of the servers and may
narrow their tools further; a client follows one profile. Every layer intersects and none can
widen, and a dangling reference fails closed to an empty set. Nothing is decided while a call is in
flight — no approval queue, no runtime scope change — which is what makes `agenthub server disable`
an unconditional kill switch rather than a suggestion.

### An unfamiliar server can run with no network and nothing mounted

`runtime: docker` gives a stdio server no network by default, mounts only the directories you
declared (read-only unless you said otherwise), holds it to explicit resource limits, and keeps
secrets out of argv. Isolation a config claims is delivered or refused: a `docker` server that
cannot be isolated fails rather than quietly running on the host.

## Capabilities

| Area | What it covers |
|---|---|
| Protocol | MCP `2026-07-28` (stateless per-request `_meta`, `server/discover`, MRTR, `subscriptions/listen`) plus the stateful generations `2025-11-25` / `2025-06-18` / `2025-03-26`, negotiated on both faces — each downstream at the highest mutual version, each client in whichever generation it speaks. Tools only; resources, prompts and extension capabilities are not forwarded (fail closed). Details: [docs/mcp-2026-07-28.md](docs/mcp-2026-07-28.md) |
| Gateway | stdio (one process per client) + streamable-http (shared daemon pool); three downstream transports: stdio / streamable-http / legacy HTTP+SSE |
| Discovery | Three modes — `full` / `grouped` / `lazy`; lazy mode ships five meta-tools (`status`, `search_tools`, `describe_tool`, `call_tool`, `fetch_result`) plus intent variants; compact signature grammar + two-stage describe |
| Access | Decided in advance, never at call time: a server offers all of its tools or a named subset, a profile narrows servers, a client follows a profile — layers intersect, none can widen, and a dangling reference fails closed to an empty set. The full model: [docs/guide.md](docs/guide.md) |
| Security | Spawn guard (anti-smuggling), bidirectional SSRF predicates with in-DialContext screening, agent tokens graded read/write/destructive on the HTTP face, cooperative call quotas. These refuse a destination or a process regardless of who asked — none of them inspects what a downstream returned |
| Isolation | **Docker isolation spawner**: `runtime: host\|docker`, no network by default, mounts only explicitly declared directories (read-only by default), resource limits, secrets never enter argv |
| Result shaping | Pagination / budgets / `fetch_result` caching / TOON one-way projection encoding (with two constructive guarantees: never-larger and numeric fidelity) |
| Credentials | Four-level resolution chain (env → explicit bare env → `secrets.enc` → OS keyring), composite vault key `(serverID, scopeName)`, headless OAuth with three callback modes + refresh coordination |
| Clients | Config adaptation for 12 client types (format-driven), two-layer skills management (library/install), skills-over-MCP provisioning |
| Operations | `agenthub doctor` full health check; encrypted, bounded tools/call history behind `agenthub calls`; `agenthub logs` merging every process's log into one stream; `agenthub events` for server/gateway/daemon state changes in a closed vocabulary; per-server JSON-RPC wire trace (off by default, `server trace`); X-Request-Id across the whole chain |

## Documentation

| File | Contents |
|---|---|
| [docs/guide.md](docs/guide.md) | **Using it**: the three nouns (server / profile / client), the everyday path, and the decisions you actually have to make |
| [docs/architecture.md](docs/architecture.md) | **Changing it**: process model, core module map, layering and dependency constraints, what a single call passes through, the three data-flow directions, the two lines of defense |
| [docs/flows.md](docs/flows.md) | Sequence diagrams and failure branches for seven runtime flows |
| [docs/modules/](docs/modules/) | Per-package documentation: responsibilities, key types, invariants and failure directions |
| [docs/canonical.md](docs/canonical.md) | The single source of truth for architectural conventions: frozen identifiers, package layout, dependency constraints, command naming rules, and every decision record |
| [docs/windows.md](docs/windows.md) | Windows status: what is implemented, what remains unverified on real hardware, and acceptance criteria |

Chinese translations cover the product surface — this README, [docs/zh-CN/guide.md](docs/zh-CN/guide.md)
and [docs/zh-CN/architecture.md](docs/zh-CN/architecture.md). The rest is English only, because those
documents move whenever the code does and the copy that gets forgotten looks exactly like the copy
that is current.

## Platforms

**Feature-complete against its design.** CI is green across both matrices, and end-to-end acceptance
passes with real Claude Code calling real downstream MCP servers through the gateway.

| Platform | Status |
|---|---|
| macOS | ✅ Supported, exercised by CI |
| Linux | ✅ Supported, exercised by CI |
| Windows | 🧪 **Experimental**: every capability is implemented (`LockFileEx` cross-process locks, named-pipe control plane with SDDL, api dialing, `daemon stop`, `client connect`, portable zip packaging) and CI gates on `GOOS=windows` build + vet, but nothing has ever run on a real Windows machine. [Details](docs/windows.md) |

## Privacy: no data collection

AgentHub **collects no data**. No telemetry, no crash reporting, no usage statistics, no update
checker — not disabled by default, not available to turn on: the channel simply does not exist.
No request ever targets an AgentHub-owned domain.

The process makes only three kinds of outbound connection, all determined by your configuration:
the downstream MCP servers in `servers.json`, those servers' OAuth authorization servers (only
after you run `agenthub auth login`), and endpoints you specify explicitly (`server add --url`).

The call ledger — lifecycle records, the frames of a traced server, and the encrypted payloads of
both — is **written to local disk only**. Version updates are left to your package manager. See the
decision record in [canonical.md](docs/canonical.md) §7, item 6.

## Development

Requires Go 1.26+ and golangci-lint v2.

```bash
make         # list every target, one line each
make build   # go build ./...
make test    # go test ./...
make lint    # golangci-lint run
make ci      # build + test + lint
make gui     # the GUI, built separately (see below)
```

The GUI is **excluded from the default build**: linking a webview needs GTK/WebKit packages that
CI runners do not have, so all Wails code carries the `//go:build wails` tag and a default build
produces a placeholder main. `make gui-frontend` / `make gui-go` build the two halves separately.
The frontend is vanilla TS + Vite with `@wailsio/runtime` as its only runtime dependency, and it
reaches the control plane solely through the `api` package — it has no capability the CLI lacks.

The contributor rules — the worktree branch flow, the commit convention, and the four
dependency-direction constraints CI enforces — live in [AGENTS.md](AGENTS.md), with the decision
records behind them in [canonical.md](docs/canonical.md).

## License

MIT © 2026 dinstein (design references are listed in [NOTICE](NOTICE))
