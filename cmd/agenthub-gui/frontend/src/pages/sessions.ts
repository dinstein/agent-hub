// Sessions page: the live sessions and the narrow-only scope controls.
//
// Scope mutations from a frontend can only TIGHTEN (ruling #8): disable a
// server for a session, restrict it to a tool subset, or reset back to the
// static baseline. Widening is a human grant with its own approval flow and
// is deliberately absent here — the daemon would refuse it with
// E_TIGHTEN_ONLY anyway, and this page must not suggest an action the
// governance model forbids.

import { EVT, hub, on } from "../bridge";
import { clear, el, empty, relTime, section, table } from "../dom";
import type { Page } from "../page";
import { failureBox } from "../page";
import type { ScopeNarrow, SessionInfo, TopicEvent } from "../types";

export function sessionsPage(): Page {
  let off: (() => void) | null = null;
  let root: HTMLElement | null = null;
  let notice: HTMLElement | null = null;

  async function apply(sessionID: string, narrow: ScopeNarrow, what: string): Promise<void> {
    try {
      await hub.setSessionScope(sessionID, narrow);
      say(`${what} applied to ${sessionID}`);
    } catch (err) {
      clear(notice!);
      notice!.append(failureBox(err));
      return;
    }
    await draw();
  }

  function say(message: string): void {
    if (!notice) return;
    clear(notice);
    notice.append(el("div", { class: "notice", text: message }));
  }

  function controls(s: SessionInfo): Node {
    const server = el("input", { class: "input", type: "text" }) as HTMLInputElement;
    server.placeholder = "server id";
    const tools = el("input", { class: "input", type: "text" }) as HTMLInputElement;
    tools.placeholder = "tool1,tool2 (empty = whole server)";

    const narrowBtn = el("button", { class: "btn", type: "button", text: "Narrow" });
    narrowBtn.addEventListener("click", () => {
      const id = server.value.trim();
      if (!id) {
        say("Enter a server id to narrow.");
        return;
      }
      const list = tools.value
        .split(",")
        .map((t) => t.trim())
        .filter(Boolean);
      const narrow: ScopeNarrow =
        list.length > 0 ? { tools: { [id]: list } } : { disable_servers: [id] };
      void apply(s.id, narrow, list.length > 0 ? `tool restriction on ${id}` : `disable ${id}`);
    });

    const resetBtn = el("button", { class: "btn btn-secondary", type: "button", text: "Reset overlay" });
    resetBtn.addEventListener("click", () => void apply(s.id, { reset: true }, "overlay reset"));

    return el("div", { class: "controls" }, [server, tools, narrowBtn, resetBtn]);
  }

  async function draw(): Promise<void> {
    if (!root) return;
    let sessions: SessionInfo[];
    try {
      sessions = await hub.listSessions();
    } catch (err) {
      clear(root);
      root.append(failureBox(err));
      return;
    }
    clear(root);
    notice = el("div", { class: "notice-slot" });
    if (sessions.length === 0) {
      root.append(
        section(
          "Sessions",
          empty("No live sessions. Sessions exist only while a client is connected — they are never persisted."),
        ),
      );
      return;
    }
    const rows = sessions.map((s) => [
      el("div", {}, [
        el("strong", { text: s.id }),
        el("div", { class: "muted", text: `${s.client_id} · ${s.origin}` }),
      ]),
      el("div", {}, [
        el("div", { text: s.profile_name || "—" }),
        s.root ? el("div", { class: "muted", text: s.root }) : null,
      ]),
      el("span", { class: s.overlay_summary ? "" : "muted", text: s.overlay_summary || "baseline" }),
      el("span", { text: relTime(s.last_seen) }),
      controls(s),
    ]);
    root.append(
      section(
        "Sessions",
        notice,
        table(["Session", "Profile / root", "Overlay", "Last seen", "Narrow scope"], rows),
        el("p", {
          class: "hint",
          text: "Overlays are volatile: they disappear when the daemon restarts and are never written to disk.",
        }),
      ),
    );
  }

  return {
    render(node) {
      root = node;
      off = on<TopicEvent>(EVT.sessions, () => void draw());
      return draw();
    },
    dispose() {
      off?.();
      off = null;
      root = null;
      notice = null;
    },
  };
}
