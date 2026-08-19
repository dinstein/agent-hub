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

You use more than one AI client. So the same MCP server is written down in four config files in four
formats, and the fourth is out of date. The same API key sits in all four, and rotating it is a
search-and-replace across your home directory. Signing in is per client, for the clients that can do
OAuth at all. And every tool you enabled is in every client's context on every turn, whether or not
that client was ever meant to use it.

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

**macOS and Linux, no package manager** — the same command installs and updates:

```bash
curl -fsSL https://raw.githubusercontent.com/dinstein/agent-hub/main/scripts/install.sh | sh
```

It checks the download against the sha256 its release pins before anything is unpacked, puts
`agenthub` in `~/.local/bin` (`--prefix` moves it), and prints the `PATH` line to add rather than
editing a shell profile. `--uninstall` reverses it, `--help` lists the rest, and
[the script](scripts/install.sh) is worth reading first if piping one into a shell is not your habit.

**With Homebrew** — and the only way to get the macOS app:

```bash
brew tap dinstein/agenthub

brew install agenthub                 # the CLI, which is all you need
brew install --cask agenthub-gui      # the macOS app, and the CLI with it
```

Take one path or the other: each puts a binary at a path it believes it owns, and neither notices the
other afterwards. The app is ad-hoc signed but **not notarized** — the cask clears the quarantine
flag macOS puts on downloads, and its pinned sha256 is what vouches for the bytes instead of
Gatekeeper. Windows has no package; take the `.zip` from
[Releases](https://github.com/dinstein/agent-hub/releases).

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
claude-code`, so every server you add later is picked up without touching that config again. To see
that it took, open Claude Code and run `/mcp`. The whole model — profiles, narrowing, discovery
modes — is in [docs/guide.md](docs/guide.md).

To let an agent drive the CLI rather than you, hand it the skill the binary carries:

```bash
mkdir -p ~/.claude/skills/agenthub && agenthub manual > ~/.claude/skills/agenthub/SKILL.md
```

`agenthub manual` prints the copy compiled into the binary you just installed, so the document and
the CLI it describes are the same release.

## What makes it different

- **It is a CLI, and the GUI has no capability it lacks.** One Go binary: `connect` is the stdio
  gateway (one process per client), `daemon` is the shared HTTP pool carrying the control and
  coordination planes, and the rest are management subcommands. No runtime, no account, no
  background updater. The optional `agenthub-gui` is a view onto the same control-plane API, so
  anything you can click you can script.
- **Sign in once, not once per client.** Credentials resolve through environment → explicit bare
  environment → the encrypted `secrets.enc` vault → the OS keyring, under the composite key
  `(serverID, scopeName)`. OAuth runs headless with cross-process refresh coordination, so four
  clients pointed at one server do not race for one token, and no key is ever copied into a client's
  config file.
- **Hundreds of tools, five names in the context.** `lazy` discovery hands a client `status`,
  `search_tools`, `describe_tool`, `call_tool` and `fetch_result`, and everything else is found on
  demand through a compact signature grammar. `grouped` and `full` are there for the surfaces small
  enough not to care.
- **What a client may reach is decided before it connects.** A server's tool subset, then a profile,
  then a client: every layer intersects, none can widen, and a dangling reference fails closed to an
  empty set. Nothing is decided while a call is in flight — no approval queue, no runtime scope
  change — which is what makes `agenthub server disable` an unconditional kill switch rather than a
  suggestion.
- **An unfamiliar server can run with no network and nothing mounted.** `runtime: docker` gives a
  stdio server no network by default, mounts only the directories you declared (read-only unless you
  said otherwise), holds it to explicit resource limits, and keeps secrets out of argv. Isolation a
  config claims is delivered or refused, never quietly downgraded to the host.

## Capabilities

The five above are the argument; this is the matrix.

| Area | What it covers |
|---|---|
| Protocol | MCP `2026-07-28` (stateless per-request `_meta`, `server/discover`, MRTR, `subscriptions/listen`) plus `2025-11-25` / `2025-06-18` / `2025-03-26`, negotiated on both faces. Tools only — resources, prompts and extension capabilities are not forwarded ([details](docs/status/mcp-2026-07-28.md)) |
| Gateway | stdio (one process per client) + streamable-http (shared daemon pool); downstream over stdio / streamable-http / legacy HTTP+SSE |
| Discovery | `full` / `grouped` / `lazy`; five meta-tools plus intent variants, compact signature grammar, two-stage describe |
| Security | Spawn guard (anti-smuggling), bidirectional SSRF predicates screened inside `DialContext`, agent tokens graded read/write/destructive, cooperative call quotas |
| Result shaping | Pagination, budgets, `fetch_result` caching, TOON projection encoding (never-larger and numeric fidelity, both constructive) |
| Clients | Config adaptation for 12 client types (format-driven), two-layer skills management, skills-over-MCP provisioning |
| Operations | `doctor` health check, encrypted bounded call ledger behind `calls`, `logs` merging every process into one stream, `events` in a closed vocabulary, per-server wire trace, X-Request-Id end to end |

## Documentation

| File | Contents |
|---|---|
| [docs/guide.md](docs/guide.md) | **Using it** — server / profile / client, the everyday path, the decisions you actually have to make |
| [docs/architecture.md](docs/architecture.md) | **Changing it** — process model, package map, what a call passes through |
| [docs/flows.md](docs/flows.md) | Sequence diagrams and failure branches for seven runtime flows |
| [docs/subsystems/](docs/subsystems/) | Per-seam invariants and failure directions |
| [docs/model.md](docs/model.md) | What a client is allowed to reach, and who decided it |
| [docs/conventions.md](docs/conventions.md) | Frozen identifiers, dependency constraints, naming rules |
| [docs/decisions/](docs/decisions/) | One settled question per file, and what would reopen it |
| [docs/status/windows.md](docs/status/windows.md) | Windows status: what is implemented, what is unverified, acceptance criteria |

## Platforms

**Feature-complete against its design.** CI is green across both matrices, and end-to-end acceptance
passes with real Claude Code calling real downstream MCP servers through the gateway.

| Platform | Status |
|---|---|
| macOS | ✅ Supported, exercised by CI |
| Linux | ✅ Supported, exercised by CI |
| Windows | 🧪 **Experimental**: every capability is implemented (`LockFileEx` cross-process locks, named-pipe control plane with SDDL, api dialing, `daemon stop`, `client connect`, portable zip packaging) and CI gates on `GOOS=windows` build + vet, but nothing has ever run on a real Windows machine. [Details](docs/status/windows.md) |

## Privacy: no data collection

AgentHub **collects no data** — no telemetry, no crash reporting, no usage statistics, no update
checker. Not disabled by default, not available to turn on: the channel does not exist, and no
request ever targets an AgentHub-owned domain.

The only outbound connections are the ones your configuration names: the downstream servers in
`servers.json`, those servers' OAuth authorization servers (only after `agenthub auth login`), and
endpoints you gave explicitly. The call ledger is **written to local disk only**, and version updates
are left to your package manager — the decision record is
[docs/decisions/0006-no-telemetry-and-no-update-checker.md](docs/decisions/0006-no-telemetry-and-no-update-checker.md).

## Development

Requires Go 1.26+ and golangci-lint v2.

```bash
make         # list every target, one line each
make ci      # build + test + lint
make gui     # the GUI, built separately — it is excluded from the default build
```

The contributor rules — the worktree branch flow, the commit convention, and the four
dependency-direction constraints CI enforces — live in [AGENTS.md](AGENTS.md), with the decision
records behind them in [docs/conventions.md](docs/conventions.md).

## License

MIT © 2026 dinstein (design references are listed in [NOTICE](NOTICE))
