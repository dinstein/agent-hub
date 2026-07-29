<div align="center">

# AgentHub

**One configuration, one set of credentials, one governance pipeline — shared by every AI client.**

Claude Code · Cursor · Codex · Open WebUI · and 8 more

[![CI](https://github.com/dinstein/agent-hub/actions/workflows/ci.yml/badge.svg)](https://github.com/dinstein/agent-hub/actions/workflows/ci.yml)
[![Go 1.26+](https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)
[![Version](https://img.shields.io/badge/version-0.3.0-blue.svg)](VERSION)
[![Platforms](https://img.shields.io/badge/platforms-macOS%20%7C%20Linux-lightgrey.svg)](#platforms)
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
> macOS and Linux are verified; Windows is **not yet usable** ([details](#platforms)).

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
| Gateway | stdio (one process per client) + streamable-http (shared daemon pool); three downstream transports: stdio / streamable-http / legacy HTTP+SSE, targeting protocol version `2025-11-25` with backward negotiation and downgrade |
| Discovery | Three modes — `full` / `grouped` / `lazy`; lazy mode ships five meta-tools (`status`, `search_tools`, `describe_tool`, `call_tool`, `fetch_result`) plus intent variants; compact signature grammar + two-stage describe |
| Governance | Three-layer scope resolution chain (global / profile / session; a client entry only selects which profile applies), tri-state per-server tool selector semantics, dangling profiles fail closed to an empty set |
| Security | Injection scanning (normalization + phrase/regex/base64/head-tail dual window), spawn guard (anti-smuggling), bidirectional SSRF predicates with in-DialContext screening, leakguard, integrity fingerprint pinning + drift grading + quarantine, HITL approval state machine (fail-closed throughout) |
| Isolation | **Docker isolation spawner**: `runtime: host\|docker`, no network by default, mounts only explicitly declared directories (read-only by default), resource limits, secrets never enter argv |
| Result shaping | Pagination / budgets / `fetch_result` caching / TOON one-way projection encoding (with two constructive guarantees: never-larger and numeric fidelity) |
| Credentials | Four-level resolution chain (env → explicit bare env → `secrets.enc` → OS keyring), composite vault key `(serverID, scopeName)`, headless OAuth with three callback modes + refresh coordination |
| Clients | Config adaptation for 12 client types (format-driven), two-layer skills management (library/install), skills-over-MCP provisioning |
| Operations | `agenthub doctor` full health check, per-server logs + stderr tail window embedded in errors, four audit streams, X-Request-Id across the whole chain |

## Documentation

| File | Contents |
|---|---|
| [docs/guide.md](docs/guide.md) | **Using it**: the three nouns (server / profile / client), the everyday path, and the decisions you actually have to make |
| [docs/architecture.md](docs/architecture.md) | **Changing it**: process model, core module map, layering and dependency constraints, what a single call passes through, the three data-flow directions, the three lines of defense |
| [docs/flows.md](docs/flows.md) | Sequence diagrams and failure branches for seven key flows |
| [docs/modules/](docs/modules/) | Per-package documentation: responsibilities, key types, invariants and failure directions |
| [docs/canonical.md](docs/canonical.md) | The single source of truth for architectural conventions: frozen identifiers, package layout, dependency constraints, command naming rules, and every decision record |
| [docs/windows.md](docs/windows.md) | Windows status: what is implemented, what is unverified, what is missing |
| [docs/backlog.md](docs/backlog.md) | Confirmed but unfixed gaps: symptom, root cause (pinned to a line), approach, and how to verify |

Chinese translations live in [docs/zh-CN/](docs/zh-CN/).

## Platforms

| Platform | Status |
|---|---|
| macOS | ✅ Supported, exercised by CI |
| Linux | ✅ Supported, exercised by CI |
| Windows | ⚠️ **Does not run**: paths and the named-pipe design are implemented and CI gates on `GOOS=windows go build ./...`, but the registry's cross-process lock and the pipe listener are still stubs — you can neither read configuration nor start the daemon. Nothing has ever run on a real Windows machine. [Details](docs/windows.md) |

## Privacy: no data collection

AgentHub **collects no data**. No telemetry, no crash reporting, no usage statistics, no update
checker — not disabled by default, not available to turn on: the channel simply does not exist.
No request ever targets an AgentHub-owned domain.

The process makes only three kinds of outbound connection, all determined by your configuration:
the downstream MCP servers in `servers.json`, those servers' OAuth authorization servers (only
after you run `agenthub auth login`), and endpoints you specify explicitly (`server add --url`).

Audit streams (`audit.jsonl` / `security.jsonl` / `savings.jsonl`) and per-server logs are
**written to local disk only**. Version updates are left to your package manager.
See the decision record in [canonical.md](docs/canonical.md) §7, item 6.

## Development

Requires Go 1.26+ and golangci-lint v2.

```bash
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
