// Audit page: the merged activity view — call ledger plus security events.
//
// Two sources, on purpose:
//   - the `activity` SSE topic is the live feed (always available);
//   - AuditTail/SecurityTail backfill what happened before the window opened.
//     These are GET /v1/audit and GET /v1/security, two routes and not one
//     route with a stream selector: they carry different record types, and a
//     mis-spelled selector that silently returned the wrong stream would be a
//     governance surface reading the wrong ledger. A daemon assembled without
//     them answers E_NOT_FOUND and the page says "unavailable" rather than
//     showing an empty history.
//
// Arguments are never shown here. Records carry an args HASH only; the live
// inspect ring buffer is a separate, opt-in surface and deliberately not
// wired into this view.

import { EVT, hub, on } from "../bridge";
import { clear, clockTime, el, empty, section, table } from "../dom";
import type { Page } from "../page";
import { failureBox } from "../page";
import type { AuditRecord, SecurityEvent, TopicEvent } from "../types";

const TAIL = 200;
/** Live feed cap: the view is a window on a stream, not a store. */
const LIVE_MAX = 200;

function severityBadge(sev: string): HTMLElement {
  const cls =
    sev === "critical" || sev === "high"
      ? "badge-unhealthy"
      : sev === "warn" || sev === "medium"
        ? "badge-degraded"
        : "badge-healthy";
  return el("span", { class: `badge ${cls}`, text: sev || "info" });
}

export function auditPage(): Page {
  let root: HTMLElement | null = null;
  let liveRoot: HTMLElement | null = null;
  let off: (() => void) | null = null;
  const live: TopicEvent[] = [];

  function drawLive(): void {
    if (!liveRoot) return;
    clear(liveRoot);
    if (live.length === 0) {
      liveRoot.append(empty("Waiting for activity…"));
      return;
    }
    for (const ev of live.slice().reverse()) {
      let text: string;
      try {
        text = JSON.stringify(ev.payload);
      } catch {
        text = "";
      }
      liveRoot.append(
        el("div", { class: "live-row" }, [
          el("span", { class: "badge badge-healthy", text: ev.kind || "event" }),
          el("span", { class: "mono", text: text ?? "" }),
        ]),
      );
    }
  }

  async function drawHistory(): Promise<void> {
    if (!root) return;
    const holder = el("div", {});
    try {
      const records: AuditRecord[] = await hub.auditTail(TAIL);
      holder.append(
        records.length === 0
          ? empty("No calls recorded yet.")
          : table(
              ["Time", "Client / session", "Server / tool", "Decision", "Duration", "Args hash"],
              records
                .slice()
                .reverse()
                .map((r) => [
                  el("span", { text: clockTime(r.ts) }),
                  el("div", {}, [
                    el("div", { text: r.client || "—" }),
                    el("div", { class: "muted", text: r.session || "" }),
                  ]),
                  el("span", { text: `${r.server}/${r.tool}` }),
                  el("span", { text: r.decision }),
                  el("span", { text: `${r.durMs} ms` }),
                  el("span", { class: "mono muted", text: r.argsHash || "—" }),
                ]),
            ),
      );
    } catch (err) {
      holder.append(failureBox(err));
      holder.append(el("p", { class: "hint", text: "Meanwhile: `agenthub audit tail -f`." }));
    }

    const sec = el("div", {});
    try {
      const events: SecurityEvent[] = await hub.securityTail(TAIL);
      sec.append(
        events.length === 0
          ? empty("No security events.")
          : table(
              ["Time", "Severity", "Event", "Where", "Detail"],
              events
                .slice()
                .reverse()
                .map((e) => [
                  el("span", { text: clockTime(e.ts) }),
                  severityBadge(e.severity),
                  el("span", { text: e.event }),
                  el("span", { text: [e.server, e.tool, e.client].filter(Boolean).join(" / ") || "—" }),
                  el("span", { class: "muted", text: e.detail || "" }),
                ]),
            ),
      );
    } catch (err) {
      sec.append(failureBox(err));
    }

    clear(root);
    liveRoot = el("div", { class: "live" });
    root.append(
      section("Live activity", liveRoot),
      section("Calls", holder),
      section("Security", sec),
      el("p", {
        class: "hint",
        text: "Records carry an argument hash, never the arguments themselves.",
      }),
    );
    drawLive();
  }

  return {
    render(node) {
      root = node;
      off = on<TopicEvent>(EVT.activity, (ev) => {
        live.push(ev);
        if (live.length > LIVE_MAX) live.shift();
        drawLive();
      });
      return drawHistory();
    },
    dispose() {
      off?.();
      off = null;
      live.length = 0;
      root = null;
      liveRoot = null;
    },
  };
}
