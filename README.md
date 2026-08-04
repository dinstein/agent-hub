<div align="center">

# AgentHub

**One configuration, one set of credentials, one aggregation point — shared by every AI client.**

Claude Code · Cursor · Codex · Open WebUI · and 8 more

[![CI](https://github.com/dinstein/agent-hub/actions/workflows/ci.yml/badge.svg)](https://github.com/dinstein/agent-hub/actions/workflows/ci.yml)
[![Go 1.26+](https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)
[![Version](https://img.shields.io/badge/version-0.27.1-blue.svg)](VERSION)
[![Platforms](https://img.shields.io/badge/platforms-macOS%20%7C%20Linux%20%7C%20Windows%20(exp.)-lightgrey.svg)](#platforms)
[![Telemetry: none](https://img.shields.io/badge/telemetry-none-brightgreen.svg)](#privacy-no-data-collection)

**[中文文档](README.zh-CN.md)** · [Architecture](docs/architecture.md) · [User guide](docs/guide.md) · [Flows](docs/flows.md)

</div>

---

Every AI client wants its own copy of your MCP server list, its own copy of your API keys, and
its own idea of what a tool is allowed to do. AgentHub is the one place that holds all three, and
hands each client exactly the surface you decided it should see.

```
   Claude Code ──┐                                   ┌── linear
   Cursor ───────┤      ┌──────────────────┐         ├── github
   Codex ────────┼─────►│     AgentHub     │────────►┼── postgres
   Open WebUI ───┤      │  one config      │         ├── filesystem
   … 8 more ─────┘      │  one credential  │         └── …
                        │  one pipeline    │
                        └──────────────────┘
```

- **A single required binary, `agenthub`** — `connect` (stdio gateway, one process per client),
  `daemon` (shared HTTP pool + control plane + coordination plane), plus CLI management subcommands
- **An optional GUI, `agenthub-gui`** — Wails3, consumes the control-plane API only; it has no
  capability the CLI lacks

> **Status: feature-complete against its design.** CI is green across both matrices, and end-to-end
> acceptance passes with real Claude Code calling real downstream MCP servers through the gateway.
> macOS and Linux are verified; Windows is **experimental** — platform layer filled in, two commands
> still unimplemented, never run on real hardware ([details](#platforms)).

## Install

```bash
brew tap dinstein/agenthub

brew install agenthub                 # the CLI, which is all you need
brew install --cask agenthub-gui      # the macOS app, and the CLI with it
```

The cask installs `AgentHub.app` and depends on the formula, so the `agenthub`
on your `$PATH` has exactly one owner either way. The app is ad-hoc signed but **not
notarized**: the cask clears the quarantine flag macOS puts on downloads, and its pinned
sha256 is what vouches for the bytes instead of Gatekeeper. Windows and Linux have no
package yet — take the `.zip` or the `.tar.gz` from
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
claude-code`, so every server you add later is picked up without touching the client's config
again. Full walkthrough — profiles, narrowing, the whole model — in [docs/guide.md](docs/guide.md).

## Capabilities

| Area | What it covers |
|---|---|
| Protocol | MCP `2026-07-28` (stateless per-request `_meta`, `server/discover`, MRTR, `subscriptions/listen`) plus the stateful generations `2025-11-25` / `2025-06-18` / `2025-03-26`, on **both faces**: downstream, `server/discover` picks the highest mutually supported version and falls back to `initialize`; upstream, the gateway answers whichever generation each client speaks. Tools only — resources and prompts are not proxied, and extension capabilities are not forwarded (fail closed). Details: [docs/mcp-2026-07-28.md](docs/mcp-2026-07-28.md) |
| Gateway | stdio (one process per client) + streamable-http (shared daemon pool); three downstream transports: stdio / streamable-http / legacy HTTP+SSE |
| Discovery | Three modes — `full` / `grouped` / `lazy`; lazy mode ships five meta-tools (`status`, `search_tools`, `describe_tool`, `call_tool`, `fetch_result`) plus intent variants; compact signature grammar + two-stage describe |
| Access | Decided in advance, never at call time: a server is on or off and offers all its tools or a named subset; a profile takes a subset of the servers and may narrow their tools further; a client follows a profile. Layers intersect and none can widen; a dangling profile reference fails closed to an empty set |
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
| [docs/flows.md](docs/flows.md) | Sequence diagrams and failure branches for six key flows |
| [docs/modules/](docs/modules/) | Per-package documentation: responsibilities, key types, invariants and failure directions |
| [docs/canonical.md](docs/canonical.md) | The single source of truth for architectural conventions: frozen identifiers, package layout, dependency constraints, command naming rules, and every decision record |
| [docs/windows.md](docs/windows.md) | Windows status: what is implemented, what still does not work, and acceptance criteria |

Chinese translations cover the product surface — this README, [docs/zh-CN/guide.md](docs/zh-CN/guide.md)
and [docs/zh-CN/architecture.md](docs/zh-CN/architecture.md). The rest is English only, because those
documents move whenever the code does and the copy that gets forgotten looks exactly like the copy
that is current.

## Platforms

| Platform | Status |
|---|---|
| macOS | ✅ Supported, exercised by CI |
| Linux | ✅ Supported, exercised by CI |
| Windows | 🧪 **Experimental**: the platform layer is filled in (`LockFileEx` cross-process locks, named-pipe control plane with SDDL, api dialing, portable zip packaging) and CI gates on `GOOS=windows` build + vet. Two things still do not work — `daemon stop` and `client connect`'s user-level paths — and nothing has ever run on a real Windows machine. [Details](docs/windows.md) |

## Privacy: no data collection

AgentHub **collects no data**. No telemetry, no crash reporting, no usage statistics, no update
checker — not disabled by default, not available to turn on: the channel simply does not exist.
No request ever targets an AgentHub-owned domain.

The process makes only three kinds of outbound connection, all determined by your configuration:
the downstream MCP servers in `servers.json`, those servers' OAuth authorization servers (only
after you run `agenthub auth login`), and endpoints you specify explicitly (`server add --url`).

The access ledger and per-server wire traces are **written to local disk only**. Version updates are left to your package manager.
See the decision record in [canonical.md](docs/canonical.md) §7, item 6.

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

Four dependency-direction constraints are enforced by CI (see [canonical.md](docs/canonical.md) §2):
`cmd/agenthub-gui` and `api` must not import `internal/*`; `internal/mcp` is standard-library-only
and is the sole MCP protocol facade; `internal/pipeline` must not import `internal/ctlapi`; and
`internal/mcp`, `internal/platform`, `internal/logx`, `internal/guard/*` stay zero-business-dependency.

## License

MIT © 2026 dinstein (design references are listed in [NOTICE](NOTICE))
