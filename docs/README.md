# docs/

> **Answers** which file answers your question, and where a new fact belongs.
> **Not here** the steps of a workflow you are about to run → [../.agents/skills/](../.agents/skills/).
> **Kept true by** `test/buildrules`, which resolves every reference into this tree.

## Where to start

| What you want | Read |
|---|---|
| To **use** agenthub: wire servers, profiles and clients up | [guide.md](guide.md) |
| What a client is allowed to reach, and who decided it | [model.md](model.md) |
| How the system is carved into processes and packages | [architecture.md](architecture.md) |
| How a flow behaves at runtime, and which way it falls | [flows.md](flows.md) |
| What a package must not do, before you change it | [subsystems/](subsystems/) |
| Whether a name, a dependency direction or a convention may move | [conventions.md](conventions.md) |
| Why a settled question was settled that way | [decisions/](decisions/) |
| What works on Windows, or which OAuth providers connect | [status/](status/) |
| To **do** one of the standard workflows | [../.agents/skills/](../.agents/skills/) |

## The tree

```
docs/
  README.md          this map, and the rules for writing here
  model.md           what a client may reach, and who decided it     [zh-CN]
  guide.md           using agenthub                                  [zh-CN]
  architecture.md    processes, packages, what a call passes through [zh-CN]
  flows.md           seven runtime sequences and their failure branches
  conventions.md     frozen names, dependency directions, engineering rules
  decisions/         one settled question per file, plus the ruling-id registry
  subsystems/        one file per seam: invariants and failure directions
  status/            snapshots: Windows, one protocol revision, OAuth providers
```

## Five kinds of content, and what keeps each one true

Each fact belongs to exactly one of these. Content duplicated across two of them will eventually
contradict itself, and readers have no way to tell which copy is current.

| Kind | Where | Kept true by |
|---|---|---|
| **Concepts** — the access model, the vocabulary | `model.md` | rarely moves; a change here is an architectural change |
| **Decisions** — what was settled, and why | `decisions/` | append-only. A superseded decision keeps its file and says so |
| **Behaviour** — how it works, seam by seam | `architecture.md`, `flows.md`, `subsystems/` | review, plus the tests each file names in its own header |
| **Rules** — what may not change | `conventions.md` | `internal/depguardtest`, `internal/cli/tree_test.go`, `test/buildrules` |
| **Snapshots** — the state of one platform or one spec | `status/` | nothing automatic; re-read them when that state moves |

## Rules for writing here

- **Every document opens with three lines**: what it **answers**, what it does **not** (with a pointer),
  and what **keeps it true**. The third line is the useful one — a section you cannot attach to a test, a
  generator or a named review habit is a section that will rot, and writing that down is how you notice.
- **Read the source first.** The value of these documents is that they match the code; writing from
  memory or from an old design draft devalues them immediately.
- **A sentence earns its place by carrying a reason, not by restating the code.** No file listings, no
  exported signatures, no narration of straightforward control flow — `ls`, `go doc` and the code are
  faster and never go stale. Write the *why*: rejected alternatives, traps that were actually hit, which
  way a predicate fails.
- **Invariants first.** The most valuable thing to write down is not "what this package does" but "what
  must not be touched, and which way failures fall".
- **Capability exists ≠ wired up.** Where a package is complete but nothing calls it, say so at that
  spot. "Thought it was in effect but it wasn't" is far more dangerous than "known to be missing".
- **A gap goes in the `subsystems/` file of the package that owns it**, beside the invariant it bends,
  not into a list of its own. The bar for writing one down is that you can point to a specific place in
  the code; once it is fixed, the same paragraph becomes the description of reality.
- **Cite a section by its anchor, never by a number.** A number is a position, so inserting a section
  silently moves every later citation onto its neighbour. `TestDocReferencesResolve` checks the anchors;
  nothing can check a number's meaning.
- **Diagrams carry structure, prose carries reasons.** A mermaid diagram for a topology, a sequence or a
  ladder; a table for an enumeration; a paragraph for why the shape is what it is. Do not narrate a
  diagram in prose underneath it.
- **Only the product surface is translated.** `README.md`, `guide.md`, `model.md` and `architecture.md`
  have zh-CN counterparts; everything else is English only. The line is drawn by what a document tracks:
  the rest moves whenever the code does, so a mirror is a second file every behaviour change has to
  remember, and the forgotten copy is indistinguishable from the current one. The exempt set is declared
  in `contributorOnlyDocs` (`test/buildrules/translations_test.go`), which fails both on an English
  document with no translation and on a leftover translation of an exempted one.

## Not here: executable workflows

Anything you *perform* lives in [../.agents/skills/](../.agents/skills/). The split is by what a file
answers, and the two rot differently: a document explains how the system works and goes quietly wrong
when the code changes; a skill is a numbered sequence executed at the machine, and goes loudly wrong — a
step fails — when a command or a gate changes.

A skill may cite a document here for the reason behind a step, and should. The reason stays here; the
step stays there.
