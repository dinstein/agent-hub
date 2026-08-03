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
  ctl_socket_lost: "bad",
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
  if (e.attempt) parts.push(`n=${e.attempt}`);
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

export function eventsPage(): Page {
  let root: HTMLElement | null = null;
  let off: (() => void) | null = null;
  let rangeHours = 24;
  let scope = "";
  let log: EventLog | null = null;
  let failure: unknown = null;
  let loading = true;

  async function load(): Promise<void> {
    try {
      const since = rangeHours > 0 ? Date.now() - rangeHours * 3600_000 : 0;
      log = await hub.eventLog(since, PAGE, scope, "", "", []);
      failure = null;
    } catch (err) {
      failure = err;
    } finally {
      loading = false;
      draw();
    }
  }

  function filters(): HTMLElement {
    const range = selectInput(
      RANGES.map((r) => ({ value: String(r.hours), label: r.label })),
      String(rangeHours),
    );
    range.addEventListener("change", () => {
      rangeHours = Number(range.value);
      loading = true;
      draw();
      void load();
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
      loading = true;
      draw();
      void load();
    });
    return el("div", { class: "activity-filters" }, [range, scopePick]);
  }

  function body(): Node {
    if (loading) return loadingState("Reading the event log…", 6);
    if (failure) return failureBox(failure);
    const events = log?.events ?? [];
    if (events.length === 0) {
      // Two different facts, and the operator has to be able to tell them
      // apart: nothing recorded at all usually means the stream is switched
      // off, while nothing in THIS window means it is on and quiet.
      return log && log.files === 0
        ? empty(
            "Nothing has been recorded yet.",
            "Any running gateway or daemon writes this stream unless events.enabled is false.",
          )
        : empty("No state changes in this range.", "The hub has been quiet — which is the good case.");
    }
    // Newest first here, unlike the wire order: an operator opens this page
    // because something just happened.
    const rows = eventRows([...events].reverse());
    return table(["When", "What", "Subject", "Detail"], rows);
  }

  function draw(): void {
    if (!root) return;
    const refresh = button("Refresh", "btn", () => {
      loading = true;
      draw();
      void load();
    });
    clear(root);
    root.append(
      pageHeader(
        "Events",
        "Every state change of a server, a gateway or the daemon, in one timeline.",
        refresh,
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
      return load();
    },
    dispose() {
      off?.();
      off = null;
      root = null;
      log = null;
    },
  };
}
