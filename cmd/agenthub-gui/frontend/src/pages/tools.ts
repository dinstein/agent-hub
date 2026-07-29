// Tools page: the per-tool kill switch, the presentation overrides and the
// integrity quarantine.
//
// Everything here works OFFLINE from a live connection: an operator must be
// able to disable a suspicious tool without first starting the server that
// serves it. That is the whole point of a kill switch, and it is why the
// listing comes from the governance store rather than from a tool discovery.
//
// Keys are RAW downstream tool names, never the exposed ones — a state keyed
// on the exposed name would move out from under itself the moment an override
// renamed the tool. The quarantine is the exception and is keyed by the
// EXPOSED name on purpose: that is the name an agent could have called.
//
// The description override is the NEUTRALIZATION path for a prompt-injection
// carrier: the downstream keeps its poisoned description, agenthub simply
// stops forwarding it.

import { hub } from "../bridge";
import { clear, el, empty, relTime, section, table } from "../dom";
import type { Page } from "../page";
import { failureState, noticeSlot, runWrite } from "../page";
import { FILTER_BLOCKS_GLOBAL, button, confirmAction, controls, field, formHost, textInput } from "../ui";
import type { QuarantineEntry, QuarantineList, Tool, ToolList } from "../types";
import { toolDrifted } from "../types";

// ---------------------------------------------------------------------------
// The quarantine alert and its dismissal scope (docs/modules/gui.md)
// ---------------------------------------------------------------------------

/**
 * Dismissing a quarantine alert stores a CONTENT SIGNATURE, never a boolean.
 *
 * A boolean "the user has seen the quarantine alert" is wrong in the one case
 * that matters. The operator looks at a drifted tool, decides it can wait,
 * and hides the notice. Two days later the same tool drifts again — new
 * definition, new hash, new timestamp, genuinely new information — and a
 * boolean keeps it hidden, because as far as it knows this notice was already
 * dealt with. The signature makes "dealt with" mean what the user actually
 * meant: dealt with THIS, and anything new comes back.
 *
 * The signature covers the exposed name, the origin, the current hash and the
 * timestamp, i.e. everything a re-quarantine would change. It deliberately
 * does not cover the reason text, so a wording change downstream cannot
 * resurrect an alert on its own.
 */
const DISMISS_KEY = "agenthub.quarantine.dismissed";

/** FNV-1a, 32 bit. Not a security primitive and not used as one: this is a
 *  cache key for "is this the same notice", and collisions cost at worst one
 *  alert that stays hidden until the next change. */
function signatureOf(entries: QuarantineEntry[]): string {
  const canon = entries
    // \u0000 as the field separator, written escaped. The same three
    // bytes used to sit in this file literally, which made git classify
    // the whole page as binary: no diff, no blame, and grep skipping it
    // entirely. A separator that cannot occur in the data is the right
    // idea; spelling it in raw bytes is not.
    .map((q) => `${q.exposed}\u0000${q.server}/${q.tool}\u0000${q.current_hash ?? ""}\u0000${q.at}`)
    .sort()
    .join("\n");
  let h = 0x811c9dc5;
  for (let i = 0; i < canon.length; i++) {
    h ^= canon.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h.toString(16).padStart(8, "0");
}

function dismissedSignature(): string {
  try {
    return localStorage.getItem(DISMISS_KEY) ?? "";
  } catch {
    return "";
  }
}

function dismissSignature(sig: string): void {
  try {
    localStorage.setItem(DISMISS_KEY, sig);
  } catch {
    // Storage unavailable: the alert simply reappears on the next visit,
    // which is the fail-toward-visibility direction.
  }
}

export function toolsPage(): Page {
  let root: HTMLElement | null = null;
  const slot = noticeSlot();
  const form = formHost();
  let tools: ToolList | null = null;
  let quarantine: QuarantineList | null = null;
  let filter = "";

  const generation = (): number => tools?.generation ?? 0;

  function overrideForm(t: Tool): Node {
    const name = textInput(t.override_name ?? "", t.tool);
    const description = el("textarea", { class: "input textarea" }) as HTMLTextAreaElement;
    description.value = t.override_description ?? "";
    description.rows = 4;
    description.placeholder = "replaces the downstream description verbatim";

    const apply = button("Apply override", "btn", () => {
      void runWrite(
        slot,
        () => draw(),
        (r) => `Override applied to ${r.server}/${r.tool}.`,
        () =>
          hub.setToolOverride(
            t.server,
            t.tool,
            { name: name.value.trim(), description: description.value },
            generation(),
          ),
      ).then((ok) => {
        if (ok) form.hide();
      });
    });
    const drop = button("Clear override", "btn btn-secondary", () => {
      void runWrite(
        slot,
        () => draw(),
        (r) => `Override cleared on ${r.server}/${r.tool}.`,
        () => hub.setToolOverride(t.server, t.tool, { clear: true }, generation()),
      ).then((ok) => {
        if (ok) form.hide();
      });
    });

    return el("div", { class: "panel panel-inset" }, [
      el("h3", { text: `Override ${t.server}/${t.tool}` }),
      field("Exposed name", name, "replaces the raw name before namespacing — this is what a client sees"),
      field("Description", description, "the neutralization path: the downstream keeps its text, agenthub stops forwarding it"),
      controls(apply, drop, button("Cancel", "btn btn-secondary", () => form.hide())),
    ]);
  }

  async function toggle(t: Tool): Promise<void> {
    const next = t.disabled;
    if (!next) {
      const ok = await confirmAction({
        title: `Disable ${t.server}/${t.tool}?`,
        body: "The tool stops being callable by every client immediately.",
        consequences: [
          "The kill switch is independent of the integrity approval: switching a tool off and back on neither discards an approval nor grants one.",
        ],
        confirmLabel: "Disable",
      });
      if (!ok) return;
    }
    await runWrite(
      slot,
      () => draw(),
      (r) => `${r.server}/${r.tool} ${next ? "enabled" : "disabled"}.`,
      () => hub.setToolEnabled(t.server, t.tool, next, generation()),
    );
  }

  async function release(exposed: string): Promise<void> {
    const ok = await confirmAction({
      title: `Re-approve ${exposed}?`,
      body: "The tool becomes callable again under the definition it has right now.",
      consequences: [
        "This is the human re-approve step: the definition that tripped the integrity check is the one you are accepting, and it becomes the new baseline.",
        "Nothing else is re-approved: other quarantined tools, and this tool's next change, still stop here.",
      ],
      confirmLabel: "Re-approve",
      danger: true,
      cli: `agenthub tool quarantine release ${exposed}`,
    });
    if (!ok) return;
    await runWrite(
      slot,
      () => draw(),
      (r) => `${r.exposed} released from quarantine.`,
      () => hub.releaseQuarantine(exposed, quarantine?.generation ?? 0),
    );
  }

  /**
   * The alert card above the quarantine listing, or the resting-state card
   * when there is nothing to report.
   *
   * The resting card (docs/modules/gui.md) costs one line and buys the
   * thing this product is otherwise structurally bad at: protection nobody
   * ever sees produces no trust. A user who has never had a tool tampered
   * with has, from the interface alone, no evidence that anything is
   * watching — and will conclude, correctly for all they can tell, that
   * nothing is.
   */
  function quarantineAlert(entries: QuarantineEntry[]): Node {
    if (entries.length === 0) {
      return el("div", { class: "resting" }, [
        el("span", { class: "dot neutral" }),
        el("span", {
          text: "Watching for tool tampering, description poisoning and injected output. Nothing is wrong right now.",
        }),
      ]);
    }

    const signature = signatureOf(entries);
    if (dismissedSignature() === signature) {
      // Dismissed for THIS content. The listing below and the sidebar
      // counter both still show it: dismissing is "stop shouting", never
      // "stop reporting".
      return el("p", {
        class: "hint",
        text: `${entries.length} quarantined ${entries.length === 1 ? "tool is" : "tools are"} listed below. The alert is hidden until something changes.`,
      });
    }

    const filtered = filter ? entries.filter((q) => q.server === filter) : entries;
    const keepBlocked = button("Keep blocked", "btn btn-secondary", () => {
      dismissSignature(signature);
      void draw();
    });
    if (filter) {
      // docs/modules/gui.md: a global action under a filter would dismiss
      // alerts for servers this view is not showing.
      keepBlocked.disabled = true;
      keepBlocked.title = FILTER_BLOCKS_GLOBAL;
    }

    return el("div", { class: "alert", role: "alert" }, [
      el("header", {}, [
        el("span", {
          class: "alert-title",
          text: `${entries.length} ${entries.length === 1 ? "tool was" : "tools were"} isolated by the integrity check`,
        }),
        keepBlocked,
      ]),
      el("span", {
        text:
          "These tools are blocked right now — no client can call them. Re-approve one only if you " +
          "know why its definition changed.",
      }),
      ...filtered.map((q) =>
        el("div", { class: "alert-item" }, [
          el("strong", { text: q.exposed }),
          el("span", { class: "meta", text: `${q.server}/${q.tool} · ${q.reason || "changed"} · ${relTime(q.at)}` }),
          q.pinned_hash ? el("div", { class: "meta mono", text: `approved ${q.pinned_hash}` }) : null,
          q.current_hash ? el("div", { class: "meta mono", text: `current  ${q.current_hash}` }) : null,
          controls(button("Re-approve", "btn btn-deny", () => void release(q.exposed))),
        ]),
      ),
      filter && filtered.length !== entries.length
        ? el("p", {
            class: "hint",
            text: `${entries.length - filtered.length} more quarantined ${entries.length - filtered.length === 1 ? "tool is" : "tools are"} hidden by the “${filter}” filter.`,
          })
        : null,
      el("p", {
        class: "hint",
        text: "“Keep blocked” hides this alert for exactly these tools and this definition. If any of them changes again, the alert comes back — and the counter in the sidebar never goes away while anything is quarantined.",
      }),
    ]);
  }

  function toolRow(t: Tool): (Node | string)[] {
    const drifted = toolDrifted(t);
    return [
      el("div", {}, [
        el("strong", { text: `${t.server}/${t.tool}` }),
        t.override_name ? el("div", { class: "muted", text: `exposed as ${t.override_name}` }) : null,
        t.override_description
          ? el("div", { class: "muted", text: "description overridden" })
          : null,
      ]),
      el("div", {}, [
        t.disabled
          ? el("span", { class: "badge badge-disabled", text: "disabled" })
          : el("span", { class: "badge badge-healthy", text: "enabled" }),
        t.status ? el("div", { class: "muted", text: t.status }) : null,
      ]),
      drifted
        ? el("div", {}, [
            el("span", { class: "badge badge-degraded", text: "drifted" }),
            el("div", { class: "muted mono", text: `approved ${t.approved_hash}` }),
            el("div", { class: "muted mono", text: `current  ${t.current_hash}` }),
          ])
        : el("span", { class: "muted", text: t.approved_hash ? "matches pin" : "—" }),
      controls(
        button(t.disabled ? "Enable" : "Disable", t.disabled ? "btn" : "btn btn-deny", () =>
          void toggle(t),
        ),
        button("Override", "btn", () => form.show(overrideForm(t))),
      ),
    ];
  }

  async function draw(): Promise<void> {
    if (!root) return;
    let toolErr: unknown = null;
    let quarErr: unknown = null;
    try {
      const answer = await hub.listTools(filter);
      tools = { ...answer, tools: answer.tools ?? [] };
    } catch (err) {
      tools = null;
      toolErr = err;
    }
    try {
      const answer = await hub.listQuarantine();
      quarantine = { ...answer, entries: answer.entries ?? [] };
    } catch (err) {
      quarantine = null;
      quarErr = err;
    }
    clear(root);

    const server = textInput(filter, "filter by server id (empty = all)");
    const applyFilter = button("Filter", "btn", () => {
      filter = server.value.trim();
      void draw();
    });

    root.append(
      section(
        "Tool governance",
        controls(server, applyFilter),
        slot.node,
        form.node,
        toolErr
          ? failureState(toolErr, "the tool records", () => void draw())
          : (tools?.tools ?? []).length === 0
            ? empty(
                "No tool records yet.",
                "A record appears the first time a tool is seen or governed. Connect a server on the Servers page and the catalog fills itself in.",
              )
            : table(
                ["Tool", "State", "Integrity", "Actions"],
                (tools?.tools ?? []).map(toolRow),
              ),
        el("p", {
          class: "hint",
          text: "Records are keyed by the ORIGINAL downstream tool name, so a rename never moves a rule out from under itself.",
        }),
      ),
      section(
        "Quarantine",
        quarErr
          ? failureState(quarErr, "the quarantine list", () => void draw())
          : quarantineAlert(quarantine?.entries ?? []),
        quarErr
          ? null
          : (quarantine?.entries ?? []).length === 0
            ? null
            : table(
                ["Exposed name", "Origin", "Reason", "Since", ""],
                (quarantine?.entries ?? []).map((q) => [
                  el("strong", { text: q.exposed }),
                  el("div", {}, [
                    el("div", { text: `${q.server}/${q.tool}` }),
                    q.pinned_hash
                      ? el("div", { class: "muted mono", text: `pinned ${q.pinned_hash}` })
                      : null,
                    q.current_hash
                      ? el("div", { class: "muted mono", text: `current ${q.current_hash}` })
                      : null,
                  ]),
                  el("span", { text: q.reason || "—" }),
                  el("span", { text: relTime(q.at) }),
                  button("Re-approve", "btn btn-deny", () => void release(q.exposed)),
                ]),
              ),
        el("p", {
          class: "hint",
          text: "The quarantine is keyed by the CLIENT-VISIBLE name: that is what an agent could have called.",
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
      form.hide();
    },
  };
}
