// Profiles page: the named tier of the scope chain.
//
// Two three-state selectors live here and both have the same failure mode:
// the difference between "no rule" and "an empty allow list" is the
// difference between exposing everything and exposing nothing. The UI keeps
// them as three visible, separately-named states — never as a list that
// happens to be empty — and refuses "only these" with nothing ticked instead
// of sending it (api/profiles.go, ui.triState).
//
// A rename is an operation, not a delete-then-create: the daemon repoints
// every client that referenced the old name, and reports which ones. A delete
// deliberately does NOT rewrite references — the clients left behind resolve
// to an EMPTY scope, which is the fail-closed direction, and they come back
// in the write's `dangling` list so the page can say it out loud.

import { hub, knownTools } from "../bridge";
import { clear, el, empty, pageHeader } from "../dom";
import type { Page } from "../page";
import { failureBox, noticeSlot, runWrite } from "../page";
import {
  button,
  confirmAction,
  controls,
  describeSelector,
  describeServerSet,
  field,
  formHost,
  selectInput,
  textInput,
  triState,
} from "../ui";
import type { Profile, ProfileList, ProfileTools, Server } from "../types";
import { ServerSet, ToolSelect } from "../types";

/** Maps a three-state selection onto the member-server field: null clears the
 *  narrowing, [] is block-all, [...] is exactly those. */
function serversFrom(sel: ProfileTools): string[] | null {
  if (sel.mode === ToolSelect.All) return null;
  if (sel.mode === ToolSelect.None) return [];
  return sel.tools ?? [];
}

function describeServersChoice(servers: string[] | null): string {
  if (servers === null) return "every registered server";
  if (servers.length === 0) return "no server at all";
  return servers.join(", ");
}

export function profilesPage(): Page {
  let root: HTMLElement | null = null;
  const slot = noticeSlot();
  const form = formHost();
  let list: ProfileList | null = null;
  let servers: Server[] = [];

  const serverIds = (): string[] => servers.map((s) => s.id);
  const generation = (): number => list?.generation ?? 0;

  // -- creation -------------------------------------------------------------

  function createForm(): Node {
    const name = textInput("", "profile name");
    const members = triState(serverIds(), undefined, {
      all: "Every registered server",
      only: "Only the ticked servers",
      none: "No server at all (block-all)",
    });
    const errors = el("div", { class: "notice-slot" });
    const save = button("Create profile", "btn", () => {
      clear(errors);
      if (!name.value.trim()) {
        errors.append(el("div", { class: "notice notice-warn", text: "A profile needs a name." }));
        return;
      }
      const sel = members.value();
      if (!sel.ok) {
        errors.append(el("div", { class: "notice notice-warn", text: sel.message }));
        return;
      }
      const chosen = serversFrom(sel.selection);
      void runWrite(
        slot,
        () => draw(),
        (r) => `Profile ${r.name} created (${describeServersChoice(chosen)}).`,
        () => hub.createProfile(name.value.trim(), chosen, generation()),
      ).then((ok) => {
        if (ok) form.hide();
      });
    });
    return el("div", { class: "panel panel-inset" }, [
      el("h3", { text: "New profile" }),
      errors,
      field("Name", name),
      field("Member servers", members.node),
      controls(save, button("Cancel", "btn btn-secondary", () => form.hide())),
    ]);
  }

  // -- member set -----------------------------------------------------------

  function membersForm(p: Profile): Node {
    const members = triState(serverIds(), { allow: p.servers }, {
      all: "Every registered server",
      only: "Only the ticked servers",
      none: "No server at all (block-all)",
    });
    const errors = el("div", { class: "notice-slot" });
    const save = button("Replace member set", "btn", () => {
      clear(errors);
      const sel = members.value();
      if (!sel.ok) {
        errors.append(el("div", { class: "notice notice-warn", text: sel.message }));
        return;
      }
      const chosen = serversFrom(sel.selection);
      void runWrite(
        slot,
        () => draw(),
        (r) => `${r.name}: members are now ${describeServersChoice(chosen)}.`,
        () =>
          hub.setProfileServers(
            p.name,
            { mode: ServerSet.Replace, servers: chosen },
            generation(),
          ),
      ).then((ok) => {
        if (ok) form.hide();
      });
    });
    return el("div", { class: "panel panel-inset" }, [
      el("h3", { text: `Member servers of ${p.name}` }),
      errors,
      members.node,
      el("p", {
        class: "hint",
        text: "“No server at all” is stored as an explicit empty set. It is not the same thing as having no rule, which exposes everything.",
      }),
      controls(save, button("Cancel", "btn btn-secondary", () => form.hide())),
    ]);
  }

  // -- per-server tool selector ---------------------------------------------

  async function toolsForm(p: Profile, server: string): Promise<Node> {
    const available = await knownTools(server);
    const picker = triState(available, p.tools?.[server]);
    const errors = el("div", { class: "notice-slot" });
    const save = button("Apply selector", "btn", () => {
      clear(errors);
      const sel = picker.value();
      if (!sel.ok) {
        errors.append(el("div", { class: "notice notice-warn", text: sel.message }));
        return;
      }
      void runWrite(
        slot,
        () => draw(),
        (r) => `${r.name}: ${server} is now "${sel.selection.mode}".`,
        () => hub.setProfileTools(p.name, server, sel.selection, generation()),
      ).then((ok) => {
        if (ok) form.hide();
      });
    });
    return el("div", { class: "panel panel-inset" }, [
      el("h3", { text: `Tools of ${server} in ${p.name}` }),
      errors,
      picker.node,
      el("p", {
        class: "hint",
        text: "Selectors are keyed by the ORIGINAL downstream tool names, not by any renamed exposure — otherwise a rename would walk out from under its own narrowing rule.",
      }),
      controls(save, button("Cancel", "btn btn-secondary", () => form.hide())),
    ]);
  }

  function toolRuleChooser(p: Profile): Node {
    const server = selectInput(
      serverIds().map((id) => ({ value: id, label: id })),
      serverIds()[0] ?? "",
    );
    const open = button("Edit selector", "btn", () => {
      if (!server.value) return;
      void toolsForm(p, server.value).then((node) => form.show(node));
    });
    return el("div", { class: "panel panel-inset" }, [
      el("h3", { text: `Tool rules of ${p.name}` }),
      field("Server", server, "the rule applies to this server's tools inside this profile"),
      controls(open, button("Cancel", "btn btn-secondary", () => form.hide())),
    ]);
  }

  // -- destructive / naming -------------------------------------------------

  function rename(p: Profile): void {
    const input = textInput(p.name, "new name");
    const errors = el("div", { class: "notice-slot" });
    form.show(
      el("div", { class: "panel panel-inset" }, [
        el("h3", { text: `Rename ${p.name}` }),
        errors,
        field("New name", input, "every client and project reference is repointed automatically"),
        controls(
          button("Rename", "btn", () => {
            clear(errors);
            const next = input.value.trim();
            if (!next || next === p.name) {
              errors.append(
                el("div", { class: "notice notice-warn", text: "Enter a different name." }),
              );
              return;
            }
            void runWrite(
              slot,
              () => draw(),
              (r) =>
                `${r.old_name ?? p.name} renamed to ${r.name}` +
                (r.repointed?.length ? ` (repointed: ${r.repointed.join(", ")})` : "") +
                ".",
              () => hub.renameProfile(p.name, next, generation()),
            ).then((ok) => {
              if (ok) form.hide();
            });
          }),
          button("Cancel", "btn btn-secondary", () => form.hide()),
        ),
      ]),
    );
  }

  async function remove(p: Profile): Promise<void> {
    const ok = await confirmAction({
      title: `Delete profile ${p.name}?`,
      body: "The profile and its selectors are removed.",
      consequences: [
        "Clients still bound to it are NOT rewritten: each one resolves to an empty scope until it is rebound, which is the fail-closed direction.",
        "The affected client ids are reported after the delete.",
      ],
      confirmLabel: "Delete",
      danger: true,
    });
    if (!ok) return;
    await runWrite(
      slot,
      () => draw(),
      (r) =>
        `Profile ${r.name} deleted.` +
        (r.dangling?.length
          ? ` These clients now resolve to an EMPTY scope: ${r.dangling.join(", ")}.`
          : "") +
        (r.active_cleared ? " The active-profile marker was cleared." : ""),
      () => hub.deleteProfile(p.name, generation()),
    );
  }

  async function setActive(p: Profile, active: boolean): Promise<void> {
    if (!active) {
      const ok = await confirmAction({
        title: "Clear the active profile?",
        body: "Every client that does not name a profile itself falls back to seeing every registered server.",
        confirmLabel: "Clear",
        danger: true,
      });
      if (!ok) return;
    }
    await runWrite(
      slot,
      () => draw(),
      () => (active ? `${p.name} is now the active profile.` : "The active-profile marker is cleared."),
      () =>
        active
          ? hub.setActiveProfile(p.name, generation())
          : hub.clearActiveProfile(p.name, generation()),
    );
  }

  // -- rendering ------------------------------------------------------------

  function toolsCell(p: Profile): Node {
    const entries = Object.entries(p.tools ?? {});
    if (entries.length === 0) return el("span", { class: "muted", text: "no rule (all tools)" });
    return el(
      "div",
      { class: "rows" },
      entries.map(([server, sel]) =>
        el("div", { class: "install" }, [
          el("strong", { text: server }),
          el("span", { class: sel.allow?.length === 0 ? "badge badge-unhealthy" : "muted", text: describeSelector(sel) }),
        ]),
      ),
    );
  }

  function profileCard(p: Profile, active: string): Node {
    const isActive = active === p.name;
    return el("article", { class: `access-card${isActive ? " access-card-active" : ""}` }, [
      el("div", { class: "access-card-head" }, [
        el("div", {}, [
          el("div", { class: "access-title-row" }, [
            el("strong", { class: "access-title", text: p.name }),
            isActive ? el("span", { class: "badge badge-healthy", text: "active default" }) : null,
          ]),
          el("div", {
            class: "muted",
            text: isActive
              ? "Used by clients that do not select a profile themselves."
              : "A reusable access boundary for one or more clients.",
          }),
        ]),
        controls(
          isActive
            ? button("Clear default", "btn btn-secondary", () => void setActive(p, false))
            : button("Make default", "btn btn-secondary", () => void setActive(p, true)),
          button("Rename", "btn btn-secondary", () => void rename(p)),
          button("Delete", "btn btn-deny", () => void remove(p)),
        ),
      ]),
      el("div", { class: "access-card-grid" }, [
        el("section", { class: "access-rule" }, [
          el("div", { class: "access-rule-head" }, [
            el("span", { class: "access-rule-label", text: "Member servers" }),
            button("Edit members", "btn btn-secondary", () => form.show(membersForm(p))),
          ]),
          el("div", {
            class: p.servers?.length === 0 ? "access-rule-value danger" : "access-rule-value",
            text: describeServerSet(p.servers),
          }),
          el("span", { class: "hint", text: "This is the first intersection in the profile scope." }),
        ]),
        el("section", { class: "access-rule" }, [
          el("div", { class: "access-rule-head" }, [
            el("span", { class: "access-rule-label", text: "Tool selectors" }),
            button("Edit rules", "btn btn-secondary", () => form.show(toolRuleChooser(p))),
          ]),
          toolsCell(p),
        ]),
      ]),
    ]);
  }

  async function draw(): Promise<void> {
    if (!root) return;
    try {
      // A Go nil slice marshals to null, so every list is normalised on
      // arrival: a page that throws on null renders nothing at all, which
      // looks exactly like "there is no configuration".
      const [pl, sl] = await Promise.all([hub.listProfiles(), hub.listServers()]);
      list = { ...pl, profiles: pl.profiles ?? [] };
      servers = sl ?? [];
    } catch (err) {
      clear(root);
      root.append(failureBox(err));
      return;
    }
    clear(root);
    const active = list.active;
    root.append(
      pageHeader(
        "Profiles",
        "Define reusable server and tool boundaries, then assign clients to the right one.",
        button("New profile", "btn btn-primary", () => form.show(createForm())),
      ),
      slot.node,
      form.node,
      el("div", { class: "scope-note" }, [
        el("span", { class: "scope-note-mark", text: "∩" }),
        el("span", {
          text: "Profiles can only narrow what a server already exposes. An empty selection is deliberately block-all, never “no rule”.",
        }),
      ]),
      list.profiles.length === 0
        ? empty("No profiles configured. Without one, every client sees every registered server.")
        : el("div", { class: "access-card-list" }, list.profiles.map((p) => profileCard(p, active))),
      el("p", {
        class: "hint page-footnote",
        text: list.active_known
          ? active
            ? `Clients that do not name a profile follow “${active}”.`
            : "No default profile: clients that do not name one see every registered server."
          : "This daemon cannot report the default profile at all — which is not the same as there being none.",
      }),
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
