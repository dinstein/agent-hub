// Settings page: daemon connection state, window appearance, and what this
// GUI is allowed to be.
//
// There is no hub configuration here: every setting the hub has lives in the
// daemon's registry and is reached through the control plane. Until the
// corresponding endpoints exist, the page states the CLI command instead of
// growing a second, GUI-only source of truth.
//
// The theme is the one genuine exception and precisely because it is not hub
// state: it is a property of THIS window on THIS machine, has no CLI
// equivalent because there is nothing for a CLI to do with it, and is stored
// in localStorage rather than the registry. Putting it anywhere else would
// imply the daemon has an opinion about it.

import { hub } from "../bridge";
import { clear, el, section } from "../dom";
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
    const st = await hub.status();
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

    root.append(
      section("Daemon", statusTable(st), slot, el("div", { class: "controls" }, [retry])),
      section(
        "Appearance",
        el("div", { class: "controls" }, [themeControl()]),
        el("p", {
          class: "hint",
          text: "“System” follows the OS and keeps following it while the window is open. The choice is stored in this window's local storage, not in the hub, and applies before the first frame is drawn.",
        }),
      ),
      section(
        "What this window can do",
        el("p", {
          text:
            "The GUI is a control-plane client with no privileges of its own: it cannot read the data " +
            "directory, cannot speak MCP and cannot do anything the agenthub CLI cannot. If something is " +
            "missing here, the endpoint is missing — not the permission.",
        }),
        el("ul", {}, [
          el("li", { text: "agenthub doctor — diagnose configuration and connectivity" }),
          el("li", { text: "agenthub server ls / add — manage downstream servers" }),
          el("li", { text: "agenthub approval watch — the terminal counterpart of the Approvals page" }),
          el("li", { text: "agenthub audit tail -f — the full activity stream" }),
          el("li", { text: "agenthub daemon start / stop / restart — lifecycle" }),
        ]),
      ),
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
