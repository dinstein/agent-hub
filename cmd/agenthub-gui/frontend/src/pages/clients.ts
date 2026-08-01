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
import type { ClientConnection, ClientDetected, ClientDetectResult } from "../types";

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
  let target = "";

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
      if (!dryRun) await draw();
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
      await draw();
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
    const access = c.denied ? "not readable" : c.writable ? "writable" : "read-only";
    const accessClass = c.denied ? "badge-unhealthy" : c.writable ? "badge-healthy" : "badge-degraded";
    return el("article", { class: "client-card" }, [
      el("div", { class: "client-mark", text: (c.name || c.client).slice(0, 1).toUpperCase() }),
      el("div", { class: "client-card-main" }, [
        el("div", { class: "client-card-head" }, [
          el("div", {}, [
            el("strong", { class: "access-title", text: c.name || c.client }),
            el("div", { class: "muted", text: `${c.client} · ${c.placement} · ${c.shape}` }),
          ]),
          el("span", { class: `badge ${accessClass}`, text: access }),
        ]),
        el("div", { class: "client-path" }, [
          el("span", { class: "mono", text: c.path }),
          el("span", { class: "muted", text: `${c.size} bytes · ${relTime(c.modified)}` }),
        ]),
        c.denied && c.remediation
          ? el("div", { class: "client-remediation", text: c.remediation })
          : c.note
            ? el("div", { class: "muted", text: c.note })
            : null,
      ]),
      el("div", { class: "client-actions" }, [
        button("Connect…", "btn", () => {
          target = c.client;
          void draw();
        }),
        button("Disconnect", "btn btn-deny", () => void disconnect(c.client)),
      ]),
    ]);
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

  async function draw(): Promise<void> {
    if (!root) return;
    let err: unknown = null;
    try {
      const answer = await hub.detectClients();
      detected = {
        found: answer.found ?? [],
        supported: answer.supported ?? [],
        indirect: answer.indirect ?? [],
      };
    } catch (e) {
      err = e;
      detected = null;
    }
    clear(root);
    root.append(
      pageHeader(
        "Clients",
        "Discover MCP-capable apps on this machine and connect each one to AgentHub.",
        button("Re-scan", "btn btn-primary", () => void draw()),
      ),
      slot.node,
      target ? connectForm(target) : el("span", {}),
      preview,
      el("div", { class: "privacy-note" }, [
        el("span", { class: "privacy-note-mark" }, [icon("privacy")]),
        el("span", {
          text: "Discovery reads file metadata only. File contents are opened only when you connect or disconnect a specific client.",
        }),
      ]),
      err
        ? failureBox(err)
        : (detected?.found ?? []).length === 0
          ? empty("No client configuration found on this machine.")
          : el("div", { class: "client-card-list" }, (detected?.found ?? []).map(clientCard)),
      detected ? supportedHint(detected) : el("span", {}),
    );
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
