// Skills page: the library, its coarse enable switch and the
// (skill x client x scope) install matrix with an ApplyState per cell.
//
// Two things this page states rather than hides:
//
//   - the enable switch is COARSE. Disabling does not unmaterialize anything:
//     the bytes stay on disk until a sync or an explicit removal converges
//     the target, and the install receipts keep reporting them honestly.
//   - installation is CLIENT-level, not per-session. The files live outside
//     agenthub's read path, so the scope chain cannot narrow them, and
//     presenting an install as a per-session control would be a lie.
//
// A refused install (drift, a foreign file at the target) is a 409 that
// re-reading does not fix. It is reported with its message and hint, and the
// only way past it is the explicit "overwrite a locally edited copy" tick —
// drift is a user telling us something, and reverting it silently is how a
// sync tool teaches people to distrust its receipts.

import { EVT, hub, on } from "../bridge";
import { clear, el, empty, section, table } from "../dom";
import type { Page } from "../page";
import { failureBox, noticeSlot } from "../page";
import { button, checkboxInput, confirmAction, controls, field, selectInput, textInput } from "../ui";
import type { Skill, SkillInstall, TopicEvent } from "../types";

function stateBadge(state: string): HTMLElement {
  const cls =
    state === "applied"
      ? "badge-healthy"
      : state === "drifted" || state === "outdated"
        ? "badge-degraded"
        : "badge-unhealthy";
  return el("span", { class: `badge ${cls}`, text: state });
}

function installCell(installs: SkillInstall[] | undefined): Node {
  if (!installs || installs.length === 0) {
    return el("span", { class: "muted", text: "not installed" });
  }
  return el(
    "div",
    { class: "installs" },
    installs.map((i) =>
      el("div", { class: "install" }, [
        stateBadge(i.state),
        el("span", { text: `${i.client_id} · ${i.scope}` }),
        el("span", { class: "muted mono", text: i.path }),
        i.detail ? el("span", { class: "hint", text: i.detail }) : null,
      ]),
    ),
  );
}

export function skillsPage(): Page {
  let root: HTMLElement | null = null;
  let off: (() => void) | null = null;
  const slot = noticeSlot();
  let skills: Skill[] = [];
  let target: Skill | null = null;

  async function toggle(s: Skill): Promise<void> {
    const next = !s.enabled;
    if (!next) {
      const ok = await confirmAction({
        title: `Disable ${s.name || s.id}?`,
        body: "The skill stops being offered.",
        consequences: [
          "Already installed copies are NOT removed: the switch is coarse, and the install receipts keep reporting the files that are still on disk.",
        ],
        confirmLabel: "Disable",
      });
      if (!ok) return;
    }
    try {
      const updated = await hub.setSkillEnabled(s.id, next);
      slot.say(`${updated.name || updated.id} ${updated.enabled ? "enabled" : "disabled"}.`);
      await draw();
    } catch (err) {
      slot.fail(err);
    }
  }

  function installForm(s: Skill): Node {
    const client = textInput("", "client id (claude, cursor, …)");
    const scope = selectInput(
      [
        { value: "user", label: "user (this account)" },
        { value: "project", label: "project (one repository)" },
      ],
      "user",
    );
    const projectRoot = textInput("", "absolute path to the project root");
    const dir = textInput("", "target directory override (required for the generic target)");
    const allowDrift = checkboxInput("Overwrite a copy that was edited outside agenthub", false);
    const syncScope = (): void => {
      projectRoot.disabled = scope.value !== "project";
    };
    scope.addEventListener("change", syncScope);
    syncScope();

    const go = button("Install", "btn", () => {
      const id = client.value.trim();
      if (!id) {
        slot.say("Name the client to install into.", "warn");
        return;
      }
      if (scope.value === "project" && !projectRoot.value.trim()) {
        slot.say("Project scope needs a project root.", "warn");
        return;
      }
      void (async () => {
        try {
          const cell = await hub.installSkill(s.id, {
            client_id: id,
            scope: scope.value,
            project_root: projectRoot.value.trim(),
            dir: dir.value.trim(),
            allow_drift: allowDrift.box.checked,
          });
          slot.say(`${s.name || s.id} → ${cell.client_id} (${cell.scope}): ${cell.state} at ${cell.path}`);
          target = null;
          await draw();
        } catch (err) {
          slot.fail(err);
        }
      })();
    });

    return el("div", { class: "panel panel-inset" }, [
      el("h3", { text: `Install ${s.name || s.id}` }),
      el("div", { class: "form-inline" }, [
        field("Client", client),
        field("Scope", scope),
        field("Project root", projectRoot),
        field("Directory", dir),
      ]),
      allowDrift.node,
      el("p", {
        class: "hint",
        text: "Installation is client-level: the files land outside agenthub's read path, so no session scope can narrow them.",
      }),
      controls(go, button("Cancel", "btn btn-secondary", () => {
        target = null;
        void draw();
      })),
    ]);
  }

  async function draw(): Promise<void> {
    if (!root) return;
    try {
      skills = (await hub.listSkills()) ?? [];
    } catch (err) {
      clear(root);
      root.append(failureBox(err));
      root.append(
        el("p", { class: "hint", text: "Meanwhile: `agenthub skill ls`, `agenthub skill verify --all`." }),
      );
      return;
    }
    clear(root);
    const body =
      skills.length === 0
        ? empty("The library is empty. Add one with `agenthub skill add <path>`.")
        : table(
            ["Skill", "State", "Installs", "Actions"],
            skills.map((s) => [
              el("div", {}, [
                el("strong", { text: s.name || s.id }),
                s.description ? el("div", { class: "muted", text: s.description }) : null,
                s.fingerprint ? el("div", { class: "muted mono", text: s.fingerprint }) : null,
              ]),
              s.enabled
                ? el("span", { class: "badge badge-healthy", text: "enabled" })
                : el("span", { class: "badge badge-disabled", text: "disabled" }),
              installCell(s.installs),
              controls(
                button(s.enabled ? "Disable" : "Enable", "btn", () => void toggle(s)),
                button("Install to…", "btn", () => {
                  target = s;
                  void draw();
                }),
              ),
            ]),
          );
    root.append(
      section("Skills", slot.node, target ? installForm(target) : el("span", {}), body),
    );
  }

  return {
    render(node) {
      root = node;
      off = on<TopicEvent>(EVT.skills, () => void draw());
      return draw();
    },
    dispose() {
      off?.();
      off = null;
      root = null;
      target = null;
    },
  };
}
