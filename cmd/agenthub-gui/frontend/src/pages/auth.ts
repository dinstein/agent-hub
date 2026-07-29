// Auth page: the stored OAuth credentials of downstream servers.
//
// SCOPE LIMIT (docs/modules/controlplane.md): only the non-interactive half
// is here. An interactive login needs a local browser and a loopback callback
// on a random port — a second, easily-broken code path with little to show
// for it — so `login` stays a CLI flow and this page names the command
// instead of offering a button that half works.
//
// RED LINE: no token, no client secret, no refresh token is ever rendered.
// `has_refresh_token` is a boolean and that is all the page can say, because
// that is all the API carries.
//
// Two readings this page is careful about:
//   - expires_at == 0 means the provider advertised NO expiry at all, which
//     is "never expires", not "expired" (docs/modules/oauth.md);
//   - logout removes the credential FROM THIS MACHINE. agenthub cannot revoke
//     it at the provider, so the UI must not claim it did.

import { hub } from "../bridge";
import { clear, el, empty, section, table } from "../dom";
import type { Page } from "../page";
import { failureBox, noticeSlot } from "../page";
import { button, confirmAction, controls, textInput } from "../ui";
import type { AuthStatus } from "../types";
import { AuthState } from "../types";

function stateBadge(state: string): HTMLElement {
  const cls =
    state === AuthState.Authorized
      ? "badge-healthy"
      : state === AuthState.Expiring
        ? "badge-degraded"
        : state === AuthState.None
          ? "badge-disabled"
          : "badge-unhealthy";
  return el("span", { class: `badge ${cls}`, text: state });
}

/** Renders the deadline honestly: an absent expiry is not an expired one. */
function expiry(s: AuthStatus): string {
  if (s.expires_at === 0) return "no expiry advertised";
  if (s.expires_in <= 0) return `expired ${Math.abs(Math.round(s.expires_in / 60))} min ago`;
  if (s.expires_in < 3600) return `in ${Math.round(s.expires_in / 60)} min`;
  if (s.expires_in < 86400) return `in ${Math.round(s.expires_in / 3600)} h`;
  return new Date(s.expires_at * 1000).toLocaleString();
}

export function authPage(): Page {
  let root: HTMLElement | null = null;
  const slot = noticeSlot();
  let rows: AuthStatus[] = [];
  let filter = "";

  async function refresh(s: AuthStatus): Promise<void> {
    try {
      const res = await hub.refreshAuth(s.server);
      slot.say(
        res.superseded
          ? `${res.server}: another writer refreshed first and this call adopted its result — that is a success, not a race lost.`
          : `${res.server} refreshed; the new token expires ${expiry({ ...s, expires_at: res.expires_at, expires_in: res.expires_in })}.`,
      );
      await draw();
    } catch (err) {
      slot.fail(err);
    }
  }

  async function logout(s: AuthStatus): Promise<void> {
    const ok = await confirmAction({
      title: `Remove the credential for ${s.server}?`,
      body: "The stored tokens are deleted from this machine.",
      consequences: [
        "This does NOT revoke anything at the provider — agenthub cannot promise that. Revoke it there as well if that is what you meant.",
        "The server needs an interactive `agenthub auth login` before it works again.",
      ],
      confirmLabel: "Remove from this machine",
      danger: true,
    });
    if (!ok) return;
    try {
      const res = await hub.logoutAuth(s.server);
      slot.say(`${res.server}: credentials removed from this machine.`);
      await draw();
    } catch (err) {
      slot.fail(err);
    }
  }

  async function draw(): Promise<void> {
    if (!root) return;
    let err: unknown = null;
    try {
      rows = (await hub.authStatus(filter)) ?? [];
    } catch (e) {
      err = e;
      rows = [];
    }
    clear(root);
    const server = textInput(filter, "one server id (empty = every server that has credentials)");
    root.append(
      section(
        "Authorization",
        controls(
          server,
          button("Show", "btn", () => {
            filter = server.value.trim();
            void draw();
          }),
        ),
        slot.node,
        err
          ? failureBox(err)
          : rows.length === 0
            ? empty(
                filter
                  ? `${filter} has no stored credentials.`
                  : "No server has stored credentials. Servers that never had any are omitted from this listing.",
              )
            : table(
                ["Server", "State", "Expiry", "Issuer / scope", "Actions"],
                rows.map((s) => [
                  el("div", {}, [
                    el("strong", { text: s.server }),
                    s.detail ? el("div", { class: "muted", text: s.detail }) : null,
                  ]),
                  el("div", {}, [
                    stateBadge(s.state),
                    el("div", {
                      class: "muted",
                      text: s.has_refresh_token
                        ? "refreshable without a human"
                        : "no refresh token: expiry needs a new login",
                    }),
                  ]),
                  el("span", { text: expiry(s) }),
                  el("div", {}, [
                    el("div", { class: "muted", text: s.issuer || "—" }),
                    s.scope ? el("div", { class: "muted mono", text: s.scope }) : null,
                    s.client_registrar
                      ? el("div", { class: "muted", text: `registrar ${s.client_registrar}` })
                      : null,
                  ]),
                  controls(
                    button("Refresh", "btn", () => void refresh(s)),
                    button("Log out", "btn btn-deny", () => void logout(s)),
                  ),
                ]),
              ),
        el("p", {
          class: "hint",
          text: "Signing in is a CLI flow: `agenthub auth login <server> --device` or `--manual`. Only status, refresh and logout live on the control plane.",
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
