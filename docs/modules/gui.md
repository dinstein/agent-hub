# GUI Frontend

The second of two topic-oriented documents organized around an **external constraint** rather than a
layer (the other is [oauth.md](oauth.md)). It covers the reasoning behind the TypeScript in
`cmd/agenthub-gui/frontend/`: **why it is shaped the way it is, and which rules must not be casually
changed**. The Go-side service body and wiring live in
[controlplane.md](controlplane.md#cmdagenthub-gui); the tech-stack rationale (vanilla TS + Vite,
`@wailsio/runtime` as the only runtime dependency) is in [../canonical.md](../canonical.md) §7 item 3.

---

## 1. Information architecture

Nine destinations in three groups: **Core** (Servers, Catalog, Playground), **Access** (Profiles,
Clients), **System** (Calls, Events, Logs, Settings). The sidebar began as a one-to-one mapping of the CLI
command tree — fourteen resource tables laid out along the domain model — and the reduction is the
point: a resource can still have a hash route without taking permanent navigation space. Catalog
follows Servers because the two are the configured and discoverable halves of one task. Credentials
are configured from the server that needs them, not from a global vault page; there is deliberately
**no global Secrets destination**. Tokens, Scope, Sessions and Skills keep routes but stay out of the
navigation until they own a task not already expressed by server, profile and client configuration.

**Calls, Events and Logs are three destinations, not one.** They answer different questions — Calls
reads the ledger (what a client ASKED agenthub for and where it went), Events reads the control-plane
stream (what HAPPENED to a server, gateway or daemon), Logs reads the processes' own prose. Merging
them would put a payload-bearing record whose bodies are opt-in and encrypted beside a default-on
state change and a line of free text in one list, and a reader would have no way to tell which
guarantees applied to which row. Calls was called "Activity" while it was the only one of them; the
name went because "activity" described any of the three. The route is still `#/activity`.

Logs arrived last and only after `GET /v1/logs` existed. The files are readable from a terminal, so
the CLI never needed a route; a window cannot read a file, which meant the destination could not exist
until the daemon served the stream — and until it did, the half of the record that explains a
downstream failure was terminal-only, because the daemon never dials a downstream and the gateway that
does writes to its own file.

Daemon state is pinned at the bottom of the shell, and an offline daemon also raises a global banner.
A footer hidden below fourteen links failed at its only job: the connection state disappeared
precisely when every page began failing.

**Each route renders into its own disposable DOM host**, removed before the next page mounts, so a
slow request or rejected render from the page being left can only write into a detached tree. The
router also grades render failures by mount generation. This is the shell's backstop, not a promise
that every page remembered its own post-await guard.

### 1.1 The Servers page owns its own status

Enabled rows are checked through the page's **own short-lived handshake**, ten at a time (bounded
because a stdio probe spawns a process, not because the daemon has a limit). Another client's gateway
report never supplies or overwrites a row status. Settled outcomes live in a process-local cache
across route changes, so returning paints the last page-owned observation immediately while a new
check runs silently. Only a row with no prior observation, or an explicit Refresh, shows `Checking…`.
Runtime-only events may cause a configuration re-read but do not retrigger probes; a newer registry
revision does, because an external editor may change an endpoint without changing its id.

**The registry read is the only thing the first paint waits on.** Credential metadata is fetched
beside the fleet probe and repaints when it lands, never in front of the list — reading it costs one
keychain lookup per stored entry, and on macOS each of those is a `security` subprocess the secrets
chain runs under a single lock, so awaiting it put a subprocess per credential between the reader and
a list containing none of that answer. What makes deferring it safe is `rowSignature`, which carries
the stored credential as a **boolean, unconditionally**: a late answer then rebuilds exactly the rows
whose menu gains or loses **Log out OAuth**. The full status stays gated on the row being expanded,
because the daemon computes `expires_in` per request and an unconditional entry would rebuild every
row on every read for a countdown only the open panel draws. Overlapping reads are ordered by epoch —
a page-entry refresh and the read a completed login runs are both legitimate, and the older must not
restore what the newer replaced. Anything added here on the strength of "it is only one more call"
belongs on the same side of the paint.

**A row moves only when the user changes configuration, never because of a probe result.** Grouping
follows configuration, which the registry answers with certainty the moment it is read: `Enabled` and
`Disabled`, both folding sections with a count, both ordered by id — the order `server ls` prints.
Enabled opens by default and Disabled does not; each remembers its own fold in localStorage. **Both
sections are always on the page, empty or not**: hiding the empty one made the page's structure
depend on its contents, so a first-run window showed neither heading, and switching the last server
of a group off made a heading vanish rather than a row move. An empty group shows its count and one
sentence distinguishing "there are none" from "this view is filtered". The `<details>` element is
built once and refilled, never rebuilt — a fresh one would lose the open state the user set. Rows are
keyed by server id and reconciled, so an unchanged row keeps its hover, focus and open disclosure
while the row beside it is still checking.

This **reverses** the earlier "lists are bucketed by state, not alphabetized", and the reason is worth
keeping: state is asynchronous and at startup unknown, so letting it decide position made every
enabled row begin life in `needs attention` — an unchecked row reports `level=degraded`, the absence
of an answer rather than a fault — and migrate as its handshake settled, re-sorting the table under
the cursor once per server. State already has three channels inside the row.

What the attention bucket provided was "which row do I look at first". That is now the **attention
chip, which filters**. The chip row describes the **whole fleet**, never the filtered view, and **a
chip whose count is zero does not appear** (`chip()` returns `null` for 0) — "0 needs attention" is
noise. Two exceptions close traps: the attention chip renders at zero **while pressed** and survives a
re-probe that withholds the other counts, because it is the control that turns the filter off. While
the fleet is settling, the verdict chips are absent rather than partial — a count that climbs one
probe at a time is motion that says nothing, since the number is only an answer once every question
has been asked.

### 1.2 Geometry rules that are easy to undo

The window opens at **1240 × 800** with a **900 × 620** minimum — the smallest size at which
navigation, a two-column form and its actions read without accidental horizontal scrolling.
Responsive rules may stack information inside that boundary but never hide the navigation.

- The document reserves a stable vertical-scrollbar gutter, so expanding a cached server detail
  cannot move the sidebar or change the content column width.
- **A modal freezes both the document and the shell's `.content` scroll container**, reference-counted
  because a confirmation may sit above an editor; closing only the confirmation must leave the editor
  locked. A wheel gesture at the modal's edge must not move the dimmed page behind it.
- **Buttons never translate or scale on hover or press.** Moving a compact control by one pixel makes
  it jump against aligned neighbours; scaling softens its label in the webview. An asynchronous
  action keeps its label width and enters the shared busy treatment.
- The native window title is blank. The sidebar is the single visible product identity, stamped with
  the release-plus-commit version **of this GUI process**, not of the connected daemon — the two may
  legitimately be different builds.

### 1.3 The three observability pages are one design

**Calls, Events and Logs answer three questions about one installation**, and somebody comparing them
switches between the three inside a minute. So the shared parts are shared in code
(`pages/observe.ts`): the time range and its options, the filter bar's position and sizing, rows
**newest first**, and one pager.

**Owed: two of the three share that code.** `pages/events.ts` and `pages/logs.ts` import from
`observe.ts`; the Calls page (`pages/activity.ts`) imports none of it and carries its own range list,
its own filter fields, its own toolbar and its own cursor walk. The duplication has already produced
the divergence this section exists to prevent, in the one place a reader is told it cannot happen:

- **The option sets differ.** `observe.RANGES` ends in **Everything** (no lower bound);
  `activity.ts`'s list ends in **30 days**, and its `sinceMillis` has no unbounded branch — so a call
  older than thirty days cannot be reached from the page at all, while an event of any age can.
- **The same range is labelled twice.** "Last hour" / "Last 24 hours" / "Last 7 days" against
  "1 hour" / "24 hours" / "7 days": two vocabularies for one idea, in front of the person switching
  between them inside that minute.

Moving Calls onto `observe.ts` changes what the page renders, so it carries a visual decision — which
list wins, and whether Calls gains **Everything** — rather than being a tidy.

**Every selector goes to the DAEMON, never to the rendered rows.** A page is one read deep, so
filtering in the browser searches only that page and answers "nothing matches" for something that is
merely older than it — and the two are indistinguishable to whoever is reading. This is why the
control plane grew `class` on the event list and `/v1/logs` at all: the Events page used to assemble
"only what went wrong" from its own tone table, and the process logs had no endpoint, so the GUI
could not show the half of the record that explains a downstream failure.

**Paging is by CURSOR, not by offset**, and the cursor names the last row served
(`internal/ctlapi/pagecursor.go`). With an offset, ten records arriving between two requests make
page two repeat ten rows it already showed — a failure a reader blames on the hub rather than on the
pager. A cursor cannot: a fresh record is newer than every cursor, so it can only appear on page one.

**Every filter change returns to page one.** A cursor taken under one filter names a row the next
filter may not contain, and paging on from it would skip records without saying so.

**Each of the three carries a Refresh in the header**, and it re-reads the CURRENT page rather than
jumping to the newest one: somebody two pages back is asking whether a line arrived in the window
they are reading, and moving them would lose the position that question is about.

**One identity per column, and never two stacked in one cell.** The daemon separates the scope, the
server and the caller; a table that folds them back together makes the row unreadable in a way the
reader cannot detect. Events showed a Subject that was a server *or* a client *or* a session,
whichever came first, with the scope underneath — so a name could not be traced to a kind, and a
record carrying both a server and a client showed only the server. Logs was worse than crowded: its
`client || server` **lost** the server on every gateway line, which names both. Both tables now run
Time, PID, then one column per join key, with the shapeless column (Detail, Message) last so the
aligned ones stay aligned. The Events header list ships beside the shared renderer, because a caller
holding its own copy mislabels every cell under it the day the renderer moves.

**The three differ only where their content does.** Calls has a drawer because a call has payloads;
Events colours rows because it has a closed vocabulary to colour by; Logs has neither and is
deliberately the plainest of the three — its whole job is to filter prose, order it and get out of
the way. Logs also takes free text for client and server rather than facets: a process log has no
bounded set of subjects, and a dropdown built from one page would offer only what that page held.
The inputs carry a `<datalist>` of the names seen in a bounded recent window, and that is a
suggestion and not a facet: a missing suggestion costs a keystroke, where a missing option would cost
the filter. This is why the window may be narrower than the range here while the Events facet read
must span it — a dropdown has to be complete, a hint does not.

### 1.4 Per-page rules worth keeping

**Clients** keeps **file capability** and **connection state** separate: `writable`/`read-only`
describes whether AgentHub may rewrite a file and says nothing about whether that file already
contains the gateway. Connection state comes only from the per-client Inspect endpoint. Inspections
stay **sequential** because protected files may raise a macOS privacy prompt, and concurrent prompts
would obscure which client requested access. Only a connected row offers Disconnect. Read-only
formats return authoritative manual setup instructions rather than opening a form the daemon cannot
apply.

**Profiles** begins with the same virtual `(default)` row the CLI prints — explanatory state, not an
object in `profiles[]`. A dangling active profile shows as a broken reference with an empty effective
scope, preserving the runtime's fail-closed behaviour. Creating or editing opens a focused modal with
its own Save / Cancel boundary; direct state changes (making a profile the default) stay direct
actions, and deletion keeps its confirmation. The GUI offers no first-class block-all member choice,
though profiles written with an explicit empty set are described accurately and must be changed
before the editor saves. Per-server tool selectors keep all three states, because blocking every tool
is a useful narrowing inside an otherwise participating server.

**The server editor is transport-shaped**, not a union of every field: `stdio` shows the local
process contract, `http`/`sse` the remote endpoint contract, with Enabled, OAuth hints and instancing
shared. `fieldset.group[hidden]` is an explicit CSS rule — the group's authored `display:flex` would
otherwise override the browser's default `[hidden]` and put Command and URL on screen together even
though the collected entry correctly accepted only one.

**Playground** omits a blank optional field rather than encoding it as an empty value, and keeps the
raw daemon result in a separate disclosure because tool content and transport metadata answer
different questions. It asks the self-test for **256 KiB** of output (`max_text_bytes`) against a
daemon default of 2 KiB sized for "does this connect": this page exists to show what the tool said,
that cut is final — nothing retains a remainder, unlike the same call through the gateway — and a
JSON answer cut mid-object stops parsing, which silently withdraws the Pretty view from exactly the
results large enough to want one. Whether a cut happened is read from the `truncated` FIELD, never
from the trailer in the text, and it is said **only when it happened**: the standing caveat this
replaced was on screen for every result, which is how a warning gets skipped.

**Calls** is one page with three views (Calls, Insights, Ledger). Its filters are applied **on the
daemon before paging**, so they never describe only the visible slice, and the Tool dropdown narrows
from range-wide statistics rather than the current page. The collection endpoint returns metadata only
and **never exposes payload references**; the detail drawer loads decrypted previews immediately, with
no second "decrypt" ritual, says that this is a local decrypted preview, and drops those strings when
it closes. Pausing capture from Ledger never deletes history or keys.

**Events** reads `GET /v1/events/log` — which exists because `cmd/agenthub-gui` may not import
`internal/*`, so where the CLI reads `events.jsonl` directly and works offline, the GUI goes through
the daemon like every other page. Rows are coloured from `kind` rather than from any text, legitimate
only because that vocabulary is CLOSED ([foundation.md](foundation.md)); **a kind this build does not
know renders neutral rather than being hidden**, because a frontend older than its daemon must not
silently drop records. The SSE `servers` topic is the re-read TRIGGER and never the data.

**Every Events selector is applied by the DAEMON, not to the rendered rows, and that is the rule to
keep.** The read is bounded, so narrowing in the browser would search only the newest records and
answer "nothing matches" for something merely older than the window — two facts a reader cannot tell
apart, and the wrong one sends them hunting a fault that never happened. Dropdown options come from
an UNFILTERED read of the same range, because deriving them from filtered rows leaves a chosen server
as the only option in its own dropdown with no way back. The kind list leads with "Problems only",
derived from the tone map and intersected with the kinds actually present — a hardcoded set that ran
ahead of the daemon would reject the whole request instead of returning fewer rows.

The same rows appear inside a server's detail panel from the same renderer: the health badge is a
value, the timeline under it is the sequence that produced it. It loads per server and only once
expanded, and is dropped alongside the cached self-test so a refresh never pairs a fresh badge with
stale history. It keeps that shared order — **newest first, like every other list here** — and shows
ten rows, which is what explains a badge without becoming the Events page; the same number bounds the
read, so the panel never carries records it will not draw.

### 1.5 The window is not the application

Closing the window used to end the process — and with it, on the ordinary path, the daemon that
process had started. A GUI other programs depend on cannot have "tidy the desktop" and "cut off every
connected client" behind one button, so the close button **hides the window** and the application
keeps running in the tray.

**The tray is a readout, a set of destinations, and one lifecycle action. It carries no registry write
of any kind**: a menu has no confirmation surface and nowhere to render a 409, so a mis-click would
change governance configuration with the refusal having nowhere to go. The one action offered is
**starting** a hub that is not running, which can only help; stopping or restarting a running one cuts
off every client mid-session and is not in the menu.

Bucketing comes from the same `Health` contract the Servers page uses — a tray that re-derived status
from connection flags would be the second opinion [controlplane.md](controlplane.md) forbids. Server
rows are capped at ten, sorted worst-first so the cap drops what nobody needs to see, and the menu
says how many it dropped.

The icon is the whole feature for anyone not looking at the window: three clients around a **hollow**
hub is no daemon, the same mark **filled** is serving, and a badge on its shoulder appears when an
enabled server is unhealthy. It is the application icon reduced — three callers rather than six,
detached rather than joined by spokes, because at 22 points six nodes smudge and a spoke fuses node,
arm and core into one blob. **Which three is not free**: `build/darwin/icon.svg` draws exactly these
three solid, so the reduction is subtraction rather than a second drawing of the same idea. It is
deliberately **not a ring** — the other local MCP hubs already put one in the status area. Two
regression tests hold shapes that were got wrong once: the hollow state is an **outline, not an
absence** (`TestTrayIconOfflineKeepsTheHubVisible`), and the glyph is drawn one `iconOpticalShift`
below the box's true centre, because one node above the hub and two below leaves the bounding box
short at the bottom (`TestTrayIconSitsInTheMiddleOfItsBox`, which measures the painted box rather than
trusting the constants, so a new shape cannot read them and ignore the shift).

Three decisions are load-bearing:

- **The first close asks.** Vanishing silently into the status area is the standard complaint about
  tray applications, and here what keeps running is a hub. The dialog is also the "I meant quit"
  escape, and the button pressed becomes what the close button does from then on (changeable in
  Settings). Dismissing it persists nothing and asks again.
- **No tray means the close button still quits.** Hiding into a status area that is not there leaves a
  running process with **no reachable surface at all**. A window that closes when you asked it to
  minimise is a surprise; a hub that cannot be quit is a trap. The frontend's copy of "is there a
  tray" is display state only; the close path reads the assembly's own flag.
- **Quit says what it costs.** Once the close button stops quitting, Quit is the only path that
  reaches the shutdown stopping a daemon this GUI started, so the item spells that out when it
  applies (`Quit AgentHub (stops the hub)`).

**A menu callback runs off the main thread.** Wails hops for you on window methods, `App.Quit` and the
clipboard, but **not** on `App.Show`/`App.Hide` — and the asymmetry is invisible at the call site.
AppKit traps a cross-thread `[NSApp unhide:]`, Wails catches the panic and calls `os.Exit(1)`, so the
symptom is a tray item that quits the application with no crash report. Anything new in this menu that
touches a native API goes through `application.InvokeSync` unless the call is known to hop already.

**Platforms.** macOS and Windows drive a tray. Linux deliberately does not: Wails registers the icon
over dbus `StatusNotifierItem`, and a desktop with no `StatusNotifierHost` — a default GNOME session,
for one — accepts the registration and then shows nothing. Windows compiles and vets but is
unverified on real hardware ([../windows.md](../windows.md)).

Deliberately not done, each for its own reason: **launch at login** (three platform mechanisms plus
their uninstall residue), **notifications when a server drops** (a new dependency, a permission prompt
and a debounce policy), and a **menu-bar-only mode** (`ActivationPolicy` accessory), which would make
the tray the only way back into an application that is not a menu-bar utility.

### 1.6 The application runs the hub

The application is the only thing that starts a hub (`--headless` aside), which fixes three rules.

**Ownership is a process handle, not a memory.** `Hub.proc` is an `api.Supervised` — the hub this
process is running — and holding it *is* the claim that licenses stopping it. The bool it replaced was
written from "did my dial start one": a fact about a past call, which outlived the daemon it
described. A **transport failure no longer disowns anything**: the connection is gone, the process is
not. What ends ownership is the process ending, which the supervisor sees directly.

**A hub that is already answering is used, never adopted.** Every connect dials first. A headless hub,
or one belonging to another AgentHub window, answers there and every page works against it — but
`proc` stays nil, so quitting leaves it running. That is what makes a second launch harmless and why
there is no single-instance lock: the danger a lock would prevent is the second window stopping the
first one's hub, and ownership already refuses that. Settings says which case applies (`Lifetime`).

**A hub that falls over is started again**, on a doubling backoff, **giving up after five consecutive
failures** (`restartLimit`): a hub that cannot start will not start on the sixth attempt, and a loop
that keeps trying spawns a process per interval and buries the first failure — the one that says why —
under thousands of identical ones. Giving up leaves the offline status and its error on screen;
`Connect` retries on demand. A run lasting a minute (`healthyRun`) counts as healthy and resets the
ladder, so a hub that dies once a day never exhausts a counter nothing clears.

The close preference lives in `localStorage`, like the theme and for the same reason: it is a property
of this window on this machine. The Go side holds a runtime copy because the close arrives natively;
the frontend pushes it at startup, and every change is announced back as an event the frontend
persists without answering. The announcement is unconditional rather than tray-only because the
preference has **two** surfaces — the tray checkbox and the Settings switch — and each has to learn
about the other's change.

---

## 2. State is the action

A server row's status cell expresses each state through **three channels at once** (dot colour, text
content, text colour). Colour is never the only channel — an accessibility requirement and a guard
against misreading.

| State | Display |
|---|---|
| connected | green dot, **Connected**; `23 tools` in the following aligned outcome column |
| needs-auth | yellow dot, **Authentication required**; the outcome column becomes `Authenticate` and signs in for real |
| needs-secret | yellow dot, **API key required** / **Secret required**; the outcome column opens the guided write-only form |
| checking | after 4 seconds, if the command is `npx`/`uvx`, becomes **`Installing…`** — reinterpreting a wait as progress |
| error | a one-line distilled headline, expandable to the full text |
| disabled | gray dot, no text |

This removes the "read status → figure out what to do → find the entry point" three-step, and is the
single biggest saver of user time here.

**Status classification never parses raw connection flags or error prose.** The ordinary Health
contract is computed by the daemon's pure function; the Servers page deliberately replaces it for
enabled rows with the typed outcome of its own self-test. The shared level/action constants are
generated from the `api` package into `generated/health.ts`, with a golden test watching for
three-way drift. `needs-auth` arrives as a typed authentication error (`E_AUTH_REQUIRED`, plus the
mixed-version `E_AUTH_FAILED` spelling) — never by testing whether prose contains `401` — and uses
the warning spine, not the red connection-failure spine, because missing authorization is an expected
setup state rather than evidence the endpoint is broken.

Three ordering rules follow from that, each easy to break:

- **The authentication observation is sticky** across route changes for the life of the process.
  Gateway reports cannot touch it and a later generic probe failure cannot downgrade it to
  `connection error`; only this page's own successful handshake clears it. It is never persisted.
- **Only the latest page-owned self-test for one server may settle its presentation.** Starting a
  newer test cancels the older wait, and that cancellation is neutral — it must not become a
  connection-failure notice or a failed Test dialog.
- **A write is never rolled back by the probe that follows it.** Entering the page, creating or
  editing an enabled server, and switching one on all run the same handshake; the write has already
  succeeded, and the probe only repaints the row.

**Semantic colours are reserved for health; accent is reserved for interaction.** Metadata like
transport, source and profile is always neutral. Green/yellow/red answer health only; the indigo
accent answers which navigation item, primary action or focus target is active, and is never a health
state. Without that split a healthy stdio server would show two unrelated green dots.

**`.brand-mark` is the one element that does not follow the theme, on purpose.** It is the application
icon at 32px, so its colours are literals lifted from `build/darwin/icon.svg` rather than tokens — a
themed chip would put a different mark in the sidebar from the one in the Dock in at least one theme.
It is also **filled** where `.nav-icon` is stroked, because a logo is not a navigation item.

**The global switch is a switch, not a verb.** Enabling used to be a word in a row of four
identically-shaped buttons at the far end of the row, which got the most-used setting on the page
wrong twice: it sat where the eye arrives last, and it named the ACTION rather than the VALUE — so
read as a label, "Disable" marked the servers that were on. It now leads the row, with two rules:

- **Its "on" colour is `--accent` (ink), never `--success`.** A green track puts a second green on a
  row that already carries a green health dot, and the two mean unrelated things — "you switched this
  on" versus "it is actually working". Every other product's switch is green, so this is written into
  `style.css` at the spot someone would go to "fix" it.
- **The position is never set by the click.** `onChange` performs the write and the page repaints from
  the answer, so a refused write leaves the switch showing what is *stored*. Both directions write
  immediately: disabling is a reversible setting change that keeps the definition, credentials and
  profile rules, so interrupting it with a destructive-action dialog trains the user to dismiss
  confirmations without reading them.

**The disclosure and the write surface are separate targets.** The row summary is one
keyboard-focusable disclosure that reveals the latest cached self-test and **never touches the
downstream**: the self-test it shows is the last one that settled, and only Refresh and Test start a
handshake. Its own reads are control-plane reads of things the dashboard payload does not carry — the
stored definition behind its first line, and the timeline under the badge — each once per server, per
expansion. Editing is an always-visible Edit button, so a click whose affordance says "show me more"
cannot open a write surface. The panel leads with the **endpoint**: the URL for `http`/`sse`, the
whole spawn command for `stdio`, because the id says what a server was named rather than what it
reaches, and reading that used to mean opening the editor — a write surface — to answer a question
about a running row. The enable switch and the Test / Edit controls never bubble into the disclosure.
Manage secrets, Remove and OAuth Log out sit in the overflow menu so they stay available without
painting every healthy row as a warning.

**A diagnostic string must never participate in the action column's width calculation.** That column
may hold only the distilled status or direct health action plus row controls; daemon detail, HTTP
responses and recovery instructions live behind a disclosure in the record body. One verbose failure
would otherwise squeeze every server's identity and make the list unreadable at the normal width.

Cached OAuth metadata in the detail covers state, expiry, issuer, scopes and whether a refresh token
exists. **It never includes token values.** Log out OAuth confirms that this deletes the local
credential but does not revoke it at the provider, then re-runs the handshake so the row returns to
Authenticate rather than keeping a stale connected result.

### 2.1 The Authenticate button signs in

It used to open a dialog whose whole content was "run `agenthub auth login` in a terminal" — in an
application whose premise is that clients never handle credentials. The control plane now serves the
login ([controlplane.md](controlplane.md#the-one-long-running-exchange-an-interactive-login)); this
page drives it.

- **Nothing is shown for the first moment, on purpose.** Choosing between the device and loopback
  flows needs the authorization server's metadata, so there is genuinely nothing to say yet. That
  state says what it is waiting for instead of spinning over an invented mode the user must unlearn.
- **The page opens the HOST browser**, never this webview. An authorization page rendered inside the
  application is agenthub asking for a provider password in a window agenthub controls: the shape of a
  phishing screen, and it removes the address bar, the lock, and the password manager's refusal to
  fill a wrong origin.
- **The loopback URL is opened once**, tracked by value — opening it per poll would bury the browser
  in a consent screen every 700ms.
- **Closing the window cancels the wait and nothing else**, and does not cancel once the session is
  terminal: asking to abandon something that already succeeded is a question with no right answer.
- **A failure says nothing was stored**, carries the flow's own suggestion, and offers a retry.
- **The device code is large, monospaced and letter-spaced.** It is the one string here a person
  retypes into another window, and `O`/`0` and `I`/`l` must separate at a glance.

The row status, the post-create probe and the Test dialog all call one `login(id)` function; three
entry points do not mean three OAuth implementations.

An unresolved placeholder follows the parallel path: `E_SECRET_REQUIRED` carries safe key names, the
row becomes **Add API key** or **Set secret**, and that server's secret manager opens with the key
locked to the typed result. That guided path is deliberately **one field**: the header and neutral key
chip carry the context, the only editable control is the write-only value, and scope stays behind an
Advanced disclosure. It does not load the server's wider credential inventory before accepting the
required value. Saving closes the modal and retests exactly that server; the value is cleared before
the write awaits and never comes back over a read surface.

---

## 3. Errors and empty states

**Distill the error, keep the full text copyable.** Daemon errors are frequently a whole stack or an
absurdly long URL. `errorHeadline()` in `dom.ts` is a pure function compressing one into a headline
(`Command or file not found (ENOENT)`, `Port already in use (127.0.0.1:39541)`), with the full text in
a scrollable region below and a Copy button.

**There are three kinds of empty state, not one** (`EmptyKind = "loading" | "failed" | "empty"`):
`loading` renders skeleton rows, `failed` offers Retry and **explicitly says "this is not empty"**,
`empty` offers a next-step CTA. Disguising "network failure" as "the registry doesn't have this" is
the easiest mistake this UI can make and the hardest to notice — the user goes hunting for a server
they know they configured and ends up doubting their own memory.

---

## 4. The presentation layer for writes

**Confirmation dialogs are for destructive actions, not reversible switches.** The title is a question
(`Remove stripe?`), the button is a verb (`Remove`), and the description spells out **what will not
happen** ("credentials stay in the keychain"). On failure the dialog **stays open**; bulk operations
confirm only above 3; and **global actions are disabled while a filter is active**, since they would
otherwise touch rows you cannot see.

**409 conflicts don't overwrite.** Write endpoints carry a `Precondition`, and a conflict returns 409
plus the current generation (see [../flows.md §3](../flows.md#3-config-writes-five-writers-and-an-optimistic-lock)).
The frontend re-fetches and reports `CONFLICT_MESSAGE` rather than writing back the view the user had
minutes ago.

**No polling after a write.** A write bumps the generation, the watcher emits an event, the control
plane pushes it over SSE, and the page refreshes from that. Your own writes and everyone else's travel
the same loop, so both behave identically.

**Credentials are never echoed back.** Inputs are password type and cleared on submit, there is **no
reveal toggle**, and the type returned by the read endpoint has no value field at all. An agent
token's plaintext appears exactly once, in the dialog reporting its creation, with an explicit note
that closing it makes the value unrecoverable.

---

## 5. CLI parity is an architectural boundary, not repeated page chrome

"The GUI may not have capabilities the CLI lacks" remains a hard boundary, but repeating an equivalent
command beside every working GUI action duplicates the interface and weakens the visual hierarchy. One
intent gets one primary control.

When the control plane genuinely lacks a GUI endpoint — currently following logs and restarting the
daemon — a small, muted terminal fallback may appear inside the relevant diagnostic disclosure, with
no icon, CTA styling or copy control. It is an escape hatch for a missing operation, not permanent
chrome beside actions the window already performs.

---

## 6. Explicitly not doing

**Not rewriting in React + shadcn.** Runtime dependencies would go from 1 to roughly 13 direct plus
hundreds of transitive. For a **security gateway** that supply-chain surface is ironic — detecting
tampering in the UI while stuffing several hundred npm packages into our own build. The "looks
presentable" part we actually want is 95% delivered by semantic colour variables, one consistent focus
ring, and three CSS classes for button/input/dialog, none of which needs a framework. Also declined:
the full Radix suite (native `<dialog>` already gives modality, Esc and focus trapping), Tailwind, and
`lucide-react` (replaced by compile-time inlining of the two dozen SVGs actually used).

**Two failures we deliberately did not copy:**

- **No "junk drawer" page** — nine things at nine levels of abstraction stacked vertically, rescued
  only by default-collapsed sections, so answering "why did that call fail" means scrolling past a
  pile of unrelated content. One page answers one question, and a page with nothing left to answer is
  deleted rather than kept as a heading.
- **Marketing copy stays out of the product UI** — tokens saved is a reasonable metric, but dressing
  it as a dollar estimate beside a Share button is soliciting the user for reach, and a hardcoded
  model price table is guaranteed to go stale. **A stale dollar figure is worse than none**: in a
  product built to detect things quietly changing, it would itself be a thing that quietly goes wrong.

---

## 7. Two seams that must not be widened

**`bridge.ts` is the only seam with the Go side** — `Call.ByName(<Go FQN>)` plus `Events.On`, with no
`wails3 generate bindings` — and it also owns `openExternal`, which opens the HOST browser and never
this webview. `generated/health.ts` is the other: **generated** by `go generate
./cmd/agenthub-gui/...` from the `api` package, never hand-edited.

**The theme is applied by an inline bootstrap in `index.html`, not in `main.ts`.** The bundle loads
too late: a theme applied after the first frame is a white flash in a desktop window, which reads as a
broken app rather than a slow one, so the only place the decision can be made is before the module
graph exists. It stamps two **attributes**, not a class — nothing here keys on a theme class, and
`style.css` selects on `:root[data-theme="dark"]`:

| Attribute | Value | Read by |
|---|---|---|
| `data-theme` | the RESOLVED answer, `light` or `dark` | `style.css` |
| `data-theme-mode` | the CHOICE, `light` / `dark` / `system` | the Settings page, so "which of the three is selected" has no second source of truth |

`system` is the default when nothing is stored, and every storage access is wrapped in `try`/`catch` —
`localStorage` throws in some embedded webview configurations, and a theme preference must never be
what stops the app from starting.
