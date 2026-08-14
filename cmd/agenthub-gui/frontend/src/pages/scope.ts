// Scope page: which profile one client is on.
//
// That is the WHOLE client layer, and the page is small because the model is
// (docs/model.md). A client selects a profile; it never narrows on
// top of one. Narrowing has exactly one home — the profile — so a client that
// needs a different surface is bound to a different profile.
//
// This page used to offer three more fields: a server subset, a per-server
// tool selector and a discovery mode, all stored on the client entry. They
// were retired together, and the daemon now answers a request carrying any of
// them with a 400 naming the field (internal/ctlapi/adminscope.go). Offering
// them here would be a form whose only possible outcome is that error — and
// the fields read as protection while producing none, which is the direction
// the retirement was about.

import { hub } from "../bridge";
import { clear, el, section } from "../dom";
import type { Page } from "../page";
import { noticeSlot, runWrite } from "../page";
import { button, confirmAction, controls, field, group, selectInput, textInput } from "../ui";
import type { ClientBinding, ScopeDetail } from "../types";
import { Binding } from "../types";

export function scopePage(): Page {
  let root: HTMLElement | null = null;
  let body: HTMLElement | null = null;
  const slot = noticeSlot();
  let client = "";
  let detail: ScopeDetail | null = null;
  let profiles: string[] = [];

  async function load(name: string): Promise<void> {
    client = name.trim();
    if (!client) {
      detail = null;
      paint();
      return;
    }
    try {
      const [d, plist] = await Promise.all([hub.getScope(client), hub.listProfiles()]);
      detail = d;
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
        { value: Binding.Named, label: "Follow a named profile" },
        { value: Binding.FollowActive, label: "Follow the globally active profile" },
      ],
      d.entry?.profileRef?.kind ?? (d.entry?.profile ? Binding.Named : Binding.FollowActive),
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

    const errors = el("div", { class: "notice-slot" });
    const save = button("Apply binding", "btn", () => {
      clear(errors);
      if (kind.value === Binding.Named && !name.value) {
        errors.append(
          el("div", { class: "notice notice-warn", text: "Pick a profile to bind to." }),
        );
        return;
      }
      const binding: ClientBinding = {
        profile:
          kind.value === Binding.Named
            ? { kind: Binding.Named, name: name.value }
            : { kind: Binding.FollowActive },
      };
      void runWrite(
        slot,
        () => load(client),
        (r) =>
          `Binding for ${r.client} written.` +
          (r.dangling
            ? ` It names a profile that does not exist (${r.dangling_profile}), so this client resolves to an EMPTY scope.`
            : ""),
        () => hub.setScope(client, binding, detail?.generation ?? 0),
      );
    });

    return el("div", { class: "form" }, [
      errors,
      group("Profile", field("Binding", kind), field("Profile", name)),
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
    const bind =
      e.profileRef ??
      (e.profile ? { kind: Binding.Named, name: e.profile } : { kind: Binding.FollowActive });
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
          el("span", {
            class: "v",
            text: bind.kind === Binding.Named ? `named: ${bind.name}` : bind.kind,
          }),
        ]),
      ]),
      el("p", {
        class: "hint",
        text: "Which servers and tools that profile carries is edited on the Profiles page — one place, so a client's surface never has to be worked out from two.",
      }),
    ]);
  }

  async function clearBinding(): Promise<void> {
    const ok = await confirmAction({
      title: `Clear the binding of ${client}?`,
      body: "The stored binding is removed entirely.",
      consequences: [
        "The client falls back to the globally active profile — which may show it MORE servers than the binding did.",
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
          text: "A binding takes effect on sessions that are already running: agenthub recomputes and pushes tools/list_changed. Only 'client connect', which edits the client's own file, needs a restart.",
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
