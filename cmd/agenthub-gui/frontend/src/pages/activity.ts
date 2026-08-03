import { asCallError, hub, isStalePrecondition } from "../bridge";
import { clear, el, empty, loadingState, pageHeader } from "../dom";
import type { Page } from "../page";
import { failureBox, failureState, noticeSlot } from "../page";
import type {
  AuditCallDetail,
  AuditCallSummary,
  AuditPayload,
  AuditStats,
  AuditStatus,
} from "../types";
import { copyButton } from "../ui";

type ActivityTab = "calls" | "insights" | "ledger";

const ranges = [
  { label: "1 hour", hours: 1 },
  { label: "24 hours", hours: 24 },
  { label: "7 days", hours: 24 * 7 },
  { label: "30 days", hours: 24 * 30 },
];

function sinceMillis(hours: number): number {
  return Date.now() - hours * 60 * 60 * 1000;
}

function formatBytes(value: number): string {
  if (!value) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const n = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
  const amount = value / 1024 ** n;
  return `${amount >= 10 || n === 0 ? amount.toFixed(0) : amount.toFixed(1)} ${units[n]}`;
}

function formatTime(raw: string): string {
  const date = new Date(raw);
  if (Number.isNaN(date.getTime())) return raw;
  const today = new Date();
  const sameDay = date.toDateString() === today.toDateString();
  return new Intl.DateTimeFormat(undefined, {
    ...(sameDay ? {} : { month: "short", day: "numeric" }),
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  }).format(date);
}

function targetOf(call: AuditCallSummary): string {
  if (call.server || call.tool) return [call.server, call.tool].filter(Boolean).join(" / ");
  return call.exposedTool || "Unrouted";
}

function outcomeTone(call: AuditCallSummary): string {
  if (!call.complete) return "badge-degraded";
  if (call.outcome === "success") return "badge-healthy";
  if (call.outcome === "cancelled") return "badge-disabled";
  return "badge-unhealthy";
}

function outcomeLabel(call: AuditCallSummary): string {
  return call.complete ? call.outcome || "finished" : "in progress";
}

function prettyJSON(raw: string): string | null {
  try {
    const parsed: unknown = JSON.parse(raw);
    if (parsed && typeof parsed === "object" && "content" in parsed && Array.isArray(parsed.content)) {
      for (const item of parsed.content) {
        if (!item || typeof item !== "object" || !("text" in item) || typeof item.text !== "string") continue;
        try {
          item.text = JSON.parse(item.text);
        } catch {
          // Tool text is often ordinary prose. Only unfold nested JSON when it
          // positively parses; Raw always preserves the exact ledger value.
        }
      }
    }
    return JSON.stringify(parsed, null, 2);
  } catch {
    return null;
  }
}

function payloadPanel(title: string, payload: AuditPayload, primary = false): HTMLElement {
  const raw = payload.text ?? "";
  const pretty = prettyJSON(raw);
  let prettyMode = pretty !== null;
  const code = el("pre", { class: "activity-payload-code" });
  const paint = () => {
    code.textContent = prettyMode && pretty !== null ? pretty : raw || "No payload captured";
  };
  paint();
  const actions: Node[] = [];
  if (pretty !== null) {
    const toggle = el("button", {
      class: "activity-text-action",
      type: "button",
      text: "Raw",
    });
    toggle.addEventListener("click", () => {
      prettyMode = !prettyMode;
      toggle.textContent = prettyMode ? "Raw" : "Pretty";
      paint();
    });
    actions.push(toggle);
  }
  actions.push(copyButton(
    () => prettyMode && pretty !== null ? pretty : raw,
    "Copy",
    "activity-text-action",
  ));
  return el("section", { class: `activity-payload${primary ? " activity-payload-primary" : ""}` }, [
    el("header", { class: "activity-payload-head" }, [
      el("div", {}, [
        el("h3", { text: title }),
        el("span", {
          class: "meta",
          text: payload.bytes ? `${formatBytes(payload.bytes)}${payload.truncated ? " · preview truncated" : ""}` : "Not captured",
        }),
      ]),
      el("div", { class: "activity-payload-actions" }, actions),
    ]),
    code,
  ]);
}

function detailFacts(detail: AuditCallDetail): HTMLElement {
  return el("div", { class: "activity-detail-facts" }, [
    el("div", {}, [el("span", { text: "Client" }), el("strong", { text: detail.client || "Unknown" })]),
    el("div", {}, [el("span", { text: "Started" }), el("strong", { text: formatTime(detail.time) })]),
    el("div", {}, [el("span", { text: "Duration" }), el("strong", { text: detail.durationMs ? `${detail.durationMs} ms` : "—" })]),
    el("div", {}, [el("span", { text: "Interface" }), el("strong", { text: detail.face || "—" })]),
  ]);
}

function detailPage(detail: AuditCallDetail): HTMLElement {
  const timeline = el("details", { class: "activity-timeline" }, [
    el("summary", { class: "activity-section-head" }, [
      el("strong", { text: "Lifecycle" }),
      el("span", { class: "meta", text: `${detail.events.length} events` }),
    ]),
    el("div", { class: "activity-event-list" }, detail.events.map((event) =>
      el("div", { class: "activity-event" }, [
        el("i", { class: event.outcome && event.outcome !== "success" ? "event-bad" : "" }),
        el("strong", { text: event.event }),
        el("time", { text: formatTime(event.time) }),
        event.server || event.tool
          ? el("span", { text: [event.server, event.tool].filter(Boolean).join(" / ") })
          : el("span", { class: "muted", text: "Gateway" }),
        event.code ? el("span", { class: "event-code", text: event.code }) : null,
      ]),
    )),
  ]);
  return el("div", { class: "activity-detail-page" }, [
    detailFacts(detail),
    ...(detail.error
      ? [el("div", { class: "activity-call-error" }, [
          el("strong", { text: detail.code || "Call failed" }),
          el("span", { text: detail.error }),
        ])]
      : []),
    payloadPanel("Request", detail.request, true),
    payloadPanel("Result", detail.result, true),
    timeline,
  ]);
}

function callDrawer(
  call: AuditCallSummary,
  load: () => Promise<AuditCallDetail>,
  close: () => void,
): HTMLElement {
  const overlay = el("div", { class: "activity-drawer-layer" });
  const titleID = `activity-call-${call.callId.replace(/[^a-zA-Z0-9_-]/g, "-")}`;
  const drawer = el("aside", {
    class: "activity-drawer", role: "dialog", "aria-modal": "true", "aria-labelledby": titleID,
  });
  const body = el("div", { class: "activity-drawer-body" }, [loadingState("Opening call…", 5)]);
  const closeButton = el("button", {
    class: "activity-close",
    type: "button",
    text: "×",
    title: "Close details",
    "aria-label": "Close details",
  });
  closeButton.addEventListener("click", close);
  overlay.addEventListener("click", (event) => {
    if (event.target === overlay) close();
  });
  overlay.addEventListener("keydown", (event) => {
    if (event.key === "Escape") close();
    if (event.key !== "Tab") return;
    const focusable = Array.from(drawer.querySelectorAll<HTMLElement>(
      "button:not([disabled]), input:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex='-1'])",
    ));
    if (focusable.length === 0) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  });
  drawer.append(
    el("header", { class: "activity-drawer-head" }, [
      el("div", {}, [
        el("div", { class: "activity-drawer-titleline" }, [
          el("h2", { id: titleID, text: targetOf(call) }),
          el("span", { class: `badge ${outcomeTone(call)}`, text: outcomeLabel(call) }),
        ]),
        el("p", { class: "activity-drawer-meta" }, [
          el("span", { class: "mono", text: call.callId }),
          el("span", { text: "Local decrypted preview · cleared on close" }),
        ]),
      ]),
      closeButton,
    ]),
    body,
  );
  overlay.append(drawer);
  queueMicrotask(() => closeButton.focus());
  void load()
    .then((detail) => {
      if (!overlay.isConnected) return;
      clear(body);
      body.append(detailPage(detail));
      body.scrollTop = 0;
    })
    .catch((err) => {
      if (!overlay.isConnected) return;
      clear(body);
      body.append(failureBox(err));
    });
  return overlay;
}

function distribution(title: string, values: Record<string, number>, total: number): HTMLElement {
  const entries = Object.entries(values).sort((a, b) => b[1] - a[1]).slice(0, 6);
  return el("section", { class: "activity-breakdown" }, [
    el("h3", { text: title }),
    entries.length === 0
      ? el("p", { class: "muted", text: "No data in this range." })
      : el(
          "div",
          { class: "activity-bars" },
          entries.map(([label, count]) =>
            el("div", { class: "activity-bar" }, [
              el("div", {}, [el("span", { text: label || "Unknown" }), el("b", { text: String(count) })]),
              el("i", { style: `width:${Math.max(3, (count / Math.max(total, 1)) * 100)}%` }),
            ]),
          ),
        ),
  ]);
}

export function activityPage(): Page {
  const pageSize = 50;
  let root: HTMLElement | null = null;
  let tab: ActivityTab = "calls";
  let rangeHours = 24;
  let clientFilter = "";
  let serverFilter = "";
  let toolFilter = "";
  let outcome = "";
  let status: AuditStatus | null = null;
  let calls: AuditCallSummary[] = [];
  let callTotal = 0;
  let nextCursor = "";
  let pageIndex = 0;
  let pageCursors = [""];
  let stats: AuditStats | null = null;
  let loadError: unknown = null;
  let drawer: HTMLElement | null = null;
  let restoreFocus: HTMLElement | null = null;
  let epoch = 0;
  let callsEpoch = 0;
  const notices = noticeSlot();

  function closeDrawer(): void {
    drawer?.remove();
    drawer = null;
    restoreFocus?.focus();
    restoreFocus = null;
  }

  function openCall(call: AuditCallSummary): void {
    closeDrawer();
    restoreFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    drawer = callDrawer(call, () => hub.auditCall(call.callId), closeDrawer);
    document.body.append(drawer);
  }

  async function load(): Promise<void> {
    const request = ++epoch;
    const callsRequest = ++callsEpoch;
    loadError = null;
    try {
      const [nextStatus, nextCalls, nextStats] = await Promise.all([
        hub.auditStatus(),
        hub.auditCalls(
          sinceMillis(rangeHours), pageSize, pageCursors[pageIndex],
          clientFilter, serverFilter, toolFilter, outcome,
        ),
        hub.auditStats(sinceMillis(rangeHours)),
      ]);
      if (!root || request !== epoch || callsRequest !== callsEpoch) return;
      status = nextStatus;
      calls = nextCalls.calls ?? [];
      callTotal = nextCalls.total ?? calls.length;
      nextCursor = nextCalls.nextCursor ?? "";
      stats = nextStats;
    } catch (err) {
      if (!root || request !== epoch || callsRequest !== callsEpoch) return;
      loadError = err;
    }
    draw();
  }

  function resetPages(): void {
    pageIndex = 0;
    pageCursors = [""];
    nextCursor = "";
  }

  async function loadCalls(): Promise<void> {
    const request = ++callsEpoch;
    loadError = null;
    try {
      const next = await hub.auditCalls(
        sinceMillis(rangeHours), pageSize, pageCursors[pageIndex],
        clientFilter, serverFilter, toolFilter, outcome,
      );
      if (!root || request !== callsEpoch) return;
      calls = next.calls ?? [];
      callTotal = next.total ?? calls.length;
      nextCursor = next.nextCursor ?? "";
    } catch (err) {
      if (!root || request !== callsEpoch) return;
      loadError = err;
    }
    draw();
  }

  async function action<T>(
    button: HTMLButtonElement,
    work: () => Promise<T>,
    message: string | ((result: T) => string) | null,
  ): Promise<void> {
    button.setAttribute("aria-busy", "true");
    try {
      const result = await work();
      if (message !== null) notices.say(typeof message === "function" ? message(result) : message);
      await load();
    } catch (err) {
      if (isStalePrecondition(asCallError(err))) {
        await load();
        notices.say("Configuration changed elsewhere. The current state has been reloaded.", "warn");
      } else {
        notices.fail(err);
      }
    } finally {
      button.removeAttribute("aria-busy");
    }
  }

  function tabs(): HTMLElement {
    return el(
      "div",
      { class: "activity-tabs", role: "tablist" },
      (["calls", "insights", "ledger"] as ActivityTab[]).map((name) => {
        const button = el("button", {
          type: "button",
          role: "tab",
          text: name[0].toUpperCase() + name.slice(1),
          "aria-selected": String(tab === name),
        });
        button.addEventListener("click", () => {
          tab = name;
          draw();
        });
        return button;
      }),
    );
  }

  function statusStrip(): HTMLElement {
    if (!status) return el("span", {});
    const toggle = el("button", {
      class: status.enabled ? "btn btn-sm" : "btn btn-primary btn-sm",
      type: "button",
      text: status.enabled ? "Pause recording" : "Enable recording",
    });
    toggle.addEventListener("click", () => {
      if (!status) return;
      void action(
        toggle,
        () => hub.setAuditEnabled(!status!.enabled, status!.generation),
        null,
      );
    });
    return el("section", { class: `activity-status ${status.enabled ? "is-on" : "is-off"}` }, [
      el("div", { class: "activity-status-dot" }),
      el("div", { class: "activity-status-copy" }, [
        el("strong", { text: status.enabled ? "Recording gateway calls" : "Activity recording is off" }),
        el("span", {
          text: status.enabled
            ? `${status.storage.days} day${status.storage.days === 1 ? "" : "s"} · ${formatBytes(status.storage.bytes)} stored locally`
            : "Enable it to build a searchable history of client-to-server calls.",
        }),
      ]),
      toggle,
    ]);
  }

  function callsView(): HTMLElement {
    const range = el("select", { class: "input activity-range", "aria-label": "Time range" });
    for (const item of ranges) {
      const option = el("option", { value: item.hours, text: item.label });
      if (item.hours === rangeHours) option.selected = true;
      range.append(option);
    }
    range.addEventListener("change", () => {
      rangeHours = Number(range.value);
      resetPages();
      void load();
    });
    const filterSelect = (
      label: string,
      allLabel: string,
      current: string,
      values: Record<string, number>,
      onChange: (value: string) => void,
    ): HTMLElement => {
      const select = el("select", { class: "input", "aria-label": label });
      select.append(el("option", { value: "", text: allLabel }));
      if (current && !(current in values)) {
        select.append(el("option", { value: current, text: `${current} (0)`, selected: true }));
      }
      for (const [value, count] of Object.entries(values).sort((a, b) => a[0].localeCompare(b[0]))) {
        if (!value) continue;
        const option = el("option", { value, text: `${value || "Unknown"} (${count})` });
        if (value === current) option.selected = true;
        select.append(option);
      }
      select.addEventListener("change", () => {
        onChange(select.value);
        resetPages();
        void loadCalls();
      });
      return el("label", { class: "activity-filter-field" }, [
        el("span", { text: label }),
        select,
      ]);
    };
    const clientSelect = filterSelect("Client", "All clients", clientFilter, stats?.clients ?? {}, (value) => {
      clientFilter = value;
    });
    const serverSelect = filterSelect("Server", "All servers", serverFilter, stats?.servers ?? {}, (value) => {
      serverFilter = value;
      if (toolFilter && value && !(toolFilter in (stats?.serverTools?.[value] ?? {}))) {
        toolFilter = "";
      }
    });
    const toolValues = serverFilter ? stats?.serverTools?.[serverFilter] ?? {} : stats?.tools ?? {};
    const toolSelect = filterSelect("Tool", "All tools", toolFilter, toolValues, (value) => {
      toolFilter = value;
    });
    const outcomeSelect = el("select", { class: "input activity-outcome", "aria-label": "Outcome" });
    for (const [value, label] of [["", "All outcomes"], ["success", "Success"], ["tool_error", "Tool error"], ["denied", "Denied"], ["protocol_error", "Protocol error"], ["cancelled", "Cancelled"]]) {
      const option = el("option", { value, text: label });
      if (value === outcome) option.selected = true;
      outcomeSelect.append(option);
    }
    outcomeSelect.addEventListener("change", () => {
      outcome = outcomeSelect.value;
      resetPages();
      void loadCalls();
    });
    const outcomeField = el("label", { class: "activity-filter-field" }, [
      el("span", { text: "Outcome" }),
      outcomeSelect,
    ]);
    const rangeField = el("label", { class: "activity-filter-field" }, [
      el("span", { text: "Time range" }),
      range,
    ]);
    const body = calls.length === 0
      ? empty(
          callTotal === 0 && !clientFilter && !serverFilter && !toolFilter && !outcome
            ? "No calls in this range"
            : "No calls match these filters",
          callTotal === 0 && !clientFilter && !serverFilter && !toolFilter && !outcome
            ? "New gateway calls will appear here while recording is enabled."
            : "Try clearing one of the dropdown filters.",
        )
      : el("div", { class: "activity-call-list" }, calls.map((call) => {
          const row = el("button", { class: "activity-call-row", type: "button" }, [
            el("time", { text: formatTime(call.time) }),
            el("div", { class: "activity-call-client" }, [
              el("strong", { text: call.client || "Unknown client" }),
              el("span", { text: call.face || "gateway" }),
            ]),
            el("strong", { class: "activity-call-server", text: call.server || "Unrouted" }),
            el("div", { class: "activity-call-tool" }, [
              el("strong", { text: call.tool || call.exposedTool || "—" }),
              call.exposedTool && call.exposedTool !== call.tool
                ? el("span", { class: "mono", text: call.exposedTool })
                : null,
            ]),
            el("span", { class: `badge ${outcomeTone(call)}`, text: outcomeLabel(call) }),
            el("span", { class: "activity-duration", text: call.durationMs ? `${call.durationMs} ms` : "—" }),
            el("span", { class: "activity-row-arrow", text: "›", "aria-hidden": "true" }),
          ]);
          row.addEventListener("click", () => openCall(call));
          return row;
        }));
    const previous = el("button", { class: "btn btn-sm", type: "button", text: "Previous" });
    previous.disabled = pageIndex === 0;
    previous.addEventListener("click", () => {
      if (pageIndex === 0) return;
      pageIndex--;
      void loadCalls();
    });
    const next = el("button", { class: "btn btn-sm", type: "button", text: "Next" });
    next.disabled = nextCursor === "";
    next.addEventListener("click", () => {
      if (!nextCursor) return;
      pageIndex++;
      pageCursors[pageIndex] = nextCursor;
      pageCursors.length = pageIndex + 1;
      void loadCalls();
    });
    const first = callTotal === 0 ? 0 : pageIndex * pageSize + 1;
    const last = callTotal === 0 ? 0 : first + calls.length - 1;
    return el("div", { class: "activity-workspace" }, [
      el("div", { class: "activity-toolbar" }, [clientSelect, serverSelect, toolSelect, outcomeField, rangeField]),
      el("div", { class: "activity-table-head" }, [
        el("span", { text: "Time" }), el("span", { text: "Client" }), el("span", { text: "Server" }),
        el("span", { text: "Tool" }),
        el("span", { text: "Outcome" }), el("span", { text: "Duration" }), el("span", {}),
      ]),
      body,
      el("footer", { class: "activity-pagination" }, [
        el("span", { text: callTotal ? `${first}–${last} of ${callTotal} calls` : "0 calls" }),
        el("div", { class: "activity-page-actions" }, [previous, next]),
      ]),
    ]);
  }

  function insightsView(): HTMLElement {
    if (!stats) return loadingState("Calculating activity…", 5);
    const success = stats.outcomes.success ?? 0;
    const finished = Object.values(stats.outcomes).reduce((sum, count) => sum + count, 0);
    const rate = finished ? Math.round((success / finished) * 100) : 0;
    return el("div", { class: "activity-insights" }, [
      el("div", { class: "activity-metrics" }, [
        metric("Calls", String(stats.calls), `in the last ${rangeHours === 24 ? "24 hours" : `${rangeHours} hours`}`),
        metric("Success rate", `${rate}%`, `${success} successful`),
        metric("Incomplete", String(stats.incomplete), "missing a finished event"),
        metric("Payload stored", formatBytes(stats.payloadStoredBytes), `${formatBytes(stats.payloadRawBytes)} before encryption`),
      ]),
      el("div", { class: "activity-breakdown-grid" }, [
        distribution("Outcomes", stats.outcomes, finished),
        distribution("Servers", stats.servers, finished),
        distribution("Clients", stats.clients, stats.calls),
        distribution("Tools", stats.tools, finished),
      ]),
    ]);
  }

  function ledgerView(): HTMLElement {
    if (!status) return loadingState("Reading ledger policy…", 4);
    const verify = el("button", { class: "btn", type: "button", text: "Verify integrity" });
    verify.addEventListener("click", () => void action(verify, async () => {
      const result = await hub.verifyAudit();
      if (!result.ok) throw new Error(`${result.failures} integrity failure(s): ${(result.issues ?? []).join("; ")}`);
      return result;
    }, (result) => `Verified ${result.events} events and ${result.payloads} payloads.`));
    const preview = el("button", { class: "btn", type: "button", text: "Preview cleanup" });
    preview.addEventListener("click", () => void action(
      preview,
      () => hub.pruneAudit(true),
      (result) => result.days
        ? `${result.days} expired day(s), ${formatBytes(result.bytes)}, are eligible for cleanup.`
        : "Nothing is currently eligible for cleanup.",
    ));
    const prune = el("button", { class: "btn", type: "button", text: "Remove expired" });
    prune.addEventListener("click", () => void action(prune, () => hub.pruneAudit(false), "Expired ledger partitions removed."));
    const rotate = el("button", { class: "btn", type: "button", text: "Rotate key" });
    rotate.addEventListener("click", () => void action(rotate, () => hub.rotateAuditKey(status!.generation), "A new encryption key is active; older keys were retained for history."));
    return el("div", { class: "activity-ledger" }, [
      el("section", { class: "activity-ledger-card" }, [
        el("div", { class: "activity-ledger-title" }, [
          el("div", {}, [el("h2", { text: "Capture policy" }), el("p", { text: "Strict local evidence for every gateway tool-call lifecycle." })]),
          el("span", { class: `badge ${status.enabled ? "badge-healthy" : "badge-disabled"}`, text: status.enabled ? "recording" : "paused" }),
        ]),
        el("div", { class: "activity-policy-grid" }, [
          policy("Arguments", status.arguments),
          policy("Results", `${status.results} · ${formatBytes(status.resultBytes)}`),
          policy("Durability", status.durability),
          policy("Retention", `${status.retentionDays} days`),
          policy("Storage cap", formatBytes(status.maxBytes)),
          policy("Free-space reserve", formatBytes(status.minFreeBytes)),
        ]),
      ]),
      el("section", { class: "activity-ledger-card" }, [
        el("div", { class: "activity-ledger-title" }, [
          el("div", {}, [el("h2", { text: "Ledger health" }), el("p", { text: "Authenticate stored metadata and encrypted payload bindings." })]),
        ]),
        el("div", { class: "activity-storage-row" }, [
          el("strong", { text: formatBytes(status.storage.bytes) }),
          el("span", { text: `${status.storage.days} day partitions · ${status.storage.packFiles} encrypted packs` }),
        ]),
        el("div", { class: "activity-ledger-actions" }, [verify, preview, prune, rotate]),
        el("p", { class: "hint", text: `Key ${status.keyId || "not created"}. Rotation keeps prior keys so existing history stays readable.` }),
      ]),
    ]);
  }

  function draw(): void {
    if (!root) return;
    clear(root);
    const refresh = el("button", { class: "btn", type: "button", text: "Refresh" });
    refresh.addEventListener("click", () => {
      refresh.setAttribute("aria-busy", "true");
      resetPages();
      void load().finally(() => refresh.removeAttribute("aria-busy"));
    });
    root.append(
      pageHeader("Calls", "Inspect every gateway-to-server call, understand usage, and maintain the encrypted local ledger.", refresh),
      notices.node,
    );
    if (loadError) {
      root.append(failureState(loadError, "activity and audit policy", () => void load()));
      return;
    }
    if (!status) {
      root.append(loadingState("Reading activity…", 6));
      return;
    }
    root.append(statusStrip(), tabs());
    if (tab === "calls") root.append(callsView());
    if (tab === "insights") root.append(insightsView());
    if (tab === "ledger") root.append(ledgerView());
  }

  return {
    render(node) {
      root = node;
      draw();
      return load();
    },
    dispose() {
      epoch++;
      callsEpoch++;
      closeDrawer();
      root = null;
    },
  };
}

function metric(label: string, value: string, detail: string): HTMLElement {
  return el("section", { class: "activity-metric" }, [
    el("span", { text: label }),
    el("strong", { text: value }),
    el("small", { text: detail }),
  ]);
}

function policy(label: string, value: string): HTMLElement {
  return el("div", {}, [el("span", { text: label }), el("strong", { text: value })]);
}
