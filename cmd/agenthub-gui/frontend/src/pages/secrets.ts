// Secrets page: the credential vault's KEY NAMES.
//
// RED LINE (docs/modules/controlplane.md rule 5): this page has no entry point
// that shows a value. The wire list type has no value field at all, and the
// writer clears its password input before awaiting the control-plane call.
// Credential correctness is verified only by a real Server self-test.

import { hub } from "../bridge";
import { clear, el, empty, pageHeader, section, table } from "../dom";
import type { Page } from "../page";
import { failureBox, noticeSlot } from "../page";
import { consumeSecretSetup, returnToServerTest } from "../secret-guidance";
import {
  advanced,
  button,
  confirmAction,
  controls,
  field,
  modalHost,
  passwordInput,
  selectInput,
  textInput,
} from "../ui";
import type { SecretRef, Server } from "../types";
import { SecretScopeGlobal } from "../types";

function backendBadge(backend: string): HTMLElement {
  const cls =
    backend === "keyring" ? "badge-healthy" : backend === "env" ? "badge-degraded" : "badge-disabled";
  return el("span", { class: `badge ${cls}`, text: backend || "—" });
}

function keyLabel(key: string): string {
  return key.endsWith("API_KEY") ? "API key" : "Secret value";
}

export function secretsPage(): Page {
  let root: HTMLElement | null = null;
  const slot = noticeSlot();
  const form = modalHost();
  let refs: SecretRef[] = [];
  let servers: Server[] = [];
  let filter = "";

  async function remove(ref: SecretRef): Promise<void> {
    const ok = await confirmAction({
      title: `Delete ${ref.key}?`,
      body: `The stored credential for ${ref.server} (scope ${ref.scope}) is removed from this machine.`,
      consequences: [
        "Any server whose definition refers to it by ${placeholder} will fail to resolve it at connect time.",
        "It cannot be recovered from agenthub: nothing here can print a value back.",
      ],
      confirmLabel: "Delete",
      danger: true,
    });
    if (!ok) return;
    try {
      const res = await hub.deleteSecret(ref.server, ref.scope, ref.key);
      slot.say(`${res.key} removed from ${res.server}.`);
      await draw();
    } catch (err) {
      slot.fail(err);
    }
  }

  function openWriter(
    initial: { server?: string; key?: string; returnToServers?: boolean } = {},
  ): void {
    const guided = Boolean(initial.server && initial.key);
    const choices = [
      { value: "", label: "Choose a server…" },
      ...servers.map((server) => ({ value: server.id, label: server.id })),
    ];
    if (initial.server && !servers.some((server) => server.id === initial.server)) {
      choices.push({ value: initial.server, label: initial.server });
    }
    const server = selectInput(choices, initial.server ?? "");
    const key = textInput(initial.key ?? "", "API_TOKEN");
    const value = passwordInput("never displayed again");
    const scope = textInput(SecretScopeGlobal, SecretScopeGlobal);
    const errors = el("div", { class: "notice-slot" });
    server.disabled = guided;
    key.disabled = guided;

    const save = button(guided ? `Save ${keyLabel(initial.key ?? "")}` : "Store secret", "btn btn-primary", () => {
      clear(errors);
      const id = server.value.trim();
      const name = key.value.trim();
      const secret = value.value;
      // Clear before the await: plaintext never remains in a focused field
      // while a slow keychain write is in flight, successful or otherwise.
      value.value = "";
      if (!id || !name) {
        errors.append(el("div", { class: "notice notice-warn", text: "Choose a server and enter the key name." }));
        return;
      }
      if (!secret) {
        errors.append(el("div", {
          class: "notice notice-warn",
          text: "Enter a value. A blank secret reads as unset and is never stored.",
        }));
        return;
      }
      save.setAttribute("aria-busy", "true");
      save.disabled = true;
      void hub
        .setSecret(id, scope.value.trim() || SecretScopeGlobal, name, secret)
        .then(async (res) => {
          form.hide();
          filter = res.server;
          if (initial.returnToServers) {
            returnToServerTest(res.server);
            return;
          }
          slot.say(`${res.key} stored for ${res.server}. Test the server to verify it.`);
          await draw();
        })
        .catch((err) => {
          errors.append(failureBox(err));
          value.focus();
        })
        .finally(() => {
          save.removeAttribute("aria-busy");
          save.disabled = false;
        });
    });

    form.show(
      guided ? `Set up ${initial.server}` : "Add secret",
      el("div", { class: "modal-form" }, [
        guided
          ? el("div", { class: "notice notice-info" }, [
              el("strong", { text: `${initial.server} needs ${initial.key}.` }),
              el("span", { text: " Store it here and AgentHub will immediately test the server again." }),
            ])
          : null,
        errors,
        field("Server", server, guided ? "selected from the failed self-test" : "the server that will receive this value"),
        field("Key", key, guided ? "required by this server definition" : "must match the ${SECRET_KEY} placeholder"),
        field(keyLabel(initial.key ?? ""), value, "write-only; this value cannot be read back"),
        advanced("Advanced", false, field("Scope", scope, `${SecretScopeGlobal} shares the value across derived instances`)),
        controls(save, button("Cancel", "btn btn-secondary", () => form.hide())),
      ]),
    );
    window.setTimeout(() => value.focus(), 0);
  }

  async function draw(): Promise<void> {
    if (!root) return;
    let err: unknown = null;
    try {
      const [r, s] = await Promise.all([hub.listSecrets(""), hub.listServers()]);
      refs = r ?? [];
      servers = s ?? [];
    } catch (e) {
      err = e;
    }
    if (!root) return;
    clear(root);

    const search = textInput(filter, "Filter by server or key…");
    search.classList.add("search-input");
    const list = el("div");
    const paintList = (): void => {
      clear(list);
      if (err) {
        list.append(failureBox(err));
        return;
      }
      const needle = filter.trim().toLowerCase();
      const shown = needle
        ? refs.filter((ref) => `${ref.server} ${ref.key}`.toLowerCase().includes(needle))
        : refs;
      if (shown.length === 0) {
        list.append(
          empty(
            needle ? `No secret name matches “${filter.trim()}”.` : "No stored credentials.",
            needle
              ? "Stored credentials are unchanged; clear the filter to see all key names."
              : "Add a write-only value for a configured server.",
            needle
              ? button("Clear filter", "btn", () => {
                  filter = "";
                  search.value = "";
                  paintList();
                })
              : button("Add secret", "btn btn-primary", () => openWriter()),
          ),
        );
        return;
      }
      list.append(
        table(
          ["Server", "Scope", "Key", "Backend", ""],
          shown.map((ref) => [
            el("strong", { text: ref.server }),
            el("span", { text: ref.scope || SecretScopeGlobal }),
            el("span", { class: "mono", text: ref.key }),
            backendBadge(ref.backend),
            button("Delete", "btn btn-deny", () => void remove(ref)),
          ]),
        ),
      );
    };
    search.addEventListener("input", () => {
      filter = search.value;
      paintList();
    });
    paintList();

    root.append(
      pageHeader(
        "Secrets",
        "Store write-only credentials by server. AgentHub shows key names and storage backends, never values.",
        button("Add secret", "btn btn-primary", () => openWriter()),
      ),
      el("div", { class: "page-toolbar" }, [
        el("div", { class: "toolbar-search toolbar-search-wide" }, [search]),
        el("span", { class: "toolbar-hint", text: "Verify a credential with Test on the Servers page." }),
      ]),
      slot.node,
      section("Stored secrets", list),
    );
  }

  return {
    async render(node) {
      root = node;
      await draw();
      const setup = consumeSecretSetup();
      if (setup) {
        openWriter({
          server: setup.server,
          key: setup.keys[0] ?? "",
          returnToServers: setup.returnToServers,
        });
      }
    },
    dispose() {
      root = null;
      form.hide();
    },
  };
}
