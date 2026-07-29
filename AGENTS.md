# AgentHub

A local hub for Agent services: one configuration, one set of credentials, one governance pipeline,
shared by every AI client (Claude Code, Cursor, Codex, Open WebUI, and others). Go + Wails3, with a
dual-mode gateway (stdio, one process per client / daemon, hosting the shared HTTP pool and the
coordination plane).

**Current status: feature-complete against its design.** CI (macOS + Linux) is green, and end-to-end
acceptance passes with real Claude Code calling real downstream MCP servers through the gateway. The
work from here is polish, bug fixes, and problems surfaced by real use — not new feature milestones.

## What to read first

| File | When to read it |
|---|---|
| [docs/architecture.md](docs/architecture.md) | You want to understand how the system is carved up, how processes are laid out, and what a single call passes through |
| [docs/flows.md](docs/flows.md) | You want to know how a given flow runs at runtime and which way it falls on failure |
| [docs/modules/](docs/modules/) | Before touching a package, read its invariants and failure directions |
| [docs/modules/oauth.md](docs/modules/oauth.md) | You cannot connect to an OAuth downstream, or you want to know which provider shapes are supported |
| [docs/canonical.md](docs/canonical.md) | You want to confirm whether a name/dependency/convention may change, or why something was decided the way it was |

Gaps that are confirmed to exist and pinned to a line, but not yet fixed, live in
[docs/backlog.md](docs/backlog.md) — look there first when you need work. When you fix one, delete it
from there and update the corresponding `docs/modules/` file to describe the new reality.

## Hard constraints (violations fail CI)

1. `cmd/agenthub-gui` and `api` **must not** import anything under `internal/*` — "the GUI is optional"
   is a compile-time constraint
2. `internal/mcp` **depends on the standard library only** (entirely in-house; do not `go get` any
   third-party MCP library); no other `internal/*` package may import a third-party MCP library —
   there is exactly one protocol facade
3. `internal/pipeline` must not import `internal/ctlapi` — the data plane does not depend on the
   control plane
4. `internal/mcp`, `internal/platform`, `internal/logx`, and `internal/guard/*` are
   zero-business-dependency foundations

Each of these has a failing case in `internal/depguardtest` that proves the rule actually blocks.
A lint rule that is configured but not in effect is more dangerous than no rule at all.

## The easiest things to get wrong when changing code

- **The gate chain order is frozen** (scope → token tier → argument pre-validation → HITL), nailed
  down by tests.
- **There is exactly one execution path**: both direct calls and `call_tool` go through the same
  `pipeline.Execute`, and tests assert the gate counts match exactly. Before adding a "shortcut",
  explain what entitles it to bypass the gates — any new path must carry its own assertion that its
  gate count matches a direct call.
- **`RouteOf` is the only legitimate provenance for an exposed name; splitting on `__` is forbidden**
  (a server id or tool name may itself contain `__`).
- **Security predicates must document their failure direction** (fail-open or fail-closed); netguard's
  bidirectional predicates are the model to follow.
- **Overlays are never persisted to disk**: a runtime relaxation that comes back from the dead is a
  security incident.
- **Audit records never contain args**, only argsHash — the field does not exist at the type level.
- **Isolation a config claims must be delivered or refused**: for fields like `runtime: docker`, it is
  better to fail closed and reject than to silently degrade into host execution (this trap has
  actually happened).

## Testing and verification

```bash
make ci          # build + test + lint
make ci-full     # everything the CI workflow actually runs (use this before pushing)
make gui         # build the GUI separately (excluded from build/lint by default)
```

**`make ci` is not the same as CI.** There are three ways a local green run still goes red after a
push, and `make ci-full` covers all of them:

1. **The depguard proof can "skip" instead of fail.** When golangci-lint is absent,
   `internal/depguardtest` calls `t.Skip` on itself, and `make test` reports that skip as success;
   CI greps the verbose output for `--- SKIP` and fails — **a skipped proof is not a proof**
   (CANONICAL §6).
2. **The entire `gui` job.** `make ci` deliberately leaves it alone ("the GUI is optional" is a
   compile-time property and must not become a prerequisite of the default build), so it is wired in
   explicitly only in `ci-full`.
3. **`make gui` is not that check.** It runs `npm install`, which will helpfully repair a
   `package-lock.json` that disagrees with `package.json`; CI runs `npm ci`, which rejects outright.
   Only `gui-frontend-ci` reproduces this one.

- **Run e2e with the CI environment simulated**: `XDG_RUNTIME_DIR=/tmp/fake-xdg-e2e go test ./test/e2e/`.
  CI's Linux runner sets this variable. It should **no longer** change where the run directory lives —
  `AGENTHUB_DATA_DIR` moves the run directory along with everything else — and that is precisely why
  you run with it set: the e2e suite is the end-to-end regression test for that rule. The class of
  "only happens on CI" problem that once took four rounds to diagnose was rooted right here.
- Preconditions inside tests (the process was killed, the file exists) must **fail hard**, never
  silently `return`: a silent skip disguises an environment difference as some other component failing.
- For hangs, add evidence before changing code: an e2e timeout SIGQUITs the process under test and
  folds the goroutine stacks into the failure message.
- Windows has only the cross-compilation gate `GOOS=windows go build ./...`, with no real-machine
  verification.
- **When touching a parser that reads untrusted input, run a round of fuzzing**:

  ```bash
  go test ./internal/mcp/ -run xxx -fuzz FuzzParseMessage -fuzztime 60s
  ```

  The five targets each guard one path by which external bytes arrive: `FuzzParseMessage` (downstream
  JSON-RPC frames), `FuzzSSEScanner` (remote SSE streams, a hand-written line scanner — the least
  trustworthy of them), `FuzzScanAuthParam` (remote `WWW-Authenticate`, hand-written index-based
  scanning), `FuzzEncodeJSON` (downstream tool results, on the response path), and
  `FuzzScanTOMLServers` in `internal/clients` (another application's config file, hand-written —
  `go test ./internal/clients/ -run xxx -fuzz FuzzScanTOMLServers`).
  `make ci` runs only their seed corpora (fast); `-fuzz` must be enabled explicitly — keep it out of CI.

## Reference implementations: read, never copy

[mcpproxy-go](https://github.com/smart-mcp-proxy/mcpproxy-go) (Go) and
[toolport](https://github.com/tsouth89/toolport) (Rust) are both **reference material only; do not
copy code**. What is inherited from them is the list of problems they hit — which edge cases exist,
what the failures look like, and what the correct behavior is.

## Collaboration conventions

- **Do every feature in its own worktree; never edit code directly in the main repository work tree**:
  `git worktree add ../agent-hub-<topic> -b <topic>`. The main work tree stays clean and is used only
  for landing branches and pushing.
- Inside the worktree, make **one commit per subtask** (every commit must compile and pass tests)
- Write commit messages in English
- **`main` is linear: rebase, never merge.** Several worktrees are normally in flight at once, and a
  merge commit per branch braids the history into something where "what landed, when, and on top of
  what" can no longer be read off `git log`. Land a finished branch like this:

  ```bash
  # in the worktree
  git fetch origin && git rebase origin/main
  XDG_RUNTIME_DIR=/tmp/fake-xdg-e2e make ci-full

  # in the main work tree
  git merge --ff-only <topic> && git push origin main
  git worktree remove ../agent-hub-<topic> && git branch -d <topic>
  ```

- **`make ci-full` runs after the rebase, not before it, and after `go clean -testcache`.** A rebase
  replays your commits onto code you have never tested against, so a green run on the old base says
  nothing about the tree that is about to land. This is the whole cost of the rebase rule, and
  skipping it is how `main` goes red.

  The cache defeats that rule silently, which is why the clean is part of it. `test/e2e` builds the
  binary under test inside `TestMain`, so a change to `cmd/agenthub` on the new base is not part of
  the key Go caches the result under: the suite reports `ok (cached)` for a tree it never ran
  against. A landing run that prints `(cached)` on most packages has verified almost nothing —
  count the lines if you are unsure:

  ```bash
  go clean -testcache
  XDG_RUNTIME_DIR=/tmp/fake-xdg-e2e make ci-full 2>&1 | tee ci.log
  grep -c '(cached)' ci.log        # want 0 on a landing run
  ```
- **`--ff-only` is the enforcement, not a formality.** If it refuses, either the rebase did not happen
  or `origin/main` moved again — rebase again. Never reach for a plain `git merge` to get past it.
- Pull with `git pull --rebase` so an out-of-date `main` does not grow a merge commit of its own.
- Rebase only branches that are yours alone. These topic branches are local and short-lived, which is
  exactly what makes rewriting them safe; once a branch has been pushed and someone may have built on
  it, it is history other people hold, and it is left alone.

## Toolchain

Go 1.26+, golangci-lint v2, make, node v22 / npm 10, and the wails3 CLI (only needed for GUI builds).
