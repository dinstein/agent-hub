---
name: new-feature
description: Build and land a feature in AgentHub using its required worktree, commit, pull-request, verification, rebase, and fast-forward workflow. Use when implementing any repository change intended to reach main.
---

# Developing a new feature

**One worktree, one PR open for the whole of it, one commit per subtask, rebase onto `main`, land
fast-forward only.** The rules are in [AGENTS.md](../../../AGENTS.md); this file is the sequence.

The branch and PR go public at step 3 and both stay undoable. The one irreversible step is step 5's
`git push origin main`, which is why the expensive checks sit immediately in front of it.

---

## 0. Read before writing

Stop as soon as the question is answered:

| Question | Read |
|---|---|
| What am I changing, and what does a call pass through on the way? | [docs/architecture.md](../../../docs/architecture.md) |
| How does this flow behave at runtime, and which way does it fail? | [docs/flows.md](../../../docs/flows.md) |
| What must not be touched in the package I am about to open? | [docs/modules/](../../../docs/modules/) — that package's file |
| Is this name / dependency / convention allowed to move? | [docs/canonical.md](../../../docs/canonical.md) |

**The `modules/` file is not optional reading** — it carries the invariants and the recorded
"capability exists ≠ wired up" gaps. A change that looks correct against the source and wrong against
that file is wrong.

If the feature touches a rule in AGENTS.md's *hard constraints* or *easiest things to get wrong*,
decide **before** writing code whether the rule bends or the design does. It is almost always the
design.

## 1. Open a worktree

```bash
git fetch origin
git worktree add ../agent-hub-<topic> -b <topic> origin/main
cd ../agent-hub-<topic>
```

**Never edit code in the main work tree** — it only lands and pushes. Branching from `origin/main`
rather than from whatever the main tree sits on is what keeps step 5's rebase a no-op.

## 2. Cut it into subtasks

Each entry must **compile and pass tests on its own** — that is the definition, not a size estimate.
A subtask that cannot stand alone is two subtasks with a shared prefix.

A useful cut: the type or interface first, then the implementation, then the assembly that calls it,
then the docs. Each compiles green with the next absent.

The list is also the PR body. Keep it untracked and per-worktree:

```bash
$EDITOR "$(git rev-parse --git-dir)/pr-body.md"    # one line of what changes, then "- [ ] 1. <subtask>"
```

## 3. The inner loop, once per subtask

```bash
make fmt                                # after ANY import block change — only make lint catches it
make ci                                 # build + test + lint
git add -A && git commit                # AGENTS.md's type(scope): summary convention
```

What `make ci` will not ask for:

| If the subtask touched | Also do |
|---|---|
| A parser reading untrusted input | `make fuzz FUZZ=<target>` — `make ci` runs only the seed corpora |
| A **new** fuzz target | All three places (the target, `FUZZ_TARGETS`, AGENTS.md's list); `test/buildrules` fails until they agree |
| A package's invariants or failure directions | The matching [docs/modules/](../../../docs/modules/) file, same commit |
| A package/command name, dependency direction, or frozen identifier | [docs/canonical.md](../../../docs/canonical.md), same commit |
| A new `.md` under `docs/` | A `docs/zh-CN/` counterpart, or a `contributorOnlyDocs` entry with the reason |
| The GUI frontend | `make gui-frontend-ci` — `make gui` runs `npm install`, repairing a lockfile CI's `npm ci` rejects |
| Anything user-visible | New flags and commands are yours; the README badge belongs to the [release skill](../release/SKILL.md) |

**Commit while nothing else is writing.** If a background agent is working in this tree, a clean
`git status` means "nothing written *yet*", not "finished".

Then push and move the PR forward — every subtask, not one batch at the end:

```bash
# first subtask only:
git push -u origin <topic>
gh pr create --draft --base main --title "<topic>: <what it changes>" \
  --body-file "$(git rev-parse --git-dir)/pr-body.md"

# every subtask after: tick its box in that file, then
git push origin HEAD && gh pr edit --body-file "$(git rev-parse --git-dir)/pr-body.md"
gh pr comment --body "<why the plan changed>"      # only when it did
```

**Name the refspec, never bare `git push`.** Under `push.default = matching` a bare push carries
every other worktree's branch along with yours.

Draft until step 4, because until then the branch cannot be landed. It cannot be opened before the
first commit — GitHub refuses a head that does not differ from its base.

## 4. Before marking it ready: the full run

```bash
make ci-full                            # everything the CI workflow runs
```

**`make ci` is not CI** — it misses a skipped depguard proof, the `gui` job, and a stale
`package-lock.json`, all three covered by `ci-full`. AGENTS.md's *Testing and verification* section
has the reasons.

Then e2e **both ways** — one is the regression test for the other:

```bash
make e2e-ci                             # with XDG_RUNTIME_DIR set, as CI's Linux runner has it
make e2e                                # this machine's environment
```

`XDG_RUNTIME_DIR` must **not** move the run directory — only `AGENTHUB_DATA_DIR` does — and this
suite exists to catch the day that stops being true.

```bash
git push origin HEAD
gh pr ready
gh pr checks --watch                    # the same jobs main gets
```

## 5. Land it

**The rebase comes first, `make ci-landing` after it.** A rebase replays your commits onto code you
never tested against.

```bash
# in the worktree
git fetch origin && git rebase origin/main
make ci-landing >/tmp/landing.log 2>&1; echo $?      # 0, or read the log
git push --force-with-lease origin HEAD  # the PR's head must be exactly what lands
```

`ci-landing` drops the test cache and fails on any `(cached)` in its own log: `test/e2e` builds its
binary inside `TestMain`, which the Go cache key does not cover.

**Read its exit status, not its last screenful — and redirect rather than pipe.** The target keeps
`.make/ci.log` and arms `set -o pipefail` itself, so an outer `tee` only breaks the reading: `$?`
then reports `tee`'s status, and `${PIPESTATUS[0]}` evaluates to empty under zsh, which spells it
`$pipestatus` and indexes from 1. A green run ends with `landing check: nothing came from cache,
every package ran`.

**That force-push is what closes the PR.** The rebase rewrote every commit; without it GitHub holds
commits that will never reach `main`.

Then, in the main work tree:

```bash
git merge --ff-only <topic> && git push origin main
sleep 3                                     # GitHub notices the push asynchronously
gh pr view <topic> --json state,mergedAt    # MERGED — main holds its head commit verbatim
git push origin --delete <topic>
git worktree remove ../agent-hub-<topic> && git branch -d <topic>
```

**`OPEN` on the first read is usually the race, not the diagnosis.** GitHub marks a PR merged after
the push returns, so a `gh pr view` issued immediately can win. Re-read before concluding anything:
closing by hand is the one step here that cannot be undone — a closed PR is never marked merged
afterwards, so the link the step exists to preserve is gone for good. Only once a re-read still says
`OPEN` is the head not what landed; `main` is fine then, only the link is missing:
`gh pr close <topic> --comment "landed on main as <sha>"`.

**`--ff-only` is the enforcement, not a formality.** If it refuses, rebase again and re-run
`ci-landing`. Never reach for a plain `git merge` — `main` is linear.

**Land promptly.** A finished branch left sitting needs the whole of step 5 again, against a `main`
that has moved further each day.

## Rebasing your own branch mid-flight

```bash
git fetch origin && git rebase origin/main && git push --force-with-lease origin HEAD
```

`main` moving under you is normal. Pull with `git pull --rebase` so an out-of-date `main` does not
grow a merge commit. The PR follows the branch — same number, same body, checks re-run.

## When it goes wrong

| Symptom | Meaning | Do |
|---|---|---|
| A depguard failure naming a path in *another* worktree | The lint cache is shared across checkouts | `echo $GOLANGCI_LINT_CACHE` — the Makefile defaults it per checkout, an exported value overrides that. `make clean-cache` |
| `make ci` green locally, PR checks red | Almost always the environment, not the code | `gh run view --log-failed`, then re-run `make ci-full` and `make e2e-ci`; compare the runner's env before reading the diff |
| `gh pr create`: "No commits between main and `<topic>`" | Opened before the first commit exists | Commit, push, then create — step 3, in that order |
| The PR reads `OPEN` after `main` was pushed | Its head is not what landed — usually an amend after the last force-push | `gh pr close <topic> --comment "landed on main as <sha>"` |
| `git push --force-with-lease` refuses | Someone else pushed to your branch | Do not force it. A shared branch is not rebased at all |
| A test hangs | — | Get evidence first: an e2e timeout SIGQUITs the process under test and folds the goroutine stacks into the failure |
| A test "passes" by returning early | A precondition silently failed | Preconditions must **fail hard** — a silent skip disguises an environment difference as some other component failing |
| `git merge --ff-only` refuses | The rebase did not happen, or `origin/main` moved | Rebase, re-run `ci-landing`, retry. Never plain-`merge` |
