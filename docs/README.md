# docs/

## Where to start

| What you want to do | Read this |
|---|---|
| Know what a client is allowed to reach, and who decided it | [model.md](model.md) |
| **Use** agenthub: how to wire servers, profiles and clients up | [guide.md](guide.md) |
| Understand how the system is carved up and how the processes are laid out | [architecture.md](architecture.md) |
| Learn what each package is responsible for | [architecture.md#the-packages](architecture.md#the-packages) |
| Work out how a given flow actually behaves at runtime | [flows.md](flows.md) |
| Get a feel for a package's constraints before changing it | [subsystems/](subsystems/) |
| Debug a downstream OAuth server you can't connect to | [status/oauth.md](status/oauth.md) |
| Touch the GUI frontend | [subsystems/gui.md](subsystems/gui.md) |
| Check whether a name, dependency, or convention is changeable | [canonical.md](canonical.md) |
| Find out what works on Windows today | [windows.md](windows.md) |
| **Do** one of the standard things — build a feature, run the tidy pass, cut a release | [../.agents/skills/](../.agents/skills/) |

## Files

| File | Contents |
|---|---|
| [model.md](model.md) | The access model in one place: the three nouns, how the layers intersect, the three-state selector, scope resolution, the two planes, the three discovery modes, and what the model deliberately does not have |
| [guide.md](guide.md) | The user-facing guide: the server / profile / client model, the everyday setup path, three-state tool selection, the three discovery modes, how to verify the wiring, which record to open when something broke, and the surprises behind most "it stopped working" reports |
| [architecture.md](architecture.md) | Architecture overview: the dual-mode process model, core module map, layering and dependency constraints, what a single call passes through, the three data flows, the two planes of scope, the two lines of defense, on-disk layout |
| [flows.md](flows.md) | Sequence diagrams and failure branches for seven key flows: gateway startup, a lazy call, config writes, config hot reload, OAuth, derived instances, the call ledger lifecycle |
| [subsystems/](subsystems/) | Per-seam docs, each one file: what the packages in it must not do, and which way each failure falls |
| [canonical.md](canonical.md) | Frozen identifiers, package layout, the four dependency constraints, command naming rules, engineering conventions, and every decision record |
| [windows.md](windows.md) | Windows status: what's implemented, **what's unverified**, what's missing, acceptance criteria |
| [mcp-2026-07-28.md](mcp-2026-07-28.md) | One protocol revision: what the two faces do about it today, and what is deliberately still absent. Cited by section number from the code implementing it |

## Not here: executable workflows

Anything you *perform* lives in [../.agents/skills/](../.agents/skills/), not in this directory. The
split is by what a file answers, and the two rot differently: a doc explains how the system works and
goes quietly wrong when the code changes; a skill workflow is a numbered sequence executed at the
machine, and goes loudly wrong — a step fails — when a command or a gate changes. Kept together, a
reader after the steps skims explanation, and a reader after the reason finds an ordered list that
never gives one.

A skill may cite a document here for the reason behind a step, and should. The reason stays here;
the step stays there.

## Three layers — don't read them interchangeably

These docs are deliberately split into three layers. Write things into the layer they belong to, and read the layer that answers your question:

| Layer | Answers | Where |
|---|---|---|
| **How the system works** | How processes are laid out, how calls flow, where data goes | `architecture.md` + `flows.md` |
| **How to change this package** | Key types, invariants, failure directions | `modules/` |
| **Whether this convention can move** | Frozen identifiers, dependency directions, naming rules, decision rationale | `canonical.md` |

Each fact belongs in exactly one layer. Content duplicated across layers will eventually contradict itself, and readers have no way to tell which copy is current.

## Rules for writing these docs

- **Read the source first.** The value of these docs is that they match the code; writing from memory or from an old design draft devalues them immediately.
- **A sentence earns its place by carrying a reason, not by restating the code.** Don't write file listings, exported signatures, or step-by-step narration of straightforward control flow — `ls`, `go doc`, and the code itself are faster and never go stale. Write down the *why*: rejected alternatives, traps that were actually hit, which way a predicate fails.
- **Invariants first.** The most valuable thing to write down isn't "what this package does" but "what must not be touched, and which way failures fall" — fail-open or fail-closed, ordering invariants, cross-process discipline.
- **Capability exists ≠ wired up.** Where a package is functionally complete but the assembly layer hasn't connected it, say so explicitly at that spot. "Thought it was in effect but it wasn't" is far more dangerous than "known to be missing."
- **A gap goes in the `modules/` doc of the package that owns it**, not into a list of its own — under "current assembly status", or beside the invariant it bends. The bar for writing one down is "you can point to a specific location in the code"; once it's fixed, the same paragraph becomes the description of reality. A gap kept next to its code is read by whoever touches that code, which is the one thing a central backlog could never manage.
- If you change an architectural convention (package name, command name, dependency direction, frozen identifier), update `canonical.md` in the same change.
- **Only the product surface is translated.** The root README, `guide.md` and `architecture.md` have zh-CN counterparts under `docs/zh-CN/`; everything else here is English only. The line is drawn by what a document tracks: these files move whenever the code does, so a mirror is a second file every behaviour change has to remember, and the forgotten copy is indistinguishable from the current one. The exempt set is declared in `contributorOnlyDocs` (`test/buildrules/translations_test.go`), which fails both on an English document with no translation and on a leftover translation of an exempted one.
