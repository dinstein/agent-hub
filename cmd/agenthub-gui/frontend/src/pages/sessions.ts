// Sessions page: the live sessions.
//
// It is read-only by design. What a session may see is decided in advance by
// configuration — server and tool enable/disable, profile membership, the
// client binding — so there is nothing to change here at runtime.

import { EVT, hub, on } from "../bridge";
import { clear, el, empty, relTime, section, table } from "../dom";
import type { Page } from "../page";
import { failureBox } from "../page";
import type { SessionInfo, TopicEvent } from "../types";

export function sessionsPage(): Page {
  let off: (() => void) | null = null;
  let root: HTMLElement | null = null;
  let notice: HTMLElement | null = null;

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
      el("span", { text: relTime(s.last_seen) }),
    ]);
    root.append(
      section(
        "Sessions",
        notice,
        table(["Session", "Profile / root", "Last seen"], rows),
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
