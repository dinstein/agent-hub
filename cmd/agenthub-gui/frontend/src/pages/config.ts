// Config page: the GLOBAL governance layer.
//
// Values here merge tighten-only downward — a lower layer may narrow but
// never widen — so this remains the only surface that can loosen a global
// setting, and internal/ctlapi's adminconfig.go allows that deliberately:
// refusing would leave an operator unable to undo what they set.
//
// NO EDIT ON THIS PAGE IS TREATED AS DANGEROUS, and none is confirmed. This
// comment used to say that "denyDestructive true -> false" and
// "blockOnInjection true -> false" were categorically different from every
// other edit, rendered with a marked row, a red button and a confirmation
// requiring explicit acknowledgement. No such rendering exists in this file
// or anywhere in the frontend, and neither key exists at all — both went with
// the removed runtime-governance surface, along with the scanning they
// switched. What is left (discovery mode, the calls policy, result budgets,
// the http face) carries no gate-relaxation semantics, so writes go straight
// through runWrite like every other edit.
//
// Describing a confirmation that is not there is how a reviewer concludes a
// control is in place when it is not; ctlapi/adminconfig.go carries the same
// correction for the audit trail it used to promise. If a key of that kind
// returns, the treatment has to be built, not restored.
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
