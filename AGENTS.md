# AgentHub

A local hub for Agent services: one configuration, one set of credentials, one governance pipeline,
shared by every AI client (Claude Code, Cursor, Codex, Open WebUI, and others). Go + Wails3, with a
dual-mode gateway (stdio, one process per client / daemon, hosting the shared HTTP pool and the
coordination plane).

**Feature-complete against its design** (as of 2026-07-30, with CI green on macOS + Linux and
end-to-end acceptance passing against real downstream MCP servers). The work from here is polish, bug
fixes, and problems surfaced by real use — not new feature milestones. A claim like this one decays
without saying so, hence the date: what is actually reached at runtime today is
[docs/architecture.md §12](docs/architecture.md#12-assembly-status-implemented-but-not-yet-wired-up)
and the test-watched inventory it points at.

## What to read first

| File | When to read it |
|---|---|
| [docs/architecture.md](docs/architecture.md) | How the system is carved up, and what a single call passes through |
| [docs/flows.md](docs/flows.md) | How a flow runs at runtime, and which way it falls on failure |
| [docs/modules/](docs/modules/) | Before touching a package — its invariants and failure directions |
| [docs/modules/oauth.md](docs/modules/oauth.md) | An OAuth downstream will not connect, or which provider shapes are supported |
| [docs/canonical.md](docs/canonical.md) | Whether a name/dependency/convention may change, and why it was decided |

Confirmed gaps, pinned to a line but not yet fixed, live in the `docs/modules/` file of the package
that owns them — under "current assembly status", or beside the invariant they bend — and, for one
platform's overall state, in [docs/windows.md](docs/windows.md). There is no separate backlog file:
a gap recorded next to the code it is about gets read by whoever touches that code, which is more
than a central list can claim.

## Hard constraints (violations fail CI)

1. `cmd/agenthub-gui` and `api` **must not** import anything under `internal/*` — "the GUI is
   optional" is a compile-time constraint
2. `internal/mcp` **depends on the standard library only**, and no other `internal/*` package may
   import a third-party MCP library — there is exactly one protocol facade, and it is in-house
3. `internal/pipeline` must not import `internal/ctlapi` — the data plane does not depend on the
   control plane
4. `internal/mcp`, `internal/platform`, `internal/logx`, and `internal/guard/*` are
   zero-business-dependency foundations

Each has a failing case in `internal/depguardtest` proving the rule actually blocks. A lint rule
configured but not in effect is worse than no rule.

## The easiest things to get wrong when changing code

- **The gate chain order is frozen**: scope → token tier → argument pre-validation → HITL.
- **There is exactly one execution path**: direct calls and `call_tool` both go through
  `pipeline.Execute`. Any new path must assert its gate count matches a direct call.
- **`RouteOf` is the only legitimate provenance for an exposed name; splitting on `__` is forbidden**
  — a server id or tool name may itself contain `__`.
- **Security predicates must document their failure direction** (fail-open or fail-closed); netguard's
  bidirectional predicates are the model.
- **Overlays are never persisted to disk**: a runtime relaxation that comes back from the dead is a
  security incident.
- **Audit records never contain args**, only argsHash — the field does not exist at the type level.
- **Isolation a config claims must be delivered or refused**: for fields like `runtime: docker`, fail
  closed rather than silently degrade to host execution.
- **Run `make fmt` after touching an import block.** `.golangci.yml` enables `gofmt`/`goimports` under
  `formatters:`, not `linters:`, so only `make lint` catches it — `go build` and `go vet` stay silent,
  and the error names the `import (` line rather than the offending import. Order is alphabetical
  within a group (`"cmp"` before `"context"`, `"maps"` before `"os"`).

## Testing and verification

```bash
make             # the target list, one line each
make fmt         # apply gofmt + goimports (they are CI failures, see above)
make ci          # build + test + lint
make ci-full     # everything the CI workflow runs (before pushing)
make ci-landing  # ci-full with caches dropped and CI's environment (before landing)
make gui         # build the GUI separately (excluded from build/lint by default)
```

**`make ci` is not CI.** Three ways a local green run still goes red, all covered by `make ci-full`:

1. **A skipped depguard proof is not a proof** (CANONICAL §6). Without golangci-lint,
   `internal/depguardtest` calls `t.Skip` and `make test` counts it as success; CI greps for
   `--- SKIP` and fails. `ci-full` and `ci-landing` grade themselves through `tee`, so both arm
   `set -o pipefail` in the recipe — GNU Make 3.81, the `/usr/bin/make` on macOS, ignores
   `.SHELLFLAGS` silently.
2. **The `gui` job**, deliberately left out of `make ci` so "the GUI is optional" does not become a
   prerequisite of the default build.
3. **`make gui` is not that check.** It runs `npm install`, which repairs a `package-lock.json` that
   disagrees with `package.json`; CI runs `npm ci`, which rejects. Only `gui-frontend-ci` reproduces it.

- **Run e2e both ways**: `make e2e-ci` (sets `XDG_RUNTIME_DIR`, as CI's Linux runner does) and
  `make e2e`. Both must pass. That variable must **not** move the run directory — only
  `AGENTHUB_DATA_DIR` does, along with everything else — and this suite is the regression test for it.
- Preconditions inside tests (the process was killed, the file exists) must **fail hard**, never
  silently `return`: a silent skip disguises an environment difference as some other component failing.
- For hangs, add evidence before changing code: an e2e timeout SIGQUITs the process under test and
  folds the goroutine stacks into the failure message.
- Windows has only the cross-compilation gates `make cross-windows` (build + vet, minus the Unix-only
  e2e suite) and `make cross-windows-gui`, with no real-machine verification.
- **When touching a parser that reads untrusted input, run a round of fuzzing**:

  ```bash
  make fuzz FUZZ=FuzzParseMessage   # one target; FUZZTIME=60s by default
  make fuzz                          # all seven, back to back
  ```

  Seven targets, one per path by which external bytes arrive: `FuzzParseMessage` (downstream JSON-RPC
  frames), `FuzzSSEScanner` (remote SSE streams, a hand-written line scanner — the least trustworthy),
  `FuzzScanAuthParam` (remote `WWW-Authenticate`, hand-written index scanning), `FuzzEncodeJSON`
  (downstream tool results), `FuzzScanTOMLServers` (another application's config file, hand-written),
  `FuzzBlankJSONC` and `FuzzSpliceEntryKeepsEverythingElse` (the JSONC comment-blanking pass, and the
  splice that edits a settings.json in place without re-encoding it). They do not all live in
  `internal/mcp`; `FUZZ_TARGETS` carries each one's package, so the target name alone runs it.
  `make ci` runs only the seed corpora — keep `-fuzz` out of CI.

  **Adding an eighth means editing three places** — the target, its `FUZZ_TARGETS` entry, and the list
  above — and `test/buildrules` fails until all three agree. The omission is otherwise invisible:
  `make ci` still runs the seed corpus, so it looks covered while `make fuzz` never reaches it.

## Reference implementations: read, never copy

[mcpproxy-go](https://github.com/smart-mcp-proxy/mcpproxy-go) (Go) and
[toolport](https://github.com/tsouth89/toolport) (Rust) are **reference material only; do not copy
code**. What is inherited is the list of problems they hit — which edge cases exist, what the
failures look like, and what the correct behavior is.

## Collaboration conventions

- **Do every feature in its own worktree; never edit code directly in the main work tree**:
  `git worktree add ../agent-hub-<topic> -b <topic>`. The main work tree only lands and pushes.
- Inside the worktree, **one commit per subtask** — every commit compiles and passes tests.
- Write commit messages in English.
- **`main` is linear: rebase, never merge.** Several worktrees are normally in flight, and a merge
  commit per branch makes "what landed, when, on top of what" unreadable from `git log`. Land like this:

  ```bash
  # in the worktree
  git fetch origin && git rebase origin/main
  make ci-landing

  # in the main work tree
  git merge --ff-only <topic> && git push origin main
  git worktree remove ../agent-hub-<topic> && git branch -d <topic>
  ```

- **`make ci-landing` runs after the rebase, not before.** A rebase replays your commits onto code you
  never tested against. It also drops the test cache and then fails on any `(cached)` in its own log:
  `test/e2e` builds its binary inside `TestMain`, which the Go cache key does not cover, so the suite
  will otherwise report `ok (cached)` for a tree it never ran.
- **`--ff-only` is the enforcement, not a formality.** If it refuses, the rebase did not happen or
  `origin/main` moved — rebase again. Never reach for a plain `git merge` to get past it.
- Pull with `git pull --rebase` so an out-of-date `main` does not grow a merge commit.
- Rebase only branches that are yours alone. Once pushed, a branch is history other people hold.

## Toolchain

Go 1.26+, golangci-lint v2, make, node v22 / npm 10, and the wails3 CLI (only needed for GUI builds).
