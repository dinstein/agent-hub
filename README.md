# AgentHub

A local hub for Agent services: one configuration, one set of credentials, one governance
pipeline — shared by every AI client (Claude Code, Cursor, Codex, Open WebUI, and others).

*[中文文档](README.zh-CN.md)*

- A single required binary, `agenthub`: `connect` (stdio gateway, one process per client) /
  `daemon` (shared HTTP pool + control plane + coordination plane) / CLI management subcommands
- An optional GUI, `agenthub-gui` (Wails3, consumes the control-plane API only)

**Status**: feature-complete against its design. CI is green across both matrices, and end-to-end
acceptance passes with real Claude Code calling real downstream MCP servers through the gateway.
Platforms: macOS and Linux are verified; Windows is **not yet usable** (see below).

## Capabilities

| Area | What it covers |
|---|---|
| Gateway | stdio (one process per client) + streamable-http (shared daemon pool); three downstream transports: stdio / streamable-http / legacy HTTP+SSE, targeting protocol version `2025-11-25` with backward negotiation and downgrade |
| Discovery | Three modes — `full` / `grouped` / `lazy`; lazy mode ships five meta-tools (`status`, `search_tools`, `describe_tool`, `call_tool`, `fetch_result`) plus intent variants; compact signature grammar + two-stage describe |
| Governance | Five-layer scope resolution chain (global / profile / client / project / session), tri-state per-server tool selector semantics, dangling profiles fail closed to an empty set |
| Security | Injection scanning (normalization + phrase/regex/base64/head-tail dual window), spawn guard (anti-smuggling), bidirectional SSRF predicates with in-DialContext screening, leakguard, integrity fingerprint pinning + drift grading + quarantine, HITL approval state machine (fail-closed throughout) |
| Isolation | **Docker isolation spawner**: `runtime: host\|docker`, no network by default, mounts only explicitly declared directories (read-only by default), resource limits, secrets never enter argv |
| Result shaping | Pagination / budgets / `fetch_result` caching / TOON one-way projection encoding (with two constructive guarantees: never-larger and numeric fidelity) |
| Credentials | Four-level resolution chain (env → explicit bare env → `secrets.enc` → OS keyring), composite vault key `(serverID, scopeName)`, headless OAuth with three callback modes + refresh coordination |
| Clients | Config adaptation for 12 client types (format-driven), two-layer skills management (library/install), skills-over-MCP provisioning |
| Operations | `agenthub doctor` full health check, per-server logs + stderr tail window embedded in errors, four audit streams, X-Request-Id across the whole chain |

## Documentation

| File | Contents |
|---|---|
| [docs/architecture.md](docs/architecture.md) | **Start here**: process model, core module map, layering and dependency constraints, what a single call passes through, the three data-flow directions, the three lines of defense |
| [docs/flows.md](docs/flows.md) | Sequence diagrams and failure branches for seven key flows |
| [docs/modules/](docs/modules/) | Per-package documentation: responsibilities, key types, invariants and failure directions |
| [docs/canonical.md](docs/canonical.md) | The single source of truth for architectural conventions: frozen identifiers, package layout, dependency constraints, command naming rules, and every decision record |
| [docs/windows.md](docs/windows.md) | Windows status: what is implemented, what is unverified, what is missing |
| [docs/backlog.md](docs/backlog.md) | Confirmed but unfixed gaps: symptom, root cause (pinned to a line), approach, and how to verify |

Chinese translations live in [docs/zh-CN/](docs/zh-CN/).

## Development

Requires Go 1.26+ and golangci-lint v2.

```bash
make build   # go build ./...
make test    # go test ./...
make lint    # golangci-lint run
make ci      # build + test + lint
```

### GUI (optional, not built by default)

`make build` and `make lint` **exclude the GUI**: linking a webview requires GTK/WebKit
development packages, which CI runners do not have. All Wails code under `cmd/agenthub-gui`
therefore carries the `//go:build wails` tag, and a default build produces a placeholder main
that explains how to build the GUI (see the decision record in [canonical.md](docs/canonical.md) §7, item 3).

```bash
make gui            # frontend npm install + vite build, then go build -tags wails
make gui-frontend   # frontend only (output at cmd/agenthub-gui/frontend/dist, embedded into the binary)
make gui-go         # Go side only (requires dist to already exist)
```

The frontend is vanilla TS + Vite, with `@wailsio/runtime` as its only runtime dependency. The
Level/AdminState/Action constants of the Health contract are generated from the `api` package into
`frontend/src/generated/health.ts` by `go generate ./cmd/agenthub-gui/...`, and a golden test
watches it to prevent drift across the three ends.
The GUI can only reach the control plane through the `api` package — it has no capability the CLI lacks.

Hard constraints on dependency direction (violations fail CI; see [canonical.md](docs/canonical.md) §2):

1. `cmd/agenthub-gui` and `api` must not import anything under `internal/*`
2. `internal/mcp` depends on the standard library only; no other package may import a third-party MCP library
3. `internal/pipeline` must not import `internal/ctlapi`
4. `internal/mcp`, `internal/platform`, `internal/logx`, and `internal/guard/*` are zero-business-dependency foundations

## Platforms

| Platform | Status |
|---|---|
| macOS | ✅ Supported, exercised by CI |
| Linux | ✅ Supported, exercised by CI |
| Windows | ⚠️ **Does not run**: path resolution is implemented, but the registry lock and named pipe are still stubs |

On Windows, the `%APPDATA%\agenthub` data directory, MSIX package-identity detection, loopback-UNC
twin-path escape handling, the control-plane named pipe path `\\.\pipe\agenthub-ctl-<sha8(SID)>` and
its SDDL are all implemented, with the seams converged into `internal/platform` per the decision
record, and CI gates on `GOOS=windows go build ./...` — but **not a single line has run on a Windows
machine**, and two prerequisites are still missing: the registry's cross-process lock and the
control-plane named pipe listener are both stubs that return unsupported, so on Windows you can
neither read configuration nor start the daemon.
See [docs/windows.md](docs/windows.md) for current status, remaining work, and acceptance criteria.

## Privacy: no data collection

AgentHub **collects no data**. There is no telemetry, no crash reporting, no usage statistics, and
no update checker — not disabled by default, not available to turn on: the channel simply does not exist.

The process makes only three kinds of outbound network connection, all determined by your configuration:

1. The downstream MCP servers you configured in `servers.json`;
2. Those servers' OAuth authorization servers (only after you run `agenthub auth login`);
3. Endpoints you specify explicitly (for example `server add --url`).

No request ever targets an AgentHub-owned domain. The audit streams (`audit.jsonl` /
`security.jsonl` / `savings.jsonl`) and the per-server logs (`logs/server-<name>.log`) are
**written to local disk only** — nobody reads them off your machine. Version updates are left to
your package manager.

See the decision record in [canonical.md](docs/canonical.md) §7, item 6.

## License

MIT © 2026 dinstein (design references are listed in [NOTICE](NOTICE))
