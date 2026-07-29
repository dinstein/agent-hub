// Scope page: the CLIENT layer of the five-layer scope chain.
//
// This is the PERSISTED binding, not a session overlay. The distinction is
// deliberate and is the reason the two live on different pages:
//
//   - here an operator may LOOSEN as well as tighten, because the control
//     plane is the only surface that can;
//   - Sessions is narrow-only and volatile, and the daemon refuses anything
//     that would widen it (E_TIGHTEN_ONLY).
//
// Every field of the edit is optional and an absent one is left untouched, so
// the form spells out "leave unchanged" as its own visible choice rather than
// sending a zero value that happens to mean something (api/scope.go).

import { hub, knownTools } from "../bridge";
import { clear, el, section, table } from "../dom";
import type { Page } from "../page";
import { noticeSlot, runWrite } from "../page";
import {
  button,
  confirmAction,
  controls,
  describeSelector,
  describeServerSet,
  field,
  group,
  namePicker,
  selectInput,
  textInput,
  triState,
} from "../ui";
import type { ClientBinding, ProfileTools, ScopeDetail, Server } from "../types";
import { Binding, DiscoveryModes, ToolSelect } from "../types";

/** "Leave the rule alone" as a form value. It is a separate option and not
 *  the empty string, because an empty server list means block-all and an
 *  empty discovery string means "clear the override": every zero value here
 *  already means something. */
const KEEP = "__keep__";

export function scopePage(): Page {
  let root: HTMLElement | null = null;
  let body: HTMLElement | null = null;
  const slot = noticeSlot();
  let client = "";
  let detail: ScopeDetail | null = null;
  let servers: Server[] = [];
  let profiles: string[] = [];

  async function load(name: string): Promise<void> {
    client = name.trim();
    if (!client) {
      detail = null;
      paint();
      return;
    }
    try {
      const [d, list, plist] = await Promise.all([
        hub.getScope(client),
        hub.listServers(),
        hub.listProfiles(),
      ]);
      detail = d;
      servers = list ?? [];
      profiles = (plist.profiles ?? []).map((p) => p.name);
    } catch (err) {
      detail = null;
      paint();
      slot.fail(err);
      return;
    }
    paint();
  }

  function bindingEditor(d: ScopeDetail): Node {
    const kind = selectInput(
      [
        { value: KEEP, label: "Leave the profile binding unchanged" },
        { value: Binding.Named, label: "Follow a named profile" },
        { value: Binding.FollowActive, label: "Follow the globally active profile" },
        { value: Binding.Inherit, label: "Inherit from the enclosing layer" },
      ],
      d.exists ? KEEP : Binding.FollowActive,
    );
    const name = selectInput(
      profiles.map((p) => ({ value: p, label: p })),
      d.entry?.profileRef?.name ?? d.entry?.profile ?? profiles[0] ?? "",
    );
    const syncName = (): void => {
      name.hidden = kind.value !== Binding.Named;
    };
    kind.addEventListener("change", syncName);
    syncName();

    // Three visible states, and no fourth: the wire has no spelling for
    // "remove the narrowing" — an absent list means "leave it alone", so the
    // way back to seeing every server is to clear the whole binding. An
    // option that does not exist is not offered.
    const serverMode = selectInput(
      [
        { value: KEEP, label: "Leave the server narrowing unchanged" },
        { value: ToolSelect.Only, label: "Narrow to the ticked servers" },
        { value: ToolSelect.None, label: "Block every server (empty set)" },
      ],
      KEEP,
    );
    const serverPick = namePicker(servers.map((s) => s.id), d.entry?.servers);
    const syncServers = (): void => {
      serverPick.node.hidden = serverMode.value !== ToolSelect.Only;
    };
    serverMode.addEventListener("change", syncServers);
    syncServers();

    const discovery = selectInput(
      [
        { value: KEEP, label: "Leave the discovery mode unchanged" },
        { value: "", label: "Clear the override (inherit)" },
        ...DiscoveryModes.map((m) => ({ value: m, label: m })),
      ],
      KEEP,
    );

    const toolServer = selectInput(
      [
        { value: "", label: "— no tool rule in this edit —" },
        ...servers.map((s) => ({ value: s.id, label: s.id })),
      ],
      "",
    );
    const toolHost = el("div", {});
    let toolPicker: ReturnType<typeof triState> | null = null;
    toolServer.addEventListener("change", () => {
      clear(toolHost);
      toolPicker = null;
      const id = toolServer.value;
      if (!id) return;
      void knownTools(id).then((tools) => {
        if (toolServer.value !== id) return;
        toolPicker = triState(tools.map((t) => t.tool), d.entry?.tools?.[id]);
        clear(toolHost);
        toolHost.append(toolPicker.node);
      });
    });

    const errors = el("div", { class: "notice-slot" });
    const save = button("Apply binding", "btn", () => {
      clear(errors);
      const binding: ClientBinding = {};
      if (kind.value !== KEEP) {
        if (kind.value === Binding.Named && !name.value) {
          errors.append(
            el("div", { class: "notice notice-warn", text: "Pick a profile to bind to." }),
          );
          return;
        }
        binding.profile =
          kind.value === Binding.Named
            ? { kind: Binding.Named, name: name.value }
            : { kind: kind.value };
      }
      if (serverMode.value === ToolSelect.None) {
        binding.servers = [];
      } else if (serverMode.value === ToolSelect.Only) {
        const picked = serverPick.value();
        if (picked.length === 0) {
          errors.append(
            el("div", {
              class: "notice notice-warn",
              text:
                "“Narrow to the ticked servers” with nothing ticked is refused: an empty subset " +
                "must never be mistaken for “no rule”. Tick a server, or choose “Block every server”.",
            }),
          );
          return;
        }
        binding.servers = picked;
      }
      if (discovery.value !== KEEP) binding.discovery = discovery.value;
      if (toolServer.value && toolPicker) {
        const sel = toolPicker.value();
        if (!sel.ok) {
          errors.append(el("div", { class: "notice notice-warn", text: sel.message }));
          return;
        }
        const tools: Record<string, ProfileTools> = {};
        tools[toolServer.value] = sel.selection;
        binding.tools = tools;
      }
      if (Object.keys(binding).length === 0) {
        errors.append(
          el("div", { class: "notice notice-warn", text: "Nothing to change in this edit." }),
        );
        return;
      }
      void runWrite(
        slot,
        () => load(client),
        (r) =>
          `Binding for ${r.client} written.` +
          (r.dangling ? ` It names a profile that does not exist (${r.dangling_profile}), so this client resolves to an EMPTY scope.` : ""),
        () => hub.setScope(client, binding, detail?.generation ?? 0),
      );
    });

    return el("div", { class: "form" }, [
      errors,
      group("Profile", field("Binding", kind), field("Profile", name)),
      group(
        "Server narrowing",
        field("Rule", serverMode),
        serverPick.node,
        el("p", {
          class: "hint",
          text: "There is no “remove the narrowing” here: on the wire an absent list means “leave it alone”. Clear the whole binding to give a client every server back.",
        }),
      ),
      group(
        "Tool selector",
        field("Server", toolServer, "one server per edit — each selector is its own operation"),
        toolHost,
      ),
      group("Discovery", field("Mode", discovery)),
      controls(save),
    ]);
  }

  function currentView(d: ScopeDetail): Node {
    if (!d.exists) {
      return el("p", {
        class: "muted",
        text: `${d.client} has no stored binding: it follows the globally active profile. Creating one below is what changes that.`,
      });
    }
    const e = d.entry ?? {};
    const bind = e.profileRef ?? (e.profile ? { kind: Binding.Named, name: e.profile } : { kind: Binding.FollowActive });
    const toolRows = Object.entries(e.tools ?? {});
    return el("div", {}, [
      d.dangling
        ? el("div", { class: "error" }, [
            el("strong", {
              text: `This binding names the profile "${d.dangling_profile}", which does not exist.`,
            }),
            el("span", {
              class: "hint",
              text: "The client resolves to an EMPTY scope until it is rebound — it is not seeing everything, it is seeing nothing.",
            }),
          ])
        : null,
      el("div", { class: "kvs" }, [
        el("div", { class: "kv" }, [
          el("span", { class: "k", text: "Profile" }),
          el("span", { class: "v", text: bind.kind === Binding.Named ? `named: ${bind.name}` : bind.kind }),
        ]),
        el("div", { class: "kv" }, [
          el("span", { class: "k", text: "Servers" }),
          el("span", { class: "v", text: describeServerSet(e.servers) }),
        ]),
        el("div", { class: "kv" }, [
          el("span", { class: "k", text: "Discovery" }),
          el("span", { class: "v", text: e.discovery || "inherited" }),
        ]),
      ]),
      toolRows.length > 0
        ? table(
            ["Server", "Tool selector"],
            toolRows.map(([id, sel]) => [
              el("strong", { text: id }),
              el("span", { text: describeSelector(sel) }),
            ]),
          )
        : el("p", { class: "muted", text: "No per-server tool rule on this client layer." }),
    ]);
  }

  async function clearBinding(): Promise<void> {
    const ok = await confirmAction({
      title: `Clear the binding of ${client}?`,
      body: "The stored binding is removed entirely.",
      consequences: [
        "The client falls back to the globally active profile — which may show it MORE servers than the binding did.",
        "Its per-server tool selectors on this layer are removed with it.",
      ],
      confirmLabel: "Clear binding",
      danger: true,
    });
    if (!ok) return;
    await runWrite(
      slot,
      () => load(client),
      (r) => `Binding for ${r.client} cleared.`,
      () => hub.clearScope(client, detail?.generation ?? 0),
    );
  }

  function paint(): void {
    if (!body) return;
    clear(body);
    if (!detail) {
      body.append(
        el("p", { class: "muted", text: "Enter a client id to load its persistent binding." }),
      );
      return;
    }
    body.append(
      el("h3", { text: detail.client }),
      currentView(detail),
      bindingEditor(detail),
      detail.exists
        ? controls(button("Clear binding", "btn btn-deny", () => void clearBinding()))
        : el("span", {}),
    );
  }

  function draw(): void {
    if (!root) return;
    clear(root);
    const id = textInput(client, "client id (claude, cursor, …)");
    const load1 = button("Load", "btn", () => void load(id.value));
    id.addEventListener("keydown", (ev) => {
      if ((ev as KeyboardEvent).key === "Enter") void load(id.value);
    });
    body = el("div", {});
    root.append(
      section(
        "Client scope",
        controls(id, load1),
        slot.node,
        body,
        el("p", {
          class: "hint",
          text: "This is the persisted client layer. Live sessions have their own volatile, narrow-only overlay on the Sessions page.",
        }),
      ),
    );
    paint();
  }

  return {
    render(node) {
      root = node;
      draw();
    },
    dispose() {
      root = null;
      body = null;
    },
  };
}
