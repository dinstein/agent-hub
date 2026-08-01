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

## 1. Information architecture: a short task spine, then state inside each page

The sidebar started life as a one-to-one mapping of the CLI command tree — fourteen resource tables
laid out along the domain model. It now has six task destinations: Servers, Catalog, Playground,
Profiles, Clients and Settings. Catalog is deliberately first-class and immediately follows Servers:
the two are the configured and discoverable halves of the same task. Credentials live with the
server that uses them, client bindings live with Clients, and appearance/daemon diagnostics live in
Settings. A resource can still have a direct hash route without taking permanent navigation space.

The daemon state is pinned at the bottom of the shell and an offline daemon also raises a global
banner above the current page. A footer hidden below fourteen links failed at its only job: the
connection state disappeared precisely when every page began failing.

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
| needs-auth | the status cell **becomes an `Authenticate` button** that signs in for real (docs/modules/controlplane.md) |
| checking | after 4 seconds, if the command is `npx`/`uvx`, it changes to **`Installing…`** — reinterpreting a wait as progress |
| error | a one-line distilled error headline, expandable to the full text |
| disabled | gray dot, no text |

This is the single biggest saver of user time: it removes the "read status → figure out what to do →
find the entry point" three-step.

**Semantic colors are reserved for health; accent is reserved for interaction.** Metadata like
transport, source, and profile is always neutral (`ChipTone`'s `neutral`). Green/yellow/red still
answer health only. The indigo accent answers a different, closed question — which navigation item,
primary action or focus target is active — and is never used for a health state. Without that split,
a healthy stdio server would show two unrelated green dots at once and color would stop meaning
anything.

**Health is always rendered, never re-derived.** The level is computed by a pure function in the
daemon; the frontend only displays it. The constants are generated from the `api` package into
`generated/health.ts`, with a golden test watching for three-way drift. A frontend that assembles a
status from connection flags is a second judgment.

**The global switch is a switch, not a verb.** Enabling and disabling used to be a word in a row of
four identically-shaped buttons at the far end of the row, which got the most-used setting on the
page wrong twice: it sat where the eye arrives last, and it named the ACTION rather than the VALUE —
so read as a label, "Disable" marked the servers that were on. It now leads the row.

Two rules on it, both easy to undo by accident:

- **Its "on" color is `--accent` (ink), never `--success`.** A green track puts a second green on a
  row that already carries a green health dot, and the two mean unrelated things — "you switched
  this on" versus "it is actually working". Every other product's switch is green, so this is
  written into `style.css` at the spot someone would go to "fix" it.
- **The position is never set by the click.** `onChange` performs the write and the page repaints
  from the answer, so a refused write, a lost precondition or a cancelled confirmation leaves the
  switch showing what is *stored*. Optimistic flips have to be walked back, and the moment they are
  wrong is exactly the moment the user looks away satisfied — the same reason a row grays out
  instead of vanishing (§5).

### 2.1 The Authenticate button signs in

It used to open a dialog whose whole content was "the GUI has no endpoint that can do this, run
`agenthub auth login` in a terminal" — in an application whose premise is that clients never handle
credentials. The control plane now serves the login
([controlplane.md](controlplane.md#the-one-long-running-exchange-an-interactive-login)); this page
drives it.

- **Nothing is shown for the first moment, on purpose.** Choosing between the device and loopback
  flows needs the authorization server's metadata, so there is genuinely nothing to say yet. That
  state says what it is waiting for instead of spinning over an invented mode the user must unlearn.
- **The page opens the HOST browser**, never this webview. An authorization page rendered inside the
  application is agenthub asking for a provider password in a window agenthub controls: the shape of
  a phishing screen, and it removes the address bar, the lock, and the password manager's refusal to
  fill a wrong origin.
- **The loopback URL is opened once**, tracked by value — opening it per poll would bury the browser
  in a consent screen every 700ms.
- **Closing the window cancels the wait and nothing else**, and does not cancel once the session is
  terminal: asking to abandon something that already succeeded is a question with no right answer.
- **A failure says nothing was stored**, carries the flow's own suggestion, and offers a retry, so
  the user is never left guessing whether the server is now half-authorized.
- **The device code is large, monospaced and letter-spaced.** It is the one string in this
  application a person retypes into a different window, and `O`/`0` and `I`/`l` have to separate at a
  glance rather than after a failed attempt.

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
[../flows.md §3](../flows.md#3-config-writes-five-writers-and-an-optimistic-lock)). On receiving one, the frontend re-fetches
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

## 5. Show the equivalent CLI command next to every action

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

## 6. Explicitly not doing

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
  scrolling past a pile of unrelated content. One page answers one question, and a page with nothing
  left to answer is deleted rather than kept as a heading.
- **Marketing copy stays out of the product UI** — tokens saved is a reasonable metric, but dressing it
  up as a dollar estimate next to a Share button is soliciting the user for reach; and a hardcoded model
  price table is guaranteed to go stale. **A stale dollar figure is worse than no figure at all**: in a
  product built to detect things quietly changing, it would itself be a thing that quietly goes wrong.

---

## 7. File map

| File | Contents |
|---|---|
| `main.ts` | Entry point: routing, sidebar, SSE subscription. **No theme code** — it cannot have any and still work, see below |
| `bridge.ts` | The only seam with the Go side: `Call.ByName(<Go FQN>)` + `Events.On` (no `wails3 generate bindings`), plus `openExternal` — the HOST browser, never this webview |
| `page.ts` | The `Page` contract, `failureBox` / `failureState`, `CONFLICT_MESSAGE`, `noticeSlot` |
| `dom.ts` | Dependency-free DOM construction: `el` / `table` / `emptyState` (three kinds) / `chip` (returns null for 0) / `errorHeadline` / time formatting |
| `ui.ts` | Form widgets: inputs, tri-state selector, pair/lines editors, confirmation dialog, `toggleSwitch` (never optimistic) |
| `types.ts` | TS mirror of the control plane DTOs |
| `generated/health.ts` | **Generated**: Health's Level/AdminState/Action constants, via `go generate ./cmd/agenthub-gui/...` |
| `pages/*.ts` | One page per resource |
| `style.css` | Semantic color variables, focus ring, the three widget classes, light/dark |

Dark mode is applied by an inline bootstrap script in `index.html` — about 20 lines of vanilla JS,
plus two sets of CSS variables. It is inline, and in `index.html` rather than in `main.ts`, for the
one reason that decides everything else about it: the bundle loads too late. A theme applied after
the first frame is a white flash in a desktop window, which reads as a broken app rather than a slow
one, so the only place the decision can be made is before the module graph exists.

It stamps two **attributes**, not a class — nothing in this codebase keys on a theme class, and
`style.css` selects on `:root[data-theme="dark"]`:

| Attribute | Value | Read by |
|---|---|---|
| `data-theme` | the RESOLVED answer, `light` or `dark` | `style.css` |
| `data-theme-mode` | the CHOICE, `light` / `dark` / `system` | the Settings page, so "which of the three is selected" has no second source of truth |

`system` is also the default when nothing is stored. Every storage access is wrapped in `try`/`catch`
— `localStorage` throws in some embedded webview configurations, and a theme preference must never be
what stops the app from starting.
