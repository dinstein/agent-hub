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

## The short lane: a change that compiles nothing

Ask the tree, not yourself — the answer is a command, because "it's only a doc change" is a judgement
and this must not be one:

```bash
git diff --name-only origin/main... | grep -vE '^(docs/|\.agents/|[A-Z]+\.md$)'
```

Empty output means the branch changes no file any build reads: prose under `docs/`, a skill, or a
capitalised root document such as this repository's `AGENTS.md`. `Makefile`, `.github/`, `.gitmessage`
and anything under `test/` are code by this rule and print, as they should — they decide what the
checks do.

The lane keeps the worktree, the commit convention, the PR and `--ff-only`. It cuts the checks, which
is where the minutes are:

```bash
go test ./test/buildrules/ -count=1      # the only suite that reads prose
```

That is the whole local verdict, before the push **and** after the rebase in step 5. `test/buildrules`
is what fails when a document cites a `docs/conventions.md` section that no longer exists, a path that has
moved, or a fuzz target nothing declares; every other package compiles from Go sources this branch did
not touch. Rebasing prose onto new code cannot break the code — but it can invalidate a citation
somebody else moved out from under you, which is the same suite.

The PR still gets the full run on both runners. If the `grep` prints anything at all, you are not in
this lane: go to step 0.

---

## 0. Read before writing

Stop as soon as the question is answered:

| Question | Read |
|---|---|
| What am I changing, and what does a call pass through on the way? | [docs/architecture.md](../../../docs/architecture.md) |
| How does this flow behave at runtime, and which way does it fail? | [docs/flows.md](../../../docs/flows.md) |
| What must not be touched in the package I am about to open? | [docs/subsystems/](../../../docs/subsystems/) — that package's file |
| Is this name / dependency / convention allowed to move? | [docs/conventions.md](../../../docs/conventions.md) |

**That `docs/subsystems/` file is not optional reading** — it carries the invariants and the recorded
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
| A package's invariants or failure directions | The matching [docs/subsystems/](../../../docs/subsystems/) file, same commit |
| A package/command name, dependency direction, or frozen identifier | [docs/conventions.md](../../../docs/conventions.md), same commit |
| A new `.md` under `docs/` | A `docs/zh-CN/` counterpart, or a `contributorOnlyDocs` entry with the reason |
| The GUI frontend | `make gui-frontend-ci` — `make gui` runs `npm install`, repairing a lockfile CI's `npm ci` rejects |
| Anything user-visible | New flags and commands are yours; the version number belongs to the [release skill](../release/SKILL.md) |

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

## 4. Mark it ready, and let the PR's checks be the full run

```bash
git push origin HEAD
gh pr ready
gh pr checks --watch                    # ubuntu + macOS, the same jobs main gets
```

**`make ci` is not CI** — it misses a skipped depguard proof, the `gui` job, and a stale
`package-lock.json`. `make ci-full` covers all three, and so does the PR, on two runners this machine
is not: what `ci-full` adds over `ci` is precisely the part that is about the environment rather than
the code, and the environment that decides is the runner's. Running it here first buys minutes of
warning at the cost of a second full build every time. Let the checks report.

Run `make ci-full` locally anyway when you expect it to be the thing that breaks, and want the answer
in one minute rather than ten:

| The branch touched | Because |
|---|---|
| The GUI frontend, or anything `-tags wails` | The whole `gui` job is outside `make ci`, and wails v3 is an alpha |
| The depguard rules, or an import that crosses `internal/*` | A skipped proof is no proof, and `make test` reports the skip as success |
| The Makefile, `.github/workflows/`, or `test/buildrules` | You are changing the check itself; a red PR then tells you nothing about the code |
| Nothing — but `gh pr checks` cannot run | Actions is down or the fork has no runners. Then `ci-full` is the whole verdict, and say so in the PR |

## 5. Land it

**The rebase comes first, `make ci-landing` after it.** A rebase replays your commits onto code you
never tested against.

```bash
# in the worktree
before=$(git rev-parse HEAD)
git fetch origin && git rebase origin/main
if [ "$(git rev-parse HEAD)" != "$before" ]; then
  make ci-landing >/tmp/landing.log 2>&1; echo $?    # 0, or read the log
fi
git push --force-with-lease origin HEAD  # the PR's head must be exactly what lands
```

**A rebase that moved nothing needs no landing run.** `ci-landing` exists for one reason — commits
replayed onto a base they were never tested against — and when `origin/main` has not moved, the sha
is unchanged and there is no such base: the tree about to land is the tree the PR's checks graded
minutes ago. Skipping it then is the rule being applied, not relaxed. Two conditions come with it,
and both are mechanical:

```bash
gh pr view --json headRefOid -q .headRefOid    # must equal git rev-parse HEAD
gh pr checks                                   # green, for that head
```

If the head differs — an amend after the last push, a commit added since the watch — the checks
graded something else, and `make ci-landing` is how you find out what. When in doubt, run it: the cost
is minutes, and the thing it guards is the one irreversible step in this file.

`ci-landing` drops the test cache and fails on any `(cached)` in its own log: `test/e2e` builds its
binary inside `TestMain`, which the Go cache key does not cover.

**Read its exit status, not its last screenful — and redirect rather than pipe.** The target keeps
`.make/ci.log` and arms `set -o pipefail` itself, so an outer `tee` only breaks the reading: `$?`
then reports `tee`'s status, and `${PIPESTATUS[0]}` evaluates to empty under zsh, which spells it
`$pipestatus` and indexes from 1. A green run ends with `landing check: nothing came from cache,
every package ran`.

**When the rebase rewrote your commits, that force-push is what closes the PR** — without it GitHub
holds commits that will never reach `main`. After a rebase that moved nothing it is a no-op, and the
head already matches; run it anyway rather than deciding which case you are in.

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
| `make ci` green locally, PR checks red | Almost always the environment, not the code | `gh run view --log-failed`, then re-run `make ci-full` — `e2e-ci` is one of its prerequisites, and reproducing the runner's environment is what it is for. Compare that env before reading the diff |
| `gh pr create`: "No commits between main and `<topic>`" | Opened before the first commit exists | Commit, push, then create — step 3, in that order |
| The PR reads `OPEN` after `main` was pushed | Its head is not what landed — usually an amend after the last force-push | `gh pr close <topic> --comment "landed on main as <sha>"` |
| `git push --force-with-lease` refuses | Someone else pushed to your branch | Do not force it. A shared branch is not rebased at all |
| A test hangs | — | Get evidence first: an e2e timeout SIGQUITs the process under test and folds the goroutine stacks into the failure |
| A test "passes" by returning early | A precondition silently failed | Preconditions must **fail hard** — a silent skip disguises an environment difference as some other component failing |
| `git merge --ff-only` refuses | The rebase did not happen, or `origin/main` moved | Rebase, re-run `ci-landing`, retry. Never plain-`merge` |
