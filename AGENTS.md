# AgentHub

A local hub for Agent services: one configuration, one set of credentials, one aggregation point,
shared by every AI client (Claude Code, Cursor, Codex, Open WebUI, and others). Go + Wails3, with a
dual-mode gateway (stdio, one process per client / daemon, hosting the shared HTTP pool and the
coordination plane).

**What a client may reach is decided in advance, by configuration, and never at call time.** A server
is on or off; a server offers all of its tools or a named subset; a profile takes a subset of the
servers and may narrow their tools further; a client follows a profile. Every layer intersects and
none can widen. There is no approval queue, no runtime scope change, no scanning of what a downstream
returned — an earlier design had all three and they were removed rather than left half-wired, because
a governance surface that does not decide anything still reads as protection.

What remains outside that model is not permission: agent tokens grade the HTTP face, rate limits keep
one runaway loop from burning a budget, and netguard / spawnguard refuse destinations and processes
regardless of who asked.

## What to read first

| File | When to read it |
|---|---|
| [docs/architecture.md](docs/architecture.md) | How the system is carved up, and what a single call passes through |
| [docs/flows.md](docs/flows.md) | How a flow runs at runtime, and which way it falls on failure |
| [docs/subsystems/](docs/subsystems/) | Before touching a package — its invariants and failure directions |
| [docs/status/oauth.md](docs/status/oauth.md) | An OAuth downstream will not connect, or which provider shapes are supported |
| [docs/model.md](docs/model.md) | What a client is allowed to reach, and who decided it |
| [docs/conventions.md](docs/conventions.md) | Whether a name, a dependency direction or a convention may change |
| [docs/decisions/](docs/decisions/) | Why a settled question was settled that way |
| [.agents/skills/](.agents/skills/) | You are about to **do** one of the standard workflows, and want its steps in order |

`docs/` explains how the system works; skills are what you execute. Four repository workflows exist
today — [new-feature](.agents/skills/new-feature/SKILL.md) (build anything, and land it),
[nightly-tidy](.agents/skills/nightly-tidy/SKILL.md) (the recurring simplify / refactor / docs-agree
pass), [release](.agents/skills/release/SKILL.md) (cut a release), and
[security-audit](.agents/skills/security-audit/SKILL.md) (the recurring security sweep — a workflow
of parallel finders, adversarial verifiers and one adjudication pass). `.agents/skills/` is the
single source; `.claude/skills` links to the entire directory so Codex and Claude execute the same
files.

Confirmed gaps, pinned to a line but not yet fixed, live in the `docs/subsystems/` file of the package
that owns them — under "current assembly status", or beside the invariant they bend — and, for one
platform's overall state, in [docs/status/windows.md](docs/status/windows.md). There is no separate backlog file:
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

- **The gate chain order is frozen**: scope → token tier. Both decide from configuration alone, and
  both fail closed. Nothing in the chain may inspect or rewrite what a call carries.
- **There is exactly one execution path**: direct calls and `call_tool` both go through
  `pipeline.Execute`. Any new path must assert its gate count matches a direct call.
- **`RouteOf` is the only legitimate provenance for an exposed name; splitting on `__` is forbidden**
  — a server id or tool name may itself contain `__`.
- **Security predicates must document their failure direction** (fail-open or fail-closed); netguard's
  bidirectional predicates are the model.
- **A tool selector is an allow list, never a deny list.** The two answer the arrival of a tool the
  downstream adds tomorrow in opposite directions, and one file must not give two answers.
- **`nil` and `[]` are different everywhere a selector appears**: nil means "no rule", `[]` means
  "nothing". `omitzero`, not `omitempty` — dropping an empty list turns block-all into allow-all.
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
make docs-gen    # regenerate the generated blocks in docs/ (make ci checks them)
make ci          # build + test + lint + generated docs
make ci-full     # everything the CI workflow runs, plus the CI-shaped e2e run
make ci-landing  # ci-full with the caches dropped (after the rebase, before landing)
make gui         # build the GUI separately (excluded from build/lint by default)
```

**`make ci` is not CI.** Three ways a local green run still goes red — all covered by `make ci-full`,
and all covered by the branch's own PR checks, which is why the skill spends the local run on the
landing rather than on every push:

1. **A skipped depguard proof is not a proof** (docs/conventions.md#engineering-conventions). Without golangci-lint,
   `internal/depguardtest` calls `t.Skip` and `make test` counts it as success; CI greps for
   `--- SKIP` and fails. `make ci-depguard-proof` is the local equivalent, and it greps a log it
   piped through `tee`; `ci-landing` does the same over the whole of `ci-full`. Those two are the
   repository's only `tee`d recipes, and each arms `set -o pipefail` **itself** because GNU Make
   3.81, the `/usr/bin/make` on macOS, ignores `.SHELLFLAGS` silently. `ci-full` needs neither: it
   is a list of prerequisites with no recipe body of its own.
2. **The `gui` job**, deliberately left out of `make ci` so "the GUI is optional" does not become a
   prerequisite of the default build.
3. **`make gui` is not that check.** It runs `npm install`, which repairs a `package-lock.json` that
   disagrees with `package.json`; CI runs `npm ci`, which rejects. Only `gui-frontend-ci` reproduces it.

- **e2e runs both ways, and `make ci-full` now runs both for you**: `make test` reaches the suite with
  this machine's environment, and `e2e-ci` (a prerequisite of `ci-full`) reaches it with
  `XDG_RUNTIME_DIR` set, as CI's Linux runner has it. That variable must **not** move the run
  directory — only `AGENTHUB_DATA_DIR` does, along with everything else — and this suite is the
  regression test for it. `make e2e` and `make e2e-ci` run one shape alone; `e2e` clears
  `XDG_RUNTIME_DIR` rather than inheriting it, so on a Linux box — where `make test` already produces
  the CI shape — it is the one that covers the other.
- Preconditions inside tests (the process was killed, the file exists) must **fail hard**, never
  silently `return`: a silent skip disguises an environment difference as some other component failing.
- For hangs, add evidence before changing code: an e2e timeout SIGQUITs the process under test and
  folds the goroutine stacks into the failure message.
- Windows has only the cross-compilation gates `make cross-windows` (build + vet, minus the Unix-only
  e2e and installer suites) and `make cross-windows-gui`, with no real-machine verification.
- **When touching a parser that reads untrusted input, run a round of fuzzing**:

  ```bash
  make fuzz FUZZ=FuzzParseMessage   # one target; FUZZTIME=60s by default
  make fuzz                          # all eight, back to back
  ```

  Eight targets, one per path by which external bytes arrive: `FuzzParseMessage` (downstream JSON-RPC
  frames), `FuzzSSEScanner` (remote SSE streams, a hand-written line scanner — the least trustworthy),
  `FuzzScanAuthParam` (remote `WWW-Authenticate`, hand-written index scanning), `FuzzEncodeJSON`
  (downstream tool results), `FuzzScanTOMLServers` (another application's config file, hand-written),
  `FuzzBlankJSONC` and `FuzzSpliceEntryKeepsEverythingElse` (the JSONC comment-blanking pass, and the
  splice that edits a settings.json in place without re-encoding it), and `FuzzDecodeHeaderValue`
  (the `Mcp-Name` base64 sentinel a caller's header carries, decoded before validation compares it
  to the body). They do not all live in `internal/mcp`; `FUZZ_TARGETS` carries each one's package,
  so the target name alone runs it.
  `make ci` runs only the seed corpora — keep `-fuzz` out of CI.

  **Adding a ninth means editing three places** — the target, its `FUZZ_TARGETS` entry, and the list
  above — and `test/buildrules` fails until all three agree. The omission is otherwise invisible:
  `make ci` still runs the seed corpus, so it looks covered while `make fuzz` never reaches it.

## Reference implementations: read, never copy

[mcpproxy-go](https://github.com/smart-mcp-proxy/mcpproxy-go) (Go) and
[toolport](https://github.com/tsouth89/toolport) (Rust) are **reference material only; do not copy
code**. What is inherited is the list of problems they hit — which edge cases exist, what the
failures look like, and what the correct behavior is.

## Collaboration conventions

These are the rules, and only the rules. The commands live in the
[new-feature skill](.agents/skills/new-feature/SKILL.md), in one copy — an order of operations
reconstructed from the reasons below is how a step ends up in the wrong place.

### Commit messages

Use one English, Conventional-Commits-shaped format:

```text
<type>[(<scope>)][!]: <summary>
```

- Type is one of `feat`, `fix`, `refactor`, `perf`, `test`, `docs`, `build`, `ci`, `chore`,
  `release`, or `revert`.
- Scope is optional and names a stable owner such as `cli`, `gateway`, `gui`, or `buildrules`;
  `gui` is a scope, not a type.
- Summary is an imperative sentence fragment, normally lowercase, with no terminal punctuation;
  keep the complete title within 80 characters.
- After one blank line, body uses the compact narrative style established by the repository's
  earlier commits: state the problem and why it matters, describe the concrete change, then note
  verification or an important limitation when useful. Prefer 2–4 short paragraphs and roughly
  80–150 words total; omit details obvious from the diff, and omit body only when the title already
  says everything. Put attribution and issue trailers last. Mark a breaking change with `!` and a
  `BREAKING CHANGE:` trailer.

Examples:

```text
docs(windows): document both Windows verification gates

The manual runbook named only cross-windows, so following it skipped the
GUI path where Wails diverges most from the macOS build.

On a fresh checkout, that GUI target also needs the gitignored frontend
bundle. Running it alone therefore fails before compiling any Windows code,
while ci-full hides the prerequisite by building the frontend first.

The runbook now includes cross-windows-gui and names the frontend build
prerequisite. Both targets were verified from a clean worktree.

docs: define the contributor commit convention
release: 0.18.0
```

Run `git config commit.template .gitmessage` once per clone to show the same rules in the editor;
the template is a prompt, while CI is the authority.

### The branch flow

Eight rules, each with what it buys. The **sequence** that carries them out — every command, in the
order it is run, including the several that are easy to type plausibly and wrongly — is the
[new-feature skill](.agents/skills/new-feature/SKILL.md). Follow that file; a change that compiles
nothing takes the short lane at the top of it.

| Rule | Why it is one |
|---|---|
| A worktree per feature, branched from `origin/main`; **never edit code in the main work tree** | Several branches are normally in flight, and one must be able to land while another is mid-edit |
| One commit per subtask, each compiling and passing tests on its own | Nothing is squashed on the way in, so a branch's commits are exactly what `main` gets, and a bisect lands on one of them |
| A PR per branch, draft from the first commit, body = the subtask list kept ticked | The only view of the branch from outside your worktree, and where CI's own machines grade it (`pull_request` runs the jobs `main` gets) |
| The PR's checks are the full run; a local `make ci-full` is for when you expect to be the one who broke it | Two runners, two operating systems, and neither of them is this laptop |
| `make ci-landing` after a rebase that moved the branch, and only then | Its reason is commits replayed onto a base they were never tested against; a rebase that changes no sha produces none |
| The landing closes the PR, not a merge button: force-push after the final rebase so its head is exactly what lands | `main` then holds that commit and GitHub marks it merged; if they differ, an open PR claims work still in flight |
| `main` is linear — rebase, never merge, and pull with `git pull --rebase` | A merge commit per branch makes "what landed, when, on top of what" unreadable from `git log` |
| `--ff-only` is the enforcement, not a formality | If it refuses, the rebase did not happen or `origin/main` moved. Never reach for a plain `git merge` to get past it |

Rebase only branches that are yours alone: once pushed, a branch is history other people hold.

## Toolchain

Go 1.26+, golangci-lint v2, make, node v22 / npm 10, the `gh` CLI (authenticated — the branch flow
opens and updates PRs through it), and the wails3 CLI (only needed for GUI builds).

**The two version floors above are floors, not a matrix, and the pair CI runs is `go1.26.x` +
`golangci-lint v2.12.2`** (`.github/workflows/ci.yml`; `internal/depguardtest` names the same lint
version in its skip message). Newer is not automatically compatible, and each way it fails is silent
in a different place:

- **golangci-lint v2.12.2 cannot analyse a Go 1.27 toolchain at all.** It is built with go1.26 and
  panics with `file requires newer Go version go1.27` inside package loading, which
  `internal/depguardtest` reports as "lint failed but not via depguard" — a depguard message for a
  problem that has nothing to do with depguard.
- **A newer golangci-lint reports findings CI does not have.** v2.13.1 raises `SA4023` on
  `internal/platform/windows.go`, correctly: `currentUserSID` always returns an error on the
  non-Windows build, so that `if err != nil` is always true there. `make ci` is then red locally and
  green on both runners.
- **`make fmt` under Go 1.27 writes formatting CI rejects.** 1.27's gofmt aligns a map literal whose
  longest key stands alone where 1.26's does not, so the documented "run `make fmt` after touching an
  import block" step introduces a `gofmt` lint failure. It is not visible locally, because the same
  new gofmt then calls the file formatted.

Reproducing the runners on a machine that has moved ahead needs both halves pinned, and nothing in
the repository has to change to do it:

```bash
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
  | sh -s -- -b "$(pwd)/bin" v2.12.2          # bin/ is gitignored, and depguardtest probes it
export GOTOOLCHAIN=go1.26.0
export AGENTHUB_GOLANGCI_LINT="$(pwd)/bin/golangci-lint"
make ci GOLANGCI_LINT="$(pwd)/bin/golangci-lint"
```

Pinning only the linter is worse than pinning neither: v2.12.2 under a 1.27 toolchain is the panic
above, so `GOTOOLCHAIN` is not the optional half.

The `codex` CLI is optional, and only the
[security-audit skill](.agents/skills/security-audit/SKILL.md) looks for it: present, it reviews the
same shards as a second independent engine; absent, that sweep runs single-engine and says so. Not a
build dependency.
