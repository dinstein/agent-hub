// Approvals page: the pending HITL queue with countdowns and decisions.
//
// Multi-frontend coexistence (docs/modules/controlplane.md) is the property this page is
// built around:
//
//   - The GUI is NEVER on the approval path. The broker inside the daemon
//     owns every decision; this page is one subscriber among several (the
//     CLI's `approval watch` is another).
//   - First decision wins. When another frontend decides first, the resolved
//     frame arrives over SSE and the card disappears here — and if this page
//     answers too late it gets E_ALREADY_DECIDED, which is idempotent, not
//     an error to shout about.
//   - The countdown uses the broker's deadline, so what the user sees expire
//     and what auto-denies are the same instant.
//   - Arguments arrive only on the SSE pending frame. They live in memory,
//     are rendered, and are dropped with the card: never stored, never
//     logged, never re-fetched.
//
// PRESENTATION (docs/modules/gui.md). Four decisions, all of them about the
// same thing — a human is being asked to decide something under time
// pressure, and everything that makes that harder is a security cost:
//
//   1. The banner is role="alertdialog" with aria-modal="false". It has to
//      be impossible to miss, and it must NOT block the window: deciding
//      well often means going to look at what the tool would touch first.
//   2. The banner states the mechanism in words — "no decision auto-denies".
//      Fail-closed written in a design document protects nobody; written on
//      the screen it tells the user what their inaction means.
//   3. The countdown is a number AND a bar, and turns red near the end. One
//      channel is a number nobody watches; two make "running out" peripheral.
//   4. Nothing is removed optimistically. A card greys out and waits for the
//      daemon. The alternative — remove on click, restore if the call failed
//      — makes a held call flash back into the queue, and a queue that
//      un-decides itself is one the operator stops trusting.

import { EVT, asCallError, hub, on } from "../bridge";
import { clear, el, emptyState, section } from "../dom";
import type { Page } from "../page";
import { failureBox } from "../page";
import { button, confirmBulk } from "../ui";
import { ErrCode, Remember } from "../types";
import type { Approval, ApprovalResolution, TopicEvent } from "../types";

/** Pending requests keyed by token. Arguments live here and nowhere else. */
type Queue = Map<string, Approval>;

/** Below this many seconds the countdown turns red. Twenty seconds is about
 *  the point where reading the arguments and deciding stops being possible,
 *  so it is the point where the display should stop being calm. */
const URGENT_SECONDS = 20;

function secondsLeft(deadline: string): number {
  const ms = Date.parse(deadline) - Date.now();
  if (Number.isNaN(ms)) return NaN;
  return Math.max(0, Math.ceil(ms / 1000));
}

function countdown(deadline: string): string {
  const s = secondsLeft(deadline);
  if (Number.isNaN(s)) return "—";
  if (s === 0) return "expired";
  return s >= 60 ? `${Math.floor(s / 60)}m ${s % 60}s` : `${s}s`;
}

function argsBlock(a: Approval): Node | null {
  if (a.args === undefined || a.args === null) {
    return el("p", {
      class: "hint",
      text: "Arguments are delivered only with the live request; reopen the queue if this card was restored from a listing.",
    });
  }
  let text: string;
  try {
    text = JSON.stringify(a.args, null, 2);
  } catch {
    text = String(a.args);
  }
  if (text.length > 4000) text = `${text.slice(0, 4000)}\n… truncated`;
  return el("pre", { class: "args", text });
}

export function approvalsPage(): Page {
  const queue: Queue = new Map();
  /** When each token was first seen here. The wire carries the deadline but
   *  not the window it was granted from, and the progress bar needs a
   *  denominator; first sight is the closest honest one this page has. */
  const firstSeen = new Map<string, number>();
  /** Tokens this window has answered and the daemon has not yet confirmed.
   *  They stay on screen, greyed — see decision 4 in the header. */
  const deciding = new Set<string>();

  let root: HTMLElement | null = null;
  let banner: HTMLElement | null = null;
  let listRoot: HTMLElement | null = null;
  let offs: (() => void)[] = [];
  let ticker: number | undefined;
  let onKey: ((ev: KeyboardEvent) => void) | null = null;
  let notice: HTMLElement | null = null;

  function say(message: string, kind = "notice"): void {
    if (!notice) return;
    clear(notice);
    notice.append(el("div", { class: kind, text: message }));
  }

  /** Pending requests in deadline order: the one closest to auto-denying
   *  first, which is also the one Escape acts on. */
  function ordered(): Approval[] {
    return Array.from(queue.values()).sort(
      (a, b) => Date.parse(a.deadline) - Date.parse(b.deadline),
    );
  }

  async function decide(a: Approval, approve: boolean, remember: string): Promise<void> {
    if (deciding.has(a.token)) return;
    deciding.add(a.token);
    drawList();
    try {
      const dec = await hub.answer(a.token, approve, remember);
      say(
        `${a.server}/${a.tool}: ${dec.decision}` +
          (dec.remember_error ? ` (remember failed: ${dec.remember_error})` : ""),
      );
    } catch (err) {
      const e = asCallError(err);
      if (e.code === ErrCode.AlreadyDecided || e.code === ErrCode.Expired) {
        // Someone else got there first, or the deadline did. Both are
        // normal outcomes of a shared queue, not failures.
        say(`Already handled elsewhere: ${e.message}`);
      } else {
        // The call failed, so nothing was decided. Hand the card back and
        // report why, rather than leaving it greyed out forever.
        deciding.delete(a.token);
        if (notice) {
          clear(notice);
          notice.append(failureBox(err));
        }
        drawList();
        return;
      }
    }
    // Re-read from the daemon rather than deleting locally: the authoritative
    // answer to "is this still pending" is the daemon's list, and it is the
    // same answer another frontend's decision would produce.
    await reload();
  }

  function decisionButton(
    label: string,
    cls: string,
    a: Approval,
    approve: boolean,
    remember: string,
    title: string,
  ): HTMLElement {
    const b = button(label, cls, () => void decide(a, approve, remember));
    b.title = title;
    return b;
  }

  function card(a: Approval): HTMLElement {
    const busy = deciding.has(a.token);
    const secs = secondsLeft(a.deadline);
    const urgent = !Number.isNaN(secs) && secs <= URGENT_SECONDS;

    const bar = el("div", {
      class: urgent ? "countdown-bar urgent" : "countdown-bar",
      "data-token": a.token,
    });
    bar.append(el("i", { style: `width:${barWidth(a)}%` }));

    // Three remembering scopes, spelled out rather than hidden behind a
    // checkbox: approval fatigue is what turns a gate into a rubber stamp,
    // and the cure is letting the operator say "yes, and stop asking" at a
    // scope they are willing to defend (docs/modules/gui.md).
    const decisions = el("div", { class: "decision-group" }, [
      el("span", { class: "glabel", text: "Allow" }),
      decisionButton("once", "btn btn-approve", a, true, Remember.None, "This call only."),
      decisionButton(
        "this session",
        "btn btn-approve",
        a,
        true,
        Remember.Session,
        "Every matching call until this session ends. Held in memory, never written down.",
      ),
      decisionButton(
        "always",
        "btn btn-approve",
        a,
        true,
        Remember.Forever,
        "Stored, and bound to the tool's fingerprint: if the tool changes, this stops matching and you are asked again.",
      ),
    ]);

    const node = el("article", { class: busy ? "card deciding" : "card", "data-token": a.token }, [
      el("header", {}, [
        el("strong", { text: `${a.server} / ${a.tool}` }),
        el("span", {
          class: urgent ? "countdown urgent" : "countdown",
          "data-deadline": a.deadline,
          text: countdown(a.deadline),
        }),
      ]),
      bar,
      el("div", { class: "meta" }, [
        el("span", { text: a.client ? `client ${a.client}` : "client —" }),
        el("span", { text: a.session_id ? ` · session ${a.session_id}` : "" }),
        el("span", { text: a.gate_reason ? ` · ${a.gate_reason}` : "" }),
      ]),
      a.args_hash ? el("div", { class: "meta mono", text: `args ${a.args_hash}` }) : null,
      a.fingerprint ? el("div", { class: "meta mono", text: `tool ${a.fingerprint}` }) : null,
      argsBlock(a),
      el("div", { class: "card-actions" }, [
        decisions,
        button("Deny", "btn btn-deny", () => void decide(a, false, Remember.None)),
      ]),
      busy
        ? el("p", { class: "hint", text: "Sent. Waiting for the daemon to confirm…" })
        : el("p", {
            class: "hint",
            text: "“Always” is keyed by tool fingerprint: if the tool changes, the entry stops matching and the call needs approval again.",
          }),
    ]);
    return node;
  }

  /** How much of the granted window is left, as a percentage. The window is
   *  measured from when this page first saw the request, so a card restored
   *  from a listing shows a bar that starts full — honest about what is
   *  known rather than inventing a start time. */
  function barWidth(a: Approval): number {
    const end = Date.parse(a.deadline);
    if (Number.isNaN(end)) return 0;
    const start = firstSeen.get(a.token) ?? Date.now();
    const total = end - start;
    if (total <= 0) return 0;
    const left = end - Date.now();
    return Math.max(0, Math.min(100, Math.round((left / total) * 100)));
  }

  function drawBanner(): void {
    if (!banner) return;
    clear(banner);
    const n = queue.size;
    if (n === 0) {
      banner.hidden = true;
      return;
    }
    banner.hidden = false;
    banner.append(
      el("span", { class: "hitl-title", text: "Approval required" }),
      // The mechanism, in the interface, in words. Not in a tooltip and not
      // in the docs: what happens if the user walks away is the single most
      // important fact on this screen.
      el("span", {
        class: "hitl-sub",
        text: `${n} tool ${n === 1 ? "call is" : "calls are"} held — no decision auto-denies.`,
      }),
      el("span", { class: "hitl-keys" }, [
        el("span", { text: "Press " }),
        el("kbd", { text: "Esc" }),
        el("span", { text: " to deny the oldest one. Denying is the safe direction." }),
      ]),
    );
  }

  function drawList(): void {
    drawBanner();
    if (!listRoot) return;
    clear(listRoot);
    if (queue.size === 0) {
      listRoot.append(
        emptyState({
          kind: "empty",
          title: "Nothing is waiting.",
          body:
            "Requests appear here the moment a gated call is made. They are held, not queued: " +
            "a request nobody answers before its deadline is denied.",
        }),
      );
      return;
    }
    for (const a of ordered()) listRoot.append(card(a));
  }

  function tick(): void {
    if (!listRoot) return;
    for (const node of Array.from(listRoot.querySelectorAll<HTMLElement>(".countdown"))) {
      const deadline = node.dataset.deadline ?? "";
      const secs = secondsLeft(deadline);
      node.textContent = countdown(deadline);
      node.classList.toggle("urgent", !Number.isNaN(secs) && secs <= URGENT_SECONDS);
      node.classList.toggle("expired", secs === 0);
    }
    for (const node of Array.from(listRoot.querySelectorAll<HTMLElement>(".countdown-bar"))) {
      const a = queue.get(node.dataset.token ?? "");
      if (!a) continue;
      const fill = node.firstElementChild as HTMLElement | null;
      if (fill) fill.style.width = `${barWidth(a)}%`;
      node.classList.toggle("urgent", secondsLeft(a.deadline) <= URGENT_SECONDS);
    }
  }

  /** Escape denies the OLDEST pending request. Escape is universally "get
   *  out of this", and for a gate the safe reading of "get out" is deny —
   *  the call has not happened yet, and a denial is recoverable by asking
   *  again while an approval is not recoverable at all. */
  function denyOldest(): void {
    const first = ordered().find((a) => !deciding.has(a.token));
    if (first) void decide(first, false, Remember.None);
  }

  /** Deny everything held. Below the bulk threshold it just happens; above
   *  it, it asks — see BULK_CONFIRM_THRESHOLD. */
  async function denyAll(): Promise<void> {
    const pending = ordered().filter((a) => !deciding.has(a.token));
    if (pending.length === 0) return;
    let ok = false;
    try {
      ok = await confirmBulk(pending.length, {
        title: `Deny all ${pending.length} held calls?`,
        body: "Every request currently on this page is denied.",
        consequences: [
          "Nothing is remembered: this denies these calls, it does not add a rule, and the same tools can be requested again immediately.",
          "Requests that arrive after you confirm are not covered — they will be waiting when this finishes.",
        ],
        confirmLabel: "Deny all",
        danger: true,
        cli: "agenthub approval ls   # then: agenthub approval deny <token>",
        perform: async () => {
          for (const a of pending) {
            deciding.add(a.token);
            try {
              await hub.answer(a.token, false, Remember.None);
            } catch (err) {
              const e = asCallError(err);
              // Someone else deciding first is a success for the human.
              if (e.code !== ErrCode.AlreadyDecided && e.code !== ErrCode.Expired) {
                deciding.delete(a.token);
                throw err;
              }
            }
          }
        },
      });
    } catch (err) {
      // Below the bulk threshold there is no dialog to fail inside, so the
      // rejection surfaces here rather than silently reading as "denied".
      if (notice) {
        clear(notice);
        notice.append(failureBox(err));
      }
      await reload();
      return;
    }
    if (ok) say(`Denied ${pending.length} held ${pending.length === 1 ? "call" : "calls"}.`);
    await reload();
  }

  async function reload(): Promise<void> {
    if (!root) return;
    try {
      const pending = await hub.listApprovals(false);
      // The REST listing never carries arguments; keep the richer SSE copy
      // when we already have one.
      for (const a of pending) {
        const known = queue.get(a.token);
        queue.set(a.token, known ? { ...a, args: known.args } : a);
        if (!firstSeen.has(a.token)) firstSeen.set(a.token, Date.now());
      }
      for (const token of Array.from(queue.keys())) {
        if (!pending.some((p) => p.token === token)) {
          queue.delete(token);
          firstSeen.delete(token);
          deciding.delete(token);
        }
      }
    } catch (err) {
      if (notice) {
        clear(notice);
        notice.append(failureBox(err));
      }
      return;
    }
    drawList();
  }

  return {
    render(node) {
      root = node;
      clear(root);
      notice = el("div", { class: "notice-slot" });
      listRoot = el("div", { class: "cards" });
      // role="alertdialog" announces it as a decision that is being demanded;
      // aria-modal="false" is the promise that it does not trap the user.
      banner = el("div", {
        class: "hitl",
        role: "alertdialog",
        "aria-modal": "false",
        "aria-live": "assertive",
      });
      banner.hidden = true;

      root.append(
        banner,
        section(
          "Approvals",
          el("div", { class: "controls" }, [button("Deny all", "btn btn-deny", () => void denyAll())]),
          notice,
          listRoot,
        ),
      );

      offs.push(
        on<TopicEvent<Approval | ApprovalResolution>>(EVT.approvals, (ev) => {
          if (ev.kind === "pending" && ev.payload) {
            const a = ev.payload as Approval;
            queue.set(a.token, a);
            if (!firstSeen.has(a.token)) firstSeen.set(a.token, Date.now());
            drawList();
            return;
          }
          if (ev.kind === "resolved" && ev.payload) {
            // Decided anywhere — collapse the card here. This is also the
            // confirmation a greyed-out card is waiting for.
            const r = ev.payload as ApprovalResolution;
            firstSeen.delete(r.token);
            deciding.delete(r.token);
            if (queue.delete(r.token)) {
              say(
                `${r.token.slice(0, 8)}… ${r.decision}${r.decided_by ? ` by ${r.decided_by}` : ""}`,
              );
              drawList();
            }
            return;
          }
          // Unknown kinds (grant frames, future additions) are ignored on
          // purpose: the topic is shared and must stay forward compatible.
        }),
      );

      onKey = (ev: KeyboardEvent): void => {
        // Not while a dialog is open: Escape belongs to the dialog then, and
        // stealing it would turn "cancel this confirmation" into "deny a
        // call", which is the exact class of surprise a gate must not have.
        if (ev.key !== "Escape" || document.querySelector("dialog[open]")) return;
        if (queue.size === 0) return;
        ev.preventDefault();
        denyOldest();
      };
      document.addEventListener("keydown", onKey);

      ticker = window.setInterval(tick, 500);
      return reload();
    },
    dispose() {
      offs.forEach((f) => f());
      offs = [];
      if (ticker !== undefined) window.clearInterval(ticker);
      ticker = undefined;
      if (onKey) document.removeEventListener("keydown", onKey);
      onKey = null;
      // Drop every argument payload with the page.
      queue.clear();
      firstSeen.clear();
      deciding.clear();
      root = null;
      banner = null;
      listRoot = null;
      notice = null;
    },
  };
}
