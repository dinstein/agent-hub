// Catalog page: the answer to "adding a server means typing
// `npx -y @modelcontextprotocol/server-filesystem` from memory"
// (docs/modules/gui.md).
//
// THE ONE JUDGEMENT THIS PAGE MAKES is the split between the entries that
// can be added with a single click and the ones that cannot. It does NOT
// make that judgement itself: `needs_config` is computed by the daemon, so
// the GUI and `agenthub catalog add` divide the same list the same way. An
// entry that needs nothing is added by one button and zero dialogs; only an
// entry with a declared parameter opens a form, and that form arrives
// pre-filled from the catalog definition rather than empty.
//
// A credential is NOT a reason to open a form. `${SECRET_X}` survives into
// the stored entry and is resolved at connect time from the vault, so an
// entry that needs a token is still one click — followed by an honest "now
// go and store <KEY>", which is what the daemon's next_steps say and what
// this page renders verbatim.
//
// PROVENANCE IS NOT A SAFETY CLAIM. Nothing in the catalog is signed and
// nothing is verified at add time, so it is never rendered as a badge that
// could read as "verified" or "safe" (api/catalog.go). It is stated once in
// the page intro and then shown per-card ONLY when it is not the ordinary
// grade: every seeded entry is curated, so a "curated" chip on all of them
// would be a token that varies with nothing, and the reader would learn to
// skip the row where a registry or user entry needs to be noticed.
// Consistently with the colour discipline on the Servers page, none of this
// metadata takes a semantic colour — the transport pill and the publisher
// link are neutral.
//
// THE THREE EMPTY STATES ARE THREE (docs/modules/gui.md). "The daemon did
// not answer" and "nothing matches what you typed" are different sentences
// with different next steps, and the failure one says out loud that it is
// not an empty result — a catalog that renders "no entries" after a dropped
// socket tells the user the directory is gone.

import { asCallError, hub, isStalePrecondition } from "../bridge";
import { chip, chipRow, clear, el, emptyState, loadingState, section } from "../dom";
import type { Page } from "../page";
import { CONFLICT_MESSAGE, failureState, noticeSlot } from "../page";
import { button, cliHint, controls, field, formHost, shellArg, textInput } from "../ui";
import type { CatalogAdded, CatalogEntry, CatalogList, CatalogParam } from "../types";
import { CatalogAuthOAuth, CatalogProvenance, Transport } from "../types";

// ---------------------------------------------------------------------------
// Provenance, rendered as origin
// ---------------------------------------------------------------------------

/** What each provenance grade MEANS, in words that cannot be misread as a
 *  verification result. "curated" is a statement about who wrote the line,
 *  not about what the server does once it runs. */
const PROVENANCE_TEXT: Record<string, string> = {
  [CatalogProvenance.Curated]: "curated — written into agenthub by its maintainers",
  [CatalogProvenance.Registry]: "registry — copied from a remote index",
  [CatalogProvenance.User]: "user — added on this machine",
};

/** The footer link's text: who published it, falling back to a plain word.
 *
 *  Naming the publisher in the link rather than beside it merges two things
 *  the reader wants together — where this came from, and where to go and read
 *  about it — into one control, and it puts the name of whoever is being
 *  trusted on the thing you click. The raw URL used to BE the link text,
 *  which is what pushed a github.com/... path past the card edge. */
function docsLabel(e: CatalogEntry): string {
  return e.publisher ? `${e.publisher} ↗` : "Docs ↗";
}

/**
 * A homepage URL, or null.
 *
 * Fail direction: CLOSED. Anything that is not plain http(s) — a `javascript:`
 * URL above all — is dropped rather than linked. The catalog is embedded in
 * the binary today, which is exactly the wrong reason to build a renderer
 * that would happily follow whatever a future remote index ships.
 */
function safeHttpUrl(raw: string | undefined): string | null {
  if (!raw) return null;
  try {
    const u = new URL(raw);
    return u.protocol === "http:" || u.protocol === "https:" ? u.href : null;
  } catch {
    return null;
  }
}

/** The invocation the entry describes, as one line. This is the thing the
 *  user actually recognises an MCP server by — the package name in the npx
 *  line, not our id for it. */
function invocation(e: CatalogEntry): string {
  if (e.transport === Transport.HTTP || e.transport === Transport.SSE) return e.url ?? "";
  return [e.command ?? "", ...(e.args ?? [])].filter(Boolean).join(" ");
}

// ---------------------------------------------------------------------------
// The equivalent CLI command (docs/modules/gui.md)
// ---------------------------------------------------------------------------

/**
 * `agenthub catalog add <id> [--param k=v …]`, mirroring
 * internal/cli.catalogAddCommand.
 *
 * With nothing typed the parameters render as `<placeholders>`, exactly as
 * `catalog show` prints them: a line the user can copy and run unchanged
 * with someone else's path in it is worse than a line they must obviously
 * fill in. Once they HAVE typed a value it goes in, because at that point
 * the command is the one this page is about to run.
 */
function cliAdd(e: CatalogEntry, params: Record<string, string> = {}, name = ""): string {
  let cmd = `agenthub catalog add ${shellArg(e.id)}`;
  if (name && name !== e.id) cmd += ` --name ${shellArg(name)}`;
  for (const p of e.params ?? []) {
    const value = (params[p.name] ?? "").trim();
    cmd += ` --param ${shellArg(`${p.name}=${value || `<${p.name}>`}`)}`;
  }
  return cmd;
}

// ---------------------------------------------------------------------------
// Layout
// ---------------------------------------------------------------------------

// Layout lives in style.css (.ledger / .rec and friends), shared with the
// Servers page because both are lists of records and there is no reason for
// them to align differently. It used to be two inline constants here on the
// theory that this page should not extend the stylesheet; the result was a
// card borrowing .card from the approvals queue, in a grid whose tracks let
// long tokens overflow.

/** internal/catalog.SourcePrefix: how a stored entry says which catalog id
 *  it came from. */
const SOURCE_PREFIX = "catalog:";

/** How long the search box waits before asking the daemon. Long enough that
 *  typing "github" is one request rather than six, short enough to feel
 *  like filtering. */
const SEARCH_DEBOUNCE_MS = 180;

// ---------------------------------------------------------------------------
// The page
// ---------------------------------------------------------------------------

export function catalogPage(): Page {
  let root: HTMLElement | null = null;
  let listRoot: HTMLElement | null = null;
  let searchBox: HTMLInputElement | null = null;
  let query = "";
  let timer: number | undefined;
  const slot = noticeSlot();
  const form = formHost();

  /** catalog id -> the server id it is stored as. */
  let installed = new Map<string, string>();
  /**
   * Every configured server id, or null when the list could not be read.
   *
   * Null is not an empty set: with the server list unavailable this page
   * claims NOTHING about what is already there. Guessing "not installed"
   * would offer a one-click Add that lands on a 409; guessing "installed"
   * would hide the button the user came for.
   */
  let takenIDs: Set<string> | null = null;

  // -- adding ----------------------------------------------------------------

  /** What still has to happen before the server actually works. The daemon
   *  words these, not us, so the GUI and the CLI say the same thing. */
  function nextSteps(added: CatalogAdded): HTMLElement | null {
    const steps = added.next_steps ?? [];
    if (steps.length === 0) return null;
    return el("div", { class: "notice notice-warn" }, [
      el("div", {
        text:
          "The definition is stored, but the server cannot connect yet. " +
          "These finish the job:",
      }),
      ...steps.map((s) => cliHint(s)),
      // The link only appears when a credential is actually one of the
      // steps. An "Open Secrets" button next to "log in with a browser"
      // would send the user to the one page that cannot help them.
      steps.some((s) => s.startsWith("agenthub secret set"))
        ? el("a", { class: "btn btn-secondary", href: "#/secrets", text: "Open Secrets" })
        : null,
    ]);
  }

  /**
   * Runs one add and reports it.
   *
   * expectedGeneration is 0 for the same reason CreateServer sends 0: an id
   * that already exists is refused as a NAME CONFLICT, not as a lost update,
   * so there is nothing here for a precondition to protect — and an operator
   * adding a server should not be blocked by an unrelated edit elsewhere.
   *
   * This spells out the three branches runWrite normally provides rather
   * than calling it, because the success branch has a fourth thing to say:
   * the daemon's next_steps. Reporting "added" without them is how a
   * one-click add ends up looking finished while the server still cannot
   * authenticate.
   */
  async function add(e: CatalogEntry, name: string, params: Record<string, string>): Promise<boolean> {
    let added: CatalogAdded;
    try {
      added = await hub.addFromCatalog(
        e.id,
        {
          ...(name && name !== e.id ? { name } : {}),
          ...(Object.keys(params).length > 0 ? { params } : {}),
        },
        0,
      );
    } catch (err) {
      // Kept for the same reason every other write keeps it: sending 0 means
      // this daemon does not check, not that no daemon ever will.
      if (isStalePrecondition(asCallError(err))) {
        await draw();
        slot.say(CONFLICT_MESSAGE, "warn");
        return false;
      }
      slot.fail(err);
      return false;
    }
    await draw();
    const warnings = added.warnings ?? [];
    slot.clear();
    slot.node.append(
      el("div", { class: warnings.length > 0 ? "notice notice-warn" : "notice" }, [
        el("div", { text: `${added.id} added from the catalog.` }),
        ...warnings.map((w) => el("div", { class: "warn-line", text: `warning: ${w}` })),
      ]),
    );
    const steps = nextSteps(added);
    if (steps) slot.node.append(steps);
    return true;
  }

  // -- the pre-filled form, for the entries that need one --------------------

  /**
   * The form an entry with declared parameters opens.
   *
   * It edits PARAMETERS, not a server definition: `{{name}}` placeholders are
   * substituted by the daemon at add time and must not survive into the
   * stored entry, which is precisely what would happen if this page sent a
   * hand-assembled ServerEntry to /v1/servers instead. So the definition is
   * shown read-only — pre-filled, visibly — and the only editable things are
   * the values the catalog says are missing.
   */
  function paramForm(e: CatalogEntry): Node {
    const nameInput = textInput(e.id, "server id");
    const inputs = new Map<string, HTMLInputElement>();
    const errors = el("div", { class: "notice-slot" });
    const preview = el("code", { class: "cmd" });
    const cliSlot = el("div", { class: "cli-list" });

    const values = (): Record<string, string> => {
      const out: Record<string, string> = {};
      for (const [k, input] of inputs) {
        const v = input.value.trim();
        if (v) out[k] = v;
      }
      return out;
    };

    /** The invocation with whatever has been typed substituted in, so the
     *  user can see the line they are about to store rather than a template. */
    const refresh = (): void => {
      const typed = values();
      let line = invocation(e);
      for (const p of e.params ?? []) {
        const v = typed[p.name];
        if (v) line = line.split(`{{${p.name}}}`).join(v);
      }
      preview.textContent = line || "—";
      clear(cliSlot);
      cliSlot.append(
        cliHint(cliAdd(e, typed, nameInput.value.trim()), {
          note: "the same add, from a terminal",
        }),
      );
    };

    const paramField = (p: CatalogParam): Node => {
      const input = textInput("", p.example ? `e.g. ${p.example}` : p.name);
      input.addEventListener("input", refresh);
      inputs.set(p.name, input);
      return field(p.name, input, p.description);
    };

    const save = button("Add server", "btn btn-primary", () => {
      clear(errors);
      const typed = values();
      // A convenience check only: the daemon refuses a missing parameter
      // itself, and its refusal is the authoritative one. This just saves a
      // round trip on an obviously empty box.
      const missing = (e.params ?? []).map((p) => p.name).filter((n) => !typed[n]);
      if (missing.length > 0) {
        errors.append(
          el("div", {
            class: "notice notice-warn",
            text: `This entry cannot be added without ${missing.join(", ")}: agenthub substitutes the value into the command line and refuses to guess one.`,
          }),
        );
        return;
      }
      void add(e, nameInput.value.trim(), typed).then((ok) => {
        if (ok) form.hide();
      });
    });

    nameInput.addEventListener("input", refresh);

    const node = el("div", { class: "panel panel-inset" }, [
      el("h3", { text: `Add ${e.name || e.id}` }),
      errors,
      field(
        "Server id",
        nameInput,
        "the name clients and profiles will refer to; the catalog id is the default",
      ),
      el("div", { class: "form" }, (e.params ?? []).map(paramField)),
      field("Will run", preview, `transport: ${e.transport || Transport.Stdio}`),
      credentialNotice(e),
      cliSlot,
      controls(save, button("Cancel", "btn btn-secondary", () => form.hide())),
    ]);
    refresh();
    return node;
  }

  // -- one card --------------------------------------------------------------

  /** What is still needed AFTER the entry is stored. Rendered on the card as
   *  well as after the add, because it is the difference between "added" and
   *  "working" and the user deserves to know before clicking. */
  /**
   * The one line on a card that differs between entries: what this thing
   * will still ask of you.
   *
   * It replaces a paragraph that spelled the same policy out on every card
   * that had a key, plus a provenance sentence — "curated — written into
   * agenthub by its maintainers · published by …" — that was byte-identical
   * on all seventeen. Text that never varies carries no information while
   * costing the same reading as text that does, and repeating it seventeen
   * times is how a grid stops being scannable. The policy it explained now
   * lives once, in the page intro; the publisher moved to the footer; and
   * the grading only appears when it is NOT the ordinary one, on the same
   * principle as the zero-count chips that do not render.
   */
  function requirementChips(e: CatalogEntry): Node | null {
    const chips: Node[] = [];
    for (const k of e.required_keys ?? []) {
      chips.push(el("span", { class: "badge badge-quarantined", text: k }));
    }
    if (e.auth === CatalogAuthOAuth) {
      chips.push(el("span", { class: "badge badge-disabled", text: "browser sign-in" }));
    }
    const params = e.params ?? [];
    if (params.length > 0) {
      chips.push(
        el("span", {
          class: "badge badge-disabled",
          title: params.map((p) => p.name).join(", "),
          text: params.length === 1 ? `1 setting` : `${params.length} settings`,
        }),
      );
    }
    // Provenance is shown only when it is surprising. Every seeded entry is
    // curated today, so a "curated" chip on all of them would be another
    // uniform token; a future remote-index or user entry is the case worth
    // interrupting the reader for.
    if (e.provenance && e.provenance !== CatalogProvenance.Curated) {
      chips.push(el("span", { class: "badge badge-quarantined", text: PROVENANCE_TEXT[e.provenance] ?? e.provenance }));
    }
    if (chips.length === 0) return null;
    return el("div", { class: "rec-needs" }, chips);
  }

  function credentialNotice(e: CatalogEntry): Node | null {
    const keys = e.required_keys ?? [];
    const oauth = e.auth === CatalogAuthOAuth;
    if (keys.length === 0 && !oauth) return null;
    const lines: string[] = [];
    if (keys.length > 0) {
      lines.push(
        `Needs ${keys.join(", ")}. Add it here first, then store the value on the Secrets page: ` +
          "the stored definition only ever holds a ${SECRET_…} reference, never the credential.",
      );
    }
    if (oauth) lines.push("Needs a browser sign-in afterwards: `agenthub auth login <id>`.");
    return el(
      "div",
      { class: "hint" },
      lines.map((t) => el("div", { text: t })),
    );
  }

  function card(e: CatalogEntry): HTMLElement {
    const serverID = installed.get(e.id);
    const collision = serverID === undefined && takenIDs?.has(e.id) === true;
    const home = safeHttpUrl(e.homepage);
    const line = invocation(e);

    // The status position, exactly as on the Servers page: an already-added
    // entry gets a SENTENCE, not a greyed-out button. A disabled control is
    // the interface refusing to explain itself — the reader is left deciding
    // whether it is broken, forbidden or already done.
    let action: Node;
    if (serverID !== undefined) {
      action = el("div", { class: "state-line" }, [
        el("span", { class: "meta", text: `Already in agenthub as “${serverID}”` }),
        el("a", { class: "btn btn-secondary", href: "#/servers", text: "Open" }),
      ]);
    } else if (e.needs_config || collision) {
      action = button("Configure & add", "btn btn-primary", () => form.show(paramForm(e)));
    } else {
      action = button("Add", "btn btn-primary", () => void add(e, "", {}));
    }

    // A hosted entry is reached over the network; a stdio one is a package
    // this machine will run. The dashed spine says which, on the same channel
    // the Servers ledger uses for the same distinction.
    const remote = e.transport === Transport.HTTP || e.transport === Transport.SSE;

    return el("div", { class: "rec" }, [
      el("div", { class: `spine off${remote ? " remote" : ""}` }),
      el("div", { class: "rec-body" }, [
        el("div", { class: "rec-title" }, [
          el("span", { class: "rec-name", text: e.name || e.id }),
          // Metadata is neutral. A coloured transport pill would put a second,
          // unrelated hue next to the health colours everywhere else.
          el("span", { class: "id-chip", text: e.transport || Transport.Stdio }),
        ]),
        el("div", { class: "rec-desc", title: e.description, text: e.description }),
        line ? el("div", { class: "rec-run", title: line, text: line }) : null,
        requirementChips(e),
        collision
          ? el("div", {
              class: "warn-line",
              text: `A server called “${e.id}” already exists and did not come from this catalog entry — adding this one needs a different id.`,
            })
          : null,
        cliHint(cliAdd(e)),
      ]),
      el("div", { class: "rec-act" }, [
        home
          ? el("a", {
              class: "cat-pub",
              href: home,
              target: "_blank",
              rel: "noopener noreferrer",
              title: home,
              text: docsLabel(e),
            })
          : null,
        action,
      ]),
    ]);
  }

  // -- rendering -------------------------------------------------------------

  function paint(entries: CatalogEntry[], asked: string): void {
    if (!listRoot) return;
    clear(listRoot);

    const already = entries.filter((e) => installed.has(e.id)).length;
    const chips = chipRow(
      chip(entries.length, entries.length === 1 ? "entry" : "entries"),
      chip(already, "already added", "success"),
    );
    if (chips) listRoot.append(chips);

    if (entries.length === 0) {
      // Two different nothings. This branch is reached only after the daemon
      // ANSWERED — the failure path returned long before here — so neither
      // sentence has to hedge about whether the catalog was readable.
      listRoot.append(
        asked
          ? emptyState({
              kind: "empty",
              title: `Nothing in the catalog matches “${asked}”.`,
              body: "The catalog was read successfully; these are search results, not a failure. Every word you type has to match, so fewer words find more.",
              actions: [
                button("Clear search", "btn", () => {
                  query = "";
                  if (searchBox) searchBox.value = "";
                  void draw();
                }),
              ],
            })
          : emptyState({
              kind: "empty",
              title: "This daemon's catalog is empty.",
              body: "Nothing to browse. A server can still be added by hand on the Servers page, or by pasting another client's configuration there.",
              actions: [
                el("a", { class: "btn btn-primary", href: "#/servers", text: "Add a server by hand" }),
              ],
            }),
      );
      return;
    }

    listRoot.append(el("div", { class: "ledger" }, entries.map(card)));
  }

  /** Which catalog entries are already stored, and which ids are taken. */
  async function refreshInstalled(): Promise<void> {
    try {
      const servers = (await hub.listServers()) ?? [];
      const map = new Map<string, string>();
      const ids = new Set<string>();
      for (const s of servers) {
        ids.add(s.id);
        if (s.source?.startsWith(SOURCE_PREFIX)) {
          map.set(s.source.slice(SOURCE_PREFIX.length), s.id);
        }
      }
      installed = map;
      takenIDs = ids;
    } catch {
      // Fail direction: the catalog still renders, minus the annotation.
      // Not knowing costs a label; claiming what we cannot verify costs the
      // user a click that then fails.
      installed = new Map();
      takenIDs = null;
    }
  }

  function frame(): void {
    if (!root || listRoot) return;
    clear(root);
    listRoot = el("div", { class: "catalog-list" }, [loadingState("Reading the catalog…")]);
    const box = textInput(query, "Search — every word must match");
    // Wide enough that the placeholder is readable: it was truncated to
    // "search the catalog (every w", which is a hint nobody can act on.
    box.className = `${box.className} cat-search`.trim();
    box.addEventListener("input", () => {
      query = box.value;
      if (timer !== undefined) window.clearTimeout(timer);
      timer = window.setTimeout(() => void draw(), SEARCH_DEBOUNCE_MS);
    });
    searchBox = box;
    root.append(
      section(
        "Catalog",
        el("p", {
          class: "hint",
          text:
            "Definitions agenthub already knows how to write — the same ones `agenthub catalog add` " +
            "stores, through the same validation. Being listed here says where the line came from, " +
            "not that the server is safe: nothing is signed and nothing is checked at add time. " +
            "Entries needing a key still add in one click; store the value on Secrets afterwards, " +
            "because the definition only ever holds a ${SECRET_…} reference.",
        }),
        controls(box),
        slot.node,
        form.node,
        listRoot,
      ),
    );
  }

  async function draw(): Promise<void> {
    frame();
    if (!listRoot) return;
    const asked = query.trim();
    let list: CatalogList;
    try {
      list = await hub.searchCatalog(asked);
    } catch (err) {
      clear(listRoot);
      listRoot.append(failureState(err, "the catalog", () => void draw()));
      return;
    }
    // The daemon echoes the query that produced the answer. An answer for a
    // query the box no longer holds is dropped rather than painted: the user
    // has typed on, and results for a prefix of what they see is the one
    // failure a search box must not have.
    if ((list.query ?? "") !== query.trim()) return;
    await refreshInstalled();
    paint(list.entries ?? [], asked);
  }

  return {
    render(node) {
      root = node;
      return draw();
    },
    dispose() {
      if (timer !== undefined) window.clearTimeout(timer);
      timer = undefined;
      root = null;
      listRoot = null;
      searchBox = null;
      installed = new Map();
      takenIDs = null;
      form.hide();
    },
  };
}
