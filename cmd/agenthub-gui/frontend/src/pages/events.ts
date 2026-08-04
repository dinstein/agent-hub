// Events page: the control-plane timeline.
//
// It answers the question the Servers page cannot: not "is this server
// healthy" but "how did it get that way". The two belong together and are
// deliberately not merged — a status is a value, a history is a sequence,
// and a page that tries to be both ends up refreshing one of them wrongly.
//
// Read-only, like Sessions and for a stronger reason: these records describe
// things that already happened, so there is nothing here to change.
//
// The vocabulary is CLOSED (docs/modules/foundation.md), which is what makes
// the tone mapping below legitimate. Colouring rows by a log message would
// be guessing; colouring them by `kind` is reading a contract. A kind this
// build does not know still renders — it lands in the neutral tone rather
// than being hidden, because a frontend older than its daemon must not
// silently drop records.

import { EVT, hub, on } from "../bridge";
import { clear, el, empty, loadingState, pageHeader, relTime, section, table } from "../dom";
import type { Page } from "../page";
import { failureBox } from "../page";
import { button, selectInput } from "../ui";
import { Pager, filterBar, pagerFooter, rangeField, rangeMillis } from "./observe";
import type { EventLog, EventRecord, TopicEvent } from "../types";

/** How many records one PAGE holds. The stream is low-rate, so this is
 *  usually the whole of an incident-sized window — but "usually" is not a
 *  contract, and a hub that flapped for an hour has more to say than one
 *  screen. The pager is what makes that readable rather than truncated. */
const PAGE = 300;

/** Tone per kind. Anything unlisted is neutral — including a kind from a
 *  newer daemon, which must still be shown. */
const TONES: Record<string, "bad" | "warn" | "good"> = {
  connect_failed: "bad",
  respawn_failed: "bad",
  health_down: "bad",
  secrets_missing: "bad",
  oauth_refresh_failed: "bad",
  registry_reload_failed: "bad",
  circuit_open: "warn",
  circuit_half_open: "warn",
  disconnected: "warn",
  respawned: "warn",
  stopping: "warn",
  stopped: "warn",
  connected: "good",
  health_up: "good",
  circuit_closed: "good",
  started: "good",
};

/** What `count` counts, per kind — the mirror of eventlog.CountNoun, kept
 *  here for the same reason TONES is: this page may not import internal/*,
 *  and the vocabulary it keys on is closed and published. An absent kind
 *  falls back to `n=13`, which is what every count looked like before the
 *  noun existed and is why a connect listing thirteen tools read as a
 *  thirteenth attempt. */
const COUNT_NOUN: Record<string, string> = {
  connected: "tools",
  tools_changed: "tools",
  respawned: "respawns",
  respawn_failed: "respawns",
  disconnected: "reconnects",
  circuit_open: "failures",
  health_down: "failures",
  health_up: "failures",
};

/** serverOf is the downstream a record is about, with its derived instance
 *  when there is one — one identity, so one cell. */
export function serverOf(e: EventRecord): string {
  if (!e.server) return "";
  return e.inst ? `${e.server}/${e.inst}` : e.server;
}

/** actorCell is who ASKED, which is a different question from which server a
 *  record is about and therefore a different column. A record can carry both:
 *  one subject cell picking a winner between them showed a login's server and
 *  never said whose session it was for.
 *
 *  A session is labelled rather than printed bare. The HTTP face's callers are
 *  tokens rather than configured clients, so a session id is all the identity
 *  there is — and an opaque id sitting under a "Client" header reads as a
 *  client whose name happens to look like a hash. */
function actorCell(e: EventRecord): Node {
  if (e.client) {
    return el("div", {}, [
      el("div", { text: e.client }),
      e.session ? el("div", { class: "muted mono", text: `session ${e.session}` }) : null,
    ]);
  }
  if (e.session) {
    return el("div", {}, [
      el("div", { class: "mono", text: e.session }),
      el("div", { class: "muted", text: "session" }),
    ]);
  }
  return el("div", { class: "muted", text: "—" });
}

/** detailOf folds the transition, the count and the free text into one
 *  string, in that order: the flip is the fact, the prose elaborates it. */
export function detailOf(e: EventRecord): string {
  const parts: string[] = [];
  if (e.from || e.to) parts.push(`${e.from || "—"} → ${e.to || "—"}`);
  if (e.count) {
    const noun = COUNT_NOUN[e.kind];
    parts.push(noun ? `${e.count} ${noun}` : `n=${e.count}`);
  }
  if (e.rev) parts.push(`rev ${e.rev}`);
  if (e.durMs) parts.push(`${e.durMs}ms`);
  if (e.detail) parts.push(e.detail);
  return parts.join(" · ");
}

/** The columns eventRows fills, in order. It ships beside the renderer for
 *  the reason the renderer is shared at all: a caller holding its own header
 *  list mislabels every cell under it the day this one moves. */
export const EVENT_COLUMNS = ["Time", "PID", "Scope", "What", "Server", "Client", "Detail"];

/** eventRows renders the shared table body, so the Servers panel and this
 *  page cannot drift into two ways of reading one record.
 *
 *  One value per column, and the three that used to share a cell — the scope,
 *  the server, the caller — each get their own. Stacked, a `daemon` under a
 *  server's name read as a fourth identity of that server rather than as the
 *  kind of process the record came from. */
export function eventRows(events: EventRecord[]): (Node | string)[][] {
  return events.map((e) => [
    el("div", { text: relTime(e.ts) }),
    el("div", { class: "muted mono", text: e.pid ? String(e.pid) : "—" }),
    el("div", { class: "muted", text: e.scope || "—" }),
    el("span", { class: `badge ${TONES[e.kind] ?? "neutral"}`, text: e.kind }),
    el("div", { text: serverOf(e) || "—" }),
    actorCell(e),
    el("div", { class: "muted", text: detailOf(e) || "—" }),
  ]);
}

/** "Only what went wrong" is the daemon's own answer now: every record
 *  carries a CLASS, routine or disruption, derived from its kind. This page
 *  used to assemble that set itself out of TONES, which was a second copy of
 *  a mapping that lives in internal/eventlog — and a copy that could only be
 *  as current as the frontend build.
 *
 *  Note what the class does that a tone cannot: `health_up` and
 *  `circuit_closed` are GOOD news and still belong to the disruption, because
 *  they are how it ended. A filter built from tones dropped them and showed
 *  every outage beginning with none of them finishing. */
const CLASS_DISRUPTION = "disruption";

/** How many records the facet read pulls. Larger than PAGE because it exists
 *  to enumerate what CAN be selected, and a dropdown that omits a server
 *  because it fell off a 300-record tail is a filter that hides data. */
const FACET_PAGE = 2000;

/** What the dropdowns offer, counted. */
interface Facets {
  servers: Record<string, number>;
  clients: Record<string, number>;
  kinds: Record<string, number>;
}

function facetsOf(events: EventRecord[]): Facets {
  const facets: Facets = { servers: {}, clients: {}, kinds: {} };
  for (const e of events) {
    if (e.server) facets.servers[e.server] = (facets.servers[e.server] ?? 0) + 1;
    if (e.client) facets.clients[e.client] = (facets.clients[e.client] ?? 0) + 1;
    if (e.kind) facets.kinds[e.kind] = (facets.kinds[e.kind] ?? 0) + 1;
  }
  return facets;
}

export function eventsPage(): Page {
  let root: HTMLElement | null = null;
  let off: (() => void) | null = null;
  let rangeHours = 24;
  let scope = "";
  let server = "";
  let client = "";
  let cls = "";
  let kind = "";
  let log: EventLog | null = null;
  const pager = new Pager();
  let facets: Facets = { servers: {}, clients: {}, kinds: {} };
  let failure: unknown = null;
  let loading = true;

  const sinceMillis = (): number => rangeMillis(rangeHours);
  const filtered = (): boolean =>
    scope !== "" || server !== "" || client !== "" || cls !== "" || kind !== "";

  /** Every selector goes to the DAEMON, not to the rendered rows.
   *
   *  The read is limited, so filtering a page client-side would search only
   *  the newest PAGE records and report "nothing matches" for something that
   *  is merely older than the window — the two are indistinguishable to the
   *  person reading it, and the wrong one sends them looking for a bug that
   *  is not there. */
  async function load(): Promise<void> {
    try {
      log = await hub.eventLog(sinceMillis(), PAGE, scope, server, client, cls,
        kind ? [kind] : [], pager.current());
      pager.accept(log.nextCursor);
      failure = null;
    } catch (err) {
      failure = err;
    } finally {
      loading = false;
      draw();
    }
  }

  /** Facets come from an UNFILTERED read of the same range, so narrowing by
   *  one selector does not empty the others. Deriving them from the filtered
   *  rows instead is the trap: picking a server would leave that server as
   *  the only option in its own dropdown, with no way back. */
  async function loadFacets(): Promise<void> {
    try {
      const all = await hub.eventLog(sinceMillis(), FACET_PAGE, "", "", "", "", []);
      facets = facetsOf(all.events);
    } catch {
      // A facet read that fails costs the dropdown its options, never the
      // page its rows. The selectors still work; they just stop suggesting.
      facets = { servers: {}, clients: {}, kinds: {} };
    }
  }

  /** Reload rows, and the facets too when the RANGE moved — the facet set is
   *  a function of the range alone. */
  function refresh(rangeMoved = false): void {
    // Back to page one. A cursor taken under one filter names a row the next
    // filter may not contain, and paging on from it would skip records
    // without ever saying so.
    pager.reset();
    loading = true;
    draw();
    void (rangeMoved ? loadFacets().then(load) : load());
  }

  function field(label: string, control: HTMLElement): HTMLElement {
    return el("label", { class: "activity-filter-field" }, [el("span", { text: label }), control]);
  }

  /** One dropdown over a counted facet.
   *
   *  A value that is currently selected but absent from the facets is still
   *  offered, shown as (0): it is reachable through a range change, and
   *  silently dropping it would reset the filter under the operator. */
  function facetField(
    label: string, allLabel: string, current: string,
    values: Record<string, number>, onChange: (v: string) => void,
  ): HTMLElement {
    const options = [{ value: "", label: allLabel }];
    if (current && !(current in values)) options.push({ value: current, label: `${current} (0)` });
    for (const [value, count] of Object.entries(values).sort((a, b) => a[0].localeCompare(b[0]))) {
      options.push({ value, label: `${value} (${count})` });
    }
    const select = selectInput(options, current);
    select.addEventListener("change", () => {
      onChange(select.value);
      refresh();
    });
    return field(label, select);
  }

  function filters(): HTMLElement {
    const scopePick = selectInput(
      [
        { value: "", label: "All scopes" },
        { value: "server", label: "Servers" },
        { value: "gateway", label: "Gateways" },
        { value: "daemon", label: "Daemon" },
      ],
      scope,
    );
    scopePick.addEventListener("change", () => {
      scope = scopePick.value;
      refresh();
    });

    // Class is its own control, above the kinds, because it is the reason
    // most people open this page and because it is not a kind: it is the
    // question "is this the hub working or the hub in trouble", which the
    // daemon answers for every record including ones this build cannot name.
    const classPick = selectInput(
      [
        { value: "", label: "Everything" },
        { value: CLASS_DISRUPTION, label: "⚠ Disruptions only" },
        { value: "routine", label: "Routine only" },
      ],
      cls,
    );
    classPick.addEventListener("change", () => {
      cls = classPick.value;
      refresh();
    });

    const kindOptions = [{ value: "", label: "All kinds" }];
    if (kind && !(kind in facets.kinds)) {
      kindOptions.push({ value: kind, label: `${kind} (0)` });
    }
    for (const [k, count] of Object.entries(facets.kinds).sort((a, b) => a[0].localeCompare(b[0]))) {
      kindOptions.push({ value: k, label: `${k} (${count})` });
    }
    const kindPick = selectInput(kindOptions, kind);
    kindPick.addEventListener("change", () => {
      kind = kindPick.value;
      refresh();
    });

    return filterBar(
      rangeField(rangeHours, (hours) => {
        rangeHours = hours;
        refresh(true);
      }),
      field("Show", classPick),
      field("Scope", scopePick),
      facetField("Server", "All servers", server, facets.servers, (v) => { server = v; }),
      facetField("Client", "All clients", client, facets.clients, (v) => { client = v; }),
      field("Kind", kindPick),
    );
  }

  function clearFilters(): void {
    scope = server = client = cls = kind = "";
    refresh();
  }

  function body(): Node {
    if (loading) return loadingState("Reading the event log…", 6);
    if (failure) return failureBox(failure);
    const events = log?.events ?? [];
    if (events.length === 0) {
      // THREE facts an operator has to be able to tell apart, and only the
      // first means anything is wrong with the setup: nothing has ever been
      // recorded (usually the stream is switched off), the hub was quiet in
      // this range, or the selectors excluded everything.
      if (log && log.files === 0) {
        return empty(
          "Nothing has been recorded yet.",
          "Any running gateway or daemon writes this stream unless events.enabled is false.",
        );
      }
      if (filtered()) {
        return empty(
          "No state changes match these filters.",
          "The range holds records; these selectors exclude them.",
          button("Clear filters", "btn", clearFilters),
        );
      }
      return empty("No state changes in this range.", "The hub has been quiet — which is the good case.");
    }
    // The daemon serves newest first, like every other observability list:
    // an operator opens this page because something just happened.
    return table(EVENT_COLUMNS, eventRows(events), "observe-table");
  }

  function draw(): void {
    if (!root) return;
    const reload = button("Refresh", "btn", () => refresh(true));
    clear(root);
    root.append(
      pageHeader(
        "Events",
        "Every state change of a server, a gateway or the daemon, in one timeline.",
        reload,
      ),
      section("Timeline", filters(), body(), pagerFooter({
        pager,
        pageSize: PAGE,
        shown: log?.events?.length ?? 0,
        total: log?.total ?? 0,
        noun: "events",
        onChange: () => {
          loading = true;
          draw();
          void load();
        },
      })),
    );
  }

  return {
    render(node) {
      root = node;
      // The SSE stream says only THAT something changed; the records are read
      // back through the control plane. Using it as a re-read trigger is
      // exactly the contract internal/event states for the bus.
      off = on<TopicEvent>(EVT.servers, () => void load());
      draw();
      // Facets first, so the dropdowns are populated by the time the rows
      // land rather than filling in a moment later under the pointer.
      return loadFacets().then(load);
    },
    dispose() {
      off?.();
      off = null;
      root = null;
      log = null;
      facets = { servers: {}, clients: {}, kinds: {} };
    },
  };
}
