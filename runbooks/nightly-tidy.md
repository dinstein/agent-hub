# Nightly tidy

A runbook: every step is a command to run or a fact to check.

A recurring pass over **one slice of the tree**, in three concerns — simplify the logic, refactor the
shape, make the docs and the code agree again. It runs on a schedule against a `main` nobody is
mid-way through, and it lands by the ordinary route in
[new-feature.md](new-feature.md).

**The whole loop is governed by one rule: every change must name the failure it prevents or the
reader it helps.** A diff that cannot answer "who is better off" is churn, and churn is not free
here — `main` is linear and several worktrees are usually in flight, so every landed commit is a
rebase for everyone else. A quiet night is a correct outcome. Landing nothing is a correct outcome.

---

## What this loop is not

It does not hunt for bugs, it does not change behaviour, and it does not decide policy. A behaviour
change discovered here becomes a note in the `docs/modules/` file that owns the code, and then a
normal feature branch — not an extra commit on tonight's tidy.

The one exception is the third pass: when the docs and the code disagree, **the code is right and the
docs get corrected**, unless the docs describe an invariant the code has stopped enforcing. That case
is not a documentation defect, and it does not get fixed by editing the sentence — see step 3.

## 0. Preconditions

```bash
cd /Users/<you>/Develop/agent-hub
git fetch origin
git status --short                      # must print nothing
git log --oneline -1 origin/main        # must equal local main
git worktree list                       # note what is in flight; do not tidy inside it
```

**Skip any package an open worktree is touching.** Tidying underneath live work turns one rebase into
a conflict, and the branch's author is the one who pays for it.

If the tree is dirty, stop. A background agent may still be writing; a clean `git status` means
"nothing written yet", not "finished".

## 1. Pick tonight's slice

One package, or one narrow theme across a few. Not the whole tree — a diff nobody can review in one
sitting gets landed on trust, which is the opposite of what this loop is for.

```bash
git log --since="2 weeks ago" --name-only --pretty=format: | sort | uniq -c | sort -rn | head -20
```

Prefer, in order:

1. **What changed recently.** Fresh code is where a simplification is still cheap and where the docs
   are most likely to have been left behind.
2. **What carries a recorded gap.** `grep -rn "current assembly status" docs/modules/` — those
   paragraphs are the standing list of "capability exists ≠ wired up".
3. **What nothing above has touched in months.** Least likely to conflict; also least likely to
   matter. Take it when the first two are empty.

Then open the worktree and read, exactly as step 0–1 of [new-feature.md](new-feature.md):

```bash
git worktree add ../agent-hub-tidy-<slice> -b tidy-<slice> origin/main
```

The three passes below are the subtask list; the draft PR opens on the first commit like any other
(step 3 there). Title it `tidy-<slice>: <what was collapsed>` — "tidy" alone tells a reader nothing
about whether it touches theirs.

Read the slice's `docs/modules/` file **before** the code. Its invariants are what tells a
simplification apart from a regression: several things in this tree look redundant and are load
bearing, and that file is where the reason was written down.

## 2. Pass A — simplify the logic

Read the slice's code for what can be **removed or collapsed**, not for what could be rewritten to
taste.

| Worth a commit | Why it clears the bar |
|---|---|
| A branch that cannot be reached | A reader spends time deciding when it fires; the answer is never |
| A helper with one caller, inlined at that caller | The indirection cost a lookup and bought nothing |
| Duplicated logic where one copy has already drifted | The drift is the finding; the dedup is the fix |
| An error wrapped twice, or swallowed and re-derived | The message a user sees is assembled twice from one fact |
| A parameter every caller passes the same value for | It reads as a choice that exists, and it does not |
| Dead exported surface with no caller in or out of the tree | Every exported name is a promise someone may already be holding — confirm the "out of tree" half before removing |

**Not worth a commit:** renames for taste, reordering that changes no meaning, comment rewording,
splitting a long function that reads fine top to bottom, or replacing a loop with a standard-library
call that is neither shorter nor clearer.

**Never simplify away a fail-closed path.** Security predicates document their failure direction on
purpose; a check that looks redundant with the one above it is usually the second of two independent
gates, and collapsing them turns two failures into one.

```bash
make fmt && make ci
git commit                              # one commit per finding, English message naming what it buys
```

## 3. Pass B — refactor the shape

Only when Pass A has left the slice genuinely hard to change. The bar is higher: a refactor must
either remove a constraint that is currently blocking work, or delete a whole category of mistake.

Before moving anything, check it is allowed to move:

| About to move | Check |
|---|---|
| A package, command, or identifier name | [docs/canonical.md](../docs/canonical.md) §1 — frozen identifiers do not move on a tidy night |
| An import across a package boundary | AGENTS.md's four dependency constraints; `internal/depguardtest` proves each one blocks |
| Anything in the gate chain | The order is frozen: scope → token tier. Both decide from configuration alone, both fail closed |
| Anything on the call path | There is exactly **one** execution path — direct calls and `call_tool` both go through `pipeline.Execute`. A new path must assert its gate count matches a direct call |
| Provenance for an exposed name | `RouteOf` is the only legitimate source; splitting on `__` is forbidden — a server id or tool name may itself contain `__` |
| A tool selector | An allow list, never a deny list; `nil` ≠ `[]`; `omitzero`, not `omitempty` |

If a refactor would be *nicer* but touches any row above, it does not happen here. Write it down in
the `docs/modules/` file and leave.

A refactor that changes an invariant or a failure direction updates that package's `docs/modules/`
file **in the same commit**. That is not bookkeeping: the next reader's only defence against a
silently changed invariant is that file.

## 4. Pass C — docs and code agree again

Start by not repeating the machine. `make ci` already fails on all of this:

| Already checked | By |
|---|---|
| A backticked path in prose or comments that no longer exists | `TestDocsCitePathsThatExist`, `TestCodeCommentsCiteFilesThatExist` |
| A comment crediting a guarantee to a test that is gone | `TestCitedTestsExist` |
| A test doc comment opening with another test's name | `TestDocCommentsNameTheirOwnTest` |
| A `canonical.md` §-citation or ruling id that resolves to nothing | `TestCanonicalCitationsResolve`, `TestHistoricalRulingIdsResolve` |
| Prose still teaching a retired command | `TestNoDocumentTeachesARetiredCommand` |
| A fuzz target missing from the Makefile or from AGENTS.md | `TestEveryFuzzTargetIsInTheMakefile`, `TestAgentsMdNamesEveryFuzzTarget` |
| A README version badge disagreeing with `VERSION` | `TestReadmeBadgesMatchVERSION` |
| An English doc under `docs/` with no translation and no exemption | `TestTranslationsHaveTheSameSectionStructure` |

What is left is what a human has to read for, and it is the whole point of this pass:

- **A documented invariant the code has stopped enforcing.** The dangerous one. It does not get fixed
  by editing the sentence — restore the enforcement, or, if it was deliberately dropped, say so at
  that spot and record what now holds instead. A rule that quietly became a suggestion reads exactly
  like a rule.
- **"Capability exists ≠ wired up" that has gone stale in either direction.** A gap that was fixed
  and still reads as a gap costs a reader a redundant investigation; a gap that was never fixed and
  no longer reads as one is how "I thought that was in effect" happens.
- **A `modules/` file describing a shape the package no longer has** — types renamed, a
  responsibility moved, a failure direction inverted.
- **Within-section translation drift.** `docs/zh-CN/` is checked for its heading skeleton and nothing
  below it. A bullet dropped or folded into another survives the check. Compare the two side by side
  for the sections the slice touched, and only those.
- **A sentence that restates the code.** Delete it. It carries no reason, and it is the sentence most
  likely to be wrong after the next change.

## 5. Land it

By [new-feature.md](new-feature.md) steps 4 and 5, unchanged and unabbreviated — `gh pr ready`, green
checks, the rebase, `make ci-landing`, the force-push, `git merge --ff-only`. A tidy branch gets no
shortcut; it is the branch most likely to have touched something whose test lives somewhere else.

If the slice touched a parser that reads untrusted input, this is a fuzz night too:

```bash
make fuzz FUZZ=<target>                 # make ci runs only the seed corpora
```

## Stop condition

The loop needs one, or it grinds the same slice forever.

**Per slice:** stop when a pass produces nothing that clears the bar. Do not lower the bar to fill the
night — the bar is the only thing separating this loop from churn.

**Per night:** one slice, one branch, landed or discarded before you finish. A tidy branch left open
overnight is the worst kind: it conflicts with real work and nobody remembers what it was for.

**Across nights:** keep the record in the tree, not in a list of its own. What was found and left
alone goes in that package's `docs/modules/` file, beside the code it is about. What was found and
fixed is in `git log`. Neither needs a separate ledger, and a separate ledger is the thing that goes
stale first.

Then close the loop:

```bash
gh pr view tidy-<slice> --json state,mergedAt    # MERGED, or close it by hand
git push origin --delete tidy-<slice>
git worktree remove ../agent-hub-tidy-<slice> && git branch -d tidy-<slice>
```

## Never touch on a tidy night

Not because they are perfect, but because a scheduled pass is the wrong place to decide them:

- The four hard constraints and their proofs in `internal/depguardtest`
- The gate chain, its order, and any predicate's failure direction
- Frozen identifiers ([docs/canonical.md](../docs/canonical.md) §1)
- `VERSION`, the README badges, and anything under `.github/workflows/` — those belong to
  [releasing.md](releasing.md)
- Any file an open worktree has claimed
