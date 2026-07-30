# docs/

## Where to start

| What you want to do | Read this |
|---|---|
| **Use** agenthub: what a server / profile / client is, how to wire them up, how discovery modes differ | [guide.md](guide.md) |
| Understand how the system is carved up and how the processes are laid out | [architecture.md](architecture.md) |
| Learn what each package is responsible for | [architecture.md §3 Core module map](architecture.md#3-core-module-map) |
| Work out how a given flow actually behaves at runtime | [flows.md](flows.md) |
| Get a feel for a package's constraints before changing it | [modules/](modules/) |
| Debug a downstream OAuth server you can't connect to | [modules/oauth.md](modules/oauth.md) |
| Touch the GUI frontend | [modules/gui.md](modules/gui.md) |
| Check whether a name, dependency, or convention is changeable | [canonical.md](canonical.md) |
| Find out what works on Windows today | [windows.md](windows.md) |
| Cut a release | [releasing.md](releasing.md) |

## Files

| File | Contents |
|---|---|
| [guide.md](guide.md) | The user-facing guide: the server / profile / client model, the everyday setup path, profiles and three-state tool selection, the three discovery modes and when each is right, how to verify the wiring, and the surprises that account for most "it stopped working" reports |
| [architecture.md](architecture.md) | Architecture overview: the dual-mode process model, core module map, layering and dependency constraints, what a single call passes through, the three data flows, the two planes of scope, the three lines of defense, on-disk layout |
| [flows.md](flows.md) | Sequence diagrams and failure branches for seven key flows: gateway startup, a lazy call, HITL approval, config writes, config hot reload, OAuth, derived instances |
| [modules/](modules/) | Per-package docs: responsibilities, key types, **invariants and failure directions**. Five documents organized by layer, plus dedicated write-ups for OAuth and the GUI |
| [canonical.md](canonical.md) | Frozen identifiers, package layout, the four dependency constraints, command naming rules, engineering conventions, and every decision record |
| [windows.md](windows.md) | Windows status: what's implemented, **what's unverified**, what's missing, acceptance criteria |
| [releasing.md](releasing.md) | Version numbering, build artifacts, release process |

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
