// Server-scoped credential inventory and write-only editor.
//
// Values travel exactly once, from the password input to SetSecret. The input
// is cleared before that call is awaited, and neither ListSecrets nor any DOM
// model has a value field that could reveal one later.

import { hub } from "./bridge";
import { clear, el, empty, loadingState, table } from "./dom";
import { failureBox } from "./page";
import {
  advanced,
  button,
  confirmAction,
  controls,
  field,
  modalHost,
  passwordInput,
  textInput,
} from "./ui";
import type { SecretRef } from "./types";
import { SecretScopeGlobal } from "./types";

function backendBadge(backend: string): HTMLElement {
  const cls =
    backend === "keyring" ? "badge-healthy" : backend === "env" ? "badge-degraded" : "badge-disabled";
  return el("span", { class: `badge ${cls}`, text: backend || "—" });
}

function valueLabel(key: string): string {
  return key.endsWith("API_KEY") ? "API key" : "Secret value";
}

export function createServerSecretsManager(
  retest: (server: string) => Promise<void>,
): { open(server: string, requiredKeys?: string[]): void; hide(): void } {
  const modal = modalHost();
  let epoch = 0;

  function open(server: string, requiredKeys: string[] = []): void {
    const current = ++epoch;
    const root = el("div", { class: "modal-form server-secrets" });
    const notices = el("div", { class: "notice-slot" });
    let refs: SecretRef[] = [];
    let adding = requiredKeys.length > 0;
    const required = requiredKeys[0] ?? "";

    const load = async (): Promise<void> => {
      try {
        refs = (await hub.listSecrets(server)) ?? [];
        if (current === epoch) paint();
      } catch (err) {
        if (current !== epoch) return;
        clear(root);
        root.append(failureBox(err), controls(button("Close", "btn btn-secondary", () => modal.hide())));
      }
    };

    const remove = async (ref: SecretRef): Promise<void> => {
      const ok = await confirmAction({
        title: `Delete ${ref.key}?`,
        body: `The stored credential for ${server} (scope ${ref.scope}) is removed from this machine.`,
        consequences: [
          "A server definition that references this key will fail closed the next time it connects.",
          "The value cannot be recovered from AgentHub because no read path exists.",
        ],
        confirmLabel: "Delete",
        danger: true,
      });
      if (!ok || current !== epoch) return;
      clear(notices);
      try {
        await hub.deleteSecret(ref.server, ref.scope, ref.key);
        refs = refs.filter((item) => !(item.scope === ref.scope && item.key === ref.key));
        notices.append(el("div", { class: "notice", text: `${ref.key} removed.` }));
        paint();
        await retest(server);
      } catch (err) {
        notices.append(failureBox(err));
      }
    };

    const writer = (): Node => {
      const key = textInput(required, "API_TOKEN");
      const value = passwordInput("never displayed again");
      const scope = textInput(SecretScopeGlobal, SecretScopeGlobal);
      key.disabled = required !== "";
      const errors = el("div", { class: "notice-slot" });
      const save = button(required ? `Save ${valueLabel(required)}` : "Store secret", "btn btn-primary", () => {
        clear(errors);
        const name = key.value.trim();
        const secret = value.value;
        value.value = "";
        if (!name) {
          errors.append(el("div", { class: "notice notice-warn", text: "Enter the key name used by this Server." }));
          return;
        }
        if (!secret) {
          errors.append(el("div", {
            class: "notice notice-warn",
            text: "Enter a value. A blank secret reads as unset and is never stored.",
          }));
          return;
        }
        save.disabled = true;
        save.setAttribute("aria-busy", "true");
        void hub
          .setSecret(server, scope.value.trim() || SecretScopeGlobal, name, secret)
          .then(async () => {
            if (current !== epoch) return;
            modal.hide();
            epoch++;
            await retest(server);
          })
          .catch((err) => {
            if (current !== epoch) return;
            errors.append(failureBox(err));
            value.focus();
          })
          .finally(() => {
            save.disabled = false;
            save.removeAttribute("aria-busy");
          });
      });
      return el("div", { class: "server-secret-writer" }, [
        required
          ? el("div", { class: "notice" }, [
              el("strong", { text: `${server} needs ${required}.` }),
              el("span", { text: " Store it here and AgentHub will immediately test this Server again." }),
            ])
          : null,
        errors,
        field("Key", key, required ? "required by this Server definition" : "must match its ${SECRET_KEY} placeholder"),
        field(valueLabel(required), value, "write-only; this value cannot be read back"),
        advanced("Advanced", false, field("Scope", scope, `${SecretScopeGlobal} shares it across derived instances`)),
        controls(
          save,
          button("Cancel", "btn btn-secondary", () => {
            adding = false;
            paint();
          }),
        ),
      ]);
    };

    const paint = (): void => {
      if (current !== epoch) return;
      clear(root);
      const nodes: (Node | null)[] = [
        el("p", {
          class: "hint",
          text: `Only key names and storage backends for ${server} are shown. Values are permanently write-only.`,
        }),
        notices,
        adding ? writer() : null,
        refs.length === 0
          ? adding
            ? null
            : empty("No stored credentials for this Server.", "Add the key named by its ${SECRET_KEY} placeholder.")
          : table(
              ["Scope", "Key", "Backend", ""],
              refs.map((ref) => [
                el("span", { text: ref.scope || SecretScopeGlobal }),
                el("span", { class: "mono", text: ref.key }),
                backendBadge(ref.backend),
                button("Delete", "btn btn-deny", () => void remove(ref)),
              ]),
            ),
        controls(
          adding ? null : button("Add secret", "btn btn-primary", () => {
            adding = true;
            paint();
          }),
          button("Close", "btn btn-secondary", () => {
            epoch++;
            modal.hide();
          }),
        ),
      ];
      root.append(...nodes.filter((node): node is Node => node !== null));
    };

    modal.show(`Secrets · ${server}`, root);
    root.append(loadingState(`Reading ${server}'s secret names…`, 2));
    void load();
  }

  return {
    open,
    hide() {
      epoch++;
      modal.hide();
    },
  };
}
