// Logs page: what the hub's own processes did.
//
// The third of the three observability views, and the one that did not exist
// here at all. `agenthub logs` reads the files directly; a window cannot, so
// until GET /v1/logs the half of the record that matters most was
// terminal-only — the daemon never dials a downstream, so every connection
// failure, circuit transition, health flip and respawn is observed and
// written by a gateway process.
//
// It is deliberately the plainest of the three. Calls has a drawer because a
// call has payloads; Events colours by a closed vocabulary because it has
// one. A log line is prose with fields attached, so the page's whole job is
// to filter it, order it, and get out of the way.

import { hub } from "../bridge";
import { clear, el, empty, loadingState, pageHeader, relTime, table } from "../dom";
import type { Page } from "../page";
import { failureBox } from "../page";
import { button, selectInput, textInput } from "../ui";
import type { ProcLogPage, ProcLogRecord } from "../types";
import { Pager, filterBar, filterField, pagerFooter, rangeField, rangeMillis } from "./observe";

/** One page of rows. Larger than the Events page because a log line is one
 *  row of prose rather than a state change worth pausing on. */
const PAGE = 200;

/** Tone per level. WARN and ERROR are the reason anybody opens this page; the
 *  other two are context and stay quiet. */
const TONES: Record<string, string> = {
  ERROR: "bad",
  WARN: "warn",
  INFO: "neutral",
  DEBUG: "muted",
};

/** fieldsText renders the attributes that did not become columns, sorted so
 *  two readings of one line agree. */
function fieldsText(record: ProcLogRecord): string {
  const fields = record.fields ?? {};
  return Object.keys(fields)
    .sort()
    .map((k) => `${k}=${fields[k]}`)
    .join(" ");
}

function logRows(records: ProcLogRecord[]): (Node | string)[][] {
  return records.map((r) => [
    el("div", {}, [
      el("div", { text: relTime(r.time) }),
      el("div", { class: "muted", text: r.pid ? `pid ${r.pid}` : "" }),
    ]),
    el("span", { class: `badge ${TONES[r.level] ?? "neutral"}`, text: r.level || "—" }),
    el("div", {}, [
      el("div", { text: r.msg || "—" }),
      el("div", { class: "muted", text: fieldsText(r) }),
    ]),
    el("div", {}, [
      el("div", { text: r.client || r.server || "—" }),
      el("div", { class: "muted", text: r.origin }),
    ]),
  ]);
}

export function logsPage(): Page {
  let root: HTMLElement | null = null;
  let rangeHours = 24;
  let source = "";
  let level = "";
  let client = "";
  let server = "";
  let page: ProcLogPage | null = null;
  let failure: unknown = null;
  let loading = true;
  const pager = new Pager();

  const filtered = (): boolean =>
    source !== "" || level !== "" || client !== "" || server !== "";

  /** Every selector goes to the DAEMON, not to the rendered rows.
   *
   *  The read is one page deep, so filtering in the browser would search only
   *  the newest 200 records and report "nothing matches" for something that
   *  is merely older than the page — and the two are indistinguishable to
   *  whoever is reading it. */
  async function load(): Promise<void> {
    try {
      page = await hub.procLogs(
        rangeMillis(rangeHours), PAGE, source, level, client, server, pager.current(),
      );
      pager.accept(page.nextCursor);
      failure = null;
    } catch (err) {
      failure = err;
    } finally {
      loading = false;
      draw();
    }
  }

  /** refresh reloads from page one. Every filter change goes through here:
   *  a cursor taken under one filter names a row the next filter may not
   *  contain, and paging on from it would skip records without saying so. */
  function refresh(): void {
    pager.reset();
    loading = true;
    draw();
    void load();
  }

  function reload(): void {
    loading = true;
    draw();
    void load();
  }

  function filters(): HTMLElement {
    const sourcePick = selectInput(
      [
        { value: "", label: "All processes" },
        { value: "daemon", label: "Daemon" },
        { value: "gateway", label: "Gateways" },
      ],
      source,
    );
    sourcePick.addEventListener("change", () => {
      source = sourcePick.value;
      refresh();
    });
    const levelPick = selectInput(
      [
        { value: "", label: "All levels" },
        { value: "warn", label: "⚠ Warnings and errors" },
        { value: "error", label: "Errors only" },
        { value: "info", label: "Info and above" },
        { value: "debug", label: "Everything, including debug" },
      ],
      level,
    );
    levelPick.addEventListener("change", () => {
      level = levelPick.value;
      refresh();
    });
    // Client and server are free text rather than facets: unlike the event
    // stream, a process log has no bounded set of subjects to enumerate, and
    // a dropdown built from one page would offer only what that page held.
    const clientInput = textInput(client, "any client");
    clientInput.addEventListener("change", () => {
      client = clientInput.value.trim();
      refresh();
    });
    const serverInput = textInput(server, "any server");
    serverInput.addEventListener("change", () => {
      server = serverInput.value.trim();
      refresh();
    });
    return filterBar(
      rangeField(rangeHours, (hours) => {
        rangeHours = hours;
        refresh();
      }),
      filterField("Process", sourcePick),
      filterField("Level", levelPick),
      filterField("Client", clientInput),
      filterField("Server", serverInput),
    );
  }

  function body(): Node {
    if (loading) return loadingState("Reading the process logs…", 6);
    if (failure) return failureBox(failure);
    const records = page?.records ?? [];
    if (records.length === 0) {
      return filtered()
        ? empty("Nothing matches these filters", "Widen the range, or clear a selector.")
        : empty(
            "No process logs yet",
            "The daemon writes one when it first runs, and every connected client writes its own.",
          );
    }
    return table(["When", "Level", "Message", "Where"], logRows(records));
  }

  function draw(): void {
    if (!root) return;
    clear(root);
    // Refresh re-reads the CURRENT page rather than jumping to the newest:
    // somebody two pages back who wants to see whether a line arrived is
    // asking about this window, and moving them would lose the position they
    // are reading from. The other two observability pages have the same
    // button in the same corner.
    root.append(
      pageHeader(
        "Logs",
        "What the daemon and the gateways did, newest first.",
        button("Refresh", "btn", reload),
      ),
      el("div", { class: "activity-workspace" }, [
        filters(),
        el("div", {}, [body()]),
        pagerFooter({
          pager,
          pageSize: PAGE,
          shown: page?.records?.length ?? 0,
          total: page?.total ?? 0,
          noun: "records",
          onChange: reload,
        }),
      ]),
    );
  }

  return {
    render(host: HTMLElement) {
      root = host;
      loading = true;
      draw();
      void load();
    },
    dispose() {
      root = null;
    },
  };
}
