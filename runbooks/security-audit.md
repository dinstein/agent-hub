# Security audit sweep

A runbook: every step is a command to run or a fact to check.

An iterative read-only sweep over the tree by **two independent engines — Claude Code and Codex —
reviewing the same shard without seeing each other's answer**. That is the whole point of running
both: one engine's finding is a claim, and two engines arriving at the same line independently is
the closest thing to evidence this loop produces. Everything after the sweep is done by the agent
running this runbook: consolidate, refute, and only then dispatch fixes.

Nothing in steps 1–6 writes to the tree. The reviewers are sandboxed read-only on purpose, and the
first step that changes a file is step 7, which lands by the ordinary route in
[new-feature.md](new-feature.md).

---

## What this is not

Claude Code ships a `/security-review` skill that reviews the **pending changes on the current
branch**. That is the per-branch check, it stays, and it is not what this is. The two ask different
questions — "did this diff introduce something" versus "what is already in the tree" — and a repo
that only ever asks the first one never finds anything older than its newest branch.

It is also not [nightly-tidy.md](nightly-tidy.md). Tidy may not change behaviour; this loop exists to
change it. A tidy finding discovered here is written down and left alone.

## 0. Preconditions

```bash
cd /Users/<you>/Develop/agent-hub
git fetch origin
git status --short                      # must print nothing
git log --oneline -1 origin/main        # must equal local main
git worktree list                       # note what is in flight
```

A dirty tree is disqualifying, not inconvenient: the reviewers quote `file:line`, and every one of
those citations is wrong the moment an uncommitted edit shifts a line. If a background agent may
still be writing, a clean `git status` means "nothing written yet", not "finished".

Review whatever `main` holds — including code an open worktree is about to change. Unlike a tidy
night there is no conflict to cause: the sweep reads, and its output is a list, not a diff.

## 1. Probe the engines

```bash
command -v claude && claude --version
command -v codex  && codex --version
```

| Found | Do |
|---|---|
| Both | The normal run. Every shard goes to both, and step 5 ranks a finding by how many engines reached it |
| Exactly one | Run it, and write `single-engine` at the top of the report. No finding may be ranked "both agreed" — the rank is unavailable, not satisfied by default |
| Neither | Stop. There is no degraded mode still worth calling a security review |

Then spend one throwaway call per engine, before sharding anything:

```bash
claude -p --model sonnet "Reply with exactly: OK"
codex exec --sandbox read-only "Reply with exactly: OK"
```

An expired login fails identically to a bad prompt, and without this it fails once per shard, in
parallel, after the sweep has already been running for minutes.

## 2. Choose the scope

**Default: the whole repo.** The sweep is affordable at this size — ~75k non-test Go lines — and a
scoped run only makes sense when you already know where to look.

| Scope given | Means |
|---|---|
| nothing | Every lane in step 3, over the whole tree |
| a path (`internal/oauthflow`) | The lanes that contain it, that path only |
| a theme ("credential handling") | The lanes it maps to, whole packages — not a grep for the word |

A themed scope still shards by lane. Narrowing to "the files that mention tokens" reviews the code
that already knows it handles credentials, which is not where the mistake usually is.

## 3. Shard it

Shard on **trust boundary first, volume second**. A shard is a lane plus a size cap, and both engines
get the identical shard — comparing two answers only means something when they were asked the same
question about the same files.

| Lane | Packages | What the reviewer is looking for |
|---|---|---|
| Untrusted parsers | `internal/mcp`, `internal/jsonl`, `internal/confops` | Bytes from outside the process: panics, unbounded allocation, hand-written index scanning walking off the end. Every path here should be reachable from a fuzz target — one that is not is itself the finding |
| Remote surface | `internal/downstream`, `internal/oauthflow`, `internal/httpbridge`, `internal/discovery` | SSRF, redirects followed past a check, a credential that reaches a URL or a log line, TLS decided per-call rather than per-config |
| The gate chain | `internal/scope`, `internal/tier`, `internal/pipeline`, `internal/router`, `internal/ratelimit`, `internal/guard` | A second path to a downstream with a different gate count, a fail-closed predicate that returns permissive on error, an allow list used as a deny list |
| Secrets | `internal/secrets`, `internal/oauthlogin`, `internal/session` | At-rest file modes, lifetime, what survives into a log or an error message, comparisons that are not constant-time |
| Spawn and isolation | `internal/guard/spawnguard`, `internal/clients`, `internal/daemon`, `internal/gateway` | Argv and env smuggling, inherited environment, and any place a claimed isolation degrades instead of refusing |
| Exposed surface | `internal/registry`, `internal/catalog`, `internal/cli`, `internal/ctlapi`, `api`, `internal/shaping`, `internal/skills` | Anything that widens what an earlier layer intersected, `nil` vs `[]`, provenance taken from a name instead of from `RouteOf` |

Then split by volume — **8k non-test lines per shard**, measured, not guessed:

```bash
git ls-files '*.go' | grep -v _test.go | xargs wc -l | sort -rn | head -40
```

A package over the cap on its own (`internal/cli` is, comfortably) splits by file, and the shard
header must **say so**: a reviewer that saw half a package and was not told will report the other
half's callers as unreachable.

Everything this loop writes goes where it stays untracked and per-worktree:

```bash
AUD="$(git rev-parse --git-dir)/audit" && mkdir -p "$AUD"
$EDITOR "$AUD/brief.md"                 # the common brief, below
$EDITOR "$AUD/shard-01.md"              # one per shard: the lane, the file list, the split note
```

### The common brief

Both engines get this, prepended to every shard. It is what turns a generic scanner into one that
can find *this* repo's bugs, and it is the file to fix when both engines report the same non-bug.

- The failure direction in a doc comment is part of the signature. A predicate whose comment says
  fail-closed and whose code returns permissive on an error is a finding, and it is the shape of
  finding this sweep exists for.
- **A tool selector is an allow list, never a deny list**, and `nil` ≠ `[]`: nil means "no rule", `[]`
  means "nothing". `omitzero`, not `omitempty` — dropping an empty list turns block-all into
  allow-all.
- **There is exactly one execution path.** Direct calls and `call_tool` both go through
  `pipeline.Execute`. Any second route that reaches a downstream is a finding whether or not it
  currently skips a gate.
- **The gate chain order is frozen**: scope → token tier, both deciding from configuration alone.
  Nothing in the chain may inspect or rewrite what a call carries.
- **`RouteOf` is the only legitimate provenance for an exposed name.** Any split on `__` is a finding
  regardless of how safe it looks — a server id or a tool name may itself contain `__`.
- **Isolation a config claims must be delivered or refused.** A `runtime: docker` that quietly runs
  on the host is the highest-severity shape in the tree.
- `internal/guard/*`, `internal/mcp`, `internal/platform` and `internal/logx` carry zero business
  dependencies. A new import into one of them is a finding even when the code is correct.
- Read the lane's [docs/modules/](../docs/modules/) file before reporting. Several things here look
  redundant and are load bearing, and that file is where the reason was written down. Re-reporting a
  recorded deliberate decision costs the consolidation step more than it costs you.

**The output contract**, verbatim, as the last paragraph of every shard prompt — one block per
finding and nothing else:

```
## <id> <one-line claim>
- file: <path>:<line>
- lane: <lane>
- severity: critical | high | medium | low
- failure: <concrete input or state → what it gets an attacker>
- evidence: <the lines that prove it, quoted>
- fix direction: <one sentence — do not write the patch>
```

**No finding without a concrete failure path.** "Consider validating X" is not a finding, and a
report full of them is worse than an empty one: it costs the same to triage and confirms nothing.
Returning nothing for a shard is a correct outcome, and step 5 needs to be able to tell that apart
from a shard that produced noise.

## 4. Run both engines, read-only

```bash
for s in "$AUD"/shard-*.md; do
  n=$(basename "$s" .md)
  claude -p --permission-mode plan --disallowedTools "Edit,Write,NotebookEdit" \
    --append-system-prompt "$(cat "$AUD/brief.md")" \
    "$(cat "$s")" >"$AUD/claude-$n.md" 2>"$AUD/claude-$n.log" &
  codex exec --sandbox read-only -o "$AUD/codex-$n.md" \
    "$(cat "$AUD/brief.md")

$(cat "$s")" >"$AUD/codex-$n.log" 2>&1 &
done
wait
```

Four shards in flight at a time, not forty. Each of those eight processes greps and builds on its
own, and the machine — not the API — is what saturates first.

Three details that decide whether the output is usable at all:

| | Why it is written that way |
|---|---|
| `--permission-mode plan` **and** the disallow list | A reviewer that edits invalidates the `file:line` citation in every other shard still running. Two independent brakes, because the failure is silent and retroactive |
| `codex exec -o <file>` | `codex exec` prints a session preamble and a token count to stdout. `-o` is the only clean copy of the answer — read that file, never the stdout log |
| Neither exit status is a verdict | Both engines exit 0 having said nothing useful. The check is that the output file is non-empty **and** parses as the contract |

```bash
wc -l "$AUD"/claude-shard-*.md "$AUD"/codex-shard-*.md    # a zero here is a failed run, not a clean lane
git status --short                                        # must still print nothing
```

That second line is not paranoia: it is the only thing standing between a reviewer that ignored its
sandbox and a consolidation step built on stale line numbers.

## 5. Consolidate — the agent running this runbook, not a subagent

This step is not delegated. It is the only place the whole picture exists, and a subagent handed
twenty files reports twenty summaries.

1. **Dedup** by file, line ±5, and claim. The same defect arrives under different names from the two
   engines far more often than it arrives twice from one.
2. **Rank**, and the rank is the engine count first:

   | Rank | Reached by |
   |---|---|
   | Confirmed candidate | Both engines, independently, at the same code |
   | Single-engine candidate | One engine, with quoted evidence that stands on its own |
   | Refuted by default | One engine, no evidence, or an argument that restates the code |

3. **Drop** style opinions, "consider" items, and anything `docs/modules/` already records as a
   deliberate decision. Cite the file when dropping — an undocumented drop reads to the next sweep
   exactly like an oversight.
4. Write one ranked file, `$AUD/report.md`, and **keep the count of what each engine found and what
   was dropped**. A sweep whose two engines overlap on nothing is telling you the shards were cut
   wrong, and that is visible only in the counts.

## 6. Refute before anything is fixed

Every surviving finding gets one attempt to **kill** it — the question is "show me this cannot
happen", not "is this plausible". Plausible is what a language model produces on demand.

The strongest refutation and the strongest confirmation are the same artifact:

```bash
go test ./internal/<pkg>/ -run <TheNewTest> -count=1
```

A test that fails on `main` confirms the finding and is the first half of the fix commit. A test that
passes refutes it, and gets deleted.

**A finding with neither a failing test nor quoted proof does not get fixed.** It gets a line in that
package's [docs/modules/](../docs/modules/) file saying it was raised and could not be reproduced.
That is not giving up: an unverified fix to a fail-closed gate is a change in the failure direction
of code nobody could demonstrate was wrong, which is strictly worse than the finding.

## 7. Dispatch the fixes

One worktree, one branch, by [new-feature.md](new-feature.md) from step 1 — the confirmed findings
are the subtask list and therefore the PR body:

```bash
git worktree add ../agent-hub-security-<scope> -b security-<scope> origin/main
```

- **One commit per confirmed finding**, each carrying the regression test from step 6 — the test
  first in the same commit, so `git log` shows the failure a reader can reproduce.
- **Parallel fixers only over non-overlapping file sets.** Two subagents in one package produce a
  conflict that costs more to referee than the serial run saved.
- **Not delegated at all**: anything touching the gate chain, a failure direction, one of the four
  hard constraints, or a frozen identifier ([docs/canonical.md](../docs/canonical.md) §1). Those are
  design decisions wearing a bug's clothes, and AGENTS.md already says which one bends — it is
  almost always the design.
- **A finding whose only available fix relaxes an invariant is not a fix.** Record it beside the
  invariant in `docs/modules/` and leave it for its own branch, with its own argument.
- A fix that changes a failure direction updates that package's `docs/modules/` file **in the same
  commit**. The next reader's only defence against a silently inverted predicate is that file.

## 8. Land it

By [new-feature.md](new-feature.md) steps 4 and 5, unchanged: `make ci-full`, `make e2e-ci` and
`make e2e`, `gh pr ready`, the rebase, `make ci-landing`, the force-push, `git merge --ff-only`.

If the sweep touched a parser that reads untrusted input — the whole first lane does by definition:

```bash
make fuzz FUZZ=<target>                 # make ci runs only the seed corpora
```

A parser fix with no fuzz round is a fix for the one input somebody thought of.

Then discard the working files, deliberately:

```bash
rm -rf "$AUD"                           # findings live in git log and docs/modules/, nowhere else
```

`$AUD` is under `.git/` and unversioned on purpose. A findings file that outlives its sweep gets read
as current, and a stale "not reproducible" is indistinguishable from a fresh one.

## Stop condition

**Per shard:** stop when both engines return nothing clearing the concrete-failure-path bar. Do not
re-run a shard hoping for a different answer — two engines disagreeing is signal, the same engine
twice is noise, and a third pass is how a report fills with items nobody can refute.

**Per sweep:** one branch, landed or discarded the same day. Confirmed findings sitting unfixed in an
untracked file are the worst state this loop can end in: the knowledge exists and nothing in the tree
records it.

**Across sweeps:** what was fixed is in `git log`; what was raised and refuted is in the package's
`docs/modules/` file. Neither needs a ledger, and a ledger is the thing that goes stale first.

## When it goes wrong

| Symptom | Meaning | Do |
|---|---|---|
| An output file is empty, exit status 0 | Nothing was written; for codex the answer may only ever have gone to stdout | Read the `-o` file, then `claude-<n>.log` / `codex-<n>.log`. Re-run that one shard alone before blaming the lane |
| Both engines report the same non-bug | The brief described the invariant wrongly | Fix `brief.md` and re-run that lane. Never patch the finding list — the next sweep gets the same answer |
| The report is prose, not finding blocks | The output contract was not the last thing in the prompt | Repeat it verbatim per shard. Models follow the final instruction, not the best one |
| `git status` is dirty after step 4 | A reviewer edited despite the sandbox | Discard, and re-run **every** shard. Line numbers across the whole sweep are now unverifiable |
| A finding says a check is redundant with the one above it | Usually the second of two independent gates | Never collapse a fail-closed path. Two failures becoming one is not a simplification |
| Every finding is `medium`, none reproducible | The engines were asked to be thorough rather than concrete | That is a failed sweep, not a clean tree. Re-cut the shards smaller and demand the failure path |
| Findings cluster in the newest package only | The shards were cut by file listing, not by lane | Re-shard by step 3. Volume-only shards review whatever is largest, which is not whatever is riskiest |
| Two fixers conflict in the same package | Their file sets overlapped | Serialize them; the second rebases on the first. Do not merge the two fixes into one commit |
| `claude -p` hangs with no output | Auth, or a settings prompt with no TTY | Step 1's throwaway call exists for exactly this — run it before assuming the shard is at fault |
| A lane returns nothing, twice, across sweeps | Possibly true, possibly unreviewable | Check the shard actually named files that exist. An empty lane and a mistyped path produce identical reports |
