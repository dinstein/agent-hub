import { asCallError, isOffline, isStalePrecondition, isUnavailable } from "./bridge";
import { clear, el, emptyState, errorBox, errorHeadline } from "./dom";
import { button, rawDetails } from "./ui";
import type { CallError, WriteResult } from "./types";

/** A page owns one view and cleans up its own subscriptions. */
export interface Page {
  render(root: HTMLElement): void | Promise<void>;
  /** Called before another page is mounted. */
  dispose?(): void;
}

/** The complete text of a failure, kept verbatim so the disclosure and the
 *  Copy button have something worth copying. */
function rawText(e: CallError): string {
  return [e.code, e.message].filter(Boolean).join(": ") +
    (e.hint ? `\nhint: ${e.hint}` : "") +
    (e.status ? `\nstatus: ${e.status}` : "");
}

/**
 * Turns a failed call into the box the user sees. The outcomes are
 * deliberately different: "daemon is down" is not "the endpoint does not
 * exist" is not "the daemon said no". Collapsing them into one empty state
 * is how a governance UI starts lying.
 *
 * A validation refusal renders the daemon's own message AND its hint: the
 * control-plane error body carries both, and the hint is usually the part
 * that says which value would have been accepted. The message itself goes
 * through errorHeadline first — a daemon that forwards a downstream stack
 * trace should not be able to push the rest of the page off screen — and the
 * untouched text stays one disclosure away with a Copy button
 * (docs/modules/gui.md).
 */
export function failureBox(err: unknown): HTMLElement {
  const e: CallError = asCallError(err);
  if (isOffline(e)) {
    return errorBox(
      "The agenthub daemon is not reachable.",
      e.hint ?? "Start it with `agenthub daemon start`, then use Retry on the Settings page.",
      rawDetails(rawText(e)),
    );
  }
  if (isUnavailable(e)) {
    return errorBox(
      "This daemon does not serve that endpoint yet.",
      "The GUI is one control-plane client among several: everything shown here is also available from the CLI.",
      rawDetails(rawText(e)),
    );
  }
  const raw = rawText(e);
  return errorBox(e.message ? errorHeadline(e.message) : e.code, e.hint, rawDetails(raw));
}

/**
 * The FAILED empty state (docs/modules/gui.md): what a listing renders when
 * the request did not come back.
 *
 * It says out loud that this is not an empty result, because the alternative
 * — an unadorned "No servers configured yet." after a dropped socket — tells
 * the operator their configuration is gone. In a tool whose whole job is to
 * report what is configured and what is allowed, that is the single worst
 * sentence we can print.
 */
export function failureState(err: unknown, what: string, retry: () => void): HTMLElement {
  const e: CallError = asCallError(err);
  const headline = isOffline(e)
    ? "The agenthub daemon is not reachable."
    : isUnavailable(e)
      ? "This daemon does not serve that endpoint yet."
      : e.message
        ? errorHeadline(e.message)
        : e.code;
  const node = emptyState({
    kind: "failed",
    title: headline,
    body: `This is NOT an empty list — ${what} could not be read, so nothing below is known one way or the other.`,
    actions: [button("Retry", "btn", retry)],
  });
  node.append(rawDetails(rawText(e)));
  return node;
}

/** What the user is told when a write lost the optimistic-concurrency check.
 *  Nothing was written and the page has re-read: the intent is intact, it
 *  simply has to be re-applied against the configuration as it now is. */
export const CONFLICT_MESSAGE =
  "The configuration was changed somewhere else, so nothing was written. This page has been " +
  "reloaded — review the current state and apply your change again.";

/** A notice area a page writes progress, warnings and failures into.
 *
 *  This is also the application's "toast" surface, and the reason there is no
 *  separate one: every failure that lands here goes through failureBox, so it
 *  arrives with a distilled headline, the full text and a Copy button already
 *  attached (docs/modules/gui.md). A second, thinner notification path would
 *  be the one place a raw error could still escape uncopyable. */
export interface NoticeSlot {
  node: HTMLElement;
  say(message: string, kind?: "notice" | "warn"): void;
  /** Renders a failed call (offline / unavailable / refused, with its hint). */
  fail(err: unknown): void;
  clear(): void;
}

/** Creates a notice area. The node is inert until something is said into it
 *  (`.notice-slot:empty` collapses it). */
export function noticeSlot(): NoticeSlot {
  const node = el("div", { class: "notice-slot" });
  return {
    node,
    say(message, kind = "notice") {
      clear(node);
      node.append(
        el("div", { class: kind === "warn" ? "notice notice-warn" : "notice", text: message }),
      );
    },
    fail(err) {
      clear(node);
      node.append(failureBox(err));
    },
    clear() {
      clear(node);
    },
  };
}

/** The tail every mutating call answers with. Warnings accompany a SUCCESSFUL
 *  write (a healed document, a client that now resolves to an empty scope) and
 *  must be surfaced rather than swallowed. */
type Written = Partial<WriteResult>;

/**
 * Runs one write and reports it, which is the same three-branch shape on
 * every page:
 *
 *   - success -> say what happened, list the daemon's warnings, reload;
 *   - 409 stale precondition -> say that nothing was written and reload, so
 *     the next attempt is computed from the configuration as it now is. The
 *     input is deliberately NOT replayed automatically: a blind retry would
 *     re-apply an intent formed against data the operator can no longer see;
 *   - anything else -> render the daemon's message and hint verbatim.
 *
 * Returns true when the write succeeded.
 */
export async function runWrite<T extends Written>(
  slot: NoticeSlot,
  reload: () => Promise<void> | void,
  describe: (result: T) => string,
  action: () => Promise<T>,
): Promise<boolean> {
  let result: T;
  try {
    result = await action();
  } catch (err) {
    if (isStalePrecondition(asCallError(err))) {
      await reload();
      slot.say(CONFLICT_MESSAGE, "warn");
      return false;
    }
    slot.fail(err);
    return false;
  }
  await reload();
  const warnings = result.warnings ?? [];
  slot.clear();
  slot.node.append(
    el("div", { class: warnings.length > 0 ? "notice notice-warn" : "notice" }, [
      el("div", { text: describe(result) }),
      ...warnings.map((w) => el("div", { class: "warn-line", text: `warning: ${w}` })),
    ]),
  );
  return true;
}
