# runbooks/

**Procedures, not documentation.** Every file here is something you *do*: a numbered sequence where
each step is a command to run or a fact to check, ending in a state you can verify. Open one when you
are about to perform the action, and follow it from the top.

| Runbook | Run it when |
|---|---|
| [new-feature.md](new-feature.md) | Building anything — the worktree, the PR, the commits, the gates, and landing on `main` |
| [nightly-tidy.md](nightly-tidy.md) | The recurring pass: simplify the logic, refactor the shape, make docs and code agree |
| [releasing.md](releasing.md) | Cutting a release, from preflight to checking what actually shipped |
| [security-audit.md](security-audit.md) | Sweeping the tree for security defects — a workflow of parallel finders, adversarial verifiers and one adjudication pass — then dispatching the fixes |

Each has a slash-command wrapper under `.claude/commands/` that does nothing but point here, so
`/new-feature`, `/nightly-tidy`, `/release` and `/security-audit` reach the same text an agent
reading the repo does.
The wrapper holds no procedure of its own — one copy, or the copy an agent happened to open decides
which steps it followed.

## Runbook or doc?

The two answer different questions and rot in different ways, which is why they are separate trees:

| | [runbooks/](.) | [docs/](../docs/) |
|---|---|---|
| Answers | "What do I do, in what order?" | "How does this work, and why is it this way?" |
| Read | While performing the action, at the machine | Before deciding what to change |
| Goes stale when | A command, gate, or tool changes | The code changes |
| Wrong looks like | A step that fails, immediately and loudly | A sentence that is quietly no longer true |

A runbook may cite a doc for the *reason* behind a step, and should — but the reason lives there and
the step lives here. Neither file restates the other; a fact written in both places eventually
disagrees with itself, and a reader has no way to tell which copy is current.

**The rules are not runbooks.** [AGENTS.md](../AGENTS.md) holds what must be true regardless of what
you are doing — the hard constraints, the invariants, the conventions. A runbook executes against
those rules and points at them; it does not restate them and it never relaxes one.

## Adding a runbook

The bar is that the procedure is **performed more than once and gets it wrong when improvised**.
Something done once is a plan, not a runbook, and something with no failure mode is a command line.

- Number the steps, and put the cheap, reversible checks in front of the irreversible one. Say
  explicitly where that line falls — releasing.md's is the tag push, new-feature.md's is the push to
  `main`.
- End with a **"when it goes wrong" table**: symptom, what it means, what to do. That table is the
  reason a runbook beats remembering, and it is the part written from failures actually hit.
- A recurring runbook needs a **stop condition**. Without one it is an instruction to keep changing
  things, which on a linear `main` costs every open worktree a rebase.
- These files are **English only**. They track the tree closely enough that a translation would be a
  second file every change has to remember, and the forgotten copy is indistinguishable from the
  current one — the same reason `docs/` exempts its contributor-facing half.
- Add the row to the table above and a wrapper under `.claude/commands/`.
