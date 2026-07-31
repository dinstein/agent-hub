# Nightly tidy

A recurring pass over **one slice of the tree**, in three concerns — simplify the logic, refactor the
shape, make docs and code agree. It runs against a `main` nobody is mid-way through and lands by
[new-feature.md](new-feature.md).

**One rule governs the loop: every change must name the failure it prevents or the reader it helps.**
A diff that cannot answer "who is better off" is churn, and churn is not free — `main` is linear and
several worktrees are usually in flight, so every landed commit is a rebase for everyone else. A
quiet night is a correct outcome. Landing nothing is a correct outcome.

It does not hunt for bugs ([security-audit.md](security-audit.md) does) and it does not change
behaviour. A behaviour change discovered here becomes a note in the owning `docs/modules/` file and
then a normal feature branch.

---

## 0. Preconditions

```bash
git fetch origin
git status --short                      # must print nothing
git log --oneline -1 origin/main        # must equal local main
git worktree list                       # note what is in flight
```

**Skip any package an open worktree is touching.** Tidying underneath live work turns one rebase into
a conflict, and the branch's author pays for it. If the tree is dirty, stop — a background agent may
still be writing.

## 1. Pick tonight's slice

One package, or one narrow theme across a few. Not the whole tree: a diff nobody can review in one
sitting gets landed on trust.

```bash
git log --since="2 weeks ago" --name-only --pretty=format: | sort | uniq -c | sort -rn | head -20
```

Prefer, in order: **what changed recently** (a simplification is still cheap and the docs are most
likely left behind); **what carries a recorded gap** (`grep -rn "current assembly status"
docs/modules/`); then what nothing has touched in months — least likely to conflict, also least
likely to matter.

```bash
git worktree add ../agent-hub-tidy-<slice> -b tidy-<slice> origin/main
```

The three passes are the subtask list; the draft PR opens on the first commit as usual. Title it
`tidy-<slice>: <what was collapsed>` — "tidy" alone tells a reader nothing about whether it touches
theirs.

Read the slice's `docs/modules/` file **before** the code. Its invariants are what tells a
simplification apart from a regression.

## 2. Pass A — simplify the logic

Read for what can be **removed or collapsed**, not for what could be rewritten to taste.

| Worth a commit | Why it clears the bar |
|---|---|
| A branch that cannot be reached | A reader spends time deciding when it fires; the answer is never |
| A helper with one caller, inlined there | The indirection cost a lookup and bought nothing |
| Duplicated logic where one copy has drifted | The drift is the finding; the dedup is the fix |
| An error wrapped twice, or swallowed and re-derived | The message a user sees is assembled twice from one fact |
| A parameter every caller passes the same value for | It reads as a choice that exists, and it does not |
| Dead exported surface with no caller in or out of the tree | Confirm the "out of tree" half before removing |

**Not worth a commit:** renames for taste, reordering that changes no meaning, comment rewording,
splitting a long function that reads fine top to bottom, or a standard-library call that is neither
shorter nor clearer.

**Never simplify away a fail-closed path.** A check that looks redundant with the one above it is
usually the second of two independent gates; collapsing them turns two failures into one.

```bash
make fmt && make ci
git commit                              # one commit per finding, naming what it buys
```

## 3. Pass B — refactor the shape

Only when Pass A left the slice genuinely hard to change. The bar is higher: a refactor must remove a
constraint that is blocking work, or delete a whole category of mistake.

Before moving anything, check it is allowed to move:

| About to move | Check |
|---|---|
| A package, command, or identifier name | [docs/canonical.md](../docs/canonical.md) §1 — frozen identifiers do not move on a tidy night |
| An import across a package boundary | AGENTS.md's four dependency constraints |
| Anything in the gate chain | The order is frozen: scope → token tier, both deciding from configuration alone, both failing closed |
| Anything on the call path | Exactly **one** execution path — direct calls and `call_tool` both go through `pipeline.Execute` |
| Provenance for an exposed name | `RouteOf` only; splitting on `__` is forbidden |
| A tool selector | An allow list, never a deny list; `nil` ≠ `[]`; `omitzero`, not `omitempty` |

If a refactor would be *nicer* but touches any row above, it does not happen here — write it down in
the `docs/modules/` file and leave. A refactor that does change an invariant or a failure direction
updates that file **in the same commit**: the next reader's only defence against a silently changed
invariant is that file.

## 4. Pass C — docs and code agree again

`make ci` already fails on the mechanical half — dead backticked paths, comments crediting a deleted
test, unresolvable `canonical.md` citations, prose teaching a retired command, a fuzz target missing
from the Makefile or AGENTS.md, a README badge disagreeing with `VERSION`, an untranslated English
doc. Do not repeat the machine.

What a human has to read for:

- **A documented invariant the code has stopped enforcing.** The dangerous one, and it does not get
  fixed by editing the sentence — restore the enforcement, or, if it was deliberately dropped, say so
  there and record what now holds instead. A rule that quietly became a suggestion reads exactly like
  a rule.
- **"Capability exists ≠ wired up" gone stale in either direction.** A fixed gap that still reads as
  a gap costs a redundant investigation; an unfixed one that no longer reads as a gap is how "I
  thought that was in effect" happens.
- **A `modules/` file describing a shape the package no longer has** — types renamed, a
  responsibility moved, a failure direction inverted.
- **Within-section translation drift.** `docs/zh-CN/` is checked for its heading skeleton and nothing
  below it, so a dropped bullet survives. Compare side by side for the sections the slice touched.
- **A sentence that restates the code.** Delete it — it carries no reason and is the sentence most
  likely to be wrong after the next change.

## 5. Land it

[new-feature.md](new-feature.md) steps 4–5, unabbreviated. A tidy branch gets no shortcut; it is the
branch most likely to have touched something whose test lives somewhere else. If the slice touched a
parser reading untrusted input, this is a fuzz night too:

```bash
make fuzz FUZZ=<target>                 # make ci runs only the seed corpora
```

## Stop condition

**Per slice:** stop when a pass produces nothing that clears the bar. Do not lower the bar to fill
the night — the bar is the only thing separating this loop from churn.

**Per night:** one slice, one branch, landed or discarded before you finish. A tidy branch left open
overnight conflicts with real work and nobody remembers what it was for.

**Across nights:** what was found and left alone goes in that package's `docs/modules/` file; what
was fixed is in `git log`. A separate ledger is the thing that goes stale first.

```bash
sleep 3                                          # GitHub notices the push asynchronously
gh pr view tidy-<slice> --json state,mergedAt    # MERGED — re-read before believing OPEN
git push origin --delete tidy-<slice>
git worktree remove ../agent-hub-tidy-<slice> && git branch -d tidy-<slice>
```

## Never touch on a tidy night

Not because they are perfect, but because a scheduled pass is the wrong place to decide them:

- The four hard constraints and their proofs in `internal/depguardtest`
- The gate chain, its order, and any predicate's failure direction
- Frozen identifiers ([docs/canonical.md](../docs/canonical.md) §1)
- `VERSION`, the README badges, and `.github/workflows/` — those belong to [releasing.md](releasing.md)
- Any file an open worktree has claimed
