// Clients page: detecting AI clients on this machine and wiring them to the
// gateway.
//
// Detection reports STATS ONLY — never file content. On macOS, reading
// another application's configuration triggers a privacy prompt, and a bulk
// scan that prompts a dozen times is worse than no scan at all. Content is
// read only by the single-client actions, where a prompt is expected and
// explainable.
//
// A location that exists but may not be inspected is rendered as its own
// state, never folded into "not found": "the client is not installed" and
// "you may not look" call for opposite user actions, and the daemon ships the
// remediation text for the second one.

import { hub } from "../bridge";
import { clear, el, empty, icon, pageHeader, relTime } from "../dom";
import type { Page } from "../page";
import { failureBox, noticeSlot } from "../page";
import { button, checkboxInput, confirmAction, controls, field, selectInput, textInput } from "../ui";
import type { ClientConnection, ClientDetected, ClientDetectResult, ClientInspection } from "../types";

type InspectionCheck =
  | { phase: "checking" }
  | { phase: "ready"; value: ClientInspection }
  | { phase: "failed"; message: string };

function connectionView(c: ClientConnection): Node {
  return el("div", { class: "kvs" }, [
    kv("Client", c.client),
    kv("Profile", c.profile || "—"),
    kv("Config file", c.path || "—"),
    kv("Command", [c.entry.command, ...(c.entry.args ?? [])].join(" ")),
    kv("Backup", c.backup || "—"),
    kv("Changed", c.dry_run ? "nothing written (dry run)" : c.changed ? "yes" : "no (already wired)"),
  ]);
}

function kv(k: string, v: string): HTMLElement {
  return el("div", { class: "kv" }, [
    el("span", { class: "k", text: k }),
    el("span", { class: "v", text: v }),
  ]);
}

export function clientsPage(): Page {
  let root: HTMLElement | null = null;
  const slot = noticeSlot();
  const preview = el("div", {});
  let detected: ClientDetectResult | null = null;
  let detectError: unknown = null;
  let target = "";
  let checkingAll = false;
  const inspections = new Map<string, InspectionCheck>();

  async function connect(
    client: string,
    dryRun: boolean,
    profile: string,
    placement: string,
    path: string,
    bin: string,
  ): Promise<void> {
    try {
      const res = await hub.connectClient(client, {
        profile,
        // An explicit path names the file outright, so the daemon rejects it
        // together with a placement. The form sends one or the other.
        placement: path === "" ? placement : "",
        path,
        bin,
        dry_run: dryRun,
      });
      clear(preview);
      preview.append(
        el("h3", { text: dryRun ? `Preview for ${client}` : `Connected ${client}` }),
        connectionView(res),
      );
      slot.say(
        dryRun
          ? `Dry run for ${client}: nothing was written.`
          : res.changed
            ? `${client} now spawns agenthub as its single MCP server.`
            : `${client} already said exactly this — nothing changed.`,
      );
      if (!dryRun) {
        target = "";
        await draw();
        await inspectOne(client);
      }
    } catch (err) {
      slot.fail(err);
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
      target = "";
      await draw();
      await inspectOne(client);
    } catch (err) {
      slot.fail(err);
    }
  }

  function connectForm(client: string): Node {
    const profile = textInput("", "bind to a profile (optional)");
    // User-level is the default because the entry carries this machine's
    // absolute agenthub path, and a project-level file is meant to be
    // committed. Which servers the client may see is a profile's job, not
    // this field's.
    const placement = selectInput(
      [
        { value: "user", label: "User — this machine's home directory" },
        { value: "project", label: "Project — the daemon's working tree" },
      ],
      "user",
    );
    const path = textInput("", "configuration file override (optional)");
    const bin = textInput("", "agenthub binary override (optional)");
    const dry = checkboxInput("Dry run — show the entry, write nothing", true);
    return el("div", { class: "panel panel-inset" }, [
      el("h3", { text: `Connect ${client}` }),
      el("div", { class: "form-inline" }, [
        field("Profile", profile),
        field("Placement", placement),
        field("Config path", path),
        field("Binary", bin),
      ]),
      dry.node,
      controls(
        button("Apply", "btn", () =>
          void connect(
            client,
            dry.box.checked,
            profile.value.trim(),
            placement.value,
            path.value.trim(),
            bin.value.trim(),
          ),
        ),
        button("Cancel", "btn btn-secondary", () => {
          target = "";
          void draw();
        }),
      ),
    ]);
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
      actions.push(
        button("Connection…", "btn", () => {
          target = c.client;
          renderPage();
        }),
        button("Disconnect", "btn btn-deny", () => void disconnect(c.client)),
      );
    } else if (check.value.state === "not_connected" || check.value.state === "unknown") {
      actions.push(
        button(check.value.state === "unknown" ? "Set up…" : "Connect…", "btn btn-primary", () => {
          target = c.client;
          renderPage();
        }),
      );
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

  async function inspectAll(): Promise<void> {
    if (checkingAll) return;
    checkingAll = true;
    renderPage();
    const ids = [...new Set((detected?.found ?? []).map((c) => c.client))];
    for (const id of ids) await inspectOne(id);
    checkingAll = false;
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
    const checkAll = button(checkingAll ? "Checking connections…" : "Check connections", "btn btn-primary", () => {
      void inspectAll();
    });
    checkAll.disabled = checkingAll || (detected?.found ?? []).length === 0;
    root.append(
      pageHeader(
        "Clients",
        "Discover MCP-capable apps on this machine and connect each one to AgentHub.",
        checkAll,
        button("Re-scan", "btn", () => {
          inspections.clear();
          void draw();
        }),
      ),
      slot.node,
      target ? connectForm(target) : el("span", {}),
      preview,
      el("div", { class: "privacy-note" }, [
        el("span", { class: "privacy-note-mark" }, [icon("privacy")]),
        el("span", {
          text:
            "Discovery reads file metadata only. Use Check connections to read configuration contents " +
            "and identify AgentHub-owned entries.",
        }),
      ]),
      detectError
        ? failureBox(detectError)
        : (detected?.found ?? []).length === 0
          ? empty("No client configuration found on this machine.")
          : el("div", { class: "client-card-list" }, (detected?.found ?? []).map(clientCard)),
      detected ? supportedHint(detected) : el("span", {}),
    );
  }

  async function draw(): Promise<void> {
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
    renderPage();
  }

  return {
    render(node) {
      root = node;
      return draw();
    },
    dispose() {
      root = null;
      target = "";
      clear(preview);
    },
  };
}
