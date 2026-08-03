# GUI Frontend

The second of two topic-oriented documents organized around an **external constraint** rather than
around a layer (the other is [oauth.md](oauth.md)). This one covers the reasoning behind the
~10k lines of TypeScript in `cmd/agenthub-gui/frontend/`: **why it looks the way it does, and which
rules must not be casually changed**. The Go-side service body and wiring live in
[controlplane.md](controlplane.md#cmdagenthub-gui).

The rationale for the tech stack (vanilla TS + Vite, `@wailsio/runtime` as the only runtime
dependency, alpha dependencies confined to three files) is in [../canonical.md](../canonical.md) §7,
item 3.

---

## 1. Information architecture: a short task spine, then state inside each page

The sidebar started life as a one-to-one mapping of the CLI command tree — fourteen resource tables
laid out along the domain model. It now has eight task destinations: Servers, Catalog, Playground,
Profiles, Clients, Calls, Events and Settings. Catalog is deliberately first-class and immediately follows Servers:
the two are the configured and discoverable halves of the same task. Credentials are configured from
the server that needs them rather than from a global vault page; client bindings live with Clients, and appearance/daemon diagnostics live in
Settings. **Calls** and **Events** sit under System because they are the operator's evidence
workspace, not a permission layer, and they are two destinations rather than one because they answer
two questions: Calls reads the encrypted access ledger — what a client INVOKED — while Events reads
the control-plane stream — what HAPPENED to a server, a gateway or the daemon. Merging them would
put a payload-bearing, opt-in, encrypted record beside a default-on state change in one list, and a
reader would have no way to tell which guarantees applied to which row. Calls was called "Activity"
while it was the only one of the two; the name went because a page should say what it shows, and
"activity" described either. A resource
can still have a direct hash route without taking permanent navigation space. Tokens and a separate Governance destination are also
outside this navigation until they own a concrete task that is not already expressed by server,
profile, and client configuration.

The daemon state is pinned at the bottom of the shell and an offline daemon also raises a global
banner above the current page. A footer hidden below fourteen links failed at its only job: the
connection state disappeared precisely when every page began failing.

Each route renders into its own disposable DOM host. Navigation removes that host before mounting
the next page, so a slow request or rejected render from the page being left can only write into a
detached tree; it cannot clear or overlap the page now on screen. The router also grades render
failures by mount generation before displaying them. This is the shell's backstop rather than a
promise that every page remembered to add the same post-await guard.

The Servers page consumes live events for **configuration membership only**. Every enabled row is checked through
the page's own short-lived handshake, with ten running at once (bounded because a stdio probe spawns a
process, not because the daemon has a limit — it does not); another client's gateway report never supplies or
overwrites the row status. Settled outcomes live in a process-local cache across route changes, so returning to the
page paints the last page-owned observation immediately while a new fleet check runs silently in the background.
Only a row with no prior observation, or an explicit **Refresh**, displays `Checking…`; the Refresh action waits for
the forced fleet check to settle. Clicking a row toggles an expanded view of the latest settled handshake result;
that disclosure is a cache read and never starts a request, and says so beside its check time. Runtime-only events
may cause a configuration re-read but do not retrigger the probes. A newer registry revision does, because an
external editor may have changed an endpoint without changing its id.

Rows are keyed by server id and survive a repaint: only the row whose rendered content changed is
rebuilt, so an unchanged row keeps its hover, its focus and its open disclosure while the row beside
it is still checking. That is a stronger promise than the whole-fleet signature it replaced, which
could only skip a repaint when **nothing** had changed — during a fleet check, never.

The current call is: **a row moves only when the user changes configuration, never because of a probe
result.** Grouping follows configuration, which the registry answers with certainty the moment it is
read: `Enabled` and `Disabled`, both folding sections with a count, both ordered by id — the order
`server ls` prints. Enabled opens by default and Disabled does not, because one is the working set
and the other is the group the operator has already decided about; each remembers its own fold in
localStorage. Both fold, because a long list of healthy servers is as much in the way as a long list
of switched-off ones when you came to the page to look at the other group. **Both sections are always
on the page, empty or not**: hiding the empty one made the page's own structure depend on its
contents, so a first-run window showed neither heading and nothing on screen said what a server is
sorted by here, while switching the last server of a group off made a heading vanish rather than a
row move. An empty group shows its count and one sentence, and that sentence distinguishes "there are
none" from "this view is filtered" — the second is the only case where a bare zero would read as
"they are gone". The `<details>` element itself is built once and refilled, never rebuilt: a fresh
one would lose the open state the user set.

This **reverses** the earlier "lists are bucketed by state, not alphabetized", and the reason is worth
keeping: state is asynchronous, changeable, and at startup unknown, so letting it decide position
made every enabled row begin life in `needs attention` — an unchecked row reports `level=degraded`,
which is the absence of an answer rather than a fault — and migrate to `active` as its handshake
settled. Twenty servers meant twenty group changes and the table re-sorting under the cursor each
time. State did not need a position; it already had three channels inside the row (spine, dot, text),
and an unsettled row now uses the neutral tone in all three, because colour belongs to outcomes.

What the attention bucket actually provided was the answer to "which row do I look at first". That is
now the **attention chip, which filters**: narrowing on demand costs nothing when nobody asks,
whereas grouping rearranged the page permanently to make the same point. A single row of overview
chips sits at the top and describes the **whole fleet**, never the filtered view (`20 servers · 13
connected · 1 needs attention · 6 disabled`), and **a chip whose count is zero simply doesn't appear**
(`chip()` in `dom.ts` returns `null` for 0, and `chipRow` drops it) — "0 needs attention" is noise,
not information. Two exceptions exist because they close traps rather than add information: the
attention chip is rendered at zero **while it is pressed**, and it survives a re-probe that withholds
the other counts, because it is the control that turns the filter off. While the fleet is settling
the verdict chips are absent rather than partial (`20 servers · 13 checking · 6 disabled`, counting
down): a count that climbs one probe at a time is motion that says nothing, since the number is only
an answer once every question has been asked.

### 1.1 Window and page geometry

The desktop window opens at **1240 × 800** with a **900 × 620** minimum. The former leaves a useful
content column beside the persistent navigation without turning every row into a full-screen strip;
the latter is the smallest size at which navigation, a two-column form, and its actions can still be
read without relying on accidental horizontal scrolling. Responsive rules may stack information
inside that boundary, but they do not hide the navigation.

Geometry follows three shared lines rather than page-local guesses:

- the page heading, toolbar, notices, cards, and empty state share one left and right content edge;
- ordinary buttons, text inputs, and selects share one control height, while compact row actions use
  the one documented small variant;
- record content stays next to its state and actions. A flexible middle column may absorb spare width,
  but metadata or a button must not be pushed across an empty card merely to occupy both edges.

The document reserves a stable vertical-scrollbar gutter. Expanding a cached server detail can make
an otherwise short page overflow, but that disclosure must not change the sidebar position or the
content column width when the scrollbar appears.

A modal freezes both the document and the shell's `.content` scroll container until its last modal
layer closes. The modal body may scroll within the available window height, but a wheel or touch
gesture at either edge must not move the dimmed page behind it. The lock is reference-counted because
a confirmation may sit above an editor; closing only the confirmation must leave the editor modal.

The native window title is blank. The sidebar is the single visible product identity: `AgentHub`
with the release-plus-commit version stamped into this GUI build. That value comes from the process,
not the connected daemon, because the two may legitimately be different builds.

Buttons never translate or scale on hover or press. Moving a compact control by one pixel makes it
jump against aligned neighbours, while scaling briefly softens its label in the webview. Hover uses a
small tone and shadow change; press deepens the tone while keeping the shadow direction; keyboard focus
uses a translucent outer halo rather than a second solid border. An asynchronous action keeps its label
width and enters the shared busy treatment instead of being replaced by a dimmed, differently sized
button. The states remain distinct without a one-frame contrast flash or moving geometry.

The Clients page keeps **file capability** and **connection state** separate. `writable`/`read-only`
describes whether AgentHub may rewrite a configuration file; it says nothing about whether that file
already contains AgentHub's gateway. Connection state comes only from the per-client Inspect endpoint.
Entering the page performs a metadata scan and then inspects each detected client in sequence, so the
first settled view already shows `Connected`, `Not connected`, `Manual setup`, or a visible read failure;
the single `Refresh` action repeats both passes. Inspections stay sequential because protected files may
raise a macOS privacy prompt and concurrent prompts would obscure which client requested access. Only a
connected row offers Disconnect, avoiding the old pair of Connect and Disconnect buttons that appeared
simultaneously for every client and claimed no state at all. A writable, disconnected row connects
immediately with the daemon's safe defaults; profile selection remains on the Profiles page and uncommon
path/binary overrides remain explicit CLI operations. Read-only formats use the same single action to
return their authoritative manual setup instructions instead of opening a configuration form the daemon
cannot apply.

Profiles begin with the same virtual `(default)` row that the CLI prints. It is explanatory state,
not an object in `profiles[]`: an unbound client follows the active named profile when one exists,
and otherwise reaches all enabled servers subject to global per-server tool rules. A dangling active
profile is shown as a broken reference and an empty effective scope, preserving the runtime's
fail-closed behavior. Keeping this row visible even when named profiles exist answers the page's
first question — what an unbound client can reach — without making the user infer it from a footnote.
Creating or editing a named profile opens a focused modal: the profile list remains stable underneath
while the operator names or renames the profile, changes its members, or edits a per-server tool selector.
These are object edits with their own Save / Cancel boundary; none expands an inline form between profile
cards. Direct state changes such as making a profile the default remain direct actions, and destructive
deletion keeps its confirmation dialog rather than being confused with an editor. Creation lets the
operator name the profile and choose either every registered server or a non-empty selected subset.
The server checklist and manual-name tail exist only for the subset choice; selecting every server
collapses them instead of showing controls whose values have no effect. The GUI does not offer a
first-class block-all member choice, although profiles written with an explicit empty set remain
accurately described and must be changed to every server or a non-empty subset before the editor saves.
Per-server tool selectors retain all three states because blocking every tool is a useful narrowing
inside an otherwise participating server.

The server editor is transport-shaped, not a union of every possible field. `stdio` shows the local
process contract (`command`, arguments, environment, working directory and optional container runtime);
`http` and `sse` show the remote endpoint contract (`url`, headers and provenance). Enabled, OAuth hints
and connection instancing are shared because the registry supports them across transports. The groups
are switched in place when Connection type changes, and `fieldset.group[hidden]` is an explicit CSS rule:
the group's authored `display:flex` would otherwise override the browser's default `[hidden]` style and
put Command and URL on screen together even though the collected entry correctly accepted only one.
Manual server creation and Catalog entries that need parameters open this editor in a focused modal.
Manual creation starts on `http`; pasted and Catalog definitions keep the transport they declare (and
the parser's absent transport retains the registry's `stdio` default).
The list remains stable underneath, the dialog header names the object being configured, long forms
scroll inside the available window height, and the save/cancel actions remain at the bottom edge. A
Catalog entry with no missing configuration remains a single-click add; the modal is not a ritual
confirmation for work that needs no input.

Entering the page, creating or editing an enabled server, and switching a disabled server on all perform the same
handshake-only self-test as the row's Test action. A write has already succeeded and is never rolled back by the
probe: a normal connection reports its tool count, a generic failure stays a visible diagnostic, and an
authentication-class error (`E_AUTH_REQUIRED`, plus the mixed-version `E_AUTH_FAILED` spelling) repaints the row as
its `Authenticate` action. Authentication has exactly that one surface: the page does not also grow a warning card
above the list, and Test closes its transient result dialog before moving the condition into the row. The frontend
branches on the daemon's code, never on whether prose happens to contain `401`.

Only the latest page-owned self-test for one Server may settle its presentation. Starting a newer test cancels the
older wait; that cancellation is neutral and must not become a connection-failure notice or a failed Test dialog.
The replacement request owns the row and supplies the eventual success or concrete failure.

An unresolved placeholder follows the parallel setup path: `E_SECRET_REQUIRED` carries safe key names, the row
becomes **Add API key** or **Set secret**, and that Server's secret manager opens with the key locked to the typed
result. This guided path is deliberately a one-field dialog: the header and neutral key chip carry the Server/key
context, the only editable control is the write-only value, and scope stays behind a quiet Advanced settings
disclosure. It does not load or expose the Server's wider credential inventory before accepting the required value,
and has one Cancel / Save footer instead of a second Close action. Every Server also exposes **Manage secrets** from
its overflow menu; the scoped modal lists only its key
names, scopes, and storage backends and owns add/delete work. There is deliberately no global Secrets destination.
Saving closes the modal and immediately retests exactly that Server. The value is cleared before the write awaits
and never comes back over a read surface. A non-OAuth server with this condition omits the unrelated “No OAuth
credential stored” section.

The Playground treats execution as the primary task, not the last step of a form. Its Call action is
inside the argument header and remains visible while a long generated schema scrolls beneath it.
Generated fields are split into explicit Required and Optional sections; a blank optional field is
omitted rather than encoded as an empty value. A rejected field receives both an invalid state and
focus. Tool text that parses as JSON opens in Pretty mode with a reversible Raw view, while arbitrary
text is left untouched. The raw daemon result remains a separate diagnostic disclosure because tool
content and transport metadata answer different questions.

Calls is one page with three focused views rather than three navigation destinations. **Calls** joins
the received/routed/finished lifecycle into one compact row; the collection endpoint returns metadata
only and never exposes payload references. Calls use stable 50-row cursor pages, show the filtered total,
and apply Client, Server, Tool and Outcome dropdowns on the daemon before
paging; filters never describe only the visible slice. Server and Tool are separate columns, and selecting
a server narrows the Tool dropdown using range-wide statistics rather than whichever calls happen to be on
the current page. Clicking anywhere on the row is the disclosure action: the detail drawer immediately
loads decrypted previews, with no second "decrypt" ritual. Detail is one scrolling page rather than a set
of tabs: Request and Result are the primary sections, compact call facts precede them, and Lifecycle is a
secondary disclosure beneath them. The authenticated effective-arguments payload remains available to API
consumers but is not duplicated as a GUI section because the request already carries the arguments and the
gateway does not rewrite call payloads. The drawer says quietly that this is a local decrypted preview and
the page drops those strings when it closes. Valid JSON opens pretty (including JSON text nested in an MCP
content item), can be copied, and can be switched back to the exact Raw value. **Insights** aggregates the
same bounded time range by outcome, client, server, and tool.
**Ledger** owns capture status, footprint, integrity verification, retention cleanup, and key rotation.
Pausing capture is a direct reversible action and never deletes history or keys.

Events is the timeline Calls cannot be. It reads `GET /v1/events/log`, which exists because
`cmd/agenthub-gui` may not import `internal/*` — the CLI reads `events.jsonl` directly and works
offline, the GUI goes through the daemon like every other page here. Rows are coloured from `kind`
rather than from any text, which is legitimate only because that vocabulary is CLOSED
([foundation.md](foundation.md)); a kind this build does not know renders in the neutral tone rather
than being hidden, because a frontend older than its daemon must not silently drop records. The SSE
`servers` topic is the re-read TRIGGER and never the data — the bus contract says a subscriber
re-reads authoritative state on notification, and that is exactly what happens here.

Every selector is applied by the DAEMON, not to the rendered rows, and that is the rule to keep. The
read is bounded, so narrowing a page in the browser would search only the newest records and answer
"nothing matches" for something that is merely older than the window — two facts a reader cannot tell
apart, and the wrong one sends them hunting a fault that never happened. The dropdowns are counted
and their options come from an UNFILTERED read of the same range, because deriving them from the
filtered rows leaves a chosen server as the only option in its own dropdown with no way back. The
kind list leads with "Problems only", derived from the tone map rather than restating the vocabulary
again, and intersected with the kinds actually present — a hardcoded set that ran ahead of the daemon
would reject the whole request instead of returning fewer rows.

The same rows appear inside a server's detail panel, from the same renderer. The health badge there
is a value — what the server is now — and the timeline under it is the sequence that produced it,
which is the question an operator actually brings to a server that keeps dropping. It loads per
server and only once expanded: fetching every server's history to draw a list would cost far more
than it shows, and it is dropped alongside the cached self-test so a refresh never pairs a fresh
badge with stale history.

### 1.2 The window is not the application

Closing the window used to end the process — and with it, on the ordinary path, the daemon that
process had started. A GUI other programs depend on cannot have "tidy the desktop" and "cut off every
connected client" behind the same button, so the close button now **hides the window** and the
application keeps running in the system tray.

The tray is a **readout, a set of destinations, and one lifecycle action**. It carries no registry
write of any kind: a menu has no confirmation surface and nowhere to render a 409, so a mis-click
there would change governance configuration and the refusal would have nowhere to go. Enabling a
server, switching a profile, editing scope — all of it stays in the window. The one action offered is
**starting** a hub that is not running, which can only help; stopping or restarting a running one
cuts off every client mid-session, and is therefore not in the menu at all.

The icon is the whole feature for anyone not currently looking at the window: three clients around a
**hollow** hub is no daemon, the same mark with the hub **filled in** is serving, and a badge on its
shoulder appears when an enabled server is not healthy. The mark is the application icon reduced —
the rounded-square hub and the callers converging on it, three rather than six and detached rather
than joined by spokes, because at 22 points six nodes smudge and a spoke fuses node, arm and core
into one blob. It is deliberately **not** a ring: a ring says nothing this product says, and the
other local MCP hubs already put one in the status area, so the two would be the same picture at a
glance. The hollow state
is an **outline**, not an absence — a two-pixel hole made "offline" and "serving" identical in the
first draft, and `TestTrayIconOfflineKeepsTheHubVisible` is that draft's headstone.
Bucketing comes from the same `Health` contract the Servers page uses — a tray that
re-derived status from connection flags would be the second opinion
[controlplane.md](controlplane.md) forbids. Server rows are capped, sorted worst-first so the cap
drops what nobody needs to see, and the menu says how many it dropped.

Three decisions are load-bearing:

- **The first close asks.** Vanishing silently into the status area is the standard complaint about
  tray applications, and here what keeps running is a hub. The dialog is also the "I meant quit"
  escape, and the button pressed becomes what the close button does from then on (changeable in
  Settings). Dismissing it persists nothing and asks again.
- **No tray means the close button still quits.** Hiding into a status area that is not there leaves
  a running process with **no reachable surface at all** — no window, no menu, only a process list. A
  window that closes when you asked it to minimise is a surprise; a hub that cannot be quit is a
  trap. The frontend's copy of "is there a tray" is display state only, because everything bound is
  settable from the webview; the close path reads the assembly's own flag.
- **Quit says what it costs.** Once the close button stops quitting, Quit is the only path that
  reaches the shutdown which stops a daemon this GUI started, so the item spells that out when it
  applies (`Quit AgentHub (stops the hub)`).

### 1.3 The application runs the hub

The application is now the only thing that starts a hub (`--headless` aside, for a server with no
desktop), which changes three things here.

**Ownership is a process handle, not a memory.** `Hub.proc` is an `api.Supervised` — the hub this
process is running — and holding it *is* the claim that licenses stopping it. The bool it replaced
was written from "did my dial start one": a fact about a past call, which outlived the daemon it
described. A **transport failure no longer disowns anything**, either: the connection is gone, the
process is not, and a hub that is briefly unreachable is still ours to stop and still ours to
restart. What ends ownership is the process ending, which the supervisor sees directly.

**A hub that is already answering is used, never adopted.** Every connect dials first. A headless hub
an operator started, or one belonging to another AgentHub window, answers there and every page works
against it — but `proc` stays nil, so quitting leaves it running. That is also what makes a second
launch harmless, and why there is no separate single-instance lock: the danger a lock would have
prevented is the second window stopping the first one's hub, and ownership already refuses that.
Settings says which case applies (`Lifetime`), because otherwise the only way to find out is to quit.

**A hub that falls over is started again.** Before, a dead daemon left the GUI offline until the user
pressed a button — acceptable while a terminal could also start one, and not acceptable now that this
application is the only other way to get one back. The supervisor restarts on a doubling backoff and
**gives up after five consecutive failures**: a hub that cannot start will not start on the sixth
attempt either, and a loop that keeps trying spawns a process per interval and buries the first
failure — the one that says why — under thousands of identical ones. Giving up leaves the offline
status and its error on screen, which is the surface the user needs; `Connect` retries on demand. A
run lasting a minute counts as healthy and resets the ladder, so a hub that dies once a day never
exhausts a counter nothing clears.

The preference lives in `localStorage`, like the theme and for the same reason: it is a property of
this window on this machine, and the registry having an opinion about it would be wrong. The Go side
holds a runtime copy because the close arrives natively; the frontend pushes it at startup, and every
change is announced back as an event the frontend persists without answering. The announcement is
unconditional rather than tray-only because the preference has **two** surfaces — the tray checkbox
and the Settings switch — and each has to learn about the other's change. Neither switch flips
itself: like every `toggleSwitch` here they render a value and the page redraws from the
authoritative one, which for this preference means redrawing on that event.

**A menu callback runs off the main thread.** Wails hops for you on window methods, `App.Quit` and the
clipboard, but **not** on `App.Show`/`App.Hide` — and the asymmetry is invisible at the call site.
AppKit traps a cross-thread `[NSApp unhide:]`, Wails catches the panic and calls `os.Exit(1)`, so the
symptom is a tray item that quits the application with no crash report to explain it. Anything new in
this menu that touches a native API goes through `application.InvokeSync` unless it has been checked
that the call already does.

**Platforms.** macOS and Windows drive a tray. Linux deliberately does not: Wails registers the icon
over the dbus `StatusNotifierItem` protocol, and a desktop with no `StatusNotifierHost` — a default
GNOME session, for one — accepts the registration and then shows nothing, so until that is verified
on a real session Linux keeps exactly the behaviour it had before. Windows compiles and vets but,
like everything else there, is unverified on a real machine ([../windows.md](../windows.md)).

Deliberately not done, each for its own reason: **launch at login** (three platform mechanisms plus
their uninstall residue), **notifications when a server drops** (a new dependency, a permission
prompt and a debounce policy), and a **menu-bar-only mode** (`ActivationPolicy` accessory), which
would make the tray the only way back into an application that is not a menu-bar utility.

---

## 2. State is the action

A server row's status cell has six states, and each one is expressed through **three channels at
once** (dot color / text content / text color). Color is never the only channel — that is both an
accessibility requirement and a guard against misreading:

| State | Display |
|---|---|
| connected | a green dot and **Connected**; **`23 tools`** occupies the following aligned outcome column |
| needs-auth | a yellow dot and **Authentication required**; the outcome column becomes `Authenticate` and signs in for real (docs/modules/controlplane.md) |
| needs-secret | a yellow dot and **API key required** / **Secret required**; the outcome column opens the guided write-only form |
| checking | after 4 seconds, if the command is `npx`/`uvx`, it changes to **`Installing…`** — reinterpreting a wait as progress |
| error | a one-line distilled error headline, expandable to the full text |
| disabled | gray dot, no text |

This is the single biggest saver of user time: it removes the "read status → figure out what to do →
find the entry point" three-step.

`needs-auth` arrives from this page's own self-test as a typed authentication error; the frontend does not infer it
from an error string. Clicking the adjacent `Authenticate` action starts the control-plane login session below, so
the row identifies the problem and places its repair beside it. It uses the warning spine, not the red
connection-failure spine: missing authorization is an expected setup state, not evidence that the endpoint or
protocol is broken.

That authentication observation is sticky across route changes for the life of the GUI process. Gateway reports
cannot touch it, and a later generic probe failure cannot downgrade it to `connection error`; only this page's own
successful handshake clears it. Login completion immediately runs that handshake rather than trusting that storing
a token made it usable. The observation is not persisted to disk and it never parses error prose.

**Semantic colors are reserved for health; accent is reserved for interaction.** Metadata like
transport, source, and profile is always neutral (`ChipTone`'s `neutral`). Green/yellow/red still
answer health only. The indigo accent answers a different, closed question — which navigation item,
primary action or focus target is active — and is never used for a health state. Without that split,
a healthy stdio server would show two unrelated green dots at once and color would stop meaning
anything.

**Status classification never parses raw connection flags or error prose.** The ordinary control-plane Health
contract is computed by the daemon's pure function. The Servers page deliberately replaces it for enabled rows with
the typed outcome of its own self-test: success, authentication refusal, missing secret, generic failure, or still checking. The
shared level/action constants are generated from the `api` package into `generated/health.ts`, with a golden test
watching for three-way drift.

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
  from the answer, so a refused write or a lost precondition leaves the switch showing what is
  *stored*. Both directions write immediately: disabling is a reversible setting change that keeps
  the definition, credentials and profile rules, so interrupting it with a destructive-action dialog
  trains the user to dismiss confirmations without reading them. Optimistic flips still have to be
  walked back, and the moment they are wrong is exactly the moment the user looks away satisfied —
  the same reason a row grays out instead of vanishing (§5).

The server overview is one keyboard-focusable disclosure target. Its leading chevron and `aria-expanded` state make the
toggle explicit: one activation reveals the latest cached self-test detail underneath and the next collapses it.
Expansion never performs I/O. The summary and detail are separate sibling boxes: the summary retains its fixed
geometry and health spine, while the detail adds a divider and quieter background underneath. Every part of the
summary except its row-local controls toggles the disclosure, including the status, tool count, and empty column
space; the enclosing bucket's own `<details>` element is not a row control. Interacting with cached detail cannot
collapse it. Editing is a
separate, always-visible **Edit** button in the action column, so a click whose visible affordance says “show me
more” cannot unexpectedly open a write surface. The leading enable switch and trailing Test / Edit controls remain
separate targets and never bubble into the disclosure. A fixed status column is followed by one aligned outcome
column: healthy rows show their tool count there, while setup rows show Authenticate or Add API key. Neither can move
the trailing buttons. Server-scoped **Manage secrets**, destructive Remove,
and OAuth Log out sit in the compact overflow menu: they stay available without painting every healthy row as a red warning. The menu opens upward on a bucket's last row when space permits and lifts its bucket above adjacent cards,
so every action remains visible without changing the ledger's layout. A successful enable/disable writes no page-level
notice because the switch and the row's own probe already show the stored and runtime outcomes; failures keep the
shared error surface.

The detail also includes cached OAuth metadata from the credential-status API: state, access-token expiry,
issuer, scopes, and whether a refresh token exists. It never includes token values. When a stored OAuth credential
exists, the summary overflow menu exposes **Log out OAuth**; the confirmation states that this deletes the local
credential but does not revoke it at the provider. Logging out immediately re-runs the page-owned handshake so the
row returns to its Authenticate action rather than retaining a stale connected result.

The action column is deliberately compact. It may contain only the distilled status or direct health
action plus the row controls; daemon detail, HTTP responses and recovery instructions live behind a
disclosure in the flexible record body. A diagnostic string must never participate in the action
column's width calculation: one verbose failure would otherwise squeeze every server's identity and
make the list unreadable at the application's normal window width.

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

The row status, the post-create/post-enable probe, and the Test dialog all call this one `login(id)` function;
three entry points do not mean three OAuth implementations.

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

**Confirmation dialogs are for destructive actions, not reversible switches.** Their copy follows a
pattern: the title is a question (`Remove stripe?`), the button is a verb (`Remove`), and the description
spells out **what will not happen** ("credentials stay in the keychain"). On failure the dialog **stays
open**; bulk operations have a threshold (confirm only above 3); and **global actions are disabled while
a filter is active** — otherwise they would touch rows you cannot see.

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

## 5. CLI parity is an architectural boundary, not repeated page chrome

"The GUI may not have capabilities the CLI lacks" remains a hard boundary, but repeating an
equivalent command beside every working GUI action duplicates the interface and weakens the visual
hierarchy. Normal pages stay task-focused: one intent gets one primary control, without a second row
of command text and Copy buttons teaching another interface.

When the control plane genuinely lacks a GUI endpoint — currently following logs and restarting the
daemon — a small, muted terminal fallback may appear inside the relevant diagnostic disclosure. It
has no icon, call-to-action styling, or copy control. It is an escape hatch for the missing operation,
not permanent chrome beside actions the window already performs.

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
| `main.ts` | Entry point: routing, sidebar, SSE subscription, tray navigation and the close question. **No theme code** — it cannot have any and still work, see below |
| `bridge.ts` | The only seam with the Go side: `Call.ByName(<Go FQN>)` + `Events.On` (no `wails3 generate bindings`), plus `openExternal` — the HOST browser, never this webview |
| `page.ts` | The `Page` contract, `failureBox` / `failureState`, `CONFLICT_MESSAGE`, `noticeSlot` |
| `dom.ts` | Dependency-free DOM construction: `el` / `table` / `emptyState` (three kinds) / `chip` (returns null for 0) / `chipToggle` (a count that is also a filter) / `reconcile` (keeps nodes across a repaint) / `errorHeadline` / time formatting |
| `ui.ts` | Form widgets: inputs, tri-state selector, pair/lines editors, confirmation dialog, `toggleSwitch` (never optimistic) |
| `types.ts` | TS mirror of the control plane DTOs, plus `WindowPrefs` (window-local, not hub state) |
| `window-prefs.ts` | The close-button preferences: `localStorage` is the durable copy, the Go side gets a runtime copy, the tray's changes arrive as an event (§1.2) |
| `close-notice.ts` | The one-time "this keeps running in the tray" dialog, and the quit escape inside it |
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
