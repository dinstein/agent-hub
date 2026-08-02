// Settings page: daemon connection state, window appearance, and what this
// GUI is allowed to be.
//
// There is no hub configuration here: every setting the hub has lives in the
// daemon's registry and is reached through the control plane.
//
// The theme and the close-button behaviour are the two genuine exceptions,
// and precisely because they are not hub state: they are properties of THIS
// window on THIS machine and are stored in localStorage rather than in the
// registry. Putting either anywhere else would imply the daemon has an
// opinion about it.

import { hub } from "../bridge";
import { clear, el, icon, pageHeader } from "../dom";
import type { Page } from "../page";
import { failureBox } from "../page";
import { themeControl, toggleSwitch } from "../ui";
import type { Status } from "../types";
import { onWindowPrefs, setWindowPrefs, windowPrefs } from "../window-prefs";

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

/**
 * The close-button preference.
 *
 * Disabled, with the reason, wherever no tray icon came up: a switch that
 * silently does nothing is worse than a switch that explains why it cannot.
 * The Go side makes the same call independently — this control cannot talk it
 * into hiding a window into a status area that is not there.
 *
 * The switch does not flip itself, here or anywhere else in this codebase
 * (`toggleSwitch`, ui.ts): it renders the value it was handed and the page
 * redraws from the authoritative one afterwards. The redraw for THIS one comes
 * from the preference subscription in settingsPage, which fires for a change
 * made here and for one made from the tray menu alike — so the two surfaces
 * cannot disagree about what the close button does.
 */
function closeBehaviour(trayReady: boolean): HTMLElement {
  const prefs = windowPrefs();
  const sw = toggleSwitch({
    checked: trayReady && prefs.closeToTray,
    label: "close button minimises to tray",
    onChange: () => setWindowPrefs({ closeToTray: !windowPrefs().closeToTray }),
  });
  if (!trayReady) {
    // Not the same disabled as "a write is in flight", which is what the
    // switch normally means by it, so it does not borrow that cursor.
    sw.disabled = true;
    sw.classList.add("switch-unavailable");
  }
  return el("div", { class: "settings-appearance-choice" }, [
    el("div", { class: "settings-control" }, [
      el("div", { class: "check" }, [sw, el("span", { text: "Close button minimises to tray" })]),
    ]),
    el("p", {
      class: "hint",
      text: trayReady
        ? "AgentHub keeps running and the clients connected to it keep working. Quit from the tray menu."
        : "No tray icon is available on this system, so the close button quits — otherwise the window " +
          "would disappear with no way to bring it back.",
    }),
  ]);
}

export function settingsPage(): Page {
  let root: HTMLElement | null = null;
  let unsubscribe: (() => void) | null = null;

  async function draw(): Promise<void> {
    if (!root) return;
    let statusError: unknown = null;
    let trayReady = false;
    try {
      trayReady = await hub.trayAvailable();
    } catch {
      // Unbound service: leave it false, which is also what the close button
      // then does.
    }
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
        el("section", { class: "settings-card settings-card-wide settings-appearance" }, [
          el("div", { class: "settings-appearance-copy" }, [
            el("div", { class: "settings-icon" }, [icon("theme")]),
            el("div", {}, [
              el("h2", { text: "Appearance" }),
              el("p", { class: "muted", text: "Choose how this window follows your workspace." }),
            ]),
          ]),
          el("div", { class: "settings-appearance-choice" }, [
            el("div", { class: "settings-control" }, [themeControl()]),
            el("p", {
              class: "hint",
              text: "System follows the OS live. This local preference is applied before the first frame is drawn.",
            }),
          ]),
        ]),
        el("section", { class: "settings-card settings-card-wide settings-appearance" }, [
          el("div", { class: "settings-appearance-copy" }, [
            el("div", { class: "settings-icon" }, [icon("window")]),
            el("div", {}, [
              el("h2", { text: "Closing the window" }),
              el("p", { class: "muted", text: "What the close button does to the application behind it." }),
            ]),
          ]),
          closeBehaviour(trayReady),
        ]),
      ]),
    );
  }

  return {
    render(node) {
      root = node;
      // Both directions of the sync land here: this page's own switch, and
      // the tray's checkbox for the same preference.
      unsubscribe = onWindowPrefs(() => void draw());
      return draw();
    },
    dispose() {
      unsubscribe?.();
      unsubscribe = null;
      root = null;
    },
  };
}
