// The two window-local preferences that decide what the close button does.
//
// WHERE THEY LIVE, AND WHY HERE. They are properties of THIS window on THIS
// machine — exactly like the theme — so localStorage is the durable copy and
// the daemon's registry has no opinion about them. The Go side needs the same
// answers to act on a close it receives natively, so it keeps a runtime copy
// that this module pushes at startup and after every change.
//
// The sync has one direction each way and no loop: this module writes to Go,
// and every change — including one made from the tray menu — comes back as an
// event which this module persists without answering. A handler that called
// back would ring.
//
// Every storage access is wrapped: localStorage throws in some embedded
// webview configurations, and a preference must never be what stops the
// application from starting — nor what stops the close button from working.

import { EVT, hub, on } from "./bridge";
import type { WindowPrefs } from "./types";

const KEY = "agenthub.window.prefs";

/** Minimising is the default because it is the behaviour the tray exists to
 *  provide. The notice is unseen, so the first close explains itself. */
export const DEFAULT_WINDOW_PREFS: WindowPrefs = { closeToTray: true, hideNoticeSeen: false };

let current: WindowPrefs = DEFAULT_WINDOW_PREFS;
const listeners = new Set<(p: WindowPrefs) => void>();

function read(): WindowPrefs {
  try {
    const raw = localStorage.getItem(KEY);
    if (raw === null) return DEFAULT_WINDOW_PREFS;
    const parsed = JSON.parse(raw) as Partial<WindowPrefs>;
    return {
      closeToTray: parsed.closeToTray ?? DEFAULT_WINDOW_PREFS.closeToTray,
      hideNoticeSeen: parsed.hideNoticeSeen ?? DEFAULT_WINDOW_PREFS.hideNoticeSeen,
    };
  } catch {
    // Unreadable or unparseable: the defaults are a working application.
    return DEFAULT_WINDOW_PREFS;
  }
}

function write(p: WindowPrefs): void {
  try {
    localStorage.setItem(KEY, JSON.stringify(p));
  } catch {
    // The preference still applies to this session; it just will not be
    // remembered.
  }
}

/** The values in effect right now, without a round trip. */
export function windowPrefs(): WindowPrefs {
  return current;
}

/** Subscribes to changes from anywhere — this window or the tray. Returns the
 *  unsubscribe function. */
export function onWindowPrefs(cb: (p: WindowPrefs) => void): () => void {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

function apply(p: WindowPrefs): void {
  const same = current.closeToTray === p.closeToTray && current.hideNoticeSeen === p.hideNoticeSeen;
  current = p;
  // The Go side announces every change, including the echo of one this window
  // just made. Re-rendering on the echo is harmless but redraws the page a
  // second time for one click, which is visible as a flicker.
  if (same) return;
  for (const cb of listeners) cb(p);
}

/**
 * Changes them: stores, tells the Go side, notifies this window.
 *
 * The local state moves first and is not rolled back if the push fails. The
 * failure mode that matters is the opposite one: a preference that looks
 * unchanged because the Go side was slow leaves the user clicking a switch
 * that appears stuck. A push that genuinely cannot happen means the bound
 * service is gone, and nothing about this window works then anyway.
 */
export async function setWindowPrefs(next: Partial<WindowPrefs>): Promise<void> {
  const p = { ...current, ...next };
  apply(p);
  write(p);
  await hub.setWindowPreferences(p);
}

/**
 * Loads the stored values, hands them to the Go side, and keeps the two in
 * step afterwards.
 *
 * Called at startup, before the user can reach a close button: until this
 * lands the Go side assumes the defaults, which are the same values a fresh
 * installation has.
 */
export function initWindowPrefs(): void {
  apply(read());
  // The tray changed it: persist and re-render, but do not answer.
  on<WindowPrefs>(EVT.windowPrefs, (p) => {
    apply(p);
    write(p);
  });
  hub.setWindowPreferences(current).catch(() => {
    // No bound service (a browser-only preview of the frontend). The window
    // has no native close button to decide about either.
  });
}
