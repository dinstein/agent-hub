---
name: security-audit
description: Run AgentHub's security sweep with parallel finders, adversarial verification, adjudication, and a report before fixes. Use for a whole-repository audit or a security review of a named path or theme.
---

# Security audit sweep

A read-only sweep over the tree, run as a **workflow**: Claude and Codex review the same shards
concurrently and independently, every finding is then attacked by verifiers, and one adjudication
pass merges what survives.

**The sweep ends at a report.** Steps 0–3 change nothing in the tree; step 3 hands the ranked list to
the user and waits. Steps 4–5 run only over the findings they named.

Not `/security-review`, which reviews the pending diff on a branch — that never finds anything older
than the newest branch. Not the [nightly-tidy skill](../nightly-tidy/SKILL.md), which may not change behaviour.

No model is pinned here. The workflow inherits the session's; pinning one is how a runbook ages into
naming something that no longer exists.

---

## 0. Preconditions

```bash
git fetch origin
git status --short                      # must print nothing
git log --oneline -1 origin/main        # must equal local main
command -v codex && codex exec --sandbox read-only "Reply with exactly: OK"
```

A dirty tree is disqualifying: findings quote `file:line`, and an uncommitted edit invalidates every
citation. Reviewing code an open worktree is about to change is fine — the sweep reads.

Codex absent or unauthenticated: run single-engine and say so at the top of the report. No finding
may then be ranked `confirmed` by engine agreement — that rank is unavailable, not satisfied by
default. Without the throwaway call, an expired login fails identically to a bad prompt, once per
shard, minutes in.

## 1. Shard it

Shard on **trust boundary first, volume second**: group by what a shard's code trusts — bytes from
outside the process, remote endpoints, the gate chain, secrets at rest, spawned processes and claimed
isolation, and the surface exposed outward. Then cap each at **~8k non-test lines**, measured:

```bash
git ls-files '*.go' | grep -v _test.go | xargs wc -l | sort -rn | head -40
```

**The tree sets the sweep's scale.** At today's size that is a dozen shards, two finders each, plus
three verifiers per finding — well past any default agent-count guideline a session carries. Cutting
fewer, larger shards to fit under one is how the overlap the health check needs disappears; the
symptom table catches that only after the sweep has run.

Both engines get the **identical** shard — comparing two answers only means something when they were
asked the same question about the same files. A package split by file must **say so** in the prompt:
a finder shown half a package and not told reports the other half's callers as unreachable.

A scope argument narrows which boundaries are swept, never how they are cut. A theme still shards by
boundary — grepping for "token" reviews the code that already knows it handles credentials, which is
not where the mistake is.

## 2. Run the workflow

**Find** — every shard to both engines, concurrently, neither seeing the other's answer. One agent
per shard per engine; the Codex agent only runs the CLI and returns its output verbatim, reviewing
nothing itself, so the engines stay independent:

```bash
codex exec --sandbox read-only -o "$OUT" "$(cat brief)

<shard file list>"
```

Read the `-o` file, never stdout — `codex exec` prints a session preamble there. Exit 0 is not a
verdict; the check is that the file is non-empty and parses as the contract.

Each finding carries `file:line`, severity, the concrete failure path, quoted evidence, one sentence
of fix direction, and its engine. **No finding without a concrete failure path** — "consider
validating X" is not one, and an empty shard is a correct outcome the next phase must be able to tell
apart from noise.

**Verify** — each finding, as its shard lands, goes to three agents told to **refute** it, on
different lenses: does it reproduce, does a gate above already stop it, does `docs/subsystems/` record
it as deliberate. Default to refuted when uncertain; survives at 2-of-3. Verifiers are not told which
engine raised it — engine count is a rank input in the next phase, not a thumb on the scale here.

**Adjudicate** — one agent over the survivors. Dedup by file, line ±5 and claim: the same defect
arrives under two names from two engines far more often than twice from one. Then rank, engine count
first — `confirmed` (both engines, independently, same code) above `single-engine` (one engine,
evidence that stands alone); drop the rest, **citing `docs/subsystems/`** where a decision was
deliberate, since an uncited drop reads to the next sweep exactly like an oversight.

It returns the ranked list plus counts: found per engine, refuted, dropped, overlap. Those counts are
the sweep's health check — no overlap means the shards were cut wrong; near-total refutation means
the brief was wrong, not that the tree is clean.

Read the returned list yourself. That reading is not delegated: it is the only place the whole
picture exists — and step 3 cannot be written from the workflow's return value alone.

### The brief every finder gets

Generic scanning finds generic bugs. This is what makes it find *this* repo's, and it is the file to
fix when both engines report the same non-bug:

- The **hard constraints and the invariants** from [AGENTS.md](../../../AGENTS.md), verbatim — the frozen
  gate chain order, the single execution path, allow-list-never-deny-list, `nil` ≠ `[]`, provenance
  only from the sanctioned accessor, the zero-dependency foundations.
- A **failure direction in a doc comment is part of the signature**. Comment says fail-closed, code
  returns permissive on error → finding, and the shape this sweep exists for.
- **Isolation a config claims must be delivered or refused** — a silent degradation to the weaker
  runtime is the highest-severity shape in the tree.
- **Every path by which external bytes arrive maps to a fuzz target** — AGENTS.md lists the current
  set, one target per path. A parser of untrusted input with no target is itself a finding:
  `test/buildrules` only proves the three declared lists agree with each other, never that a
  newly-landed path joined them.
- Read the [docs/subsystems/](../../../docs/subsystems/) files covering the shard first — they are cut by
  plane, so a trust-boundary shard usually spans more than one, and `security.md` is always among
  them: it holds what earlier sweeps raised, declined, and closed. Several things here look redundant
  and are load bearing; those files are where the reason was written down.

## 3. Report and stop

Per finding: title, `file:line`, the concrete failure path, the evidence, the fix direction, and
`confirmed` or `single-engine`. Then adjudication's counts — four findings is a different claim after
forty refutations than after four.

**Nothing is fixed until the user names which findings to fix.** Approval covers the list as
reported: not what the sweep raises afterwards, and not a fix that turns out to need an invariant
relaxed. A sweep arriving with a branch already open has taken the one decision here that is not
technical — whether a risk is worth changing the tree for — from the person who lives with both.

Declined and unanswered findings get their `docs/subsystems/` line, beside the invariant they bend. That
is a tree change like any other, so it lands on step 5's branch; if nothing was approved, it *is* the
branch.

## 4. Reproduce

Only approved findings reach this step. Each earns a **failing test on `main`** before it earns a
patch. A test that fails confirms it and is the first half of the fix commit; one that passes refutes
it, gets deleted, and goes back to the user — they approved a fix for something that is not there.

**A finding with neither a failing test nor quoted proof does not get fixed.** It gets a line in the
owning `docs/subsystems/` file saying it was raised and could not be reproduced. An unverified fix to a
fail-closed gate changes the failure direction of code nobody could demonstrate was wrong.

## 5. Fix and land

One worktree by the [new-feature skill](../new-feature/SKILL.md); the **approved** findings are the subtask list and
therefore the PR body. Confirmed but unapproved is not the same set, and does not appear here.

- **One commit per finding**, its test first, in the same commit.
- **Parallel fixers only over non-overlapping file sets** — two in one package cost more to referee
  than the serial run saved.
- **Not delegated at all**: the gate chain, any failure direction, a hard constraint, a frozen
  identifier. Those are design decisions wearing a bug's clothes, and AGENTS.md already says which
  one bends.
- **A fix that only works by relaxing an invariant is not a fix.** Record it beside the invariant and
  bring it back: they approved closing a hole, not bending a rule. Its own branch, its own argument.
- A fix changing a failure direction updates that `docs/subsystems/` file in the same commit.
- A parser fix gets `make fuzz FUZZ=<target>` — `make ci` runs only the seed corpora.

## Stop condition

**Per shard:** stop when Find returns nothing clearing the concrete-failure bar. Do not re-run hoping
for a different answer — two engines disagreeing is signal, the same engine twice is noise.

**Per sweep:** the sweep ends at step 3's report — a complete run, not an abandoned one. The approval
starts a branch, and that branch lands or is discarded the same day. Waiting for an answer is the
expected shape; **the bad end is a finding nobody wrote down**, not one nobody fixed.

**Across sweeps:** fixed is in `git log`; declined and unanswered are in `docs/subsystems/`. A separate
ledger goes stale first. Verify's refutations are recorded nowhere, deliberately — most are noise, and
writing them down would bury the two lists that matter. The exception is a non-bug that keeps coming
back: adjudication's counts show it, and its correction goes **into the brief below**, which every
future finder reads — otherwise each sweep pays three verifiers to re-refute the same sentence.

## When it goes wrong

| Symptom | Meaning | Do |
|---|---|---|
| Everything refuted, across shards | The brief describes an invariant wrongly | Fix the brief and re-run. Never patch the finding list |
| The two engines overlap on nothing | Shards cut too large to compare | Re-cut smaller. Without overlap it is two single-engine sweeps |
| A Codex shard is empty, exit 0 | The answer may only ever have gone to stdout | Read the `-o` file, then the log. Re-run that shard before blaming the boundary |
| `codex` hangs with no output | Auth, or a prompt with no TTY | Step 0's throwaway call exists for this |
| Findings are prose, not the block shape | The output contract was not last in the prompt | Repeat it verbatim per shard; models follow the final instruction |
| All `medium`, none reproducible | Finders asked to be thorough rather than concrete | A failed sweep, not a clean tree — re-cut smaller and demand the failure path |
| Findings cluster in the newest package | Sharded by file listing, not by trust boundary | Re-shard by step 1; volume-only shards review whatever is largest, not riskiest |
| A finding calls a check redundant with the one above | Usually the second of two independent gates | Never collapse a fail-closed path |
| A boundary returns nothing, twice, across sweeps | Possibly true, possibly unreviewable | Check the shard named files that exist — an empty shard and a mistyped path read identically |
| `git status` dirty after step 2 | A finder wrote despite being read-only | Discard and re-run the whole sweep; every line number is now unverifiable |
| A branch or a test exists before the user answered | Step 3's gate was skipped | Discard the branch, not the findings — the list is still good |
