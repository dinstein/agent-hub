// The one-time question the close button asks before it starts minimising.
//
// WHY IT EXISTS AT ALL. Silently vanishing into the status area is the
// standard complaint about tray applications: the user closes a window,
// believes the application is gone, and finds out otherwise days later. It is
// worse here than in most applications, because what keeps running is a hub
// other programs are connected to.
//
// It is also the escape hatch. Someone who meant "quit" would otherwise have
// to discover the tray menu to undo a click they did not know was reversible,
// so the answer they give here becomes what the close button does from now on
// — said out loud in the dialog, and changeable in Settings.

import { hub } from "./bridge";
import { el } from "./dom";
import { button, controls, openModal } from "./ui";
import { setWindowPrefs } from "./window-prefs";

/**
 * Shows the dialog. The Go side has already cancelled the close and brought
 * the window forward, so there is something to show it in.
 *
 * Dismissing it (Escape, the ×, a click outside) does nothing and persists
 * nothing: the window stays open and the question is asked again next time.
 * An accidental dismissal must never be read as either answer.
 */
export async function askAboutClosing(): Promise<void> {
  let stopsTheHub = false;
  try {
    stopsTheHub = await hub.ownsDaemon();
  } catch {
    // Offline or unbound: the warning below is simply not shown. Claiming
    // the hub will stop when we cannot tell would be the wrong guess.
  }

  let close = (): void => {};
  const minimise = button("Minimise to tray", "btn btn-primary", () => {
    close();
    void setWindowPrefs({ closeToTray: true, hideNoticeSeen: true }).then(() => hub.hideWindow());
  });
  const quit = button("Quit AgentHub", "btn btn-secondary", () => {
    close();
    // The preference is recorded before quitting so the next launch's close
    // button does what this click asked for, rather than asking again.
    void setWindowPrefs({ closeToTray: false, hideNoticeSeen: true }).then(() => hub.quitApplication());
  });

  close = openModal("Keep AgentHub running?", [
    el("p", {
      text:
        "Closing the window leaves AgentHub running in the tray, so the clients connected " +
        "to it keep working. Its icon stays in the menu bar — click it to bring the window back.",
    }),
    stopsTheHub
      ? el("p", {
          class: "hint",
          text: "Quitting stops the hub as well: this window started it, and nothing else is holding it open.",
        })
      : null,
    el("p", {
      class: "hint",
      text: "Whichever you choose becomes what the close button does. You can change it in Settings.",
    }),
    controls(minimise, quit),
  ]);
}
