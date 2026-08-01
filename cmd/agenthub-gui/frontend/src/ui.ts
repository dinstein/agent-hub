// Form and dialog widgets shared by the control-plane pages.
//
// No framework and no template strings: everything is built with
// createElement and textContent, because server ids, tool names, secret key
// names and daemon messages all originate downstream, and a proxy that
// renders untrusted strings as markup is an injection surface (see dom.ts).
//
// Two widgets here carry governance meaning rather than convenience:
//
//   - triState() is the only way this UI expresses a tool or server selector.
//     It keeps "every tool", "exactly these" and "none at all" as three
//     separate, visible states, and refuses an empty subset instead of
//     sending it — an empty `only` list is precisely the input that collapses
//     block-all into allow-all if anything downstream normalises it away.
//   - confirmAction() is the second step in front of every destructive or
//     safety-relaxing write. Its `acknowledge` variant needs an explicit tick
//     before the button works: relaxing a safety gate is the one thing in
//     this application that cannot be undone by re-tightening later (the
//     calls that got through are already through).

import { copyText } from "./bridge";
import { clear, el, errorBox, errorHeadline, icon } from "./dom";
import type { ProfileTools, ToolSelect, ToolSelector } from "./types";
import { ToolSelect as Mode } from "./types";

// ---------------------------------------------------------------------------
// Inputs
// ---------------------------------------------------------------------------

/** A labelled form row. */
export function field(label: string, control: Node, hint?: string): HTMLElement {
  return el("label", { class: "field" }, [
    el("span", { class: "field-label", text: label }),
    control,
    hint ? el("span", { class: "hint", text: hint }) : null,
  ]);
}

export function textInput(value = "", placeholder = ""): HTMLInputElement {
  const node = el("input", { class: "input", type: "text" }) as HTMLInputElement;
  node.value = value;
  node.placeholder = placeholder;
  return node;
}

/** A password input. There is no reveal toggle anywhere in this application:
 *  the control plane has no read path for a secret value, so there would be
 *  nothing to reveal it from. */
export function passwordInput(placeholder = ""): HTMLInputElement {
  const node = el("input", { class: "input", type: "password" }) as HTMLInputElement;
  node.placeholder = placeholder;
  node.autocomplete = "off";
  return node;
}

export type Option = { value: string; label: string };

export function selectInput(options: Option[], value = ""): HTMLSelectElement {
  const node = el("select", { class: "input" }) as HTMLSelectElement;
  for (const o of options) {
    const opt = el("option", { text: o.label }) as HTMLOptionElement;
    opt.value = o.value;
    node.append(opt);
  }
  node.value = value;
  return node;
}

export function checkboxInput(label: string, checked: boolean): {
  node: HTMLElement;
  box: HTMLInputElement;
} {
  const box = el("input", { type: "checkbox" }) as HTMLInputElement;
  box.checked = checked;
  return { node: el("label", { class: "check" }, [box, el("span", { text: label })]), box };
}

export function button(label: string, cls: string, onClick: () => void): HTMLButtonElement {
  const b = el("button", { class: cls, type: "button", text: label }) as HTMLButtonElement;
  b.addEventListener("click", onClick);
  return b;
}

/**
 * A switch, for a setting that takes effect the moment it is flipped.
 *
 * THE POSITION IS NEVER SET BY THE CLICK. `onChange` performs the write and
 * the page repaints from what came back, so a write that was refused, lost
 * its precondition or never reached the daemon leaves the switch showing what
 * is actually stored rather than what the user hoped. An optimistic flip that
 * has to be walked back is the same failure a removed row avoids by greying
 * out instead of vanishing (docs/modules/gui.md §2): for one moment the
 * interface states something untrue, and that is exactly the moment the user
 * looks away satisfied.
 *
 * `label` names the THING, not the action — "clerk enabled", never "Enable
 * clerk". role="switch" carries on/off in aria-checked already, so a label
 * with a verb in it is announced as "Enable clerk, switch, on".
 */
export function toggleSwitch(opts: {
  checked: boolean;
  label: string;
  onChange: () => void | Promise<void>;
}): HTMLButtonElement {
  const b = el("button", {
    class: "switch",
    type: "button",
    role: "switch",
    "aria-checked": String(opts.checked),
    "aria-label": opts.label,
  }) as HTMLButtonElement;
  b.append(el("span", { class: "switch-thumb" }));
  b.addEventListener("click", () => {
    if (b.disabled) return;
    // Held down for the whole round trip, so a second click cannot queue a
    // second write against a generation the first one is about to move.
    b.disabled = true;
    b.setAttribute("aria-busy", "true");
    void Promise.resolve(opts.onChange()).finally(() => {
      // Usually the page has redrawn by now and this node is detached. When
      // it has not — the write failed and the row survived — the control has
      // to come back, or the row is stuck at one attempt.
      b.disabled = false;
      b.removeAttribute("aria-busy");
    });
  });
  return b;
}

/** A group of controls laid out in a row. */
export function controls(...children: (Node | null)[]): HTMLElement {
  return el("div", { class: "controls" }, children.filter((c): c is Node => c !== null));
}

/** A collapsible block. Used for the halves of a form that only one transport
 *  needs, and for per-row editors that would otherwise drown the table. */
export function group(title: string, ...children: (Node | null)[]): HTMLElement {
  return el("fieldset", { class: "group" }, [
    el("legend", { text: title }),
    ...children.filter((c): c is Node => c !== null),
  ]);
}

/**
 * A collapsed section for settings that have a working default.
 *
 * `configured` is the whole point and is not optional: a section folds away
 * only while every field in it still holds its default. The moment one does
 * not — a container runtime, a pinned OAuth issuer, an env var — it opens,
 * because a form that hides a value somebody deliberately set is lying about
 * what the server is. That is worse than the long form it replaces: the
 * reader would be looking at what appears to be a plain host process while
 * the entry actually runs in Docker.
 *
 * So this shortens the CREATE path, where everything is default by
 * definition, and leaves the EDIT path as long as the entry really is.
 */
export function advanced(summary: string, configured: boolean, ...children: (Node | null)[]): HTMLElement {
  const d = el("details", { class: "adv" }, [
    el("summary", { text: summary }),
    el("div", { class: "adv-body" }, children.filter((c): c is Node => c !== null)),
  ]) as HTMLDetailsElement;
  d.open = configured;
  return d;
}

// ---------------------------------------------------------------------------
// Repeating editors
// ---------------------------------------------------------------------------

/** A key/value editor (environment variables, HTTP headers). */
export interface PairEditor {
  node: HTMLElement;
  /** The edited map; empty when no row has a key. */
  value(): Record<string, string>;
}

export function pairEditor(
  initial: Record<string, string> | null | undefined,
  opts: { keyLabel?: string; valueLabel?: string; hint?: string } = {},
): PairEditor {
  const rows = el("div", { class: "rows" });
  const add = (k = "", v = ""): void => {
    const key = textInput(k, opts.keyLabel ?? "KEY");
    const val = textInput(v, opts.valueLabel ?? "value");
    const row = el("div", { class: "row" }, [
      key,
      val,
      button("×", "btn btn-icon", () => row.remove()),
    ]);
    rows.append(row);
  };
  for (const [k, v] of Object.entries(initial ?? {})) add(k, v);
  return {
    node: el("div", { class: "editor" }, [
      rows,
      controls(button("+ add", "btn btn-secondary", () => add())),
      opts.hint ? el("span", { class: "hint", text: opts.hint }) : null,
    ]),
    value() {
      const out: Record<string, string> = {};
      for (const row of Array.from(rows.children)) {
        const inputs = row.querySelectorAll("input");
        const k = (inputs[0] as HTMLInputElement).value.trim();
        if (!k) continue;
        out[k] = (inputs[1] as HTMLInputElement).value;
      }
      return out;
    },
  };
}

/** A one-per-line string list (command arguments, scopes, docker extra args).
 *  Lines keep their internal spaces: an argument is one line, never a split
 *  on whitespace — quoting rules a GUI invents are a spawn-line bug waiting
 *  to happen. */
export interface LinesEditor {
  node: HTMLElement;
  value(): string[];
}

export function linesEditor(initial: string[] | null | undefined, placeholder = ""): LinesEditor {
  const area = el("textarea", { class: "input textarea" }) as HTMLTextAreaElement;
  area.value = (initial ?? []).join("\n");
  area.placeholder = placeholder;
  area.rows = 3;
  return {
    node: area,
    value() {
      return area.value
        .split("\n")
        .map((s) => s.trim())
        .filter(Boolean);
    },
  };
}

// ---------------------------------------------------------------------------
// The three-state selector
// ---------------------------------------------------------------------------

export type TriStateResult =
  | { ok: true; selection: ProfileTools }
  | { ok: false; message: string };

export interface TriState {
  node: HTMLElement;
  /** The edit to send, or the reason it cannot be sent. */
  value(): TriStateResult;
}

/** A tick list over known names plus a free-text tail for the ones this
 *  daemon cannot enumerate (a server that is down still has tools, and an
 *  operator must be able to name them). */
export interface NamePicker {
  node: HTMLElement;
  value(): string[];
}

export function namePicker(available: string[], chosen: string[] | undefined): NamePicker {
  const picked = new Set(chosen ?? []);
  // A stored subset may name something the live list does not know (the
  // server is down, or the entry was withdrawn). Keeping it visible and
  // ticked is the only way an edit does not silently drop it.
  const names = Array.from(new Set([...available, ...picked])).sort();
  const boxes: { name: string; box: HTMLInputElement }[] = [];
  const list = el(
    "div",
    { class: "tool-list" },
    names.length === 0
      ? [el("span", { class: "hint", text: "Nothing known to tick — type the names below." })]
      : names.map((name) => {
          const box = el("input", { type: "checkbox" }) as HTMLInputElement;
          box.checked = picked.has(name);
          boxes.push({ name, box });
          return el("label", { class: "check" }, [box, el("span", { text: name })]);
        }),
  );
  const extra = linesEditor([], "names not listed above, one per line");
  return {
    node: el("div", { class: "tool-picker" }, [list, extra.node]),
    value() {
      return Array.from(
        new Set([...boxes.filter((b) => b.box.checked).map((b) => b.name), ...extra.value()]),
      );
    },
  };
}

/**
 * The three-state tool selector: all / only these / none.
 *
 * The three states are spelled out rather than encoded as "a list that may be
 * empty" because the last two differ ONLY by an empty vs non-empty list, and
 * any encoding that can confuse a missing field with an empty one turns
 * "block every tool" into "expose every tool" the first time something
 * normalises it (api/profiles.go). So:
 *
 *   - "All tools" sends mode=all and drops the rule;
 *   - "Only these" with nothing ticked is REFUSED here, with the message
 *     saying which state the operator probably meant;
 *   - "Block all tools" sends mode=none, which is stored, not dropped.
 */
export function triState(
  available: string[],
  stored: ToolSelector | undefined,
  labels: { all: string; only: string; none: string } = {
    all: "All tools (no rule)",
    only: "Only the ticked tools",
    none: "Block every tool",
  },
): TriState {
  const initial: ToolSelect =
    stored?.allow === undefined ? Mode.All : stored.allow.length === 0 ? Mode.None : Mode.Only;

  const radioName = `tri-${Math.random().toString(36).slice(2)}`;
  const radios: Record<string, HTMLInputElement> = {};
  const radio = (value: ToolSelect, label: string): HTMLElement => {
    const box = el("input", { type: "radio" }) as HTMLInputElement;
    box.name = radioName;
    box.value = value;
    box.checked = initial === value;
    radios[value] = box;
    box.addEventListener("change", () => sync());
    return el("label", { class: "check" }, [box, el("span", { text: label })]);
  };

  const picker = namePicker(available, stored?.allow);
  const onlyBlock = picker.node;

  function sync(): void {
    onlyBlock.hidden = !radios[Mode.Only]?.checked;
  }

  const node = el("div", { class: "tristate" }, [
    el("div", { class: "radio-row" }, [
      radio(Mode.All, labels.all),
      radio(Mode.Only, labels.only),
      radio(Mode.None, labels.none),
    ]),
    onlyBlock,
  ]);
  sync();

  return {
    node,
    value(): TriStateResult {
      if (radios[Mode.All]?.checked) return { ok: true, selection: { mode: Mode.All } };
      if (radios[Mode.None]?.checked) return { ok: true, selection: { mode: Mode.None } };
      const tools = picker.value();
      if (tools.length === 0) {
        return {
          ok: false,
          message:
            "“Only the ticked tools” with nothing ticked is not how a server is blocked — it is " +
            "refused so it can never be read as “no rule”. Pick at least one tool, or choose " +
            "“Block every tool”.",
        };
      }
      return { ok: true, selection: { mode: Mode.Only, tools } };
    },
  };
}

/** Renders a STORED selector for a listing: the same three states, in words. */
export function describeSelector(sel: ToolSelector | undefined): string {
  if (!sel || sel.allow === undefined) return "all tools";
  if (sel.allow.length === 0) return "blocked (no tools)";
  return sel.allow.join(", ");
}

/** Renders a stored member-server set (same three states as a selector). */
export function describeServerSet(servers: string[] | undefined): string {
  if (servers === undefined) return "every registered server";
  if (servers.length === 0) return "blocked (no server)";
  return servers.join(", ");
}

// ---------------------------------------------------------------------------
// Modal dialogs
// ---------------------------------------------------------------------------

/** Opens a modal. Returns the function that closes it. */
export function openModal(
  title: string,
  body: (Node | null)[],
  opts: { danger?: boolean; onClose?: () => void } = {},
): () => void {
  const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  const titleID = `modal-title-${Math.random().toString(36).slice(2)}`;
  const overlay = el("div", { class: "overlay" });
  const close = (): void => {
    overlay.remove();
    document.removeEventListener("keydown", onKey);
    opts.onClose?.();
    if (previousFocus?.isConnected) previousFocus.focus();
  };
  const onKey = (ev: KeyboardEvent): void => {
    if (ev.key === "Escape") close();
  };
  const panel = el("div", {
    class: opts.danger ? "modal modal-danger" : "modal",
    role: "dialog",
    "aria-modal": "true",
    "aria-labelledby": titleID,
    tabindex: "-1",
  }, [
    el("header", {}, [
      el("strong", { id: titleID, text: title }),
      (() => {
        const closeButton = button("×", "btn btn-icon", close);
        closeButton.setAttribute("aria-label", "Close dialog");
        return closeButton;
      })(),
    ]),
    el("div", { class: "modal-body" }, body.filter((n): n is Node => n !== null)),
  ]);
  overlay.append(panel);
  overlay.addEventListener("click", (ev) => {
    if (ev.target === overlay) close();
  });
  document.addEventListener("keydown", onKey);
  document.body.append(overlay);
  window.setTimeout(() => {
    // Prefer a one-line field. The server dialog begins with a collapsed
    // paste disclosure whose textarea precedes the visible Id input in DOM
    // order; focusing that closed descendant fails and leaves focus on the X.
    const firstField =
      panel.querySelector<HTMLElement>("input:not([disabled])") ??
      panel.querySelector<HTMLElement>("select:not([disabled])") ??
      panel.querySelector<HTMLElement>("textarea:not([disabled])");
    const first = firstField ?? panel.querySelector<HTMLElement>("button:not([disabled]), a[href]");
    (first ?? panel).focus();
  }, 0);
  return close;
}

export interface ConfirmOptions {
  /**
   * The title, as a QUESTION the user can answer: "Remove stripe?".
   * A noun phrase ("Confirm removal") forces the reader to reconstruct what
   * they are being asked, and the reconstruction is where mistakes live.
   */
  title: string;
  /** What is about to happen, in one sentence. */
  body: string;
  /**
   * What will NOT happen. This is the load-bearing half of the pattern: the
   * hesitation in front of a destructive button is almost never "does this
   * delete the thing" — it is "does this also take the credentials / the
   * history / the bindings with it". Say so, and the click stops being a
   * gamble.
   */
  consequences?: string[];
  /** The button is a VERB — "Remove", "Disable", "Deny" — never "OK"/"Yes",
   *  so the confirming click names the act even when read on its own. */
  confirmLabel?: string;
  /** When set, the confirm button stays disabled until this is ticked. Used
   *  for the writes that weaken enforcement rather than merely delete data. */
  acknowledge?: string;
  danger?: boolean;
  /** The equivalent CLI command, shown inside the dialog (docs/modules/gui.md). */
  cli?: string;
  /**
   * When set, the write runs INSIDE the dialog and a rejection keeps the
   * dialog open with the failure rendered in it. A dialog that closes on
   * failure makes the user re-navigate, re-open and re-read the whole
   * confirmation to retry an action they already decided on — and, worse, a
   * closed dialog looks exactly like a success.
   */
  perform?: () => Promise<void>;
}

/**
 * The second step in front of a destructive or safety-relaxing write.
 *
 * Built on the native <dialog> because it already does the three things a
 * hand-rolled overlay gets wrong: modality, Escape, and focus containment.
 *
 * Cancelling is the default in every ambiguous case: Escape, the Cancel
 * button and any other route to `close` all resolve false, so an accidental
 * dismissal can never be read as consent.
 */
export function confirmAction(opts: ConfirmOptions): Promise<boolean> {
  return new Promise((resolve) => {
    let settled = false;
    const dlg = el("dialog", {
      class: opts.danger ? "dlg danger" : "dlg",
    }) as HTMLDialogElement;

    const finish = (ok: boolean): void => {
      if (settled) return;
      settled = true;
      resolve(ok);
      if (dlg.open) dlg.close();
      dlg.remove();
    };

    const label = opts.confirmLabel ?? "Confirm";
    const failures = el("div", { class: "notice-slot" });
    const cancel = button("Cancel", "btn btn-secondary", () => finish(false));
    const confirm = button(label, opts.danger ? "btn btn-deny" : "btn btn-primary", () => {
      if (!opts.perform) {
        finish(true);
        return;
      }
      clear(failures);
      confirm.disabled = true;
      cancel.disabled = true;
      confirm.textContent = "Working…";
      opts.perform().then(
        () => finish(true),
        (err: unknown) => {
          // Stay open. The intent is unchanged; only the attempt failed.
          confirm.disabled = false;
          cancel.disabled = false;
          confirm.textContent = label;
          failures.append(errorDetail(err));
        },
      );
    });

    let ack: HTMLElement | null = null;
    if (opts.acknowledge) {
      const { node, box } = checkboxInput(opts.acknowledge, false);
      confirm.disabled = true;
      box.addEventListener("change", () => {
        confirm.disabled = !box.checked;
      });
      ack = node;
    }

    dlg.append(
      el("h2", { text: opts.title }),
      el("div", { class: "dlg-body" }, [
        el("p", { text: opts.body }),
        opts.consequences && opts.consequences.length > 0
          ? el(
              "ul",
              { class: "consequences" },
              opts.consequences.map((c) => el("li", { text: c })),
            )
          : null,
        opts.cli ? cliHint(opts.cli, { note: "the same thing from a terminal" }) : null,
        ack,
        failures,
        el("div", { class: "dlg-actions" }, [cancel, confirm]),
      ]),
    );
    dlg.addEventListener("close", () => finish(false));
    document.body.append(dlg);
    dlg.showModal();
    cancel.focus();
  });
}

/**
 * Above this many affected items, a bulk action asks first.
 *
 * The threshold exists because a confirmation on every two-item action is how
 * users learn to dismiss confirmations without reading them — and once that
 * habit forms, the dialog in front of the twenty-item action does not work
 * either. Three is small enough that the operator can still see every row
 * they are about to change.
 */
export const BULK_CONFIRM_THRESHOLD = 3;

/** Confirms a bulk action, but only once it is big enough to be worth a
 *  dialog. Below the threshold it resolves true without asking. */
export function confirmBulk(count: number, opts: ConfirmOptions): Promise<boolean> {
  if (count <= BULK_CONFIRM_THRESHOLD) {
    // No dialog, so there is nowhere to render a failure: the rejection is
    // handed to the caller, which has a notice slot. Swallowing it here
    // would make a failed bulk action look like a completed one.
    if (!opts.perform) return Promise.resolve(true);
    return opts.perform().then(() => true);
  }
  return confirmAction(opts);
}

/**
 * Why a global action is switched off while a filter is on.
 *
 * A button labelled "Dismiss all" next to a filtered list is a lie in one of
 * two directions: either it acts on rows the operator cannot see, or it acts
 * on the filtered subset while saying "all". Neither is fixable with better
 * wording, so the button is disabled and says why.
 */
export const FILTER_BLOCKS_GLOBAL =
  "Clear the filter first: a global action while a filter is on would either " +
  "touch rows you cannot see or quietly mean something narrower than “all”.";

// ---------------------------------------------------------------------------
// Small renderers
// ---------------------------------------------------------------------------

/** A form panel that can be swapped in and out of a page without redrawing
 *  the listing around it. */
export function formHost(): { node: HTMLElement; show(content: Node): void; hide(): void } {
  const node = el("div", { class: "form-host" });
  return {
    node,
    show(content) {
      clear(node);
      node.append(content);
      node.scrollIntoView({ block: "nearest" });
    },
    hide() {
      clear(node);
    },
  };
}

/** A replaceable form host for focused configuration work. Unlike formHost,
 *  it does not move the page or widen the listing when a long editor opens. */
export function modalHost(): { show(title: string, content: Node): void; hide(): void } {
  let current: (() => void) | null = null;
  return {
    show(title, content) {
      current?.();
      let close: (() => void) | null = null;
      close = openModal(title, [content], {
        onClose: () => {
          if (current === close) current = null;
        },
      });
      current = close;
    },
    hide() {
      const close = current;
      current = null;
      close?.();
    },
  };
}

export function badge(text: string, cls: string): HTMLElement {
  return el("span", { class: `badge ${cls}`, text });
}

// ---------------------------------------------------------------------------
// Copying, and the equivalent CLI command
// ---------------------------------------------------------------------------

/** A button that copies `text` and says so for a moment. Used everywhere a
 *  string's purpose is to end up somewhere else: an error, a command, a
 *  fingerprint. */
export function copyButton(text: () => string, label = "Copy", cls = "btn btn-icon"): HTMLButtonElement {
  const b = button(label, cls, () => {
    void copyText(text());
    b.textContent = "copied";
    window.setTimeout(() => {
      b.textContent = label;
    }, 1200);
  });
  return b;
}

/**
 * The full text of a failure, one disclosure away, with a Copy button.
 *
 * Every distilled headline is paired with one of these. The headline answers
 * "what do I do"; this answers "what actually happened", which is the thing
 * that gets pasted into an issue — and a UI that makes the user retype a
 * stack trace has simply moved the error somewhere less useful.
 */
export function rawDetails(text: string, summary = "Show the full text"): HTMLElement {
  const pre = el("pre", { class: "raw-text", text });
  return el("details", { class: "raw" }, [
    el("summary", { text: summary }),
    pre,
    el("div", { class: "controls" }, [copyButton(() => text, "Copy full text", "btn btn-secondary")]),
  ]);
}

/** The raw text behind a rejected bridge call, without importing the
 *  classification in page.ts (which imports this module). */
function rawOf(err: unknown): string {
  const cause = (err as { cause?: unknown } | undefined)?.cause;
  if (cause && typeof cause === "object") {
    const c = cause as { code?: string; message?: string; hint?: string; status?: number };
    const head = [c.code, c.message].filter(Boolean).join(": ");
    return [head, c.hint, c.status ? `status ${c.status}` : ""].filter(Boolean).join("\n");
  }
  if (err instanceof Error) return err.stack ?? err.message;
  return String(err);
}

/** A failure rendered the way every failure in this application is rendered:
 *  a distilled one-line headline, the full text, and a Copy button. */
export function errorDetail(err: unknown): HTMLElement {
  const raw = rawOf(err);
  return errorBox(errorHeadline(raw), undefined, rawDetails(raw));
}

/**
 * Quotes one argument for a POSIX shell.
 *
 * The command strings this module renders are meant to be pasted and run, so
 * a server id with a space in it has to survive the round trip. Single quotes
 * with the standard '\'' escape is the only form that needs no knowledge of
 * what is inside the string.
 */
export function shellArg(value: string): string {
  if (value === "") return "''";
  if (/^[A-Za-z0-9_@%+=:,./-]+$/.test(value)) return value;
  return `'${value.replace(/'/g, `'\\''`)}'`;
}

/**
 * The equivalent CLI command for a GUI action (docs/modules/gui.md).
 *
 * This is a property of the architecture rather than a feature: "the GUI can
 * do nothing the CLI cannot" means every button here HAS an exact command,
 * so showing it costs nothing and buys three things — a scriptable path for
 * the operator who wants one, something pasteable into a ticket, and a GUI
 * that teaches its own CLI. The command text must match the real command
 * (internal/cli); a plausible-looking command that does not exist would be
 * worse than showing none.
 */
export function cliHint(command: string, opts: { note?: string } = {}): HTMLElement {
  return el("div", { class: "cli-hint" }, [
    icon("terminal", "cli-icon"),
    el("code", { text: command }),
    copyButton(() => command, "Copy", "btn btn-icon cli-copy"),
    opts.note ? el("span", { class: "meta", text: opts.note }) : null,
  ]);
}

export interface CliEntry {
  label: string;
  command: string;
  note?: string;
}

/** Several equivalent commands behind one disclosure, for a row that has
 *  more than one write on it. */
export function cliBlock(entries: CliEntry[], summary = "⌘ Equivalent CLI"): HTMLElement {
  return el("details", { class: "cli-block" }, [
    el("summary", { text: summary }),
    el(
      "div",
      { class: "cli-list" },
      entries.map((e) =>
        el("div", { class: "action" }, [
          el("span", { class: "meta", text: e.label }),
          cliHint(e.command, e.note ? { note: e.note } : {}),
        ]),
      ),
    ),
  ]);
}

// ---------------------------------------------------------------------------
// Theme (docs/modules/gui.md)
// ---------------------------------------------------------------------------

export type ThemeMode = "light" | "dark" | "system";

const THEME_KEY = "agenthub.theme";

/** The stored CHOICE, which is not the same as the resolved appearance:
 *  "system" is a real, persistent answer and must survive a reload as
 *  "system" rather than as whatever the OS happened to say at the time. */
export function themeMode(): ThemeMode {
  const m = document.documentElement.getAttribute("data-theme-mode");
  return m === "light" || m === "dark" ? m : "system";
}

function applyTheme(mode: ThemeMode): void {
  let dark = mode === "dark";
  if (mode === "system") {
    try {
      dark = window.matchMedia("(prefers-color-scheme: dark)").matches;
    } catch {
      dark = true;
    }
  }
  const root = document.documentElement;
  root.setAttribute("data-theme", dark ? "dark" : "light");
  root.setAttribute("data-theme-mode", mode);
}

export function setThemeMode(mode: ThemeMode): void {
  try {
    localStorage.setItem(THEME_KEY, mode);
  } catch {
    // Storage is unavailable in some embedded webview configurations. The
    // choice still applies to this window; it simply will not be remembered.
  }
  applyTheme(mode);
}

/** Keeps "system" actually meaning system: the OS can flip while the window
 *  is open, and a theme that only tracks the OS at startup is a theme that is
 *  wrong for the rest of the session. The pre-paint bootstrap in index.html
 *  has already set the first value. */
export function initTheme(): void {
  try {
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    mq.addEventListener("change", () => {
      if (themeMode() === "system") applyTheme("system");
    });
  } catch {
    // No matchMedia: the bootstrap's fallback stands.
  }
}

/** The light / dark / system control. Lives on the Settings page. */
export function themeControl(): HTMLElement {
  const modes: { value: ThemeMode; label: string }[] = [
    { value: "light", label: "Light" },
    { value: "dark", label: "Dark" },
    { value: "system", label: "System" },
  ];
  const group = el("div", { class: "segmented", role: "group", "aria-label": "Theme" });
  const buttons: { value: ThemeMode; node: HTMLButtonElement }[] = [];
  const sync = (): void => {
    const active = themeMode();
    for (const b of buttons) b.node.setAttribute("aria-pressed", String(b.value === active));
  };
  for (const m of modes) {
    const node = el("button", { type: "button", text: m.label }) as HTMLButtonElement;
    node.addEventListener("click", () => {
      setThemeMode(m.value);
      sync();
    });
    buttons.push({ value: m.value, node });
    group.append(node);
  }
  sync();
  return group;
}
