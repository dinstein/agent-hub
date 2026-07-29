// Secrets page: the credential vault's KEY NAMES.
//
// RED LINE (docs/modules/controlplane.md rule 5): this page has no entry point that shows a
// value, and it is not that the field is left blank — the wire type has no
// value field at all, so there is nothing here to reveal. Consequences that
// are visible in this file:
//
//   - the input is a password field with no reveal toggle, and it is cleared
//     the moment the write is submitted, whether it succeeded or not;
//   - the value is never kept in a variable that outlives the call, never put
//     in a model object, never echoed back into the form on failure;
//   - "is this credential right?" is answered by Test connection on the
//     Servers page, which makes a REAL call. agenthub sends secrets, it does
//     not read them back.

import { hub } from "../bridge";
import { clear, el, empty, section, table } from "../dom";
import type { Page } from "../page";
import { failureBox, noticeSlot } from "../page";
import { button, confirmAction, controls, field, passwordInput, textInput } from "../ui";
import type { SecretRef, Server } from "../types";
import { SecretScopeGlobal } from "../types";

function backendBadge(backend: string): HTMLElement {
  const cls =
    backend === "keyring" ? "badge-healthy" : backend === "env" ? "badge-degraded" : "badge-disabled";
  return el("span", { class: `badge ${cls}`, text: backend || "—" });
}

export function secretsPage(): Page {
  let root: HTMLElement | null = null;
  const slot = noticeSlot();
  let refs: SecretRef[] = [];
  let servers: Server[] = [];
  let filter = "";

  async function store(server: string, scope: string, key: string, input: HTMLInputElement): Promise<void> {
    const value = input.value;
    // Cleared before the await, so the plaintext never sits in a focused
    // field while a slow write is in flight.
    input.value = "";
    try {
      const res = await hub.setSecret(server, scope, key, value);
      slot.say(`${res.key} stored for ${res.server} (scope ${res.scope}).`);
      await draw();
    } catch (err) {
      slot.fail(err);
    }
  }

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

  function writeForm(): Node {
    const server = textInput("", "server id");
    const scope = textInput("", SecretScopeGlobal);
    const key = textInput("", "API_TOKEN");
    const value = passwordInput("value (never displayed again)");
    const save = button("Store secret", "btn", () => {
      const id = server.value.trim();
      const name = key.value.trim();
      if (!id || !name) {
        slot.say("A secret needs a server id and a key name.", "warn");
        return;
      }
      if (!value.value) {
        // The vault treats a blank as unset at every resolution level, so
        // storing one would report success and leave the server exactly as
        // broken as before.
        slot.say("A blank value is not stored: it would read as unset everywhere.", "warn");
        return;
      }
      void store(id, scope.value.trim(), name, value);
    });
    return el("div", { class: "form-inline" }, [
      field("Server", server),
      field("Scope", scope, `empty selects ${SecretScopeGlobal}`),
      field("Key", key),
      field("Value", value, "write-only: there is no read path on the control plane"),
      save,
    ]);
  }

  async function draw(): Promise<void> {
    if (!root) return;
    let err: unknown = null;
    try {
      const [r, s] = await Promise.all([hub.listSecrets(filter), hub.listServers()]);
      refs = r ?? [];
      servers = s ?? [];
    } catch (e) {
      err = e;
    }
    clear(root);
    const server = textInput(filter, "filter by server id (empty = all)");
    const known = el("datalist", { id: "secret-servers" }, servers.map((s) => {
      const opt = el("option") as HTMLOptionElement;
      opt.value = s.id;
      return opt;
    }));
    server.setAttribute("list", "secret-servers");

    root.append(
      section(
        "Secrets",
        controls(server, button("Filter", "btn", () => {
          filter = server.value.trim();
          void draw();
        })),
        known,
        slot.node,
        err
          ? failureBox(err)
          : refs.length === 0
            ? empty("No stored credentials.")
            : table(
                ["Server", "Scope", "Key", "Backend", ""],
                refs.map((r) => [
                  el("strong", { text: r.server }),
                  el("span", { text: r.scope || SecretScopeGlobal }),
                  el("span", { class: "mono", text: r.key }),
                  backendBadge(r.backend),
                  button("Delete", "btn btn-deny", () => void remove(r)),
                ]),
              ),
        el("p", {
          class: "hint",
          text: "Names and backends only. The backend says where a resolution would find the value today — the environment shadows both persistent stores.",
        }),
      ),
      section(
        "Store a secret",
        writeForm(),
        el("p", {
          class: "hint",
          text: "To check that a credential works, use Test connection on the Servers page: it makes a real call, which is the only verification that exists.",
        }),
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
