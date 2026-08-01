// Clients page: detecting AI clients on this machine and wiring them to the
// gateway.
//
// Detection reports STATS ONLY — never file content. The page follows that
// metadata pass with one inspection per detected client so its first frame of
// useful data already answers which clients are connected. Inspections stay
// sequential: on macOS a protected configuration may trigger a privacy
// prompt, and piling several prompts on top of each other is unusable.
//
// A location that exists but may not be inspected is rendered as its own
// state, never folded into "not found": "the client is not installed" and
// "you may not look" call for opposite user actions, and the daemon ships the
// remediation text for the second one.

import { hub } from "../bridge";
import { clear, el, empty, icon, loadingState, pageHeader, relTime } from "../dom";
import type { Page } from "../page";
import { failureBox, noticeSlot } from "../page";
import { button, confirmAction } from "../ui";
import type { ClientDetected, ClientDetectResult, ClientInspection } from "../types";

type InspectionCheck =
  | { phase: "checking" }
  | { phase: "ready"; value: ClientInspection }
  | { phase: "failed"; message: string };

export function clientsPage(): Page {
  let root: HTMLElement | null = null;
  const slot = noticeSlot();
  let detected: ClientDetectResult | null = null;
  let detectError: unknown = null;
  let refreshing = false;
  let refreshRun = 0;
  const inspections = new Map<string, InspectionCheck>();
  const connecting = new Set<string>();

  async function connect(client: string): Promise<void> {
    if (connecting.has(client)) return;
    connecting.add(client);
    slot.clear();
    renderPage();
    try {
      const res = await hub.connectClient(client, {});
      slot.say(
        res.changed
          ? `${client} is connected to AgentHub. A backup was kept before the configuration changed.`
          : `${client} was already connected — nothing changed.`,
      );
      await inspectOne(client);
    } catch (err) {
      slot.fail(err);
    } finally {
      connecting.delete(client);
      renderPage();
    }
  }

  async function disconnect(client: string): Promise<void> {
    const ok = await confirmAction({
      title: `Disconnect ${client}?`,
      body: "agenthub's gateway entry is removed from that client's configuration.",
      consequences: [
        "Only entries agenthub itself wrote are removed — ownership decides, never the name, so a hand-written server that happens to share a name survives.",
        "The client stops reaching every downstream server through this hub until it is connected again.",
      ],
      confirmLabel: "Disconnect",
      danger: true,
    });
    if (!ok) return;
    try {
      const res = await hub.disconnectClient(client);
      slot.say(
        `${res.client}: removed ${res.removed.length > 0 ? res.removed.join(", ") : "nothing"} from ${res.path}` +
          (res.backup ? ` (backup at ${res.backup})` : ""),
      );
      await draw();
      await inspectOne(client);
    } catch (err) {
      slot.fail(err);
    }
  }

  function clientCard(c: ClientDetected): Node {
    const check = inspections.get(c.client);
    let stateLabel = "Not checked";
    let stateClass = "badge-disabled";
    let stateDetail = "Connection status has not been read yet.";
    if (check?.phase === "checking") {
      stateLabel = "Checking…";
      stateDetail = "Reading this client's MCP configuration.";
    } else if (check?.phase === "failed") {
      stateLabel = "Check failed";
      stateClass = "badge-unhealthy";
      stateDetail = check.message;
    } else if (check?.phase === "ready") {
      switch (check.value.state) {
        case "connected":
          stateLabel = "Connected";
          stateClass = "badge-healthy";
          stateDetail = "This client contains an AgentHub-owned gateway entry.";
          break;
        case "not_connected":
          stateLabel = "Not connected";
          stateDetail = "The configuration was read and contains no AgentHub-owned entry.";
          break;
        case "denied":
          stateLabel = "Access denied";
          stateClass = "badge-unhealthy";
          stateDetail = check.value.note || "AgentHub could not read this client's configuration.";
          break;
        case "unreadable":
          stateLabel = "Unreadable";
          stateClass = "badge-unhealthy";
          stateDetail = check.value.note || "The configuration exists but could not be interpreted safely.";
          break;
        case "unknown":
          stateLabel = "Manual setup";
          stateClass = "badge-degraded";
          stateDetail = check.value.note || "AgentHub does not rewrite this configuration format.";
          break;
      }
    }

    const actions: Node[] = [];
    if (!check || check.phase === "failed") {
      actions.push(button(check ? "Retry status" : "Check status", "btn", () => void inspectOne(c.client)));
    } else if (check.phase === "checking") {
      const waiting = button("Checking…", "btn btn-secondary", () => {});
      waiting.disabled = true;
      actions.push(waiting);
    } else if (check.value.state === "connected") {
      actions.push(button("Disconnect", "btn btn-deny", () => void disconnect(c.client)));
    } else if (check.value.state === "not_connected" || check.value.state === "unknown") {
      const pending = connecting.has(c.client);
      const connectButton = button(
        check.value.state === "unknown" ? "Show setup" : "Connect",
        "btn btn-primary",
        () => void connect(c.client),
      );
      connectButton.disabled = pending;
      if (pending) connectButton.setAttribute("aria-busy", "true");
      actions.push(connectButton);
    } else {
      actions.push(button("Check again", "btn", () => void inspectOne(c.client)));
    }

    const capability = c.denied
      ? "File access denied"
      : c.writable
        ? "Writable configuration"
        : "Read-only configuration";
    return el("article", { class: "client-card" }, [
      el("div", { class: "client-mark", text: (c.name || c.client).slice(0, 1).toUpperCase() }),
      el("div", { class: "client-card-main" }, [
        el("div", { class: "client-card-head" }, [
          el("strong", { class: "access-title", text: c.name || c.client }),
          el("span", { class: `badge ${stateClass}`, text: stateLabel, title: stateDetail }),
        ]),
        el("div", { class: "client-meta muted" }, [
          el("span", { text: `${c.client} · ${c.placement} · ${c.shape}` }),
          el("span", { class: "client-capability", text: capability }),
        ]),
        el("div", { class: "client-path" }, [
          el("span", { class: "mono", text: c.path }),
          el("span", { class: "muted", text: `${c.size} bytes · ${relTime(c.modified)}` }),
        ]),
        check?.phase === "failed" ||
        (check?.phase === "ready" && ["denied", "unreadable", "unknown"].includes(check.value.state))
          ? el("div", {
              class: check.phase === "failed" ? "client-remediation" : "client-state-detail muted",
              text: stateDetail,
              title: stateDetail,
            })
          : null,
        c.denied && c.remediation
          ? el("div", { class: "client-remediation", text: c.remediation })
          : c.note
            ? el("div", { class: "muted", text: c.note })
            : null,
      ]),
      el("div", { class: "client-actions" }, actions),
    ]);
  }

  async function inspectOne(client: string): Promise<void> {
    if (inspections.get(client)?.phase === "checking") return;
    inspections.set(client, { phase: "checking" });
    renderPage();
    try {
      inspections.set(client, { phase: "ready", value: await hub.inspectClient(client) });
    } catch (err) {
      inspections.set(client, {
        phase: "failed",
        message: err instanceof Error ? err.message : String(err),
      });
    }
    renderPage();
  }

  // supportedHint renders the line under the table. It used to read
  // "Directly supported: <every id>", which named codex, continue and
  // open-webui as directly handled while their own rows carried a "read-only"
  // badge — and then told the reader that "anything else" needs wiring by
  // hand, when two of the three listed do. The read-only ones are now named
  // as such, from the same answer the badges come from.
  function supportedHint(d: ClientDetectResult): Node {
    const indirect = d.indirect ?? [];
    const parts = [`Supported: ${d.supported.join(", ") || "—"}.`];
    if (indirect.length > 0) {
      parts.push(
        `agenthub does not write these itself: ${indirect.join(", ")} — Connect says what to do instead.`,
      );
    }
    parts.push("Any other MCP client can still point at agenthub by hand.");
    return el("p", { class: "hint", text: parts.join(" ") });
  }

  function renderPage(): void {
    if (!root) return;
    clear(root);
    const refreshButton = button("Refresh", "btn btn-primary", () => {
      void refresh();
    });
    refreshButton.disabled = refreshing;
    if (refreshing) refreshButton.setAttribute("aria-busy", "true");
    root.append(
      pageHeader(
        "Clients",
        "Discover MCP-capable apps on this machine and connect each one to AgentHub.",
        refreshButton,
      ),
      slot.node,
      el("div", { class: "privacy-note" }, [
        el("span", { class: "privacy-note-mark" }, [icon("privacy")]),
        el("span", {
          text:
            "Refresh scans client metadata, then reads each detected configuration in sequence " +
            "to identify AgentHub-owned entries.",
        }),
      ]),
      detectError
        ? failureBox(detectError)
        : detected === null
          ? loadingState("Discovering clients…", 4)
          : detected.found.length === 0
            ? empty("No client configuration found on this machine.")
            : el("div", { class: "client-card-list" }, detected.found.map(clientCard)),
      detected ? supportedHint(detected) : el("span", {}),
    );
  }

  async function draw(render = true): Promise<void> {
    if (!root) return;
    try {
      const answer = await hub.detectClients();
      detected = {
        found: answer.found ?? [],
        supported: answer.supported ?? [],
        indirect: answer.indirect ?? [],
      };
      detectError = null;
    } catch (e) {
      detectError = e;
      detected = null;
    }
    if (render) renderPage();
  }

  async function refresh(): Promise<void> {
    if (!root || refreshing) return;
    const run = ++refreshRun;
    refreshing = true;
    renderPage();
    try {
      await draw(false);
      if (!root || run !== refreshRun) return;
      const ids = [...new Set((detected?.found ?? []).map((c) => c.client))];
      inspections.clear();
      for (const id of ids) inspections.set(id, { phase: "checking" });
      renderPage();
      for (const id of ids) {
        try {
          const value = await hub.inspectClient(id);
          if (!root || run !== refreshRun) return;
          inspections.set(id, { phase: "ready", value });
        } catch (err) {
          if (!root || run !== refreshRun) return;
          inspections.set(id, {
            phase: "failed",
            message: err instanceof Error ? err.message : String(err),
          });
        }
        renderPage();
      }
    } finally {
      if (run === refreshRun) {
        refreshing = false;
        renderPage();
      }
    }
  }

  return {
    render(node) {
      root = node;
      return refresh();
    },
    dispose() {
      refreshRun += 1;
      refreshing = false;
      root = null;
      connecting.clear();
    },
  };
}
