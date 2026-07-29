// Tokens page: the agent tokens of the daemon's HTTP data plane.
//
// The plaintext appears EXACTLY ONCE, in the dialog that follows a successful
// create. agenthub keeps only an HMAC, so it genuinely cannot print the value
// again — the dialog says so in those words, offers a copy button, and the
// value is dropped when the dialog closes. Nothing on this page stores it,
// re-renders it or puts it anywhere a later listing could pick it up: the
// listing type carries a 12-character prefix and metadata, and no value field
// exists on it.
//
// Revoked rows are KEPT rather than removed: the name stays reserved and an
// operator reading an audit record can still resolve the name that produced
// it.

import { copyText, hub } from "../bridge";
import { clear, el, empty, relTime, section, table } from "../dom";
import type { Page } from "../page";
import { failureBox, noticeSlot } from "../page";
import {
  button,
  confirmAction,
  controls,
  field,
  namePicker,
  openModal,
  selectInput,
  textInput,
} from "../ui";
import type { Server, Token, TokenCreated } from "../types";
import { Tier, TokenServerWildcard } from "../types";

/** How a token's server allowlist reads, keeping the three states apart:
 *  null = every server, ["*"] = every server explicitly, [] = NOTHING. */
function describeAllowlist(servers: string[] | null): string {
  if (servers === null) return "every server";
  if (servers.length === 0) return "no server at all";
  if (servers.length === 1 && servers[0] === TokenServerWildcard) return "every server (explicit)";
  return servers.join(", ");
}

function stateBadge(state: string): HTMLElement {
  const cls =
    state === "active"
      ? "badge-healthy"
      : state === "expired"
        ? "badge-degraded"
        : state === "invalid"
          ? "badge-unhealthy"
          : "badge-disabled";
  return el("span", { class: `badge ${cls}`, text: state });
}

/** The one-time reveal. It is a modal rather than a row so that closing it is
 *  a deliberate act, and it is the only place in this application where a
 *  credential is ever rendered. */
function showValueOnce(created: TokenCreated): void {
  const value = el("code", { class: "token-value", text: created.value });
  const copy = button("Copy", "btn", () => {
    void copyText(created.value);
    copy.textContent = "copied";
    setTimeout(() => (copy.textContent = "Copy"), 1200);
  });
  openModal(
    `Token ${created.token.name}`,
    [
      el("p", {
        class: "danger-hint",
        text: "This is the only time this value is ever shown. agenthub stores only its hash — once this dialog is closed the value cannot be recovered, from here or from the CLI.",
      }),
      value,
      controls(copy),
      el("div", { class: "kvs" }, [
        el("div", { class: "kv" }, [
          el("span", { class: "k", text: "Prefix" }),
          el("span", { class: "v", text: created.token.prefix }),
        ]),
        el("div", { class: "kv" }, [
          el("span", { class: "k", text: "Tier" }),
          el("span", { class: "v", text: created.token.tier }),
        ]),
        el("div", { class: "kv" }, [
          el("span", { class: "k", text: "Servers" }),
          el("span", { class: "v", text: describeAllowlist(created.token.servers) }),
        ]),
      ]),
    ],
    { danger: true },
  );
}

export function tokensPage(): Page {
  let root: HTMLElement | null = null;
  const slot = noticeSlot();
  let tokens: Token[] = [];
  let servers: Server[] = [];
  let creating = false;

  async function revoke(t: Token): Promise<void> {
    const ok = await confirmAction({
      title: `Revoke ${t.name}?`,
      body: "The credential stops working at its holder's next request — the check is per request, not per connection.",
      consequences: [
        "Revocation cannot be undone: a new token has to be minted, with a new value.",
        "The row is kept so audit records naming it stay resolvable, and the name stays reserved.",
      ],
      confirmLabel: "Revoke",
      danger: true,
    });
    if (!ok) return;
    try {
      const res = await hub.revokeToken(t.name);
      slot.say(`${res.name} (${res.prefix}) revoked.`);
      await draw();
    } catch (err) {
      slot.fail(err);
    }
  }

  function createForm(): Node {
    const name = textInput("", "token name");
    const tier = selectInput(
      [
        { value: Tier.Read, label: "read (read-classified tools only)" },
        { value: Tier.Write, label: "write (adds write-classified tools)" },
        { value: Tier.Destructive, label: "destructive (adds destructive tools)" },
      ],
      Tier.Read,
    );
    const reach = selectInput(
      [
        { value: "all", label: "Every server" },
        { value: "only", label: "Only the ticked servers" },
        { value: "none", label: "No server at all" },
      ],
      "all",
    );
    const picker = namePicker(servers.map((s) => s.id), []);
    const syncReach = (): void => {
      picker.node.hidden = reach.value !== "only";
    };
    reach.addEventListener("change", syncReach);
    syncReach();
    const profile = textInput("", "pin to a profile (optional)");
    const expires = textInput("", "expires in seconds (0 or empty = never)");

    const go = button("Create token", "btn", () => {
      const label = name.value.trim();
      if (!label) {
        slot.say("A token needs a name.", "warn");
        return;
      }
      let allow: string[] | null = null;
      if (reach.value === "none") {
        allow = [];
      } else if (reach.value === "only") {
        allow = picker.value();
        if (allow.length === 0) {
          slot.say(
            "“Only the ticked servers” with nothing ticked would be the empty allowlist, which reaches nothing. Tick a server, or choose “No server at all” deliberately.",
            "warn",
          );
          return;
        }
      }
      const seconds = Number.parseInt(expires.value.trim(), 10);
      void (async () => {
        try {
          const created = await hub.createToken({
            name: label,
            tier: tier.value,
            servers: allow,
            profile: profile.value.trim(),
            expires_in_seconds: Number.isFinite(seconds) && seconds > 0 ? seconds : 0,
          });
          creating = false;
          await draw();
          slot.say(`${created.token.name} created — copy the value now, it is not shown again.`, "warn");
          showValueOnce(created);
        } catch (err) {
          slot.fail(err);
        }
      })();
    });

    return el("div", { class: "panel panel-inset" }, [
      el("h3", { text: "New agent token" }),
      el("div", { class: "form-inline" }, [
        field("Name", name),
        field("Tier", tier, "an unstated tier is read: the closed end of the ladder"),
        field("Servers", reach),
        field("Profile", profile, "pins the token to one profile permanently"),
        field("Expires in", expires),
      ]),
      picker.node,
      controls(go, button("Cancel", "btn btn-secondary", () => {
        creating = false;
        void draw();
      })),
    ]);
  }

  async function draw(): Promise<void> {
    if (!root) return;
    let err: unknown = null;
    try {
      const [t, s] = await Promise.all([hub.listTokens(), hub.listServers()]);
      tokens = t ?? [];
      servers = s ?? [];
    } catch (e) {
      err = e;
    }
    clear(root);
    root.append(
      section(
        "Agent tokens",
        controls(
          button("New token", "btn", () => {
            creating = true;
            void draw();
          }),
        ),
        slot.node,
        creating ? createForm() : el("span", {}),
        err
          ? failureBox(err)
          : tokens.length === 0
            ? empty("No agent tokens. They authenticate callers of the daemon's HTTP data plane.")
            : table(
                ["Name", "Prefix", "Tier", "Reach", "State", ""],
                tokens.map((t) => [
                  el("div", {}, [
                    el("strong", { text: t.name }),
                    el("div", { class: "muted", text: `created ${relTime(t.created_at)}` }),
                    t.expires_at
                      ? el("div", { class: "muted", text: `expires ${relTime(t.expires_at)}` })
                      : null,
                  ]),
                  el("span", { class: "mono", text: t.prefix }),
                  el("span", { text: t.tier }),
                  el("div", {}, [
                    el("div", {
                      class: t.servers?.length === 0 ? "badge badge-unhealthy" : "",
                      text: describeAllowlist(t.servers),
                    }),
                    t.profile ? el("div", { class: "muted", text: `profile ${t.profile}` }) : null,
                  ]),
                  stateBadge(t.state),
                  t.state === "revoked"
                    ? el("span", { class: "muted", text: relTime(t.revoked_at) })
                    : button("Revoke", "btn btn-deny", () => void revoke(t)),
                ]),
              ),
        el("p", {
          class: "hint",
          text: "Listings carry a prefix and metadata only. The value exists once, in the dialog that follows a create.",
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
      creating = false;
    },
  };
}
