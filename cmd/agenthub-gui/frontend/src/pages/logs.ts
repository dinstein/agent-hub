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

/** How many records the SUGGESTION read pulls, and it is a window rather than
 *  the whole range on purpose.
 *
 *  A process log has no bounded set of subjects, which is why the Client and
 *  Server controls here are free text where the Events page has dropdowns.
 *  That stays true: this list only offers what it has recently seen, and a
 *  name older than the window is still typeable and still matches on the
 *  daemon. Offering the same values as a dropdown would turn "not in the last
 *  thousand lines" into "not filterable", which is the failure the Events
 *  page's much wider facet read exists to avoid and this page cannot afford —
 *  its stream is the noisiest of the three. */
const SUGGEST_PAGE = 1000;

/** The two suggestion lists. Their ids are constants because this page owns
 *  its whole subtree and redraws all of it at once. */
const CLIENT_VALUES = "logs-client-values";
const SERVER_VALUES = "logs-server-values";

/** What the inputs suggest: the distinct names seen in the window, sorted so
 *  two readings agree. */
interface Suggestions {
  clients: string[];
  servers: string[];
}

function suggestionsOf(records: ProcLogRecord[]): Suggestions {
  const clients = new Set<string>();
  const servers = new Set<string>();
  for (const r of records) {
    if (r.client) clients.add(r.client);
    if (r.server) servers.add(r.server);
  }
  const sorted = (v: Set<string>): string[] => [...v].sort((a, b) => a.localeCompare(b));
  return { clients: sorted(clients), servers: sorted(servers) };
}

function valueList(id: string, values: string[]): HTMLElement {
  return el("datalist", { id }, values.map((v) => el("option", { value: v })));
}

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

/** The columns logRows fills, in order. The message goes last because it is
 *  the only one with no fixed shape: everything ahead of it lines up down the
 *  page, and prose given a middle column pushes whatever follows it out of
 *  alignment on every row. */
const LOG_COLUMNS = ["Time", "PID", "Level", "Process", "Client", "Server", "Message"];

/** One value per column.
 *
 *  `r.client || r.server` is what this replaces, and it lost data rather than
 *  merely crowding it: a gateway line names both — the client it serves and
 *  the downstream it was dialling — and only the client survived. Nothing in
 *  the row said which of the two the surviving name was, either, and the
 *  process kind sat underneath it as though it were a third reading of the
 *  same field. */
function logRows(records: ProcLogRecord[]): (Node | string)[][] {
  return records.map((r) => [
    el("div", { text: relTime(r.time) }),
    el("div", { class: "muted mono", text: r.pid ? String(r.pid) : "—" }),
    el("span", { class: `badge ${TONES[r.level] ?? "neutral"}`, text: r.level || "—" }),
    el("div", { class: "muted", text: r.origin || "—" }),
    el("div", { text: r.client || "—" }),
    el("div", { text: r.server || "—" }),
    el("div", {}, [
      el("div", { text: r.msg || "—" }),
      el("div", { class: "muted", text: fieldsText(r) }),
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
  let suggest: Suggestions = { clients: [], servers: [] };

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

  /** The suggestion read is UNFILTERED, for the reason the Events page's
   *  facet read is: narrowing to one client and then offering only that
   *  client leaves no way back to the others. */
  async function loadSuggestions(): Promise<void> {
    try {
      const seen = await hub.procLogs(rangeMillis(rangeHours), SUGGEST_PAGE, "", "", "", "");
      suggest = suggestionsOf(seen.records);
    } catch {
      // A suggestion read that fails costs the inputs their hints, never the
      // page its rows. Both controls still filter; they just stop guessing.
      suggest = { clients: [], servers: [] };
    }
  }

  /** refresh reloads from page one. Every filter change goes through here:
   *  a cursor taken under one filter names a row the next filter may not
   *  contain, and paging on from it would skip records without saying so.
   *
   *  The suggestions follow the RANGE and nothing else — they are the names
   *  that window holds, which no other selector changes. */
  function refresh(rangeMoved = false): void {
    pager.reset();
    loading = true;
    draw();
    void (rangeMoved ? loadSuggestions().then(load) : load());
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
    // Client and server stay free text rather than becoming facets: unlike
    // the event stream, a process log has no bounded set of subjects to
    // enumerate, and a dropdown is a claim that the list is complete.
    //
    // The attached value list is the other half of that: it says what has
    // been seen recently, so nobody has to remember the exact spelling of a
    // client id, while anything it omits is still typeable and still matches.
    // A suggestion that is missing costs a keystroke; an option that is
    // missing costs the filter.
    const clientInput = textInput(client, "any client");
    clientInput.setAttribute("list", CLIENT_VALUES);
    clientInput.addEventListener("change", () => {
      client = clientInput.value.trim();
      refresh();
    });
    const serverInput = textInput(server, "any server");
    serverInput.setAttribute("list", SERVER_VALUES);
    serverInput.addEventListener("change", () => {
      server = serverInput.value.trim();
      refresh();
    });
    const clientField = filterField("Client", clientInput);
    clientField.append(valueList(CLIENT_VALUES, suggest.clients));
    const serverField = filterField("Server", serverInput);
    serverField.append(valueList(SERVER_VALUES, suggest.servers));
    return filterBar(
      rangeField(rangeHours, (hours) => {
        rangeHours = hours;
        refresh(true);
      }),
      filterField("Process", sourcePick),
      filterField("Level", levelPick),
      clientField,
      serverField,
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
    return table(LOG_COLUMNS, logRows(records), "observe-table");
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
      // Suggestions first, so the inputs are attached to a populated list by
      // the time the rows land rather than filling in a moment later under
      // the pointer — the same order the Events page loads its facets in.
      void loadSuggestions().then(load);
    },
    dispose() {
      root = null;
      page = null;
      suggest = { clients: [], servers: [] };
    },
  };
}
