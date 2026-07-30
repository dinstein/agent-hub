// Config page: the GLOBAL governance layer.
//
// Every switch here merges tighten-only downward, which has one consequence
// that shapes this whole page: a lower layer can turn a gate ON but never
// OFF, so THIS SURFACE IS THE ONLY PLACE A SAFETY GATE CAN BE RELAXED. That
// makes "denyDestructive true -> false" and "blockOnInjection true -> false"
// categorically different from every other edit in the application, and they
// are rendered differently: the row is marked, the button is red, and the
// confirmation states what stops being enforced and requires an explicit
// acknowledgement before it will fire (docs/modules/controlplane.md).
//

import { hub } from "../bridge";
import { clear, el, section, table } from "../dom";
import type { Page } from "../page";
import { failureBox, noticeSlot, runWrite } from "../page";
import { button, field, selectInput, textInput } from "../ui";
import type { GovernanceList, GovernanceValue } from "../types";
import { DiscoveryModes, GovernanceKind, ResultBudgetPrefix } from "../types";

function valueEditor(entry: GovernanceValue): { node: HTMLElement; value(): string } {
  if (entry.kind === GovernanceKind.Bool) {
    const sel = selectInput(
      [
        { value: "true", label: "true (enforced)" },
        { value: "false", label: "false" },
      ],
      entry.value === "true" ? "true" : "false",
    );
    return { node: sel, value: () => sel.value };
  }
  if (entry.kind === GovernanceKind.Enum) {
    const sel = selectInput(
      [{ value: "", label: "unset" }, ...DiscoveryModes.map((m) => ({ value: m, label: m }))],
      DiscoveryModes.includes(entry.value as (typeof DiscoveryModes)[number]) ? entry.value : "",
    );
    return { node: sel, value: () => sel.value };
  }
  // Bytes and anything a newer daemon adds: a plain string, sent verbatim.
  // An unparseable value is refused by the daemon and leaves the switch
  // untouched — a typo must never read as "false".
  const input = textInput(entry.value, "e.g. 65536 or 65536! to force it");
  return { node: input, value: () => input.value.trim() };
}

export function configPage(): Page {
  let root: HTMLElement | null = null;
  const slot = noticeSlot();
  let list: GovernanceList | null = null;

  async function apply(entry: GovernanceValue, next: string): Promise<void> {
    if (next === entry.value) {
      slot.say(`${entry.key} is already "${next || "unset"}".`);
      return;
    }
    // Tightening needs no ceremony: it is the direction the merge already
    // favours, and it is reversible from this same page.
    await runWrite(
      slot,
      () => draw(),
      (r) => `${r.key}: ${r.previous || "unset"} → ${r.value || "unset"}.`,
      () => hub.setConfig(entry.key, next, list?.generation ?? 0),
    );
  }

  function budgetForm(): Node {
    const server = textInput("", "server id, or * for the default");
    const value = textInput("", "65536 — suffix ! to make it merge tighten-only");
    return el("div", { class: "form-inline" }, [
      field("Result budget for", server),
      field("Bytes", value),
      button("Set budget", "btn", () => {
        const id = server.value.trim();
        if (!id) {
          slot.say("Name a server id (or *) for the budget.", "warn");
          return;
        }
        void runWrite(
          slot,
          () => draw(),
          (r) => `${r.key}: ${r.previous || "unset"} → ${r.value || "unset"}.`,
          () => hub.setConfig(ResultBudgetPrefix + id, value.value.trim(), list?.generation ?? 0),
        );
      }),
    ]);
  }

  function row(entry: GovernanceValue): (Node | string)[] {
    const editor = valueEditor(entry);
    const save = button("Apply", "btn", () => void apply(entry, editor.value()));
    return [
      el("div", {}, [
        el("strong", { text: entry.key }),
        entry.doc ? el("div", { class: "muted", text: entry.doc }) : null,
      ]),
      el("span", { class: "mono", text: entry.value || "unset" }),
      el("div", { class: "action" }, [editor.node]),
      save,
    ];
  }

  async function draw(): Promise<void> {
    if (!root) return;
    try {
      const answer = await hub.listConfig();
      list = { ...answer, entries: answer.entries ?? [] };
    } catch (err) {
      clear(root);
      root.append(failureBox(err));
      return;
    }
    clear(root);
    root.append(
      section(
        "Governance",
        slot.node,
        table(["Key", "Current", "New value", ""], list.entries.map(row)),
        el("p", {
          class: "hint",
          text: "These switches merge tighten-only downward: a profile, a client or a session can turn one ON, never OFF.",
        }),
      ),
      section("Result budgets", budgetForm()),
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
