# Security audit sweep

A read-only sweep over the tree, run as a **workflow**: Claude and Codex review the same shards
concurrently and independently, every finding is then attacked by verifiers, and one adjudication
pass merges what survives. Nothing writes to the tree until step 4.

Not `/security-review`, which reviews the pending diff on a branch — that never finds anything older
than the newest branch. Not [nightly-tidy.md](nightly-tidy.md), which may not change behaviour.

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
different lenses: does it reproduce, does a gate above already stop it, does `docs/modules/` record
it as deliberate. Default to refuted when uncertain; survives at 2-of-3. Verifiers are not told which
engine raised it — engine count is a rank input in the next phase, not a thumb on the scale here.

**Adjudicate** — one agent over the survivors. Dedup by file, line ±5 and claim: the same defect
arrives under two names from two engines far more often than twice from one. Then rank, engine count
first — `confirmed` (both engines, independently, same code) above `single-engine` (one engine,
evidence that stands alone); drop the rest, **citing `docs/modules/`** where a decision was
deliberate, since an uncited drop reads to the next sweep exactly like an oversight.

It returns the ranked list plus counts: found per engine, refuted, dropped, overlap. Those counts are
the sweep's health check — no overlap means the shards were cut wrong; near-total refutation means
the brief was wrong, not that the tree is clean.

Read the returned list yourself. That reading is not delegated: it is the only place the whole
picture exists.

### The brief every finder gets

Generic scanning finds generic bugs. This is what makes it find *this* repo's, and it is the file to
fix when both engines report the same non-bug:

- The **hard constraints and the invariants** from [AGENTS.md](../AGENTS.md), verbatim — the frozen
  gate chain order, the single execution path, allow-list-never-deny-list, `nil` ≠ `[]`, provenance
  only from the sanctioned accessor, the zero-dependency foundations.
- A **failure direction in a doc comment is part of the signature**. Comment says fail-closed, code
  returns permissive on error → finding, and the shape this sweep exists for.
- **Isolation a config claims must be delivered or refused** — a silent degradation to the weaker
  runtime is the highest-severity shape in the tree.
- Read the shard's [docs/modules/](../docs/modules/) file first. Several things here look redundant
  and are load bearing; that file is where the reason was written down.

## 3. Reproduce

Every confirmed finding earns a **failing test on `main`** before it earns a patch. A test that fails
confirms it and is the first half of the fix commit; one that passes refutes it and gets deleted.

**A finding with neither a failing test nor quoted proof does not get fixed.** It gets a line in the
owning `docs/modules/` file saying it was raised and could not be reproduced. An unverified fix to a
fail-closed gate changes the failure direction of code nobody could demonstrate was wrong.

## 4. Fix and land

One worktree by [new-feature.md](new-feature.md); the confirmed findings are the subtask list and
therefore the PR body.

- **One commit per finding**, its test first, in the same commit.
- **Parallel fixers only over non-overlapping file sets** — two in one package cost more to referee
  than the serial run saved.
- **Not delegated at all**: the gate chain, any failure direction, a hard constraint, a frozen
  identifier. Those are design decisions wearing a bug's clothes, and AGENTS.md already says which
  one bends.
- **A fix that only works by relaxing an invariant is not a fix.** Record it beside the invariant and
  leave it for its own branch, with its own argument.
- A fix changing a failure direction updates that `docs/modules/` file in the same commit.
- A parser fix gets `make fuzz FUZZ=<target>` — `make ci` runs only the seed corpora.

## Stop condition

**Per shard:** stop when Find returns nothing clearing the concrete-failure bar. Do not re-run hoping
for a different answer — two engines disagreeing is signal, the same engine twice is noise.

**Per sweep:** one branch, landed or discarded the same day. Confirmed findings sitting unfixed is
the worst state this loop can end in — the knowledge exists and nothing in the tree records it.

**Across sweeps:** fixed is in `git log`, refuted is in `docs/modules/`. A ledger goes stale first.

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
