// Onboarding: the four steps between "installed" and "a call actually
// arrived" (docs/subsystems/gui.md).
//
// THE ONE CLAIM THIS PAGE MAKES. The last step does not say "you are set up"
// because the user clicked through four screens. It runs a real health probe
// against every registered server and reports what came back, and then it
// waits for a real client to open a session against the gateway. Those are
// the two things that can actually be false at the end of a setup, and "you
// finished the wizard therefore it works" is precisely the lie that makes a
// user distrust everything else the UI says afterwards.
//
// APPEARING AT ALL. The wizard auto-appears only for a genuinely new
// installation — no server registered — and the answer is LATCHED for the
// lifetime of the window. Without the latch, the
// wizard would evaporate mid-flow the moment step 2 added the first server,
// which is exactly when the user is relying on it. The decision also fails
// CLOSED: if the freshness probe cannot be answered (daemon down, endpoint
// missing), the wizard does NOT take over the window. Hijacking someone's
// window because we could not read their configuration is the same class of
// mistake as printing "no servers configured" after a dropped socket.

import { asCallError, hub, isOffline, isUnavailable } from "../bridge";
import { clear, el, emptyState, errorHeadline, loadingState, section } from "../dom";
import type { Page } from "../page";
import { failureBox, noticeSlot } from "../page";
import { button, controls, copyButton } from "../ui";
import type { CallError, ClientDetected, Server, SessionInfo } from "../types";
import { ErrCode } from "../types";

// ---------------------------------------------------------------------------
// The restart sentence
// ---------------------------------------------------------------------------

/**
 * What has to be said after a client is wired, in the ONE wording every
 * surface uses.
 *
 * It is a single function rather than a string repeated at three call sites
 * because the failure it prevents is specific and common: an MCP client reads
 * its configuration once, at start-up, so a user who connects Cursor and then
 * asks Cursor to use a tool gets nothing, concludes agenthub is broken, and
 * starts undoing correct work. The instruction has to be identical everywhere
 * — a second, softer phrasing somewhere else ("you may need to restart") is
 * read as optional.
 */
export function restartHint(client: string): string {
  return (
    `${client} reads its MCP configuration once, when it starts. Quit it completely and open it ` +
    `again — until you do, it is still talking to whatever it was configured with before, and ` +
    `nothing done here will show up in it.`
  );
}

/** The restart instruction as a rendered notice. */
export function restartNotice(client: string): HTMLElement {
  return el("div", { class: "notice notice-warn" }, [
    el("div", { text: `Restart ${client} now.` }),
    el("div", { class: "warn-line", text: restartHint(client) }),
  ]);
}

// ---------------------------------------------------------------------------
// Should the wizard take over the window?
// ---------------------------------------------------------------------------

const SEEN_KEY = "agenthub.onboarding.seen";

/** The latched answer. `null` = not decided yet; once decided it never
 *  changes for the lifetime of this window. */
let autoStartDecision: boolean | null = null;

function seen(): boolean {
  try {
    return localStorage.getItem(SEEN_KEY) === "1";
  } catch {
    // Storage is unavailable in some embedded webview configurations. Failing
    // toward "already seen" is the polite direction: a wizard that cannot
    // remember being dismissed is a wizard that reappears every launch.
    return true;
  }
}

function markSeen(): void {
  try {
    localStorage.setItem(SEEN_KEY, "1");
  } catch {
    // Nothing to do: the flag simply will not survive this window.
  }
}

/**
 * Is this a genuinely new installation?
 *
 * One condition, and it has to be readable: no server is registered. There
 * used to be a second — an empty call ledger — and it went with the audit
 * stream; an installation with servers already in it is not new whatever a
 * ledger would have said.
 *
 * Fail direction: FAIL-CLOSED toward "not new". Any failure to read the
 * signal answers false, so an unreachable daemon leaves the user on the page
 * they asked for.
 */
async function probeFresh(): Promise<boolean> {
  try {
    const servers = await hub.listServers();
    return servers.length === 0;
  } catch {
    return false;
  }
}

/**
 * Whether the shell should redirect to the wizard on start-up. The answer is
 * computed at most once per window and remembered, so a step that registers
 * the first server cannot pull the wizard out from under the user.
 */
export async function shouldAutoStart(): Promise<boolean> {
  if (autoStartDecision !== null) return autoStartDecision;
  if (seen()) {
    autoStartDecision = false;
    return false;
  }
  autoStartDecision = await probeFresh();
  if (autoStartDecision) markSeen();
  return autoStartDecision;
}

// ---------------------------------------------------------------------------
// The health probe (step 4, first half)
// ---------------------------------------------------------------------------

/**
 * The outcome of probing ONE server.
 *
 * "ok" and "failed" are answers about the server. "skipped" is not: it means
 * the probe did not run, so nothing is known about that server either way.
 * Folding the third into the second would report a healthy server as broken
 * whenever the daemon went away mid-probe — the report would be wrong in the
 * alarming direction, which is the direction that destroys trust fastest.
 */
type ProbeOutcome = "ok" | "failed" | "skipped";

interface ProbeRow {
  id: string;
  outcome: ProbeOutcome;
  /** For "ok": the handshake summary. For the others: why. */
  detail: string;
  /** The untouched failure, kept so the full text stays one click away. */
  error?: unknown;
}

/** Did the probe fail to RUN, rather than answer? Offline and a missing
 *  endpoint are properties of the daemon; the docker refusal is a deliberate
 *  fail-closed on the daemon's side (it will not probe a container definition
 *  on the host) and is likewise not a verdict on the server. */
function probeDidNotRun(e: CallError): boolean {
  return isOffline(e) || isUnavailable(e) || e.code === ErrCode.Conflict;
}

async function probeServer(s: Server): Promise<ProbeRow> {
  try {
    const res = await hub.testServer(s.id, {});
    return {
      id: s.id,
      outcome: "ok",
      detail: `connected in ${res.connect_ms} ms, ${res.tool_count} tool${res.tool_count === 1 ? "" : "s"}`,
    };
  } catch (err) {
    const e = asCallError(err);
    if (probeDidNotRun(e)) {
      return {
        id: s.id,
        outcome: "skipped",
        detail: isOffline(e)
          ? "the daemon became unreachable, so nothing was probed"
          : isUnavailable(e)
            ? "this daemon does not serve the self-test endpoint"
            : errorHeadline(e.message),
        error: err,
      };
    }
    return { id: s.id, outcome: "failed", detail: errorHeadline(e.message), error: err };
  }
}

/** The honest one-line summary of a probe run. */
function probeSummary(rows: ProbeRow[]): { text: string; tone: "notice" | "warn" } {
  const ok = rows.filter((r) => r.outcome === "ok").length;
  const failed = rows.filter((r) => r.outcome === "failed").length;
  const skipped = rows.filter((r) => r.outcome === "skipped").length;
  if (rows.length === 0) {
    return { text: "There are no servers to probe yet.", tone: "warn" };
  }
  if (ok === 0 && failed === 0) {
    // Everything was skipped: the probe did not run. This must never be
    // rendered as "nothing is wrong".
    return {
      text: `The health probe did not run for any of the ${rows.length} servers, so their state is unknown.`,
      tone: "warn",
    };
  }
  const parts = [`${ok} of ${rows.length} server${rows.length === 1 ? "" : "s"} answered`];
  if (failed > 0) parts.push(`${failed} did not start`);
  if (skipped > 0) parts.push(`${skipped} could not be probed`);
  return { text: `${parts.join(", ")}.`, tone: failed > 0 || skipped > 0 ? "warn" : "notice" };
}

// ---------------------------------------------------------------------------
// The verification poll (step 4, second half)
// ---------------------------------------------------------------------------

/** The read-only sentence the user is asked to paste into their client. It is
 *  deliberately a LOOK, not a DO: a verification step must not be the first
 *  thing that changes something on a downstream system. */
const VERIFY_PROMPT =
  "List the MCP tools you can reach through agenthub, then call one read-only tool and show me the result.";

/** How long the poll waits before admitting it did not see anything. Long
 *  enough for a client restart plus a model turn; short enough that nobody
 *  sits watching a spinner wondering whether it is stuck. */
const VERIFY_TIMEOUT_MS = 75_000;
const VERIFY_INTERVAL_MS = 2_000;
/** What the step watches for. A client that opens a session has completed
 *  the whole chain up to the gateway: it found the configuration, spawned
 *  agenthub, and handshaked. It is a weaker claim than "a tool call
 *  arrived" — the call ledger that used to back that claim went with the
 *  governance streams — so the wording below claims exactly this and no
 *  more. */
function sessionKey(s: SessionInfo): string {
  return s.id;
}

// ---------------------------------------------------------------------------
// The page
// ---------------------------------------------------------------------------

const STEPS = ["Welcome", "Add servers", "Connect a client", "Check it works"] as const;

export function onboardingPage(): Page {
  let root: HTMLElement | null = null;
  let step = 0;
  let disposed = false;
  const slot = noticeSlot();

  const body = el("div", {});

  // Shared, re-read on the steps that show them.
  let clients: ClientDetected[] | null = null;
  let clientsError: unknown = null;
  let servers: Server[] | null = null;
  let serversError: unknown = null;

  // Step 4 state.
  let probeRows: ProbeRow[] | null = null;
  let probing = false;
  let verifyTimer: number | undefined;
  let verifyDeadline = 0;
  let verifyBaseline: Set<string> | null = null;
  let verifyState: "idle" | "waiting" | "found" | "timeout" | "unavailable" = "idle";
  let verifyFound: SessionInfo | null = null;
  let verifyProblem = "";

  function stopVerify(): void {
    if (verifyTimer !== undefined) window.clearInterval(verifyTimer);
    verifyTimer = undefined;
  }

  // -- chrome ----------------------------------------------------------------

  const progressHost = el("div", { class: "controls", role: "list", "aria-label": "Setup progress" });

  function drawProgress(): void {
    const row = progressHost;
    clear(row);
    STEPS.forEach((name, i) => {
      const tone = i < step ? "ok" : i === step ? "warning" : "neutral";
      row.append(
        el("span", { class: "check", role: "listitem" }, [
          el("span", { class: `dot ${tone}` }),
          el("span", {
            class: i === step ? "" : "muted",
            text: `${i + 1}. ${name}`,
          }),
        ]),
      );
    });
  }

  function go(next: number): void {
    stopVerify();
    slot.clear();
    step = Math.max(0, Math.min(STEPS.length - 1, next));
    void drawStep();
  }

  function footer(opts: { skipLabel?: string; nextLabel?: string; last?: boolean } = {}): HTMLElement {
    return controls(
      step > 0 ? button("Back", "btn btn-secondary", () => go(step - 1)) : null,
      opts.last
        ? el("a", { class: "btn btn-primary", href: "#/servers", text: "Finish" })
        : button(opts.nextLabel ?? "Next", "btn btn-primary", () => go(step + 1)),
      opts.last
        ? null
        : button(opts.skipLabel ?? "Skip this step", "btn btn-secondary", () => go(step + 1)),
    );
  }

  // -- step 1: welcome -------------------------------------------------------

  function clientsBlock(): Node {
    if (clientsError) {
      return el("div", {}, [
        el("p", { class: "hint", text: "The client scan did not complete, so this list is not a claim that nothing is installed:" }),
        failureBox(clientsError),
      ]);
    }
    const found = clients ?? [];
    if (found.length === 0) {
      return emptyState({
        kind: "empty",
        title: "No AI client configuration found on this machine.",
        body:
          "That is not a problem yet — agenthub can be pointed at by hand from any MCP client. The " +
          "Clients page lists which ones are supported directly.",
        actions: [el("a", { class: "btn", href: "#/clients", text: "Open Clients" })],
      });
    }
    return el(
      "div",
      { class: "installs" },
      found.map((c) =>
        el("div", { class: "install" }, [
          el("strong", { text: c.name || c.client }),
          el("span", { class: "meta", text: `${c.placement} · ${c.shape}` }),
          el("span", { class: "mono muted", text: c.path }),
          c.denied ? el("span", { class: "badge badge-unhealthy", text: "not readable" }) : null,
        ]),
      ),
    );
  }

  function stepWelcome(): Node {
    return el("div", {}, [
      el("p", {
        text:
          "agenthub is one place to keep every MCP server you use, and one gateway every AI client " +
          "talks to instead of talking to those servers directly.",
      }),
      el("p", {
        class: "hint",
        text:
          "One configuration and one set of credentials, shared by all of your clients; one place " +
          "that decides in advance which servers and which of their tools each client may reach — " +
          "settled before a client connects, never asked about mid-call.",
      }),
      el("h3", { text: "Clients detected on this machine" }),
      el("p", {
        class: "hint",
        text: "Detection reads file metadata only. The contents are opened later, when you connect one.",
      }),
      clientsBlock(),
      footer({ skipLabel: "Skip the guide" }),
    ]);
  }

  // -- step 2: servers -------------------------------------------------------

  /** Whether the Catalog page exists yet. The sidebar link is the evidence:
   *  the catalog ships as a page plus a nav entry, so if the entry is not
   *  there the page is not either, and this step points at the manual route
   *  instead of at a dead link. */
  function catalogAvailable(): boolean {
    return document.querySelector('a.nav[data-route="catalog"]') !== null;
  }

  function stepServers(): Node {
    const count = (servers ?? []).length;
    return el("div", {}, [
      el("p", {
        text:
          "A server is one MCP tool provider — a filesystem bridge, a GitHub client, a database. " +
          "agenthub keeps their definitions and credentials so no client has to.",
      }),
      serversError
        ? el("div", {}, [
            el("div", {
              class: "notice notice-warn",
              text: "The server list could not be read, so this step cannot tell you whether you have any.",
            }),
            failureBox(serversError),
          ])
        : count > 0
          ? el("div", { class: "notice", text: `${count} server${count === 1 ? " is" : "s are"} registered.` })
          : el("div", {
              class: "notice notice-warn",
              text: "No servers are registered yet. A client connected now would reach an empty gateway.",
            }),
      controls(
        catalogAvailable()
          ? el("a", { class: "btn btn-primary", href: "#/catalog", text: "Browse the catalog" })
          : el("a", { class: "btn btn-primary", href: "#/servers", text: "Add a server by hand" }),
        el("a", { class: "btn", href: "#/clients", text: "Import from a client you already use" }),
        button("Re-check", "btn btn-secondary", () => void drawStep()),
      ),
      catalogAvailable()
        ? null
        : el("p", {
            class: "hint",
            text:
              "This build has no curated catalog yet, so a server is added by naming its command or URL " +
              "on the Servers page.",
          }),
      el("p", {
        class: "hint",
        text: "Already using MCP servers in Cursor or Claude Code? The Clients page can read them out of that client's configuration instead of you retyping them.",
      }),
      footer(),
    ]);
  }

  // -- step 3: connect a client ----------------------------------------------

  const connectResults = el("div", {});

  async function connect(c: ClientDetected): Promise<void> {
    slot.clear();
    try {
      const res = await hub.connectClient(c.client, {});
      const name = c.name || c.client;
      clear(connectResults);
      connectResults.append(
        el("div", { class: "panel panel-inset" }, [
          el("h3", { text: res.changed === false ? `${name} was already wired` : `${name} is connected` }),
          el("div", { class: "kvs" }, [
            kv("Configuration", res.path || "—"),
            kv("Command", [res.entry.command, ...(res.entry.args ?? [])].join(" ")),
            kv("Backup", res.backup || "—"),
          ]),
          restartNotice(name),
        ]),
      );
    } catch (err) {
      clear(connectResults);
      connectResults.append(
        el("div", { class: "panel panel-inset" }, [
          el("h3", { text: `${c.name || c.client} was not connected` }),
          failureBox(err),
        ]),
      );
    }
  }

  function kv(k: string, v: string): HTMLElement {
    return el("div", { class: "kv" }, [
      el("span", { class: "k", text: k }),
      el("span", { class: "v", text: v }),
    ]);
  }

  function stepClients(): Node {
    const found = clients ?? [];
    return el("div", {}, [
      el("p", {
        text:
          "Connecting a client rewrites its MCP configuration so that agenthub is the single server it " +
          "spawns. A backup of the previous file is written first, and only entries agenthub itself " +
          "wrote are ever removed later.",
      }),
      clientsError
        ? failureBox(clientsError)
        : found.length === 0
          ? emptyState({
              kind: "empty",
              title: "Nothing to connect automatically.",
              body: "No supported client configuration was found. Any MCP client can still be pointed at agenthub by hand from the Clients page.",
              actions: [el("a", { class: "btn", href: "#/clients", text: "Open Clients" })],
            })
          : el(
              "div",
              { class: "cards" },
              found.map((c) =>
                el("div", { class: "card" }, [
                  el("header", {}, [
                    el("strong", { text: c.name || c.client }),
                    el("span", { class: "meta", text: `${c.placement} · ${c.shape}` }),
                  ]),
                  el("div", { class: "mono muted", text: c.path }),
                  c.denied
                    ? el("div", { class: "hint", text: c.remediation || "This file exists but may not be read." })
                    : null,
                  controls(
                    button("Connect", "btn btn-primary", () => void connect(c)),
                    !c.writable ? el("span", { class: "hint", text: "The file is read-only — connecting will fail until that is fixed." }) : null,
                  ),
                ]),
              ),
            ),
      connectResults,
      footer(),
    ]);
  }

  // -- step 4: does it actually work? ----------------------------------------

  const probeBox = el("div", {});
  const verifyBox = el("div", {});

  async function runProbe(): Promise<void> {
    if (probing) return;
    probing = true;
    probeRows = null;
    drawProbe();
    const list = servers ?? [];
    const rows: ProbeRow[] = [];
    for (const s of list) {
      if (disposed) return;
      rows.push(await probeServer(s));
    }
    probing = false;
    probeRows = rows;
    drawProbe();
  }

  function drawProbe(): void {
    clear(probeBox);
    const inner: (Node | null)[] = [el("h3", { text: "Health probe" })];
    if (serversError) {
      // The probe did not run, and cannot: there is no list to iterate. This
      // is the "the probe itself did not happen" outcome at whole-run scale,
      // and it must not render as a clean bill of health.
      inner.push(
        el("div", {
          class: "notice notice-warn",
          text: "The health probe did not run: the server list could not be read, so there was nothing to probe against.",
        }),
        failureBox(serversError),
        controls(button("Try again", "btn", () => void drawStep())),
      );
      probeBox.append(el("div", { class: "panel panel-inset" }, inner));
      return;
    }
    if (probing) {
      inner.push(loadingState("Connecting to every registered server…", 3));
    } else if (!probeRows) {
      inner.push(el("p", { class: "hint", text: "Not run yet." }));
    } else if (probeRows.length === 0) {
      inner.push(
        emptyState({
          kind: "empty",
          title: "There is nothing to probe.",
          body: "No servers are registered, so the gateway has nothing to offer a client yet.",
          actions: [el("a", { class: "btn", href: "#/servers", text: "Add a server" })],
        }),
      );
    } else {
      const sum = probeSummary(probeRows);
      inner.push(
        el("div", { class: sum.tone === "warn" ? "notice notice-warn" : "notice", text: sum.text }),
        el(
          "div",
          { class: "cards" },
          probeRows.map((r) =>
            el("div", { class: "card" }, [
              el("header", {}, [
                el("strong", { text: r.id }),
                el("span", {
                  class:
                    r.outcome === "ok"
                      ? "badge badge-healthy"
                      : r.outcome === "failed"
                        ? "badge badge-unhealthy"
                        : "badge badge-disabled",
                  text: r.outcome === "ok" ? "answered" : r.outcome === "failed" ? "did not start" : "not probed",
                }),
              ]),
              el("div", { class: "muted", text: r.detail }),
              r.error ? failureBox(r.error) : null,
            ]),
          ),
        ),
      );
    }
    inner.push(controls(button(probeRows ? "Probe again" : "Run the probe", "btn", () => void runProbe())));
    probeBox.append(el("div", { class: "panel panel-inset" }, inner));
  }

  async function startVerify(): Promise<void> {
    stopVerify();
    verifyFound = null;
    verifyProblem = "";
    try {
      const open = await hub.listSessions();
      verifyBaseline = new Set(open.map(sessionKey));
    } catch (err) {
      // Without a baseline there is no way to tell a NEW call from an old
      // one, and claiming success on a record that was already there would be
      // the exact dishonesty this step exists to avoid.
      verifyBaseline = null;
      verifyState = "unavailable";
      verifyProblem = errorHeadline(asCallError(err).message);
      drawVerify();
      return;
    }
    verifyState = "waiting";
    verifyDeadline = Date.now() + VERIFY_TIMEOUT_MS;
    drawVerify();
    verifyTimer = window.setInterval(() => void pollVerify(), VERIFY_INTERVAL_MS);
  }

  async function pollVerify(): Promise<void> {
    if (disposed || verifyState !== "waiting" || !verifyBaseline) return;
    let open: SessionInfo[];
    try {
      open = await hub.listSessions();
    } catch {
      // A transient read failure is not evidence of anything. Keep waiting;
      // the deadline below still bounds the wait.
      if (Date.now() > verifyDeadline) {
        stopVerify();
        verifyState = "timeout";
        drawVerify();
      }
      return;
    }
    const fresh = open.find((sess) => !verifyBaseline?.has(sessionKey(sess)));
    if (fresh) {
      stopVerify();
      verifyFound = fresh;
      verifyState = "found";
      drawVerify();
      return;
    }
    if (Date.now() > verifyDeadline) {
      stopVerify();
      verifyState = "timeout";
    }
    drawVerify();
  }

  function verifyTroubleshooting(): HTMLElement {
    return el("div", {}, [
      el("p", { class: "hint", text: "Nothing arrived. In the order that resolves this fastest:" }),
      el("ul", { class: "consequences" }, [
        el("li", { text: "Did you fully quit and reopen the client? A reload or a new chat is not enough — it re-reads its configuration only at start-up." }),
        el("li", { text: "Does the client list agenthub among its MCP servers? If it does not, step 3 did not write to the file that client actually reads." }),
        el("li", { text: "Did the assistant actually call a tool, or only talk about calling one? Only a real call reaches the ledger." }),
        el("li", { text: "Are any servers failing the health probe above? A gateway with no working server has no tool to offer." }),
        el("li", { text: "Is the client on a profile that excludes every server? Check the Profiles page — a client bound to a profile naming no server sees nothing, which is the fail-closed direction." }),
      ]),
      controls(
        el("a", { class: "btn btn-primary", href: "#/playground", text: "Try a tool directly in the Playground" }),
        el("a", { class: "btn", href: "#/sessions", text: "Open the Sessions list" }),
        button("Wait again", "btn btn-secondary", () => void startVerify()),
      ),
      el("p", {
        class: "hint",
        text:
          "The Playground calls a tool through the daemon rather than through a client. If it works there " +
          "and not in your client, the problem is the client wiring, not the server.",
      }),
    ]);
  }

  function drawVerify(): void {
    clear(verifyBox);
    const inner: (Node | null)[] = [
      el("h3", { text: "Did the client actually connect?" }),
      el("p", {
        text:
          "Paste this into the client you connected, then come back to this window. Nothing is claimed " +
          "here until that client opens a session against agenthub.",
      }),
      el("div", { class: "verify-prompt" }, [
        el("span", { class: "meta", text: "❝" }),
        el("code", { text: VERIFY_PROMPT }),
        copyButton(() => VERIFY_PROMPT, "Copy", "btn btn-icon"),
      ]),
    ];
    if (verifyState === "idle") {
      inner.push(controls(button("Start waiting", "btn btn-primary", () => void startVerify())));
    } else if (verifyState === "waiting") {
      const left = Math.max(0, Math.round((verifyDeadline - Date.now()) / 1000));
      inner.push(
        el("div", { class: "notice" }, [
          el("div", { text: `Watching for a new session… ${left}s left.` }),
          el("div", {
            class: "warn-line",
            text: "Only sessions opened from now on count — anything already connected was there before you started.",
          }),
        ]),
        controls(
          button("Stop waiting", "btn btn-secondary", () => {
            stopVerify();
            verifyState = "timeout";
            drawVerify();
          }),
        ),
      );
    } else if (verifyState === "found" && verifyFound) {
      inner.push(
        el("div", { class: "notice" }, [
          el("div", {}, [
            el("span", { class: "badge badge-healthy", text: "connected" }),
            el("span", {
              text: `  A client just opened a session: ${verifyFound.client_id || verifyFound.id}`,
            }),
          ]),
          el("div", {
            class: "warn-line",
            text: `Origin ${verifyFound.origin}${verifyFound.profile_name ? ` · profile ${verifyFound.profile_name}` : ""}.`,
          }),
        ]),
        el("p", {
          class: "hint",
          text: "The client found its configuration, spawned agenthub and handshaked. What it may then see is decided by its profile.",
        }),
        controls(el("a", { class: "btn", href: "#/sessions", text: "See it in Sessions" })),
      );
    } else if (verifyState === "timeout") {
      inner.push(verifyTroubleshooting());
    } else if (verifyState === "unavailable") {
      inner.push(
        el("div", { class: "notice notice-warn" }, [
          el("div", { text: "This check cannot run: the session list could not be read." }),
          el("div", { class: "warn-line", text: verifyProblem }),
          el("div", {
            class: "warn-line",
            text:
              "Without a starting point there is no way to tell a new session from an old one, and saying " +
              "“it works” on that basis would be a guess.",
          }),
        ]),
        controls(
          button("Try again", "btn", () => void startVerify()),
          el("a", { class: "btn btn-secondary", href: "#/playground", text: "Use the Playground instead" }),
        ),
      );
    }
    verifyBox.append(el("div", { class: "panel panel-inset" }, inner));
  }

  function stepDone(): Node {
    return el("div", {}, [
      el("p", {
        text:
          "Two things are checked here, and both of them can come back negative. Nothing on this page " +
          "reports success because you reached it.",
      }),
      probeBox,
      verifyBox,
      footer({ last: true }),
    ]);
  }

  // -- rendering -------------------------------------------------------------

  async function loadClients(): Promise<void> {
    try {
      const res = await hub.detectClients();
      clients = res.found ?? [];
      clientsError = null;
    } catch (err) {
      clients = null;
      clientsError = err;
    }
  }

  async function loadServers(): Promise<void> {
    try {
      servers = await hub.listServers();
      serversError = null;
    } catch (err) {
      // NOT an empty list. Every reader below branches on serversError first,
      // because "you have no servers" after a failed read is the single most
      // damaging sentence a configuration UI can print.
      servers = null;
      serversError = err;
    }
  }

  async function drawStep(): Promise<void> {
    if (!root) return;
    drawProgress();
    clear(body);
    body.append(loadingState("Looking around…", 2));
    if (step === 0 || step === 2) await loadClients();
    if (step === 1 || step === 3) await loadServers();
    if (disposed || !root) return;
    clear(body);
    body.append(
      step === 0
        ? stepWelcome()
        : step === 1
          ? stepServers()
          : step === 2
            ? stepClients()
            : stepDone(),
    );
    if (step === 3) {
      drawProbe();
      drawVerify();
      if (!probeRows && !probing) void runProbe();
    }
    // The wizard is a flow: redrawing a step must not leave the reader
    // half-way down the previous one.
    body.scrollIntoView({ block: "nearest" });
  }

  return {
    render(node) {
      root = node;
      disposed = false;
      markSeen();
      clear(node);
      node.append(
        section(
          "Set up agenthub",
          el("p", {
            class: "hint",
            text: "Four steps. Every one of them can be skipped, and every one of them is also reachable from the sidebar later.",
          }),
          progressHost,
          slot.node,
          body,
        ),
      );
      return drawStep();
    },
    dispose() {
      disposed = true;
      stopVerify();
      root = null;
      clients = null;
      servers = null;
      probeRows = null;
      verifyBaseline = null;
      verifyState = "idle";
      verifyFound = null;
    },
  };
}
