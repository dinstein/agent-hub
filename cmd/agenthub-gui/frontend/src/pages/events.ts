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
import type { EventLog, EventRecord, TopicEvent } from "../types";

/** How many records one read pulls. The stream is low-rate; this is an
 *  incident-sized window rather than a page of a long list. */
const PAGE = 300;

/** Ranges offered, in hours. */
const RANGES: { label: string; hours: number }[] = [
  { label: "Last hour", hours: 1 },
  { label: "Last 24 hours", hours: 24 },
  { label: "Last 7 days", hours: 24 * 7 },
  { label: "Everything", hours: 0 },
];

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

/** subjectOf is what a record is ABOUT: a server (with its derived instance
 *  when there is one) or the client whose gateway spoke. */
export function subjectOf(e: EventRecord): string {
  if (!e.server) return e.client ?? "";
  return e.inst ? `${e.server}/${e.inst}` : e.server;
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

/** eventRows renders the shared table body, so the Servers drawer and this
 *  page cannot drift into two ways of reading one record. */
export function eventRows(events: EventRecord[]): (Node | string)[][] {
  return events.map((e) => [
    el("div", {}, [
      el("div", { text: relTime(e.ts) }),
      el("div", { class: "muted", text: `pid ${e.pid}` }),
    ]),
    el("span", { class: `badge ${TONES[e.kind] ?? "neutral"}`, text: e.kind }),
    el("div", {}, [
      el("div", { text: subjectOf(e) || "—" }),
      el("div", { class: "muted", text: e.scope }),
    ]),
    el("div", { class: "muted", text: detailOf(e) || "—" }),
  ]);
}

/** Kinds that mean something went wrong, derived from TONES rather than
 *  listed again — the fourth copy of the vocabulary in this file is the one
 *  that would drift.
 *
 *  A deliberate shutdown is not a problem. `stopped` and `stopping` carry the
 *  warn tone because they change what is available, but an operator asking
 *  "show me only what went wrong" does not mean "show me every time I quit
 *  the app", and a filter that answers with those is one they stop using. */
const NORMAL_SHUTDOWN = new Set(["stopped", "stopping"]);
const PROBLEM_KINDS = Object.entries(TONES)
  .filter(([kind, tone]) => tone !== "good" && !NORMAL_SHUTDOWN.has(kind))
  .map(([kind]) => kind);

/** The Kind select's sentinel for "anything that went wrong". It is a value
 *  in the same control rather than a separate toggle: both narrow by kind,
 *  and two controls writing one query field would have to define which wins. */
const KIND_PROBLEMS = "\u0000problems";

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
  let kind = "";
  let log: EventLog | null = null;
  let facets: Facets = { servers: {}, clients: {}, kinds: {} };
  let failure: unknown = null;
  let loading = true;

  const sinceMillis = (): number => (rangeHours > 0 ? Date.now() - rangeHours * 3600_000 : 0);
  const filtered = (): boolean => scope !== "" || server !== "" || client !== "" || kind !== "";

  /** The kinds `kind` selects, as the API wants them. Problems-only is
   *  intersected with what the facet read actually saw, so this can never
   *  send a kind the daemon does not define — a hardcoded list that ran ahead
   *  of the daemon would 400 the whole page instead of showing fewer rows. */
  function selectedKinds(): string[] {
    if (kind === KIND_PROBLEMS) return PROBLEM_KINDS.filter((k) => k in facets.kinds);
    return kind ? [kind] : [];
  }

  /** Every selector goes to the DAEMON, not to the rendered rows.
   *
   *  The read is limited, so filtering a page client-side would search only
   *  the newest PAGE records and report "nothing matches" for something that
   *  is merely older than the window — the two are indistinguishable to the
   *  person reading it, and the wrong one sends them looking for a bug that
   *  is not there. */
  async function load(): Promise<void> {
    try {
      log = await hub.eventLog(sinceMillis(), PAGE, scope, server, client, selectedKinds());
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
      const all = await hub.eventLog(sinceMillis(), FACET_PAGE, "", "", "", []);
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
    const range = selectInput(
      RANGES.map((r) => ({ value: String(r.hours), label: r.label })),
      String(rangeHours),
    );
    range.addEventListener("change", () => {
      rangeHours = Number(range.value);
      refresh(true);
    });

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

    // Problems-only leads the kind list, above the individual kinds, because
    // it is the reason most people open this page. It is offered only when
    // the range actually holds one — an option that can only ever return
    // nothing is worse than no option.
    const hasProblems = PROBLEM_KINDS.some((k) => k in facets.kinds);
    const kindOptions = [{ value: "", label: "All kinds" }];
    if (hasProblems) kindOptions.push({ value: KIND_PROBLEMS, label: "⚠ Problems only" });
    if (kind && kind !== KIND_PROBLEMS && !(kind in facets.kinds)) {
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

    return el("div", { class: "activity-toolbar" }, [
      field("Time range", range),
      field("Scope", scopePick),
      facetField("Server", "All servers", server, facets.servers, (v) => { server = v; }),
      facetField("Client", "All clients", client, facets.clients, (v) => { client = v; }),
      field("Kind", kindPick),
    ]);
  }

  function clearFilters(): void {
    scope = server = client = kind = "";
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
    // Newest first here, unlike the wire order: an operator opens this page
    // because something just happened.
    return table(["When", "What", "Subject", "Detail"], eventRows([...events].reverse()));
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
      section("Timeline", filters(), body()),
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
