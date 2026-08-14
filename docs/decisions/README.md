# Decisions

> **Answers** what was settled, and what would have to change for it to be reopened.
> **Not here** the rules themselves → [../conventions.md](../conventions.md).
> **Kept true by** `TestHistoricalRulingIdsResolve`, which refuses a ruling id this directory does not register.

One decision per file, named by a stable number that never moves. A decision is **append-only**: a
superseded one keeps its file and says so in its status line, because a record that can be deleted is one
a reader cannot trust to be complete.

| # | Decided | Status |
|---|---|---|
| [0001](0001-eager-connect.md) | eager connect, not lazy | active |
| [0002](0002-shaping-cache-is-plain-files.md) | the shaping cache is plain files | active |
| [0003](0003-wails3-and-the-frontend-stack.md) | Wails3 alpha, vanilla TS, three tagged files | active |
| [0004](0004-toon-and-signature-grammars.md) | TOON is one-way; both grammars frozen | active |
| [0005](0005-dev-mode-falls-back-to-the-encrypted-file.md) | dev mode falls back to `secrets.enc` | active |
| [0006](0006-no-telemetry-and-no-update-checker.md) | no telemetry, no update checker | active |
| [0007](0007-tool-commands-live-under-server.md) | tool commands live under `server` | active |
| [0008](0008-rules-are-read-where-they-are-stored.md) | a rule is read where it is stored | active |
| [0009](0009-savings-ledger-removed.md) | the savings ledger was removed | active |
| [0010](0010-a-shell-installer-and-nothing-in-the-binary.md) | a shell installer, nothing in the binary | active |
| [0011](0011-a-call-that-cannot-be-recorded-still-runs.md) | a call that cannot be recorded still runs | active |

## Historical ruling ids

Around sixty comments cite a ruling by a number from the original design document — `ruling #8`,
`A.1 #8`, `A.6 #5`. **That document is not in this repository**, so without this table those citations
resolve to nothing. The ids are kept because they are *stable* while the rules are *live*.

**`A.6 #N` is decision `000N`.** The appendix's open questions are the decision files above, in the same
order; prefer the file name in new comments.

Two conventions, so this table cannot quietly rot: the bare `#N` and the `A.x #N` spellings of one ruling
are one row, and **a number not listed here may not be cited**. Milestone task numbers are not rulings and
are not citable at all.

| Cited as | What it ruled | Where the rule lives now |
|---|---|---|
| `#7`, `A.1 #7` | two id shapes on purpose: the human `client:seq` for the CLI, a random token for the protocol | [subsystems/scope.md](../subsystems/scope.md) |
| `#18` | lazy mode's `call_tool` may split into read/write/destructive intent variants | [model.md](../model.md#how-the-surface-is-presented), [subsystems/exposure.md](../subsystems/exposure.md) |
| `#27` | determinism is the contract: goldens pin the wire shape; fix the code, never the golden | [conventions.md#engineering-conventions](../conventions.md#engineering-conventions) |
| `#29` | legacy HTTP+SSE is a read-side transport only, never offered on the exposure side | [conventions.md#mcp-protocol-scope](../conventions.md#mcp-protocol-scope) |
| `#32` | `internal/mcp` is standard-library only — one first-party protocol facade | [conventions.md#dependency-directions](../conventions.md#dependency-directions) |
| `A.2 #9` | the manual paste loop, for providers that cannot reach a loopback redirect | [status/oauth.md](../status/oauth.md) |
| `A.2 #10` | refresh is serialized: daemon singleflight online, a file lock offline | [status/oauth.md](../status/oauth.md) |
| `A.3 #1` | cross-process shared state takes a file lock or an atomic rename, proven by an N-process test | [subsystems/registry.md](../subsystems/registry.md), [subsystems/credentials.md](../subsystems/credentials.md) |
| `A.3 #2` | `kill -9` on the daemon: the stdio data plane is untouched and gateways re-register | [flows.md](../flows.md) |
| `A.3 #4` | a daemon restart makes the session overlay vanish on both sides — retired by its own logic: a session carries no scope of its own at all | [subsystems/scope.md](../subsystems/scope.md) |
| `A.3 #5` | skills materialization is client-granular, never per-session | [conventions.md#capability-boundaries](../conventions.md#capability-boundaries) |
| `A.5 #23` | Windows is confined to a seam inside `internal/platform` | [status/windows.md](../status/windows.md) |
| `A.5 #26` | the composite vault key `(serverID, scopeName)` from day one | [conventions.md#three-things-that-must-never-be-retrofitted](../conventions.md#three-things-that-must-never-be-retrofitted) |
| `A.5 #30` | the roots migration seam (`RootSource`), in place before upstream deprecation | [conventions.md#upstream-deprecation-tracking](../conventions.md#upstream-deprecation-tracking) |
