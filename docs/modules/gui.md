# GUI Frontend

The second of two topic-oriented documents organized around an **external constraint** rather than
around a layer (the other is [oauth.md](oauth.md)). This one covers the reasoning behind the
~10k lines of TypeScript in `cmd/agenthub-gui/frontend/`: **why it looks the way it does, and which
rules must not be casually changed**. The Go-side service body and wiring live in
[controlplane.md](controlplane.md#cmdagenthub-gui).

The rationale for the tech stack (vanilla TS + Vite, `@wailsio/runtime` as the only runtime
dependency, alpha dependencies confined to two files) is in [../canonical.md](../canonical.md) §7,
item 3.

---

## 1. Information architecture: organized by "does this need you?", not by domain model

The sidebar started life as a one-to-one mapping of the CLI command tree — fourteen resource tables
laid out along the domain model. That works for someone who already knows what they are looking for;
for someone who just connected their first server, it demands you learn our object model first.

The current call is: **lists are bucketed by state, not alphabetized**. The server list has three
buckets — needs attention / active / disabled — sorted within each bucket, **empty buckets are not
rendered**, and disabled is collapsed by default (collapse state lives in localStorage). A single row
of overview chips sits at the top (`20 servers · 13 connected · 1 needs attention · 6 disabled`), and
**a chip whose count is zero simply doesn't appear** (`chip()` in `dom.ts` returns `null` for 0, and
`chipRow` drops it). "0 needs attention" is noise, not information.

---

## 2. State is the action

A server row's status cell has five states, and each one is expressed through **three channels at
once** (dot color / text content / text color). Color is never the only channel — that is both an
accessibility requirement and a guard against misreading:

| State | Display |
|---|---|
| connected | **`23 tools`** — not "connected". An informative number displaces a redundant status word |
| needs-auth | the status cell **becomes an `Authenticate` button** |
| checking | after 4 seconds, if the command is `npx`/`uvx`, it changes to **`Installing…`** — reinterpreting a wait as progress |
| error | a one-line distilled error headline, expandable to the full text |
| disabled | gray dot, no text |

This is the single biggest saver of user time: it removes the "read status → figure out what to do →
find the entry point" three-step.

**Semantic colors are reserved for health.** Metadata like transport, source, and profile is always
neutral (`ChipTone`'s `neutral`). Otherwise a healthy stdio server would show two unrelated green
dots at once, and color would stop meaning anything.

**Health is always rendered, never re-derived.** The level is computed by a pure function in the
daemon; the frontend only displays it. The constants are generated from the `api` package into
`generated/health.ts`, with a golden test watching for three-way drift. A frontend that assembles a
status from connection flags is a second judgment.

---

## 3. Errors and empty states

**Distill the error, keep the full text copyable.** Errors returned by the daemon are frequently a
whole stack or an absurdly long URL. `errorHeadline()` in `dom.ts` is a pure function that compresses
it into a one-line headline (`Command or file not found (ENOENT)` /
`Port already in use (127.0.0.1:39541)` / `Authentication required (401)`), with the full text kept
in a scrollable region below and a Copy button in the top right.

**There are three kinds of empty state, not one** (`EmptyKind = "loading" | "failed" | "empty"`):

| Kind | Rendering |
|---|---|
| `loading` | skeleton rows |
| `failed` | with Retry, and **explicitly says "this is not empty"** |
| `empty` | with a next-step CTA |

Disguising "network failure" as "the registry doesn't have this" is the easiest mistake this UI can
make and the hardest to notice — the user goes hunting for a server they know they configured, and
ends up doubting their own memory.

---

## 4. The presentation layer for writes

**Confirmation dialog copy follows a pattern**: the title is a question (`Remove stripe?`), the button
is a verb (`Remove`), and the description spells out **what will not happen** ("credentials stay in
the keychain"). On failure the dialog **stays open**; bulk operations have a threshold (confirm only
above 3); and **global actions are disabled while a filter is active** — otherwise they would touch
rows you cannot see.

**409 conflicts don't overwrite.** Control plane write endpoints carry a `Precondition`, and a
conflict returns 409 plus the current generation (see
[../flows.md §4](../flows.md#4-config-writes-five-writers-and-an-optimistic-lock)). On receiving one, the frontend re-fetches
and reports "configuration was modified elsewhere, refreshed" (`CONFLICT_MESSAGE` in `page.ts`)
rather than writing back the view the user had a few minutes ago.

**No polling after a write.** A write bumps the generation, the watcher emits an event, the control
plane pushes it back over SSE, and the page refreshes from that. Your own writes and everyone else's
travel the same loop, so both behave identically in the UI.

**Credentials are never echoed back.** Input fields are password type and are cleared immediately on
submit, and there is **no "reveal" toggle**. The type returned by the read endpoint has no value field
at all. An agent token's plaintext appears exactly once, in the dialog that reports its creation, with
an explicit note that closing it makes the value unrecoverable.

---

## 5. The presentation layer for HITL

The approval panel is the one part of the GUI that cannot be half-done — and it is all DOM + CSS:

- pinned to the top, **non-blocking** for the rest of the interface (`role="alertdialog"` but
  `aria-modal="false"`) — an approval box that locks the whole window makes people dismiss it just to
  see the context they need
- the subtitle states the mechanism outright: **"3 tool calls held — no decision auto-denies"**.
  fail-closed belongs on the screen, not only in the docs
- the countdown uses both a progress bar and a seconds readout, turning red at ≤20 seconds
- **Esc = deny the oldest pending item** (denial is the safe direction)
- three decision scopes: this call / this session / permanently — this is what keeps approval fatigue
  in check
- **decisions are not optimistically removed**: the row grays out and waits for backend confirmation,
  otherwise it would "flash back" to pending

**The dismiss scope for an isolation alert is a content signature** (a hash of the tool name plus
timestamp), not a boolean. So a tool that was dismissed once and later drifted again **pops back up**.
A persistent count badge in the sidebar backstops discoverability.

**The quiescent card**: when nothing is wrong, a gray line reads "monitoring for tool tampering,
poisoning, and injected output; nothing wrong right now". It costs almost nothing, but **protection
the user cannot see generates no trust**.

---

## 6. Show the equivalent CLI command next to every action

```
[Remove]  ⌘  agenthub server rm stripe
```

This is **differentiation handed to us by a hard constraint**. "The GUI may not have capabilities the
CLI lacks" means every GUI action **necessarily has** an equivalent command — so display it and make
it copyable. That incidentally solves two real needs: power users want to script things, and users
want to paste the operation into a doc or a ticket.

The side effect is that **the GUI becomes a teaching interface for the CLI** — a few clicks and the
user knows what the commands look like.

---

## 7. Explicitly not doing

**Not rewriting in React + shadcn.** The cost, concretely: runtime dependencies go from 1 to roughly
13 direct plus hundreds of transitive ones. For a **security gateway** product that supply-chain
surface is ironic — we detect poisoning and tampering in the UI while stuffing several hundred npm
packages into our own build.

The "looks presentable" part we actually want is 95% delivered by three things: **a set of semantic
color CSS variables, one consistent focus ring, and three CSS classes for button/input/dialog**. None
of the three needs a framework.

Also declined: the full Radix suite (native `<dialog>` already gives modality / Esc / focus trapping),
Tailwind, and `lucide-react` (replaced by compile-time inlining of the two dozen SVGs actually used,
zero runtime dependency).

**Two failures we deliberately did not copy**:

- **No "junk drawer" page** — nine things at nine different levels of abstraction stacked vertically on
  one page, rescued only by default-collapsed sections, so answering "why did that call fail" means
  scrolling past a pile of unrelated content. Audit is split into Overview / Calls / Security.
- **Marketing copy stays out of the product UI** — tokens saved is a reasonable metric, but dressing it
  up as a dollar estimate next to a Share button is soliciting the user for reach; and a hardcoded model
  price table is guaranteed to go stale. **A stale dollar figure is worse than no figure at all**: in a
  product built to detect things quietly changing, it would itself be a thing that quietly goes wrong.

---

## 8. File map

| File | Contents |
|---|---|
| `main.ts` | Entry point: theme bootstrap, routing, sidebar, SSE subscription |
| `bridge.ts` | The only seam with the Go side: `Call.ByName(<Go FQN>)` + `Events.On` (no `wails3 generate bindings`) |
| `page.ts` | The `Page` contract, `failureBox` / `failureState`, `CONFLICT_MESSAGE`, `noticeSlot` |
| `dom.ts` | Dependency-free DOM construction: `el` / `table` / `emptyState` (three kinds) / `chip` (returns null for 0) / `errorHeadline` / time formatting |
| `ui.ts` | Form widgets: inputs, tri-state selector, pair/lines editors, confirmation dialog |
| `types.ts` | TS mirror of the control plane DTOs |
| `generated/health.ts` | **Generated**: Health's Level/AdminState/Action constants, via `go generate ./cmd/agenthub-gui/...` |
| `pages/*.ts` | One page per resource, 17 pages |
| `style.css` | Semantic color variables, focus ring, the three widget classes, light/dark |

Dark mode is applied by an inline bootstrap script in `index.html` that stamps the theme class before
the first frame (to prevent a white flash), with light / dark / system states — about 20 lines of
vanilla JS plus two sets of CSS variables.
