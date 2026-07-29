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
// The safety-key list is restated client-side (types.isSafetyKey) rather than
// read off the wire: the daemon does not label its keys, and deriving the
// warning from a field that may be absent would make the loud path the one
// that can silently go quiet.

import { hub } from "../bridge";
import { clear, el, section, table } from "../dom";
import type { Page } from "../page";
import { failureBox, noticeSlot, runWrite } from "../page";
import { button, confirmAction, field, selectInput, textInput } from "../ui";
import type { GovernanceList, GovernanceValue } from "../types";
import { DiscoveryModes, GovernanceKind, ResultBudgetPrefix, isSafetyKey, relaxesSafety } from "../types";

/** What stops being enforced, per gate. Shown in the confirmation of a
 *  relaxing write — an operator turning one off is entitled to read the
 *  consequence rather than remember it. */
const RELAX_CONSEQUENCES: Record<string, string[]> = {
  denyDestructive: [
    "Destructive tool calls stop being refused outright.",
    "No lower layer can restore this: the merge is tighten-only, so every client and session inherits the relaxed setting.",
  ],
  blockOnInjection: [
    "A call whose payload trips the prompt-injection guard is no longer blocked.",
    "Detection keeps running and keeps writing security events — only the block goes away, so the damage is visible afterwards but not prevented.",
  ],
  humanApproval: [
    "Tool calls stop waiting for a human decision.",
    "Calls that would have queued for approval now execute unattended.",
  ],
};

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
    if (relaxesSafety(entry.key, entry.value, next)) {
      const ok = await confirmAction({
        title: `Turn off ${entry.key}?`,
        body: "This weakens enforcement for every client and every session of this hub.",
        consequences: RELAX_CONSEQUENCES[entry.key] ?? [
          "This gate stops being enforced everywhere.",
        ],
        confirmLabel: `Turn ${entry.key} off`,
        acknowledge: "I understand that calls allowed while this is off cannot be taken back.",
        danger: true,
      });
      if (!ok) return;
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
    const safety = isSafetyKey(entry.key);
    const enforced = entry.value === "true";
    const save = button("Apply", safety && enforced ? "btn btn-deny" : "btn", () =>
      void apply(entry, editor.value()),
    );
    return [
      el("div", {}, [
        el("strong", { text: entry.key }),
        safety
          ? el("div", { class: "badge badge-unhealthy", text: "safety gate" })
          : null,
        entry.doc ? el("div", { class: "muted", text: entry.doc }) : null,
      ]),
      el("span", {
        class: safety && !enforced ? "badge badge-unhealthy" : "mono",
        text: entry.value || "unset",
      }),
      el("div", { class: "action" }, [
        editor.node,
        safety && enforced
          ? el("span", {
              class: "hint danger-hint",
              text: "Turning this off weakens enforcement for every client and session, and this page is the only place it can be turned off at all.",
            })
          : null,
      ]),
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
