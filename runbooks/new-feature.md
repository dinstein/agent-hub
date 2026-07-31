# Developing a new feature

A runbook: every step is a command to run or a fact to check.

The shape is fixed — **one worktree, one commit per subtask, rebase onto `main`, land fast-forward
only**. The rules behind it are in [AGENTS.md](../AGENTS.md); this file is the sequence.

Nothing here is reversible-in-public until step 5's `git push origin main`. Everything before it is
local, which is why the expensive checks sit immediately in front of it.

---

## 0. Read before writing

In this order, stopping as soon as the question is answered:

| Question | Read |
|---|---|
| What am I changing, and what does a call pass through on the way? | [docs/architecture.md](../docs/architecture.md) |
| How does this flow behave at runtime, and which way does it fail? | [docs/flows.md](../docs/flows.md) |
| What must not be touched in the package I am about to open? | [docs/modules/](../docs/modules/) — the file for that package |
| Is this name / dependency / convention allowed to move? | [docs/canonical.md](../docs/canonical.md) |

**The `modules/` file is not optional reading.** It carries the invariants and the gaps recorded
beside the code that owns them, and it is where "capability exists ≠ wired up" is written down. A
change that looks correct against the source and wrong against that file is wrong.

If the feature touches a rule in AGENTS.md's *hard constraints* or *easiest things to get wrong* —
the four dependency directions, the gate chain order, `RouteOf`, allow-list-not-deny-list, `nil` vs
`[]` — decide **before** writing code whether the rule bends or the design does. It is almost always
the design.

## 1. Open a worktree

```bash
git fetch origin
git worktree add ../agent-hub-<topic> -b <topic> origin/main
cd ../agent-hub-<topic>
```

**Never edit code directly in the main work tree.** It only lands and pushes. Several branches are
normally in flight; the main tree is the one place they all have to agree, and a dirty file there is
indistinguishable from work someone else was mid-way through.

Branching from `origin/main` rather than from whatever the main tree happens to be sitting on is what
keeps step 5's rebase a no-op in the common case.

## 2. Cut it into subtasks

Write the list down before writing code. Each entry must be a change that **compiles and passes tests
on its own** — that is the definition of a subtask here, not a size estimate. A subtask that cannot
stand alone is two subtasks with a shared prefix, or one that was cut in the wrong place.

A useful cut for anything non-trivial: the type or interface first, then the implementation, then the
assembly that calls it, then the docs. Each of those compiles green with the next one absent.

## 3. The inner loop, once per subtask

```bash
# write the code, then:
make fmt                                # after ANY import block changes — see below
make ci                                 # build + test + lint
git add -A && git commit                # message in English
```

**`make fmt` is not cosmetic and `go build` will not catch it.** `.golangci.yml` enables
`gofmt`/`goimports` under `formatters:`, not `linters:`, so only `make lint` reports them, and the
error names the `import (` line rather than the offending import. Imports are alphabetical within a
group (`"cmp"` before `"context"`, `"maps"` before `"os"`).

Extra work certain kinds of change pull in, none of which `make ci` will ask for:

| If the subtask touched | Also do |
|---|---|
| A parser reading untrusted input | `make fuzz FUZZ=<target>` — `make ci` runs only the seed corpora |
| A **new** fuzz target | Edit all three places (the target, `FUZZ_TARGETS`, AGENTS.md's list); `test/buildrules` fails until they agree |
| A package's invariants or failure directions | The matching [docs/modules/](../docs/modules/) file, in the same commit |
| A package name, command name, dependency direction, or frozen identifier | [docs/canonical.md](../docs/canonical.md), in the same commit |
| A new `.md` under `docs/` | Either a `docs/zh-CN/` counterpart, or an entry in `contributorOnlyDocs` with the reason |
| The GUI frontend | `make gui-frontend-ci` — `make gui` runs `npm install`, which repairs a lockfile CI's `npm ci` rejects |
| Anything user-visible | The README badge is **not** derived; the release runbook bumps it, but new flags and commands are yours |

**Commit while nothing else is writing.** If a background agent is working in this tree, a clean
`git status` means "nothing written *yet*", not "finished". Confirm the work is done before staging,
and re-verify in a clean checkout afterwards.

## 4. Before you push: the full run

```bash
make ci-full                            # everything the CI workflow runs
```

**`make ci` is not CI.** Three ways a green `make ci` still goes red on the runner, all covered here:

| Gap | Why `make ci` misses it |
|---|---|
| A skipped depguard proof | Without golangci-lint, `internal/depguardtest` calls `t.Skip` and `make test` counts it as success; CI greps for `--- SKIP` and fails |
| The `gui` job | Deliberately outside `make ci`, so "the GUI is optional" does not become a prerequisite of the default build |
| A stale `package-lock.json` | `make gui` runs `npm install` and repairs it; CI runs `npm ci` and rejects it |

Then the end-to-end suite **both ways**, because one of them is the regression test for the other:

```bash
make e2e-ci                             # with XDG_RUNTIME_DIR set, as CI's Linux runner has it
make e2e                                # this machine's environment
```

`XDG_RUNTIME_DIR` must **not** move the run directory — only `AGENTHUB_DATA_DIR` does, along with
everything else — and this suite exists to catch the day that stops being true.

```bash
git push -u origin <topic>
```

## 5. Land it

**The rebase comes first, and `make ci-landing` comes after it.** A rebase replays your commits onto
code you never tested against; a green run from before it is a claim about a tree that no longer
exists.

```bash
# in the worktree
git fetch origin && git rebase origin/main
make ci-landing
```

`ci-landing` drops the test cache and then fails on any `(cached)` in its own log — `test/e2e` builds
its binary inside `TestMain`, which the Go cache key does not cover, so the suite would otherwise
report `ok (cached)` for a tree it never ran.

**Read its exit status, not its last screenful.** Piping through `tee` reports `tee`'s status:

```bash
make ci-landing 2>&1 | tee /tmp/landing.log; echo "${PIPESTATUS[0]}"   # NOT $?
```

Then, in the main work tree:

```bash
git merge --ff-only <topic> && git push origin main
git worktree remove ../agent-hub-<topic> && git branch -d <topic>
```

**`--ff-only` is the enforcement, not a formality.** If it refuses, either the rebase did not happen
or `origin/main` moved while you were checking. Rebase again and re-run `ci-landing`. Never reach for
a plain `git merge` to get past it — `main` is linear, and a merge commit per branch makes "what
landed, when, on top of what" unreadable from `git log`.

**Land promptly.** A finished branch left sitting is a branch that will need the whole of step 5
again, against a `main` that has moved further each day.

## When it goes wrong

| Symptom | Meaning | Do |
|---|---|---|
| A depguard failure naming a path in *another* worktree | The lint cache is shared across checkouts, so the result came from a tree you are not in | `echo $GOLANGCI_LINT_CACHE` — the Makefile defaults it per checkout, an exported value overrides that. `make clean-cache` |
| `make ci` green locally, CI red | Almost always the environment, not the code | Re-run `make ci-full`, then `make e2e-ci`; compare the runner's env before reading the diff |
| A test hangs | — | Get evidence before changing code: an e2e timeout SIGQUITs the process under test and folds the goroutine stacks into the failure message |
| A test "passes" by returning early | A precondition silently failed | Preconditions must **fail hard**. A silent skip disguises an environment difference as some other component failing |
| `git merge --ff-only` refuses | The rebase did not happen, or `origin/main` moved | Rebase, re-run `ci-landing`, retry. Never plain-`merge` |
| The branch is shared with someone else | Rebasing it rewrites history other people hold | Do not rebase it. Rebase only branches that are yours alone |

## Rebasing your own branch mid-flight

`main` moving under you is normal; several worktrees are usually open.

```bash
git fetch origin && git rebase origin/main
```

Pull with `git pull --rebase` so an out-of-date `main` does not grow a merge commit. Everything
already pushed under `<topic>` is yours alone, so a force-push after the rebase is fine:
`git push --force-with-lease`.
