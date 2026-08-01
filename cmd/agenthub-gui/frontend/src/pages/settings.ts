// Settings page: daemon connection state, window appearance, and what this
// GUI is allowed to be.
//
// There is no hub configuration here: every setting the hub has lives in the
// daemon's registry and is reached through the control plane.
//
// The theme is the one genuine exception and precisely because it is not hub
// state: it is a property of THIS window on THIS machine and is stored in
// localStorage rather than the registry. Putting it anywhere else would imply
// the daemon has an opinion about it.

import { hub } from "../bridge";
import { clear, el, icon, pageHeader } from "../dom";
import type { Page } from "../page";
import { failureBox } from "../page";
import { themeControl } from "../ui";
import type { Status } from "../types";

function statusTable(st: Status): HTMLElement {
  const row = (k: string, v: string) =>
    el("div", { class: "kv" }, [el("span", { class: "k", text: k }), el("span", { class: "v", text: v })]);
  return el("div", { class: "kvs" }, [
    row("State", st.connected ? "connected" : "offline"),
    row("Socket", st.socket || "—"),
    row("Daemon version", st.version || "—"),
    row("PID", st.pid ? String(st.pid) : "—"),
    row("Registry generation", String(st.generation ?? 0)),
    st.error ? row("Last error", st.error) : el("span", {}),
  ]);
}

export function settingsPage(): Page {
  let root: HTMLElement | null = null;

  async function draw(): Promise<void> {
    if (!root) return;
    let statusError: unknown = null;
    let st: Status;
    try {
      st = await hub.status();
    } catch (err) {
      // Appearance is window-local and must remain usable while the daemon is
      // offline. Represent the missing runtime answer explicitly instead of
      // letting the router replace the entire Settings page with an error.
      statusError = err;
      st = { connected: false, socket: "", version: "", pid: 0, generation: 0 };
    }
    clear(root);

    const retry = el("button", { class: "btn", type: "button", text: "Connect / retry" });
    const slot = el("div", { class: "notice-slot" });
    retry.addEventListener("click", () => {
      retry.setAttribute("disabled", "");
      hub
        .connect()
        .then(() => draw())
        .catch((err: unknown) => {
          clear(slot);
          slot.append(failureBox(err));
          retry.removeAttribute("disabled");
        });
    });
    if (statusError) slot.append(failureBox(statusError));

    root.append(
      pageHeader(
        "Settings",
        "Window preferences, daemon diagnostics, and the boundaries of this control-plane client.",
      ),
      el("div", { class: "settings-layout" }, [
        el("section", { class: "settings-card settings-card-wide" }, [
          el("div", { class: "settings-card-head" }, [
            el("div", { class: `status-orb ${st.connected ? "online" : "offline"}` }),
            el("div", {}, [
              el("h2", { text: "AgentHub daemon" }),
              el("p", {
                class: "muted",
                text: st.connected
                  ? "The desktop control plane is connected and ready."
                  : "The window cannot read or update runtime configuration.",
              }),
            ]),
            el("span", {
              class: `badge ${st.connected ? "badge-healthy" : "badge-unhealthy"}`,
              text: st.connected ? "connected" : "offline",
            }),
          ]),
          statusTable(st),
          slot,
          el("div", { class: "settings-card-actions" }, [retry]),
        ]),
        el("section", { class: "settings-card" }, [
          el("div", { class: "settings-icon" }, [icon("theme")]),
          el("h2", { text: "Appearance" }),
          el("p", { class: "muted", text: "Choose how this window follows your workspace." }),
          el("div", { class: "settings-control" }, [themeControl()]),
          el("p", {
            class: "hint",
            text: "System follows the OS live. This local preference is applied before the first frame is drawn.",
          }),
        ]),
      ]),
    );
  }

  return {
    render(node) {
      root = node;
      return draw();
    },
    dispose() {
      root = null;
    },
  };
}
