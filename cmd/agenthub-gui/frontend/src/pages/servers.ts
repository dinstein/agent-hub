// Servers page: the dashboard, the definition editor and the live self-test.
//
// Enabled rows are based only on this page's short-lived self-tests, never on
// another gateway's connection report. The outcome is classified by the
// typed success/error contract, not by parsing raw flags or prose. Disabled
// state still comes from the registry-backed daemon row. Shared level/action
// spellings come from the generated Go api constants
// (src/generated/health.ts).
//
// INFORMATION ARCHITECTURE (docs/modules/gui.md / 2.2)
//
//   - Rows are grouped by CONFIGURATION — Enabled and Disabled — and ordered
//     by id inside each. Both sections are always on the page, empty or not,
//     with a count beside the heading; the disabled one is collapsed by
//     default and its collapse state is remembered. See isDisabled() for why
//     grouping stopped following state, and sectionPlaceholder() for why an
//     empty group stays.
//   - The status cell is an ACTION where there is one. A "needs-auth" server
//     does not display the words "needs auth" next to a menu the user then
//     has to go and find — the status position becomes the button.
//
// The editor is a read-modify-write against the STORED entry, not against the
// dashboard row: Get returns the definition together with the generation it
// was read at, and that generation goes back with the write. A concurrent
// edit then answers 409 and this page re-reads instead of overwriting
// (api/servers.go, ServerDetail).
//
// Credentials never pass through here. An env or header value may hold a
// `${SECRET_X}` placeholder and is stored verbatim; the way to check that it
// resolves is "Test connection", which makes a REAL call — the vault has no
// read path and this page must not grow one.

import { asCallError, EVT, hub, isCancelled, on, openExternal } from "../bridge";
import { chip, chipRow, chipToggle, clear, el, emptyState, errorHeadline, icon, loadingState, pageHeader, reconcile, table } from "../dom";
import { AdminState, HealthAction, HealthLevel } from "../generated/health";
import { eventRows } from "./events";
import type { Page } from "../page";
import { failureBox, failureState, noticeSlot, runWrite } from "../page";
import { consumeServerSecrets } from "../secret-guidance";
import { createServerSecretsManager } from "../server-secrets";
import {
  advanced,
  button,
  checkboxInput,
  confirmAction,
  controls,
  copyButton,
  field,
  group,
  linesEditor,
  modalHost,
  openModal,
  pairEditor,
  selectInput,
  textInput,
  toggleSwitch,
} from "../ui";
import type {
  AuthLogin,
  AuthStatus,
  DockerMount,
  DockerRuntime,
  EventRecord,
  ParsedClientConfig,
  ParsedSkip,
  Server,
  ServerDetail,
  ServerEntry,
  ServerTestResult,
  TopicEvent,
} from "../types";
import { AuthState, ErrCode, LoginMode, LoginPhase, Provenance, Runtime, Transport } from "../types";

// ---------------------------------------------------------------------------
// Health presentation
// ---------------------------------------------------------------------------

/** Terminal fallbacks exist only where the control plane has no endpoint.
 *  They are diagnostic escape hatches, not teaching chrome beside every
 *  ordinary GUI action. */
const TERMINAL_ACTIONS: Record<string, { label: string; command: string; note?: string }> = {
  [HealthAction.ViewLogs]: { label: "View logs", command: "agenthub server logs <id> --follow" },
  [HealthAction.Restart]: {
    label: "Restart",
    command: "agenthub daemon restart",
    note: "restarts the hub; there is no per-server restart endpoint",
  },
};

function suggestion(
  server: Server,
  missingSecrets: string[] = [],
  openSecrets?: () => void,
): Node | null {
  const action = server.health.action ?? HealthAction.None;
  if (action === HealthAction.None || action === HealthAction.Enable) return null;
  if (action === HealthAction.SetSecret) {
    return button(missingSecrets.length === 1 ? "Set secret" : "Open Secrets", "btn btn-secondary", () => {
      openSecrets?.();
    });
  }
  const spec = TERMINAL_ACTIONS[action];
  if (!spec) return el("span", { class: "meta", text: action });
  const command = spec.command.replace("<id>", server.id);
  return el("p", { class: "terminal-fallback" }, [
    el("span", { text: `${spec.label} from a terminal:` }),
    el("code", { text: command }),
    spec.note ? el("span", { text: spec.note }) : null,
  ]);
}

// ---------------------------------------------------------------------------
// Grouping
// ---------------------------------------------------------------------------

/**
 * A ROW MOVES ONLY WHEN THE USER CHANGES CONFIGURATION, NEVER BECAUSE OF A
 * PROBE RESULT.
 *
 * The list used to be grouped by state — needs attention / active / disabled —
 * and that made position depend on an asynchronous, changeable quantity that
 * is UNKNOWN at startup. Every enabled row therefore began life in "needs
 * attention" (an unchecked row reports level=degraded, which is not a fault,
 * it is an absence of an answer) and migrated to "active" as its handshake
 * settled: twenty servers meant twenty rows changing group, the whole table
 * re-sorting under the cursor each time, and a warning header that was telling
 * the truth about nothing.
 *
 * Grouping now follows CONFIGURATION, which the registry answers with
 * certainty the moment it is read, and state is expressed inside the row —
 * where it already had three channels (spine, dot, text). What the old
 * attention bucket really provided was the answer to "which row should I look
 * at first"; that is now the attention chip, which filters on demand instead
 * of rearranging the page permanently to answer a question nobody has asked
 * yet.
 *
 * The classification still comes from the Health contract rather than from
 * `enabled` or the raw connection state (docs/modules/controlplane.md): a
 * disabled server reports level=healthy on purpose, and admin_state is the
 * field that says so.
 */
function isDisabled(s: Server): boolean {
  return s.health.admin_state === AdminState.Disabled;
}

/**
 * Whether this row is asking for something. Deliberately NOT "level is not
 * healthy": a row whose handshake is still in flight also reports degraded,
 * and counting an unanswered question as a fault is what produced a page that
 * opened claiming everything was broken.
 */
function needsAttention(s: Server): boolean {
  if (isDisabled(s) || s.state === "connecting") return false;
  return s.health.level !== HealthLevel.Healthy;
}

type ProbeObservation =
  | { kind: "checking" }
  | { kind: "connected"; tools: number; result: ServerTestResult; checkedAt: number }
  | { kind: "auth"; checkedAt: number }
  | { kind: "secret"; keys: string[]; checkedAt: number }
  | { kind: "error"; summary: string; detail: string; checkedAt: number };

type SettledProbeObservation = Exclude<ProbeObservation, { kind: "checking" }>;

/**
 * The latest settled outcomes survive route changes for this GUI process.
 * They still come only from this page's own handshakes: gateway reports never
 * enter this cache. A newly mounted page silently rechecks every enabled row,
 * so a cached result is the first paint rather than a claim that health can
 * never change. "checking" is deliberately page-local; leaving halfway
 * through a first probe must not strand that label on the next visit.
 */
const probeCache = new Map<string, SettledProbeObservation>();

/** SERVER_EVENT_LIMIT bounds the per-server history in the detail panel. It
 *  is a recent-past window, not a log viewer: the whole timeline is the
 *  Events page, and this is the tail that explains the badge above it. Ten
 *  rows are what fits under a badge without turning the panel into that page;
 *  it bounds the READ as well as the render, because a row nobody can see is
 *  not worth carrying over the link. */
const SERVER_EVENT_LIMIT = 10;

/** Per-server event cache, keyed by id. Cleared with probeCache so a manual
 *  refresh re-reads both rather than showing a fresh badge over stale
 *  history. */
const eventCache = new Map<string, EventRecord[]>();

/** Per-server stored definition, keyed by id and fetched AT MOST ONCE per id.
 *  The dashboard payload carries neither the endpoint nor the spawn command,
 *  and two places need one: the first line of the detail panel, and the
 *  "Installing…" label. Caching the promise rather than its value is what
 *  makes a second reader join the first request instead of starting another
 *  one. Dropped with the cached self-test, and on any write to the
 *  definition. */
const entryCache = new Map<string, Promise<ServerEntry | null>>();

/** The stored definition, or null when it cannot be read. A failure resolves
 *  rather than rejecting: every caller here degrades to saying less, and none
 *  of them is a reason to raise a page-level failure. */
function storedEntry(id: string): Promise<ServerEntry | null> {
  let pending = entryCache.get(id);
  if (!pending) {
    pending = hub.getServer(id).then((d) => d.entry).catch(() => null);
    entryCache.set(id, pending);
  }
  return pending;
}

/**
 * Replaces gateway-observed runtime state with this page's own short-lived
 * handshake. The daemon list remains authoritative for configuration and
 * enabled/disabled state; it is deliberately not authoritative for whether
 * this page can connect right now.
 */
function withProbe(s: Server, observation: ProbeObservation | undefined): Server {
  if (!s.enabled) return s;
  const probe = observation ?? { kind: "checking" };
  if (probe.kind === "connected") {
    return {
      ...s,
      state: "connected",
      tools: probe.tools,
      health: {
        level: HealthLevel.Healthy,
        admin_state: AdminState.Enabled,
        summary: "ok",
        detail: "",
        action: HealthAction.None,
      },
    };
  }
  if (probe.kind === "auth") {
    return {
      ...s,
      state: "error",
      tools: 0,
      health: {
        level: HealthLevel.Unhealthy,
        admin_state: AdminState.Enabled,
        summary: "authentication required",
        detail: "",
        action: HealthAction.Login,
      },
    };
  }
  if (probe.kind === "secret") {
    return {
      ...s,
      state: "error",
      tools: 0,
      health: {
        level: HealthLevel.Unhealthy,
        admin_state: AdminState.Enabled,
        summary: probe.keys.length === 1 && probe.keys[0].endsWith("API_KEY") ? "API key required" : "secret required",
        detail: probe.keys.join(", "),
        action: HealthAction.SetSecret,
      },
    };
  }
  if (probe.kind === "error") {
    return {
      ...s,
      state: "error",
      tools: 0,
      health: {
        level: HealthLevel.Unhealthy,
        admin_state: AdminState.Enabled,
        summary: probe.summary,
        detail: probe.detail,
        action: HealthAction.None,
      },
    };
  }
  return {
    ...s,
    state: "connecting",
    tools: 0,
    health: {
      level: HealthLevel.Degraded,
      admin_state: AdminState.Enabled,
      summary: "checking",
      detail: "",
      action: HealthAction.None,
    },
  };
}

/**
 * The two groups, and the localStorage key each remembers its fold in.
 *
 * The key spellings predate the grouping change and are kept so that nobody's
 * folded section quietly springs open on upgrade.
 *
 * Enabled opens by default and Disabled does not: one is the working set, the
 * other is the group the operator has already decided about and must not push
 * the servers in service off-screen. Both fold, because a long list of healthy
 * servers is as much in the way as a long list of switched-off ones when you
 * came to the page to look at the other group.
 */
const SECTIONS = {
  enabled: { title: "Enabled", key: "agenthub.servers.bucket.enabled.collapsed", foldedByDefault: false },
  disabled: { title: "Disabled", key: "agenthub.servers.bucket.disabled.collapsed", foldedByDefault: true },
} as const;

type SectionName = keyof typeof SECTIONS;

interface SectionHost {
  node: HTMLDetailsElement;
  body: HTMLElement;
  count: HTMLElement;
  placeholder: HTMLElement;
}

/**
 * What an empty group says instead of disappearing.
 *
 * BOTH SECTIONS ARE ALWAYS ON THE PAGE. Hiding the empty one made the page's
 * own structure depend on its contents: a first-run window showed no Enabled
 * and no Disabled heading at all, so nothing on screen said that "enabled" and
 * "disabled" are what a server is sorted by here, and switching the last
 * server of a group off made a heading vanish rather than a row move. The
 * count beside a permanent heading answers "none" perfectly well, and it
 * answers it in the same place every time.
 *
 * The two reasons a group can be empty are not the same fact, so they do not
 * get the same sentence: `filtered` means the rows exist and this view is
 * narrowed — the one case where "none" without explanation would read as
 * "they are gone".
 */
function sectionPlaceholder(name: SectionName, filtered: boolean): string {
  if (filtered) {
    return name === "enabled"
      ? "No enabled server matches this filter."
      : "No disabled server matches this filter.";
  }
  return name === "enabled"
    ? "No servers are enabled. An enabled server is one agenthub offers to your clients."
    : "No servers are disabled. A disabled server keeps its definition but is never connected.";
}

function sectionFolded(name: SectionName): boolean {
  try {
    const v = localStorage.getItem(SECTIONS[name].key);
    if (v === "1") return true;
    if (v === "0") return false;
  } catch {
    // Storage unavailable: fall through to the default.
  }
  return SECTIONS[name].foldedByDefault;
}

function setSectionFolded(name: SectionName, folded: boolean): void {
  try {
    localStorage.setItem(SECTIONS[name].key, folded ? "1" : "0");
  } catch {
    // The section still opens and closes; it just will not be remembered.
  }
}

// ---------------------------------------------------------------------------
// "checking" -> "Installing…"
// ---------------------------------------------------------------------------

/**
 * After this long, a connecting stdio server launched through a package
 * runner is almost certainly downloading rather than connecting.
 *
 * The number is a presentation choice, not a protocol one: below it, saying
 * "Installing…" would be a guess; above it, "Checking…" is the wrong story
 * about a wait that can last a minute. Re-labelling the wait as progress is
 * the cheapest honesty upgrade on this page — the daemon reports the same
 * state either way, but "Checking…" for sixty seconds reads as a hang.
 */
const INSTALL_HINT_MS = 4000;

/** Package runners that fetch before they exec. `npx`/`uvx` are the two the
 *  ecosystem actually publishes install lines for; `bunx` and `pnpm dlx`
 *  behave identically and are cheap to recognise. */
const RUNNERS = new Set(["npx", "uvx", "bunx", "dlx", "pnpx"]);

function isPackageRunner(command: string): boolean {
  const base = command.split(/[\\/]/).pop() ?? "";
  return RUNNERS.has(base.toLowerCase());
}

// ---------------------------------------------------------------------------
// The definition form
// ---------------------------------------------------------------------------

type Collected = { ok: true; entry: ServerEntry } | { ok: false; message: string };

/** The empty definition a manually-created server starts from. HTTP is the
 *  useful GUI default because most operators arrive with a remote MCP URL;
 *  imported definitions retain the registry's stdio zero-value semantics. */
function blankEntry(): ServerEntry {
  return {
    transport: Transport.HTTP,
    command: "",
    args: null,
    env: null,
    cwd: "",
    url: "",
    headers: null,
    oauth: null,
    provenance: "",
    derive: "",
    runtime: "",
    docker: null,
    enabled: true,
    source: "gui",
  };
}

interface EntryForm {
  node: HTMLElement;
  collect(): Collected;
}

function entryForm(initial: ServerEntry): EntryForm {
  const transport = selectInput(
    [
      { value: Transport.Stdio, label: "stdio (spawn a process)" },
      { value: Transport.HTTP, label: "http (MCP Streamable HTTP)" },
      { value: Transport.SSE, label: "sse (legacy HTTP+SSE)" },
    ],
    initial.transport || Transport.Stdio,
  );

  // -- stdio half ------------------------------------------------------------
  const command = textInput(initial.command, "/usr/local/bin/some-mcp-server");
  const args = linesEditor(initial.args, "one argument per line");
  const env = pairEditor(initial.env, {
    keyLabel: "NAME",
    hint: "stored verbatim — put ${SECRET_NAME} here, never the credential itself",
  });
  const cwd = textInput(initial.cwd, "working directory (optional)");
  const runtime = selectInput(
    [
      { value: "", label: "host (spawn on this machine)" },
      { value: Runtime.Docker, label: "docker (run in a container)" },
    ],
    initial.runtime === Runtime.Docker ? Runtime.Docker : "",
  );

  const d = initial.docker;
  const image = textInput(d?.image ?? "", "ghcr.io/example/server:tag");
  const network = textInput(d?.network ?? "", "docker network (empty = none)");
  const memory = textInput(d?.memory ?? "", "512m");
  const cpus = textInput(d?.cpus ?? "", "1.5");
  const dockerUser = textInput(d?.user ?? "", "uid:gid");
  const workdir = textInput(d?.workdir ?? "", "/workspace");
  const extraArgs = linesEditor(d?.extraArgs, "extra `docker run` arguments, one per line");
  const mounts = linesEditor(
    (d?.mounts ?? []).map((m) => [m.source, m.target ?? "", m.write ? "rw" : "ro"].join(":")),
    "/host/path:/container/path:ro — one mount per line",
  );
  const dockerBlock = group(
    "Container",
    field("Image", image, "required for the docker runtime"),
    field("Network", network, "empty means no network at all — a server that needs one must say so"),
    field("Mounts", mounts.node, "source[:target][:ro|rw]; the default is read-only"),
    el("div", { class: "grid2" }, [
      field("Memory", memory),
      field("CPUs", cpus),
      field("User", dockerUser),
      field("Workdir", workdir),
    ]),
    field("Extra docker args", extraArgs.node, "appended verbatim; the isolation defaults win"),
  );
  // Command and arguments are the whole of a stdio server for almost every
  // entry; a working directory and container isolation are real but rare.
  const stdioAdvanced =
    Object.keys(initial.env ?? {}).length > 0 ||
    (initial.cwd ?? "") !== "" ||
    initial.runtime === Runtime.Docker ||
    initial.docker != null;
  const stdioBlock = group(
    "Local process",
    field("Command", command),
    field("Arguments", args.node),
    advanced(
      "Environment, working directory and isolation",
      stdioAdvanced,
      field("Environment variables", env.node),
      field("Working directory", cwd),
      field("Runtime", runtime),
      dockerBlock,
    ),
  );

  // -- http/sse half ---------------------------------------------------------
  const url = textInput(initial.url, "https://example.com/mcp");
  const headers = pairEditor(initial.headers, {
    keyLabel: "Header",
    hint: "stored verbatim — put ${SECRET_NAME} here, never the credential itself",
  });
  const provenance = selectInput(
    [
      { value: "", label: "remote (screened — private addresses refused)" },
      { value: Provenance.Local, label: "local (allow a literal loopback endpoint)" },
    ],
    initial.provenance === Provenance.Local ? Provenance.Local : "",
  );
  const issuer = textInput(initial.oauth?.issuer ?? "", "https://auth.example.com");
  const scopes = linesEditor(initial.oauth?.scopes, "one scope per line");
  const resourceMeta = textInput(
    initial.oauth?.resourceMetadataUrl ?? "",
    "RFC 9728 protected-resource document (optional)",
  );
  // A URL is usually all an HTTP server needs: discovery finds the
  // authorization server, and headers are for the ones that want a static
  // token. Both stay one click away, and open by themselves once set.
  const httpAdvanced =
    Object.keys(initial.headers ?? {}).length > 0 || initial.provenance === Provenance.Local;
  const httpBlock = group(
    "Remote endpoint",
    field("URL", url),
    advanced(
      "Headers and network provenance",
      httpAdvanced,
      field("Headers", headers.node),
      field(
        "Provenance",
        provenance,
        "local unblocks a LITERAL loopback address only, never a hostname whose DNS answer claims to be local",
      ),
    ),
  );

  // -- common ---------------------------------------------------------------
  const derive = selectInput(
    [
      { value: "", label: "none (one shared instance)" },
      { value: "root", label: "root (one instance per project root)" },
      { value: "session", label: "session (one instance per session)" },
    ],
    initial.derive || "",
  );
  const enabled = checkboxInput("Enabled", initial.enabled);
  const transportContext = el("div", { class: "transport-context", "aria-live": "polite" });
  const transportField = field(
    "Connection type",
    transport,
    "Choose how AgentHub reaches this server. The configuration fields below change with this selection.",
  );
  transportField.classList.add("transport-field");

  function sync(): void {
    const stdio = transport.value === Transport.Stdio;
    stdioBlock.hidden = !stdio;
    httpBlock.hidden = stdio;
    dockerBlock.hidden = !stdio || runtime.value !== Runtime.Docker;
    if (stdio) {
      transportContext.textContent = "stdio runs a local process. Configure its command, arguments and environment.";
    } else if (transport.value === Transport.HTTP) {
      transportContext.textContent = "HTTP connects to an MCP Streamable HTTP URL. No local command is started.";
      url.placeholder = "https://example.com/mcp";
    } else {
      transportContext.textContent = "SSE connects to a legacy HTTP+SSE URL. No local command is started.";
      url.placeholder = "https://example.com/sse";
    }
  }
  transport.addEventListener("change", sync);
  runtime.addEventListener("change", sync);

  const node = el("div", { class: "form" }, [
    transportField,
    transportContext,
    stdioBlock,
    httpBlock,
    // Enabled stays in the open: it is the one switch here whose default a
    // reader may actually want to change while adding.
    enabled.node,
    advanced(
      "OAuth hints",
      initial.oauth != null,
      field("Issuer", issuer, "pins the authorization server and skips discovery"),
      field("Scopes", scopes.node),
      field("Resource metadata URL", resourceMeta),
    ),
    advanced(
      "Connection instancing",
      (initial.derive ?? "") !== "",
      field(
        "Derive",
        derive,
        "a connection-plane choice: it changes which process runs a call, never what a session may see",
      ),
    ),
  ]);
  sync();

  function parseMounts(): { ok: true; mounts: DockerMount[] } | { ok: false; message: string } {
    const out: DockerMount[] = [];
    for (const line of mounts.value()) {
      const parts = line.split(":");
      if (parts.length > 3 || !parts[0]) {
        return { ok: false, message: `Mount "${line}" is not source[:target][:ro|rw].` };
      }
      const mode = parts.length === 3 ? parts[2] : "ro";
      if (mode !== "ro" && mode !== "rw") {
        return { ok: false, message: `Mount "${line}": the mode must be ro or rw.` };
      }
      const mount: DockerMount = { source: parts[0] };
      if (parts.length >= 2 && parts[1]) mount.target = parts[1];
      if (mode === "rw") mount.write = true;
      out.push(mount);
    }
    return { ok: true, mounts: out };
  }

  return {
    node,
    collect(): Collected {
      // Every key of the entry is filled in: an update is a WHOLESALE
      // replacement, so a field the form leaves out would otherwise keep a
      // stored value the operator can no longer see (api/servers.go).
      const entry = blankEntry();
      entry.transport = transport.value;
      entry.enabled = enabled.box.checked;
      entry.derive = derive.value;
      entry.source = initial.source || "gui";
      const scopeList = scopes.value();
      if (issuer.value.trim() || scopeList.length > 0 || resourceMeta.value.trim()) {
        entry.oauth = {};
        if (issuer.value.trim()) entry.oauth.issuer = issuer.value.trim();
        if (scopeList.length > 0) entry.oauth.scopes = scopeList;
        if (resourceMeta.value.trim()) entry.oauth.resourceMetadataUrl = resourceMeta.value.trim();
      }

      if (transport.value === Transport.Stdio) {
        if (!command.value.trim()) {
          return { ok: false, message: "A stdio server needs a command." };
        }
        entry.command = command.value.trim();
        const argv = args.value();
        entry.args = argv.length > 0 ? argv : null;
        const envMap = env.value();
        entry.env = Object.keys(envMap).length > 0 ? envMap : null;
        entry.cwd = cwd.value.trim();
        entry.runtime = runtime.value;
        if (runtime.value === Runtime.Docker) {
          if (!image.value.trim()) {
            return { ok: false, message: "The docker runtime needs an image." };
          }
          const parsed = parseMounts();
          if (!parsed.ok) return parsed;
          const docker: DockerRuntime = { image: image.value.trim() };
          if (network.value.trim()) docker.network = network.value.trim();
          if (parsed.mounts.length > 0) docker.mounts = parsed.mounts;
          if (memory.value.trim()) docker.memory = memory.value.trim();
          if (cpus.value.trim()) docker.cpus = cpus.value.trim();
          if (dockerUser.value.trim()) docker.user = dockerUser.value.trim();
          if (workdir.value.trim()) docker.workdir = workdir.value.trim();
          const extra = extraArgs.value();
          if (extra.length > 0) docker.extraArgs = extra;
          entry.docker = docker;
        }
        return { ok: true, entry };
      }

      if (!url.value.trim()) {
        return { ok: false, message: `A ${transport.value} server needs a URL.` };
      }
      entry.url = url.value.trim();
      const headerMap = headers.value();
      entry.headers = Object.keys(headerMap).length > 0 ? headerMap : null;
      entry.provenance = provenance.value;
      return { ok: true, entry };
    },
  };
}

// ---------------------------------------------------------------------------
// Pasting another client's configuration (docs/modules/gui.md)
// ---------------------------------------------------------------------------
//
// The daemon parses; this half decides what the user is shown before they
// confirm. Two properties hold the design together:
//
//   - NOTHING IS WRITTEN BY THE PARSE. The preview is a list of proposals,
//     each of which is stored afterwards by the ordinary create path — which
//     is also where validation happens, so a proposal the registry will
//     refuse can and does appear here.
//   - EVERY PROPOSAL IS TICKED AND EVERY PROPOSAL IS UNTICKABLE. A pasted
//     configuration usually contains several servers and the user rarely
//     wants all of them; an all-or-nothing import would be answered by
//     pasting the file and deleting the rest afterwards, which is strictly
//     worse than deciding here.

/**
 * Shells, in the sense that matters here: a command whose ARGUMENTS are the
 * program.
 *
 * Fail direction: ADVISORY, and open by construction. `npx -y whatever` runs
 * arbitrary code too, so this is not a safety gate and it never disables the
 * confirm button — the user pasted the configuration on purpose. What it
 * catches is the case where the row's own "command" column stops being
 * informative because the real work is inside a `-c` string.
 */
const SHELL_COMMANDS = new Set([
  "sh", "bash", "zsh", "dash", "ash", "ksh", "fish", "csh", "tcsh",
  "cmd", "powershell", "pwsh", "wsl",
]);

function runsShellCommand(command: string | undefined): boolean {
  const base = (command ?? "").split(/[\\/]/).pop()?.toLowerCase() ?? "";
  return SHELL_COMMANDS.has(base.replace(/\.exe$/, ""));
}

/**
 * True when a host is private, loopback, link-local or otherwise not on the
 * public internet.
 *
 * Fail direction: WARN when in doubt. A hostname that does not parse as an
 * address is NOT warned about — a public hostname is the normal case, and
 * warning on every one would train the user to ignore the yellow — but a URL
 * that does not parse at all is, because a URL we cannot read is a URL we
 * cannot clear.
 *
 * This is a presentation aid, not the check. The enforcing one is the
 * daemon's SSRF screening, which resolves the name and refuses a private
 * ANSWER unless the entry declares provenance=local — so a hostname that
 * quietly resolves to 127.0.0.1 is caught there, not here.
 */
function isPrivateHost(host: string): boolean {
  const h = host.replace(/^\[|\]$/g, "").toLowerCase();
  if (h === "" || h === "localhost" || h.endsWith(".localhost")) return true;
  if (h.endsWith(".local") || h.endsWith(".internal") || h.endsWith(".home.arpa")) return true;
  if (h === "::1" || h === "::") return true;
  if (/^f[cd][0-9a-f]{2}:/.test(h)) return true; // fc00::/7 unique local
  if (/^fe[89ab][0-9a-f]:/.test(h)) return true; // fe80::/10 link local
  const m = /^(\d{1,3})\.(\d{1,3})\.\d{1,3}\.\d{1,3}$/.exec(h);
  if (!m) return false;
  const a = Number(m[1]);
  const b = Number(m[2]);
  if (a === 0 || a === 10 || a === 127) return true;
  if (a === 192 && b === 168) return true;
  if (a === 172 && b >= 16 && b <= 31) return true;
  if (a === 169 && b === 254) return true; // link local
  if (a === 100 && b >= 64 && b <= 127) return true; // carrier-grade NAT
  return false;
}

function isPrivateEndpoint(raw: string): boolean {
  try {
    return isPrivateHost(new URL(raw).hostname);
  } catch {
    return true; // unreadable: see the fail direction above
  }
}

/** The two things a pasted entry can be that the user must see BEFORE they
 *  confirm. Both are shown in yellow next to the row they belong to, never
 *  as a summary at the top: a warning that does not say which line it is
 *  about is a warning the reader has to go and match up by hand. */
function pasteWarnings(entry: Partial<ServerEntry>): string[] {
  const out: string[] = [];
  if (runsShellCommand(entry.command)) {
    out.push(
      "This runs a shell command, so what actually executes is inside its arguments. " +
        "Only import a configuration you trust.",
    );
  }
  if (entry.url && isPrivateEndpoint(entry.url)) {
    out.push(
      "This connects to a private or internal address. agenthub refuses those at connect " +
        "time unless the entry is marked local, so check that this is the machine you meant.",
    );
  }
  return out;
}

/**
 * The placeholder, which doubles as the list of accepted shapes.
 *
 * Naming the formats is the point: a bare "paste JSON here" leaves the user
 * guessing whether their file is one of the ones we read, and the failure it
 * produces ("could not parse") does not answer that either.
 */
const PASTE_PLACEHOLDER = [
  '{"mcpServers": {"github": {"command": "npx", "args": ["-y", "…"]}}}',
  '        — Claude Code / Claude Desktop / Cursor / Windsurf',
  '{"servers": {…}}  or  {"mcp": {"servers": {…}}}',
  "        — VS Code",
  '{"context_servers": {…}}',
  "        — Zed",
  '{"command": "npx", "args": ["-y", "…"]}',
  "        — one entry on its own, as `claude mcp add-json` takes it",
  "",
  "Codex TOML and Continue YAML are recognised but not parsed; agenthub says so",
  "and hands back the manual steps rather than taking a parser dependency.",
].join("\n");

/** The one inline layout rule in this file: a <details> body needs padding
 *  and `.bucket-body` deliberately has none (the server rows bring their
 *  own). Adding a class would mean editing style.css from here. */
const PASTE_BODY_STYLE = "padding:12px";

/** The spawn invocation on one line, command and every argument. Truncating
 *  it would hide exactly the argument worth reading. */
function commandLine(entry: Partial<ServerEntry>): string {
  return [entry.command ?? "", ...(entry.args ?? [])].filter(Boolean).join(" ");
}

/** The whole invocation on one line: the thing a user recognises a pasted
 *  server by, whichever transport it turned out to be. */
function parsedCommandLine(entry: Partial<ServerEntry>): string {
  if (entry.url) return entry.url;
  return commandLine(entry);
}

/**
 * A parsed proposal turned into a storable definition.
 *
 * The parser answers in the REGISTRY document shape, where every optional
 * field is omitted rather than nulled; a create sends the api shape, where
 * every key is present. Overlaying the proposal on a blank entry is what
 * bridges the two — and it keeps `source: "pasted"`, so `server ls` can say
 * afterwards where the definition came from.
 */
function fromParsed(partial: Partial<ServerEntry>): ServerEntry {
  const entry: ServerEntry = { ...blankEntry(), ...partial };
  if (!partial.transport) entry.transport = Transport.Stdio;
  return entry;
}

function kv(k: string, v: string): HTMLElement {
  return el("div", { class: "kv" }, [
    el("span", { class: "k", text: k }),
    el("span", { class: "v", text: v }),
  ]);
}

function testResultView(res: ServerTestResult): Node {
  return el("div", { class: "kvs" }, [
    kv("Transport", res.transport),
    kv("Server", res.server_info || "—"),
    kv("Protocol", res.protocol_version || "—"),
    kv("Connected in", `${res.connect_ms} ms`),
    kv("Tools", String(res.tool_count)),
    kv("Tool names", res.tools?.length ? res.tools.join(", ") : "—"),
  ]);
}

// ---------------------------------------------------------------------------
// The page
// ---------------------------------------------------------------------------

export function serversPage(): Page {
  let off: (() => void) | null = null;
  let root: HTMLElement | null = null;
  let listRoot: HTMLElement | null = null;
  let search: HTMLInputElement | null = null;
  let ticker: number | undefined;
  let filter = "";
  /** Narrows the list to the rows that are asking for something — the job the
   *  old "needs attention" bucket did by rearranging the page permanently. */
  let attentionOnly = false;
  const slot = noticeSlot();
  const form = modalHost();
  const secretManager = createServerSecretsManager(retestAfterSecretChange);

  /** Runtime state owned by this page's short-lived handshakes. Seed settled
   *  results from the prior mount so route changes do not flash "Checking…". */
  const probes = new Map<string, ProbeObservation>(probeCache);
  const probeVersions = new Map<string, number>();
  const inFlightProbes = new Map<string, ReturnType<typeof hub.testServer>>();
  const expandedServers = new Set<string>();
  const authStatuses = new Map<string, AuthStatus>();
  let authStatusLoaded = false;
  let authStatusError = "";
  let listedServers: Server[] = [];
  let fleetProbeEpoch = 0;
  let lastServerRevision = 0;

  /** When each currently-connecting server was first seen connecting. Reset
   *  whenever it leaves that state, so a reconnect starts the clock again. */
  const connectingSince = new Map<string, number>();
  /** id -> spawn command, resolved from the stored definition and read
   *  SYNCHRONOUSLY by the label ticker. It is filled lazily and ONLY for
   *  connecting servers: there is no reason to pull every definition to label
   *  one wait. */
  const commands = new Map<string, string>();

  /**
   * The nodes that survive a repaint, and the signature each was built from.
   *
   * A repaint happens on every probe start and every probe result — dozens of
   * them while a fleet settles — and the page used to answer each one by
   * rebuilding the entire table. Keeping the row nodes keyed by server id
   * turns that into "rebuild the one row whose rendered content changed",
   * which is what makes an unchanged row hold its hover, its focus and its
   * open disclosure while the row beside it is still checking.
   */
  const rowNodes = new Map<string, { node: HTMLElement; sig: string }>();
  let chipsHost: HTMLElement | null = null;
  let noticeHost: HTMLElement | null = null;
  const sectionHosts = new Map<SectionName, SectionHost>();
  let chipsSignature = "";
  let noticeSignature = "";

  function noteConnecting(servers: Server[]): void {
    const now = Date.now();
    const live = new Set<string>();
    for (const s of servers) {
      if (s.state !== "connecting") continue;
      live.add(s.id);
      if (!connectingSince.has(s.id)) connectingSince.set(s.id, now);
      if (!commands.has(s.id)) {
        commands.set(s.id, ""); // claim the slot so one paint queues one read
        // Not knowing the command only costs the "Installing…" label, which
        // is why an unreadable definition needs no handling of its own.
        void storedEntry(s.id).then((entry) => commands.set(s.id, entry?.command ?? ""));
      }
    }
    for (const id of Array.from(connectingSince.keys())) {
      if (!live.has(id)) connectingSince.delete(id);
    }
  }

  /** Re-labels the connecting rows in place. Redrawing the page on a timer
   *  would throw away scroll position and any open disclosure. */
  function tick(): void {
    if (!listRoot) return;
    const now = Date.now();
    for (const node of Array.from(listRoot.querySelectorAll<HTMLElement>(".state-checking"))) {
      const id = node.dataset.server ?? "";
      const since = connectingSince.get(id);
      const installing =
        since !== undefined &&
        now - since >= INSTALL_HINT_MS &&
        isPackageRunner(commands.get(id) ?? "");
      node.textContent = installing ? "Installing…" : "Checking…";
    }
  }

  function repaint(): void {
    if (!listRoot) return;
    const servers = listedServers.map((s) => withProbe(s, probes.get(s.id)));
    noteConnecting(servers);
    paint(servers);
    tick();
  }

  /** Refreshes credential metadata only. The API deliberately carries no
   *  token values, so this cache can answer the disclosure without turning
   *  an expansion click into a downstream request or a secret read. */
  async function loadAuthStatuses(): Promise<void> {
    try {
      const statuses = (await hub.authStatus("")) ?? [];
      authStatuses.clear();
      for (const status of statuses) authStatuses.set(status.server, status);
      authStatusError = "";
    } catch (err) {
      authStatusError = asCallError(err).message;
    } finally {
      authStatusLoaded = true;
    }
  }

  /** Runs one authoritative page-owned handshake. An authentication refusal
   *  is sticky: a later generic failure cannot downgrade a known credential
   *  problem into "connection error". Only a successful handshake clears it. */
  async function probeOne(id: string, showChecking = true): Promise<ServerTestResult> {
    const version = (probeVersions.get(id) ?? 0) + 1;
    probeVersions.set(id, version);
    const previous = probes.get(id);
    if ((!previous || showChecking) && previous?.kind !== "auth") {
      probes.set(id, { kind: "checking" });
    }
    repaint();

    inFlightProbes.get(id)?.cancel();
    const request = hub.testServer(id, {});
    inFlightProbes.set(id, request);
    try {
      const result = await request;
      if (probeVersions.get(id) === version && root) {
        const observation: SettledProbeObservation = {
          kind: "connected",
          tools: result.tool_count,
          result,
          checkedAt: Date.now(),
        };
        probes.set(id, observation);
        probeCache.set(id, observation);
        repaint();
      }
      return result;
    } catch (err) {
      if (probeVersions.get(id) === version && root) {
        const failure = asCallError(err);
        if (failure.code === ErrCode.SecretRequired && failure.missingSecrets?.length) {
          const keys = Array.from(new Set(failure.missingSecrets.filter((key) => key.trim() !== "")));
          const observation: SettledProbeObservation = { kind: "secret", keys, checkedAt: Date.now() };
          probes.set(id, observation);
          probeCache.set(id, observation);
        } else if (authenticationRequired(err)) {
          const observation: SettledProbeObservation = { kind: "auth", checkedAt: Date.now() };
          probes.set(id, observation);
          probeCache.set(id, observation);
        } else if (probes.get(id)?.kind !== "auth") {
          const observation: SettledProbeObservation = {
            kind: "error",
            summary: errorHeadline(failure.message),
            detail: [failure.message, failure.hint].filter(Boolean).join("\n"),
            checkedAt: Date.now(),
          };
          probes.set(id, observation);
          probeCache.set(id, observation);
        }
        repaint();
      }
      throw err;
    } finally {
      if (inFlightProbes.get(id) === request) inFlightProbes.delete(id);
    }
  }

  async function retestAfterSecretChange(id: string): Promise<void> {
    const server = listedServers.find((item) => item.id === id);
    if (!server?.enabled) {
      repaint();
      return;
    }
    try {
      await probeOne(id);
    } catch {
      // The row owns the next missing key or the concrete connection failure.
    }
  }

  /**
   * How many probes run at once.
   *
   * Bounded, not unbounded: a stdio probe SPAWNS A PROCESS — often `npx` or
   * `uvx`, which may download before it answers — so the ceiling is what the
   * machine is asked to start at the same moment, not what the daemon can
   * serve. It has no limit of its own; every /v1/servers/{id}/test is handled
   * independently, so this number is the only one there is.
   *
   * Ten rather than four because the previous ceiling made a fleet settle in
   * visible waves, and one slow package runner held a quarter of the capacity
   * for as long as it took to install.
   */
  const PROBE_CONCURRENCY = 10;

  /** Probes a fleet at bounded concurrency. A runtime SSE repaint with the
   *  same registry revision finds observations already present and starts
   *  nothing; a real registry revision change asks every enabled definition
   *  again because an external editor may have changed its endpoint. */
  function probeFleet(
    servers: Server[],
    force: boolean,
    showChecking: boolean,
  ): Promise<void> {
    const present = new Set(servers.map((s) => s.id));
    for (const id of Array.from(probes.keys())) {
      const server = servers.find((s) => s.id === id);
      if (!present.has(id) || !server?.enabled) {
        probes.delete(id);
        probeCache.delete(id);
        eventCache.delete(id);
        entryCache.delete(id);
        commands.delete(id);
        probeVersions.delete(id);
        if (!present.has(id)) expandedServers.delete(id);
      }
    }
    const ids = servers
      .filter((s) => s.enabled && (force || !probes.has(s.id)))
      .map((s) => s.id);
    if (ids.length === 0) return Promise.resolve();

    const epoch = ++fleetProbeEpoch;
    let cursor = 0;
    const worker = async (): Promise<void> => {
      while (root && epoch === fleetProbeEpoch) {
        const id = ids[cursor++];
        if (!id) return;
        try {
          await probeOne(id, showChecking);
        } catch {
          // The row owns the typed failure; fleet probing has no second
          // warning surface and must continue with the remaining servers.
        }
      }
    };
    return Promise.all(
      Array.from({ length: Math.min(PROBE_CONCURRENCY, ids.length) }, () => worker()),
    ).then(
      () => undefined,
    );
  }

  // -- the interactive login -------------------------------------------------

  /** How often the login session is polled. Fast enough that the browser
   *  opens without a visible pause, slow enough not to be a busy loop against
   *  a socket that is also carrying the rest of the page. */
  const LOGIN_POLL_MS = 700;

  const sleep = (ms: number): Promise<void> =>
    new Promise((resolve) => window.setTimeout(resolve, ms));

  /** The host of a URL, for a sentence that names where the user is being
   *  sent without pasting a 400-character authorization URL into prose. */
  function hostOf(raw: string): string {
    try {
      return new URL(raw).host;
    } catch {
      return raw;
    }
  }

  /**
   * Signs in to one server, for real.
   *
   * This replaces a modal whose entire content was "the GUI cannot do this,
   * run `agenthub auth login` in a terminal" — in an application whose premise
   * is that clients never handle credentials.
   *
   * THE SHAPE OF THE WAIT. The daemon runs the flow and this window polls it.
   * Nothing is shown for the first moment on purpose: choosing between the
   * device and loopback flows needs the authorization server's metadata, so
   * there is genuinely nothing to say yet, and inventing a mode before the
   * daemon has picked one would be a guess the user then has to unlearn.
   *
   * THE BROWSER IS OPENED BY US, through the HOST browser. An authorization
   * page rendered inside this webview would be agenthub asking for a
   * provider password in a window agenthub controls — the exact shape of a
   * phishing screen, and it takes away every check the user has (address bar,
   * lock, the password manager refusing to fill a wrong origin).
   *
   * CLOSING THE WINDOW CANCELS THE WAIT AND NOTHING ELSE. A consent already
   * granted at the provider stays granted and a credential already stored is
   * kept; this is the same caveat every cancellable surface here carries.
   */
  async function login(id: string): Promise<void> {
    const body = el("div", { class: "login" });
    let session = "";
    let stopped = false;
    // Set the moment the session reaches a terminal phase. Closing the window
    // afterwards must not send a cancel: the daemon answers it correctly
    // (cancelling a finished login keeps its result), but asking to abandon
    // something that already succeeded is a question with no right answer, and
    // a round trip that exists only because of the order this code runs in.
    let finished = false;
    let openedURL = "";

    const close = openModal(`Authenticate ${id}`, [body], {
      onClose: () => {
        stopped = true;
        if (session && !finished) {
          // Best effort: the window is already gone, and a failure to cancel
          // costs a session that expires on its own a few minutes later.
          void hub.cancelLogin(session).catch(() => undefined);
        }
      },
    });

    const show = (...nodes: (Node | null)[]): void => {
      clear(body);
      body.append(...nodes.filter((n): n is Node => n !== null));
    };

    const remaining = (deadline: number | undefined): Node | null => {
      if (!deadline) return null;
      const left = Math.max(0, deadline * 1000 - Date.now());
      const mins = Math.floor(left / 60000);
      const secs = Math.floor((left % 60000) / 1000);
      return el("p", {
        class: "hint",
        text: `This sign-in expires in ${mins}:${String(secs).padStart(2, "0")}.`,
      });
    };

    show(el("p", { class: "muted", text: `Contacting ${id}…` }));

    try {
      const started = await hub.startLogin(id);
      session = started.id;
    } catch (err) {
      show(failureBox(err));
      return;
    }

    for (;;) {
      if (stopped) return;
      let st: AuthLogin;
      try {
        st = await hub.loginStatus(session);
      } catch (err) {
        show(failureBox(err));
        return;
      }
      if (stopped) return;

      if (st.phase === LoginPhase.Complete) {
        finished = true;
        close();
        await loadAuthStatuses();
        try {
          await probeOne(id);
        } catch {
          // The page-owned probe has already kept the authentication action
          // or replaced it with its own concrete failure.
        }
        return;
      }

      if (st.phase === LoginPhase.Failed) {
        finished = true;
        show(
          el("div", { class: "login-failure" }, [
            el("strong", { text: errorHeadline(st.error || "The sign-in did not complete.") }),
            st.hint ? el("span", { class: "hint", text: st.hint }) : null,
          ]),
          controls(
            button("Try again", "btn btn-primary", () => {
              close();
              void login(id);
            }),
            button("Close", "btn btn-secondary", () => close()),
          ),
        );
        return;
      }

      // Still pending. Two shapes, and a third for "the daemon has not
      // decided yet" — which is a real state and not a spinner over nothing.
      if (st.mode === LoginMode.Device && st.user_code) {
        const target = st.verification_uri_complete || st.verification_uri || "";
        show(
          el("p", { text: `Open ${hostOf(target)} and enter this code:` }),
          el("div", { class: "login-code" }, [
            el("code", { text: st.user_code }),
            copyButton(() => st.user_code ?? "", "Copy"),
          ]),
          controls(
            button("Open the page", "btn btn-primary", () => {
              void openExternal(target).catch((err) => {
                body.prepend(failureBox(err));
              });
            }),
          ),
          el("p", { class: "hint", text: "This window notices on its own once you have approved it." }),
          remaining(st.deadline),
        );
      } else if (st.mode === LoginMode.Loopback && st.authorization_url) {
        // Opened once, not on every poll: re-opening a tab every 700ms would
        // bury the user's browser in identical consent screens.
        if (openedURL !== st.authorization_url) {
          openedURL = st.authorization_url;
          void openExternal(st.authorization_url).catch((err) => {
            body.prepend(failureBox(err));
          });
        }
        const url = st.authorization_url;
        show(
          el("p", { text: `Your browser is opening ${hostOf(url)}. Approve the request there.` }),
          controls(
            button("Open it again", "btn", () => {
              void openExternal(url).catch((err) => {
                body.prepend(failureBox(err));
              });
            }),
            copyButton(() => url, "Copy the link"),
          ),
          el("p", { class: "hint", text: "This window notices on its own once you have approved it." }),
          remaining(st.deadline),
        );
      } else {
        show(el("p", { class: "muted", text: "Working out how this provider signs you in…" }));
      }
      await sleep(LOGIN_POLL_MS);
    }
  }

  // -- the five status shapes (docs/modules/gui.md) -------------------------

  function dot(tone: string, extra = ""): HTMLElement {
    return el("span", { class: `dot ${tone}${extra ? ` ${extra}` : ""}` });
  }

  /** The status position of one row. Three channels every time — dot colour,
   *  the words, and the colour of the words — so the state survives a
   *  greyscale screenshot and a colour-blind reader. */
  function statusCell(s: Server): Node {
    const admin = s.health.admin_state;

    // Disabled stays quiet, but still says what the grey dot means. The list
    // is compact enough that one short word improves scanning without making
    // intentionally inactive rows compete with a failure.
    if (admin === AdminState.Disabled) {
      return el("div", { class: "srv-status" }, [
        el("div", { class: "state-line" }, [
          dot("neutral"),
          el("span", { class: "state-text t-muted", text: "Disabled" }),
        ]),
      ]);
    }

    // missing-secret: this is configuration work, not a failed connection.
    // The structured key name comes from E_SECRET_REQUIRED; no error string
    // is parsed to decide which form to open.
    if (s.health.action === HealthAction.SetSecret) {
      const observation = probes.get(s.id);
      const keys = observation?.kind === "secret" ? observation.keys : [];
      const message = keys.length === 1 && keys[0].endsWith("API_KEY")
        ? "API key required"
        : "Secret required";
      return el("div", { class: "srv-status" }, [
        el("div", { class: "state-line" }, [
          dot("warning"),
          el("span", { class: "state-text t-warning", text: message }),
        ]),
      ]);
    }

    // Keep state and recovery as two adjacent columns: the first answers what
    // is wrong, the second is the direct operation that repairs it.
    if (s.health.action === HealthAction.Login) {
      return el("div", { class: "srv-status" }, [
        el("div", { class: "state-line" }, [
          dot("warning"),
          el("span", { class: "state-text t-warning", text: "Authentication required" }),
        ]),
      ]);
    }

    // checking: a wait re-told as progress once it is long enough to need one.
    //
    // NEUTRAL, not warning. An unanswered handshake is the absence of an
    // answer, not a fault, and painting it yellow meant a page that opened
    // with every row shouting and then quietly took it back one row at a
    // time. The pulse is what says "in progress"; colour is reserved for
    // outcomes.
    if (s.state === "connecting") {
      return el("div", { class: "srv-status" }, [
        el("div", { class: "state-line" }, [
          dot("neutral", "pulse"),
          el("span", {
            class: "state-text t-muted state-checking",
            "data-server": s.id,
            text: "Checking…",
          }),
        ]),
      ]);
    }

    // error: one distilled line only. The daemon's full detail and suggested
    // recovery live in the record body, where they can grow vertically
    // without stealing all available width from the server name.
    if (s.health.level !== HealthLevel.Healthy) {
      const tone = s.health.level === HealthLevel.Unhealthy ? "danger" : "warning";
      return el("div", { class: "srv-status" }, [
        el("div", { class: "state-line" }, [
          dot(tone),
          el("span", {
            class: `state-text t-${tone}`,
            text: s.health.summary || s.state,
            title: s.health.summary || s.state,
          }),
        ]),
      ]);
    }

    // The inventory now has its own following column, so the status column can
    // say plainly what the green dot means.
    if (s.state === "connected") {
      return el("div", { class: "srv-status" }, [
        el("div", { class: "state-line" }, [
          dot("success"),
          el("span", { class: "state-text t-success", text: "Connected" }),
        ]),
      ]);
    }

    // Nobody is holding a connection to this server. That is a statement
    // about the observer, not about the server, and the daemon words it that
    // way in the summary — so it stays neutral, not yellow.
    return el("div", { class: "srv-status" }, [
      el("div", { class: "state-line" }, [
        dot("neutral"),
        el("span", {
          class: "state-text t-muted",
          text: s.health.summary || s.state,
          title: s.health.summary || s.state,
        }),
      ]),
    ]);
  }

  /**
   * The fixed outcome column carries either inventory or the direct setup
   * action. Both are short, high-value outcomes of the status immediately to
   * their left, and keeping the column fixed aligns every Test/Edit group.
   */
  function outcomeCell(s: Server): Node {
    let content: Node | null = null;
    if (s.health.action === HealthAction.SetSecret) {
      const observation = probes.get(s.id);
      const keys = observation?.kind === "secret" ? observation.keys : [];
      const label = keys.length === 1 && keys[0].endsWith("API_KEY") ? "Add API key" : "Set secret";
      content = button(label, "btn btn-primary btn-sm", () => secretManager.open(s.id, keys));
    } else if (s.health.action === HealthAction.Login) {
      content = button("Authenticate", "btn btn-primary btn-sm", () => void login(s.id));
    } else if (s.state === "connected") {
      content = el("span", {
        class: "server-tool-count",
        text: s.tools === 1 ? "1 tool" : `${s.tools} tools`,
        "aria-label": `${s.tools} tools available`,
      });
    }
    return el("div", { class: "server-row-outcome" }, [content]);
  }

  /**
   * The expanded row is a read of the last SETTLED page-owned handshake. It
   * never starts a request: Refresh and Test are the two explicit operations
   * that do that. This distinction is stated in the UI because a cached
   * diagnostic presented as live would be worse than no diagnostic.
   */
  function probeDetails(s: Server, detailID: string): Node {
    const observation = probeCache.get(s.id);
    const missingSecrets = observation?.kind === "secret" ? observation.keys : [];
    const action = s.health.action === HealthAction.Login
      ? null
      : suggestion(s, missingSecrets, () => secretManager.open(s.id, missingSecrets));
    let content: Node;
    if (!observation) {
      content = el("p", {
        class: "meta",
        text: s.enabled
          ? "No completed self-test is cached yet. Use Refresh or Test to run one."
          : "This server is disabled, so there is no self-test result to show.",
      });
    } else if (observation.kind === "connected") {
      content = testResultView(observation.result);
    } else if (observation.kind === "auth") {
      content = el("p", {
        class: "meta",
        text: "The latest self-test requires authentication. Authenticate, then the page will test the stored credential again.",
      });
    } else if (observation.kind === "secret") {
      content = el("p", {
        class: "meta",
        text: `The latest self-test needs ${observation.keys.join(", ")}. Store it, then AgentHub will test this server again.`,
      });
    } else {
      content = el("p", {
        class: "meta",
        text: observation.detail || observation.summary,
        title: observation.detail || observation.summary,
      });
    }
    const checked = observation
      ? `Cached result · checked ${new Date(observation.checkedAt).toLocaleTimeString()}`
      : "No cached result";
    return el("div", { class: "server-probe-detail", id: detailID }, [
      endpointLine(s),
      el("div", { class: "server-probe-detail-head" }, [
        el("strong", { text: "Latest self-test" }),
        el("span", { class: "meta", text: checked }),
      ]),
      el("div", { class: "server-health-detail-body" }, [content, action]),
      authDetails(s),
      recentEvents(s),
    ]);
  }

  /** What this server IS, ahead of everything about how it is doing: the
   *  endpoint for http/sse, the whole spawn command for stdio.
   *
   *  It leads the panel because the id says what a server was NAMED, never
   *  what it reaches, and "which endpoint is this actually" is the first
   *  question asked of a row that answers wrongly — until now it could only
   *  be answered by opening the editor, which is a write surface.
   *
   *  Read per server and only once expanded, like the timeline below it: the
   *  dashboard payload carries no definition, and pulling every one to fill a
   *  line nobody has opened would cost far more than it shows. */
  function endpointLine(s: Server): Node {
    const remote = s.transport === Transport.HTTP || s.transport === Transport.SSE;
    const label = remote ? "URL" : "Command";
    const host = el("div", { class: "server-probe-detail-head" }, [
      el("strong", { text: "Endpoint" }),
    ]);
    const body = el("div", { class: "server-health-detail-body" }, [
      el("div", { class: "kvs" }, [kv(label, "loading…")]),
    ]);
    const show = (value: string): void => {
      clear(body);
      body.append(el("div", { class: "kvs" }, [kv(label, value)]));
    };
    void storedEntry(s.id).then((entry) => {
      // A definition that cannot be read says so. Rendering an empty value
      // would claim this server is configured to reach nothing, which is a
      // different fault and one the operator would go looking for.
      if (!entry) return show("unavailable");
      show((remote ? entry.url : commandLine(entry)) || "—");
    });
    return el("div", {}, [host, body]);
  }

  /** The health badge above is a VALUE — what this server is right now. This
   *  is the sequence that produced it, which is a different question and the
   *  one an operator actually asks about a server that keeps dropping.
   *
   *  Loaded per server and only once expanded: the whole timeline lives on
   *  the Events page, and fetching every server's history to draw a list
   *  would cost far more than it shows. */
  function recentEvents(s: Server): Node {
    const host = el("div", { class: "server-probe-detail-head" }, [
      el("strong", { text: "Recent events" }),
      el("span", { class: "meta", text: "loading…" }),
    ]);
    const body = el("div", { class: "server-health-detail-body" });
    const cached = eventCache.get(s.id);
    if (cached) {
      renderEvents(host, body, cached);
    } else {
      void hub
        .eventLog(0, SERVER_EVENT_LIMIT, "server", s.id, "", "", [])
        .then((log) => {
          eventCache.set(s.id, log.events);
          renderEvents(host, body, log.events);
        })
        .catch(() => {
          clear(host);
          host.append(
            el("strong", { text: "Recent events" }),
            el("span", { class: "meta", text: "unavailable" }),
          );
        });
    }
    return el("div", {}, [host, body]);
  }

  function renderEvents(host: HTMLElement, body: HTMLElement, events: EventRecord[]): void {
    clear(host);
    host.append(
      el("strong", { text: "Recent events" }),
      el("span", { class: "meta", text: events.length === 0 ? "none recorded" : `${events.length} shown` }),
    );
    clear(body);
    if (events.length === 0) {
      body.append(
        el("p", {
          class: "meta",
          text: "Nothing recorded for this server. The stream is written by any running gateway unless events.enabled is false.",
        }),
      );
      return;
    }
    // Newest first, which is the order the endpoint already answers in and
    // the order the Events page renders it in: the reason this section is
    // open is that something just happened. Reversing here put the oldest
    // record of the window under the badge it was supposed to explain.
    body.append(table(["When", "What", "Subject", "Detail"], eventRows(events)));
  }

  function authExpiry(status: AuthStatus): string {
    if (status.expires_at === 0) return "No expiry advertised";
    const deadline = new Date(status.expires_at * 1000).toLocaleString();
    if (status.expires_in <= 0) {
      return `${deadline} (expired ${Math.abs(Math.round(status.expires_in / 60))} min ago)`;
    }
    if (status.expires_in < 3600) {
      return `${deadline} (in ${Math.round(status.expires_in / 60)} min)`;
    }
    if (status.expires_in < 86400) {
      return `${deadline} (in ${Math.round(status.expires_in / 3600)} h)`;
    }
    return deadline;
  }

  function authStateBadge(state: string): HTMLElement {
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

  function hasStoredCredential(id: string): boolean {
    const status = authStatuses.get(id);
    return status !== undefined && status.state !== AuthState.None;
  }

  /** Cached OAuth metadata belongs beside the cached self-test. Servers with
   *  no OAuth state and no authentication action omit this section entirely:
   *  an API-key server should not claim that an OAuth credential is missing.
   *  No token or refresh token value exists in this API shape. */
  function authDetails(s: Server): Node | null {
    const status = authStatuses.get(s.id);
    if (!status && s.health.action !== HealthAction.Login) return null;
    let body: Node;
    let badge: Node;
    if (!authStatusLoaded) {
      badge = el("span", { class: "badge badge-disabled", text: "not loaded" });
      body = el("p", { class: "meta", text: "Authorization status has not been loaded yet." });
    } else if (authStatusError && !status) {
      badge = el("span", { class: "badge badge-unhealthy", text: "unavailable" });
      body = el("p", { class: "meta", text: authStatusError, title: authStatusError });
    } else if (!status || status.state === AuthState.None) {
      badge = el("span", { class: "badge badge-disabled", text: "not stored" });
      body = el("p", {
        class: "meta",
        text: "No OAuth credential is stored for this server on this machine.",
      });
    } else {
      badge = authStateBadge(status.state);
      body = el("div", { class: "kvs" }, [
        kv("Access token", authExpiry(status)),
        kv("Refresh token", status.has_refresh_token ? "Available" : "Not available"),
        kv("Issuer", status.issuer || "—"),
        kv("Scope", status.scope || "—"),
        status.client_registrar ? kv("Client registrar", status.client_registrar) : null,
        status.detail ? kv("Detail", status.detail) : null,
      ]);
    }
    return el("section", { class: "server-auth-detail" }, [
      el("div", { class: "server-auth-detail-head" }, [
        el("strong", { text: "Authorization" }),
        badge,
      ]),
      el("div", { class: "server-auth-detail-body" }, [body]),
    ]);
  }

  // -- pasting another client's configuration --------------------------------

  /**
   * The collapsed area at the top of the Add form.
   *
   * Collapsed, because it is the shortcut and not the subject: someone who
   * came here to type a definition should not have to scroll past a
   * clipboard box. Open, it is the fastest path in the application — most
   * users arrive with a README fragment or another client's config already
   * copied, and the alternative is reading it and retyping it as flags.
   */
  function pasteBlock(): HTMLElement {
    const area = el("textarea", { class: "input textarea" }) as HTMLTextAreaElement;
    area.rows = 7;
    area.placeholder = PASTE_PLACEHOLDER;
    const notices = el("div", { class: "notice-slot" });
    const preview = el("div", { class: "form" });

    const parse = button("Parse", "btn btn-primary", () => void runParse());
    const reset = button("Clear", "btn btn-secondary", () => {
      area.value = "";
      clear(notices);
      clear(preview);
    });

    async function runParse(): Promise<void> {
      clear(notices);
      clear(preview);
      const text = area.value;
      if (!text.trim()) {
        notices.append(
          el("div", { class: "notice notice-warn", text: "Paste a configuration first." }),
        );
        return;
      }
      parse.disabled = true;
      parse.setAttribute("aria-busy", "true");
      try {
        // The daemon parses and writes NOTHING. What comes back is a list of
        // proposals; the ones the user keeps are stored one by one below,
        // which is also where each is validated.
        renderPreview(await hub.parseClientConfig(text));
      } catch (err) {
        // E_UNSUPPORTED_FORMAT (TOML, YAML) lands here like any other
        // refusal, and failureBox renders the daemon's hint — which for that
        // code is the manual route. It is not "your paste is broken".
        notices.append(failureBox(err));
      } finally {
        parse.disabled = false;
        parse.removeAttribute("aria-busy");
      }
    }

    interface Row {
      box: HTMLInputElement;
      name: HTMLInputElement;
      entry: ServerEntry;
    }

    function renderPreview(parsed: ParsedClientConfig): void {
      const proposals = parsed.servers ?? [];
      const skipped = parsed.skipped ?? [];

      const where = (parsed.section ?? []).join(".");
      preview.append(
        el("span", {
          class: "hint",
          text: where
            ? `Recognized as a ${parsed.shape} configuration; the servers were found under “${where}”.`
            : `Recognized as a ${parsed.shape} configuration.`,
        }),
      );

      if (proposals.length === 0) {
        preview.append(
          emptyState({
            kind: "empty",
            title: "Nothing in there to add.",
            body:
              skipped.length > 0
                ? "Every entry agenthub recognised was deliberately skipped — see below."
                : "The document was read, but it proposes no server agenthub can store.",
          }),
        );
        appendSkipped(skipped);
        return;
      }

      const rows: Row[] = [];
      for (const p of proposals) {
        const entry = fromParsed(p.entry);
        const box = el("input", { type: "checkbox" }) as HTMLInputElement;
        // Everything starts ticked: the common case is "import this file",
        // and an empty selection would make the confirm button dead on
        // arrival with no clue that ticking is what unlocks it.
        box.checked = true;
        box.addEventListener("change", () => sync());
        const name = textInput(p.name, "server id");
        name.addEventListener("input", () => sync());
        // The tick box carries its own label rather than sharing a <label>
        // with the id field: a label wrapping two controls sends clicks
        // meant for the text box to the checkbox in some engines, and an
        // import that silently unticks a row while you rename it is the
        // worst possible bug on this surface.
        box.setAttribute("aria-label", `Add ${p.name || "this server"}`);
        rows.push({ box, name, entry });

        const line = parsedCommandLine(p.entry);
        // The parser's own warnings and ours are rendered the same way and
        // in the same place. They answer the same question — "is this line
        // what you think it is" — and separating them by origin would make
        // the reader collate two lists.
        const warnings = [...(p.warnings ?? []), ...pasteWarnings(p.entry)];
        preview.append(
          el("div", { class: "card" }, [
            el("header", {}, [
              el("div", { class: "controls" }, [box, name]),
              el("span", {
                class: "badge badge-disabled",
                text: entry.transport || Transport.Stdio,
              }),
            ]),
            line ? el("code", { class: "cmd", title: line, text: line }) : null,
            entry.enabled
              ? null
              : el("div", { class: "meta", text: "Marked disabled in the source configuration; it will be stored switched off." }),
            ...warnings.map((w) => el("div", { class: "warn-line", text: w })),
          ]),
        );
      }

      appendSkipped(skipped);

      const confirm = button("", "btn btn-primary", () => void importSelected(rows));
      function sync(): void {
        const n = rows.filter((r) => r.box.checked).length;
        confirm.disabled = n === 0;
        // The label counts what is about to happen. A permanent "Import"
        // next to five ticked rows is the button that gets clicked without
        // looking at which five.
        confirm.textContent =
          n === 0 ? "Select a server" : n === 1 ? "Add 1 server" : `Add ${n} servers`;
      }
      preview.append(el("div", { class: "controls" }, [confirm]));
      sync();
    }

    function appendSkipped(skipped: ParsedSkip[]): void {
      if (skipped.length === 0) return;
      preview.append(
        el("div", { class: "hint" }, [
          el("div", { text: "Recognized but not offered:" }),
          ...skipped.map((s) =>
            el("div", { text: `${s.name || "(unnamed)"} — ${s.reason}` }),
          ),
        ]),
      );
    }

    async function importSelected(rows: Row[]): Promise<void> {
      clear(notices);
      const chosen = rows.filter((r) => r.box.checked);
      const names = chosen.map((r) => r.name.value.trim());
      if (names.some((n) => n === "")) {
        notices.append(
          el("div", {
            class: "notice notice-warn",
            text: "Every server you keep needs an id — it is the name clients and profiles will refer to. One pasted entry names nothing, so give it one here.",
          }),
        );
        return;
      }
      if (new Set(names).size !== names.length) {
        notices.append(
          el("div", {
            class: "notice notice-warn",
            text: "Two of the servers you kept would be stored under the same id. Rename one, or untick it.",
          }),
        );
        return;
      }

      // Each proposal is created on its own, and a refusal does NOT abort
      // the rest: the entries are unrelated, and stopping at the first bad
      // one would leave the user with a partial import they cannot tell
      // apart from a complete one. What they get instead is exactly which
      // ones landed and why the others did not.
      const added: string[] = [];
      const failed: { name: string; err: unknown }[] = [];
      for (const r of chosen) {
        const name = r.name.value.trim();
        try {
          // No precondition, same as the Add form: an id already taken is a
          // name conflict, not a lost update.
          await hub.createServer({ id: name, entry: r.entry }, 0);
          added.push(name);
        } catch (err) {
          failed.push({ name, err });
        }
      }

      await draw();
      if (failed.length === 0) {
        form.hide();
        slot.say(
          added.length === 1
            ? `${added[0]} added from the pasted configuration.`
            : `${added.length} servers added from the pasted configuration: ${added.join(", ")}.`,
        );
        return;
      }
      notices.append(
        el("div", { class: "notice notice-warn" }, [
          el("div", {
            text:
              added.length > 0
                ? `Added ${added.join(", ")}. ${failed.length} could not be stored:`
                : "Nothing was stored:",
          }),
          ...failed.map((f) =>
            el("div", { class: "action" }, [
              el("span", { class: "meta", text: f.name }),
              failureBox(f.err),
            ]),
          ),
        ]),
      );
    }

    return el("details", { class: "bucket" }, [
      el("summary", {}, [
        el("span", { text: "Paste a client configuration" }),
        el("span", { class: "bucket-count", text: "JSON from Claude Code, Cursor, VS Code, Zed…" }),
      ]),
      el("div", { class: "bucket-body" }, [
        el("div", { class: "form", style: PASTE_BODY_STYLE }, [
          el("span", {
            class: "hint",
            text:
              "agenthub reads it and shows you what it would store. Nothing is written until you " +
              "confirm, and you can untick anything you do not want.",
          }),
          area,
          controls(parse, reset),
          notices,
          preview,
        ]),
      ]),
    ]);
  }

  // -- writes ----------------------------------------------------------------

  function authenticationRequired(err: unknown): boolean {
    const code = asCallError(err).code;
    return code === ErrCode.AuthRequired || code === ErrCode.AuthFailed;
  }

  /** Checks an enabled definition immediately after the operator creates,
   *  edits, or switches it on. The probe is descriptive: the registry write
   *  already succeeded, so a failed handshake must never roll it back.
   *
   *  An auth refusal is the actionable branch. It carries the daemon's typed
   *  authentication error code rather than being inferred from prose, and
   *  the resulting button starts the same login session as the row status
   *  action. */
  async function probeAfterWrite(
    id: string,
    changed: string,
    announceSuccess = true,
  ): Promise<void> {
    try {
      const res = await probeOne(id);
      if (announceSuccess) {
        slot.say(`${id} ${changed}; connection check passed with ${res.tool_count} tool(s).`);
      }
    } catch (err) {
      // A newer page-owned probe supersedes and cancels this one. The newer
      // request owns the row state, so cancellation is neutral rather than a
      // failed connection check.
      if (isCancelled(err)) return;
      if (authenticationRequired(err)) {
        // The row is the single authentication surface. Do not create a
        // second warning card above the fleet for the same typed condition.
        slot.clear();
        return;
      }
      slot.clear();
      slot.node.append(
        el("div", { class: "notice notice-warn" }, [
          el("div", {
            text: `${id} ${changed}, but its connection check failed.`,
          }),
          failureBox(err),
        ]),
      );
    }
  }

  function editor(id: string, detail: ServerDetail | null): Node {
    const creating = detail === null;
    const idInput = textInput(id, "server id");
    idInput.disabled = !creating;
    const fields = entryForm(detail ? detail.entry : blankEntry());
    const errors = el("div", { class: "notice-slot" });

    const save = button(creating ? "Create server" : "Save changes", "btn btn-primary", () => {
      clear(errors);
      const name = idInput.value.trim();
      if (!name) {
        errors.append(el("div", { class: "notice notice-warn", text: "A server needs an id." }));
        return;
      }
      const collected = fields.collect();
      if (!collected.ok) {
        errors.append(el("div", { class: "notice notice-warn", text: collected.message }));
        return;
      }
      const spec = { id: name, entry: collected.entry };
      void runWrite(
        slot,
        () => draw(),
        (r) => `${r.id} ${creating ? "created" : "updated"}.`,
        () =>
          detail === null
            ? // A create needs no precondition: an id that already exists is
              // refused as a name conflict, so there is no lost update to
              // guard against — and an operator adding a server should not be
              // blocked by an unrelated edit elsewhere.
              hub.createServer(spec, 0)
            : hub.updateServer(spec, detail.generation),
      ).then(async (ok) => {
        if (!ok) return;
        // This write is the one that can change the endpoint, so the cached
        // definition behind the detail panel's first line goes with it.
        entryCache.delete(name);
        commands.delete(name);
        form.hide();
        if (collected.entry.enabled) {
          await probeAfterWrite(name, creating ? "created" : "updated");
        }
      });
    });

    const node = el("div", { class: "modal-form" }, [
      // Only when creating: pasting a configuration proposes NEW servers, so
      // offering it while editing an existing one would be an unrelated
      // action wearing the same form's clothes.
      creating ? pasteBlock() : null,
      errors,
      field("Id", idInput, creating ? "the name clients and profiles refer to" : "ids are not renamed"),
      fields.node,
      controls(save, button("Cancel", "btn btn-secondary", () => form.hide())),
    ]);
    return node;
  }

  async function openEditor(id: string): Promise<void> {
    try {
      const detail = await hub.getServer(id);
      form.show(`Edit ${id}`, editor(id, detail));
    } catch (err) {
      slot.fail(err);
    }
  }

  async function remove(s: Server): Promise<void> {
    // The write runs inside the dialog: a refusal keeps the confirmation
    // open with the reason in it, so the retry does not start with
    // "find the row again".
    const ok = await confirmAction({
      title: `Remove ${s.id}?`,
      body: "The stored definition is deleted from the registry.",
      consequences: [
        "Its stored credentials go with it, in the vault and in the OS keychain — an id re-added later must not inherit the last server's token.",
        "Profiles lose it from their server list and their tool rules; client bindings are untouched, because they name a profile rather than a server.",
        "Its log file is kept: a log that forgot deleted servers would be worthless as evidence.",
      ],
      confirmLabel: "Remove",
      danger: true,
      perform: async () => {
        // The generation is re-read INSIDE the attempt, so a 409 caused by
        // an edit elsewhere is fixed simply by pressing Remove again.
        const detail = await hub.getServer(s.id);
        await hub.deleteServer(s.id, detail.generation);
      },
    });
    if (!ok) return;
    probes.delete(s.id);
    probeCache.delete(s.id);
    eventCache.delete(s.id);
    entryCache.delete(s.id);
    commands.delete(s.id);
    probeVersions.delete(s.id);
    await draw();
    slot.say(`${s.id} removed.`);
  }

  async function logoutCredential(s: Server): Promise<void> {
    const ok = await confirmAction({
      title: `Remove the credential for ${s.id}?`,
      body: "The stored OAuth tokens are deleted from this machine.",
      consequences: [
        "This does NOT revoke anything at the provider. Revoke it there as well if that is what you meant.",
        "The server needs to be authenticated again before it works.",
      ],
      confirmLabel: "Log out",
      danger: true,
    });
    if (!ok) return;
    try {
      await hub.logoutAuth(s.id);
      authStatuses.delete(s.id);
      authStatusError = "";
      slot.clear();
      if (s.enabled) {
        try {
          await probeOne(s.id);
        } catch {
          // The row owns the expected authentication result or concrete
          // failure; logout needs no duplicate notice above the fleet.
        }
      } else {
        repaint();
      }
    } catch (err) {
      slot.fail(err);
    }
  }

  async function toggle(s: Server): Promise<void> {
    const next = !s.enabled;
    try {
      const detail = await hub.getServer(s.id);
      await hub.setServerEnabled(s.id, next, detail.generation);
    } catch (err) {
      slot.fail(err);
      return;
    }
    // The switch already shows the stored answer. A second success notice
    // only repeats the state while pushing the fleet down the page.
    slot.clear();
    // The stored definition just moved. Anything cached from the previous
    // read of it describes the entry before this write.
    entryCache.delete(s.id);
    commands.delete(s.id);
    await draw();
    if (next) {
      await probeAfterWrite(s.id, "enabled", false);
    } else {
      probes.delete(s.id);
      probeCache.delete(s.id);
      eventCache.delete(s.id);
      probeVersions.set(s.id, (probeVersions.get(s.id) ?? 0) + 1);
    }
  }

  async function test(s: Server): Promise<void> {
    const body = el("div", {}, [el("p", { class: "muted", text: "Connecting…" })]);
    const close = openModal(`Test ${s.id}`, [body]);
    try {
      // Handshake only: naming a tool here would CALL it, and a dashboard
      // button must not have side effects on a downstream system.
      const res = await probeOne(s.id);
      clear(body);
      body.append(
        el("p", {
          class: "hint",
          text:
            "A real connection and a real handshake. This is also how a credential is verified — " +
            "agenthub sends secrets, it never reads them back.",
        }),
        testResultView(res),
      );
    } catch (err) {
      if (isCancelled(err)) {
        close();
        return;
      }
      if (authenticationRequired(err)) {
        close();
        slot.clear();
        return;
      }
      clear(body);
      body.append(failureBox(err));
    }
  }

  // -- rendering -------------------------------------------------------------

  /** The spine's colour channel: health, with admin state winning — a server
   *  switched off on purpose is not broken. */
  function spineTone(s: Server): string {
    const admin = s.health.admin_state;
    if (admin === AdminState.Disabled) return "off";
    // Before the level is consulted: an unsettled row reports degraded, and
    // the spine is the widest state channel on the row — twenty yellow spines
    // on a page that has not asked anything yet is the whole complaint.
    if (s.state === "connecting") return "checking";
    // A credential is missing, not a connection broken. The warning spine
    // matches the setup action and avoids painting routine auth/key setup as
    // the same red failure used for network and protocol faults.
    if (s.health.action === HealthAction.Login || s.health.action === HealthAction.SetSecret) return "warning";
    if (s.health.level === HealthLevel.Healthy) return "ok";
    if (s.health.level === HealthLevel.Degraded) return "warning";
    return "bad";
  }

  /** Destructive removal is intentionally one level below the frequent Test
   *  action. Repeating a red outlined button on every healthy row makes the
   *  whole list read as an alert surface even when nothing is wrong. */
  function rowMenu(s: Server): HTMLElement {
    const menu = el("details", { class: "server-row-menu" }) as HTMLDetailsElement;
    const summary = el("summary", {
      "aria-label": `More actions for ${s.id}`,
      "aria-haspopup": "menu",
      title: "More actions",
      text: "⋯",
    });
    const items: Node[] = [];
    const secretsButton = el("button", {
      class: "server-row-menu-item",
      type: "button",
      role: "menuitem",
      text: "Manage secrets",
    });
    secretsButton.addEventListener("click", () => {
      menu.open = false;
      secretManager.open(s.id);
    });
    items.push(secretsButton);
    if (hasStoredCredential(s.id)) {
      const logoutButton = el("button", {
        class: "server-row-menu-item",
        type: "button",
        role: "menuitem",
        text: "Log out OAuth",
      });
      logoutButton.addEventListener("click", () => {
        menu.open = false;
        void logoutCredential(s);
      });
      items.push(logoutButton);
    }
    const removeButton = el("button", {
      class: "server-row-menu-item server-row-menu-danger",
      type: "button",
      role: "menuitem",
      text: "Remove server",
    });
    removeButton.addEventListener("click", () => {
      menu.open = false;
      void remove(s);
    });
    items.push(removeButton);
    menu.addEventListener("keydown", (ev) => {
      if (ev.key !== "Escape") return;
      ev.preventDefault();
      menu.open = false;
      summary.focus();
    });
    menu.addEventListener("toggle", () => {
      const record = menu.closest(".rec");
      const bucket = menu.closest(".bucket");
      record?.classList.toggle("menu-open", menu.open);
      bucket?.classList.toggle("menu-open", menu.open);
      menu.classList.remove("open-up");
      if (!menu.open) return;

      // A menu on the bucket's last row must not disappear beneath the next
      // bucket. Prefer opening it upward when there is room; still fall back
      // to the viewport-safe direction for short windows and one-row groups.
      const popover = menu.querySelector<HTMLElement>(".server-row-menu-popover");
      const bucketBody = record?.parentElement;
      const lastRow = bucketBody?.lastElementChild === record;
      if (popover) {
        const triggerRect = summary.getBoundingClientRect();
        const popoverHeight = popover.getBoundingClientRect().height;
        const gap = 6;
        const edge = 12;
        const roomAbove = triggerRect.top - edge;
        const roomBelow = window.innerHeight - triggerRect.bottom - edge;
        const opensUp = (lastRow && roomAbove >= popoverHeight + gap)
          || (roomBelow < popoverHeight + gap && roomAbove > roomBelow);
        menu.classList.toggle("open-up", opensUp);
      }
      document.addEventListener(
        "pointerdown",
        (ev) => {
          if (!menu.contains(ev.target as Node)) menu.open = false;
        },
        { once: true },
      );
    });
    menu.append(
      summary,
      el("div", { class: "server-row-menu-popover", role: "menu" }, items),
    );
    return menu;
  }

  function row(s: Server): HTMLElement {
    // The spine's second channel: dashed means this definition is reached
    // over the network rather than run here. Same convention as the Catalog
    // ledger, because it is the same distinction.
    const remote = s.transport === Transport.HTTP || s.transport === Transport.SSE;
    const expanded = expandedServers.has(s.id);
    const detailID = `server-probe-${encodeURIComponent(s.id)}`;
    const toggleDetails = (): void => {
      if (expanded) expandedServers.delete(s.id);
      else expandedServers.add(s.id);
      repaint();
    };
    const overview = el("button", {
      class: "rec-overview",
      type: "button",
      "aria-label": `${expanded ? "Hide" : "Show"} details for ${s.id}`,
      "aria-expanded": String(expanded),
      "aria-controls": detailID,
    }, [
      el("span", { class: "rec-overview-cue", "aria-hidden": "true", text: "▸" }),
      el("span", { class: "rec-title" }, [
        el("span", { class: "rec-name", text: s.id }),
        // Metadata is NEUTRAL: a green "stdio" next to a green health dot
        // would put two unrelated greens on one row (see style.css).
        el("span", { class: "id-chip", text: s.transport || "stdio" }),
      ]),
    ]) as HTMLButtonElement;
    overview.addEventListener("click", toggleDetails);

    const summary = el("div", { class: "server-summary-row" }, [
      el("div", { class: `spine ${spineTone(s)}${remote ? " remote" : ""}` }),
      // The global switch, in the leading position: enabling and disabling is
      // the setting this page exists to change, and it was previously a word
      // in a row of four identical-looking buttons at the far end of the row.
      // A switch also states the CURRENT value, which the word "Disable"
      // could only imply — and implied it backwards for anyone who reads a
      // button as a label rather than as a verb.
      el("div", { class: "rec-lead" }, [
        toggleSwitch({
          checked: s.enabled,
          label: `${s.id} enabled`,
          onChange: () => toggle(s),
        }),
      ]),
      el("div", { class: "rec-body" }, [overview]),
      statusCell(s),
      outcomeCell(s),
      el("div", { class: "rec-act" }, [
        controls(
          button("Test", "btn btn-sm", () => void test(s)),
          button("Edit", "btn btn-sm", () => void openEditor(s.id)),
          rowMenu(s),
        ),
      ]),
    ]);
    summary.addEventListener("click", (event) => {
      const target = event.target;
      if (!(target instanceof Element)) return;
      // Controls keep their own meaning. Everything else in the fixed summary
      // row is the disclosure target; the detail panel is a sibling and can
      // never bubble into this listener.
      const control = target.closest("button, a, input, label, summary, details");
      // The row itself lives inside the bucket's <details>. Only controls
      // contained by this summary are exclusions; treating any ancestor
      // <details> as a control makes the status, tool count and whitespace
      // inert because they all find that outer bucket.
      if (control && summary.contains(control)) return;
      toggleDetails();
    });

    return el("div", { class: "rec server-record" }, [
      summary,
      expanded ? probeDetails(s, detailID) : null,
    ]);
  }

  /** One group header, built ONCE.
   *
   *  Built once because <details> carries state the user set: rebuilding the
   *  element on every probe result would close a section they had just opened,
   *  and the stored preference cannot distinguish that from a choice. */
  function buildSection(name: SectionName): SectionHost {
    const details = el("details", { class: "bucket" }) as HTMLDetailsElement;
    details.open = !sectionFolded(name);
    // Setting `open` above queues a toggle event of its own; ignore that one
    // so a redraw never rewrites the stored preference with what it just
    // read. Only a real click is a choice.
    let settled = false;
    details.addEventListener("toggle", () => {
      if (!settled) {
        settled = true;
        return;
      }
      setSectionFolded(name, !details.open);
    });
    if (!details.open) settled = true;
    const count = el("span", { class: "bucket-count", text: "0" });
    const body = el("div", { class: "bucket-body ledger" });
    // Built with the section and reused, never rebuilt: it is what the body
    // holds while the group is empty, and a node replaced on every repaint
    // would flicker underneath a fleet that is still settling.
    const placeholder = el("p", { class: "bucket-empty meta" });
    details.append(el("summary", {}, [el("span", { text: SECTIONS[name].title }), count]), body);
    return { node: details, body, count, placeholder };
  }

  /**
   * Everything `row` reads, flattened. A row is rebuilt when this changes and
   * reused when it does not.
   *
   * The whole server DTO goes in rather than the fields the row happens to
   * render today: a reused node keeps the closures it was built with, so a
   * field that reaches a click handler without appearing on screen would
   * otherwise go stale invisibly. The expanded body's inputs are included only
   * while it is open, so a cached diagnostic arriving for a collapsed row does
   * not rebuild it.
   */
  function rowSignature(s: Server): string {
    const expanded = expandedServers.has(s.id);
    const observation = probes.get(s.id);
    return JSON.stringify([
      s,
      expanded,
      observation?.kind === "secret" ? observation.keys : null,
      expanded ? probeCache.get(s.id) ?? null : null,
      expanded ? authStatuses.get(s.id) ?? null : null,
      expanded ? [authStatusLoaded, authStatusError] : null,
    ]);
  }

  function rowFor(s: Server): HTMLElement {
    const sig = rowSignature(s);
    const cached = rowNodes.get(s.id);
    if (cached && cached.sig === sig) return cached.node;
    const node = row(s);
    rowNodes.set(s.id, { node, sig });
    return node;
  }

  /** Creates the four hosts. They are the page's furniture: everything a
   *  repaint changes happens INSIDE them.
   *
   *  The test is "are they still MY children", not "do I hold them". A failed
   *  registry read replaces the whole list with a failure state, and holding
   *  references to hosts that were just detached is how a page ends up
   *  painting, correctly and invisibly, into a discarded tree. */
  function ensureHosts(): void {
    if (!listRoot) return;
    if (chipsHost && chipsHost.parentElement === listRoot) return;
    clear(listRoot); // the "Reading the registry…" skeleton, or a failure state
    rowNodes.clear();
    sectionHosts.clear();
    chipsSignature = "";
    noticeSignature = "";
    chipsHost = el("div", { class: "server-chips-host" });
    noticeHost = el("div", { class: "server-notice-host" });
    for (const name of ["enabled", "disabled"] as SectionName[]) {
      sectionHosts.set(name, buildSection(name));
    }
    listRoot.append(
      chipsHost,
      noticeHost,
      sectionHosts.get("enabled")!.node,
      sectionHosts.get("disabled")!.node,
    );
  }

  /** Drops BOTH filters. One button, because a view narrowed twice and
   *  cleared once is a view the user has to discover a second control for. */
  function clearFilters(): void {
    filter = "";
    attentionOnly = false;
    if (search) search.value = "";
    repaint();
  }

  function paint(servers: Server[]): void {
    if (!listRoot) return;
    ensureHosts();
    const enabledHost = sectionHosts.get("enabled");
    const disabledHost = sectionHosts.get("disabled");
    if (!chipsHost || !noticeHost || !enabledHost || !disabledHost) return;

    const needle = filter.trim().toLowerCase();
    const shown = servers.filter(
      (s) =>
        (needle === "" || s.id.toLowerCase().includes(needle)) &&
        (!attentionOnly || needsAttention(s)),
    );

    // Chips describe the WHOLE fleet, not the filtered view: a summary that
    // silently means "of what you can currently see" is the same lie as a
    // global action under a filter.
    const connected = servers.filter((s) => s.state === "connected").length;
    const attention = servers.filter(needsAttention).length;
    const disabled = servers.filter(isDisabled).length;
    // Still checking: the page's own handshake is what puts an enabled row in
    // the connecting state, so this counts unanswered questions, not servers
    // the daemon believes are mid-connection.
    const checking = servers.filter((s) => !isDisabled(s) && s.state === "connecting").length;

    const chipsSig = JSON.stringify([servers.length, connected, attention, disabled, checking, attentionOnly]);
    if (chipsSig !== chipsSignature) {
      chipsSignature = chipsSig;
      const hadFocus = chipsHost.contains(document.activeElement);
      clear(chipsHost);
      // WHILE THE FLEET IS SETTLING the verdict chips are absent rather than
      // partial. A "connected" count that climbs from 0 to 13 one probe at a
      // time is motion that says nothing: the number is only an answer once
      // every question has been asked. What is shown instead is the progress
      // itself, counting down.
      // The toggle survives a re-probe even while the counts are withheld: it
      // is the control that turns the filter OFF, and a filter whose only
      // switch has gone missing is a view the user cannot leave.
      const attentionChip = chipToggle(
        attention,
        attention === 1 ? "needs attention" : "need attention",
        "warning",
        {
          pressed: attentionOnly,
          onToggle: () => {
            attentionOnly = !attentionOnly;
            repaint();
          },
        },
      );
      const chips = checking > 0
        ? chipRow(
            chip(servers.length, servers.length === 1 ? "server" : "servers"),
            chip(checking, "checking"),
            attentionOnly ? attentionChip : null,
            chip(disabled, "disabled"),
          )
        : chipRow(
            chip(servers.length, servers.length === 1 ? "server" : "servers"),
            chip(connected, "connected", "success"),
            attentionChip,
            chip(disabled, "disabled"),
          );
      if (chips) chipsHost.append(chips);
      // A settling probe rebuilds these chips underneath whoever just clicked
      // one. Putting focus back is what keeps the toggle usable from the
      // keyboard while the fleet is still answering.
      if (hadFocus) chipsHost.querySelector<HTMLElement>(".chip-toggle")?.focus();
    }

    // Which empty state applies, if any. Rebuilt only when the answer
    // changes, because both of them contain a button.
    const notice = servers.length === 0
      ? "none"
      : shown.length > 0
        ? ""
        : attentionOnly && attention === 0
          ? "settled"
          : "filtered";
    const noticeSig = JSON.stringify([notice, filter.trim(), servers.length, attentionOnly]);
    if (noticeSig !== noticeSignature) {
      noticeSignature = noticeSig;
      clear(noticeHost);
      if (notice === "none") {
        noticeHost.append(
          emptyState({
            kind: "empty",
            title: "No servers configured yet.",
            body: "A server is one downstream MCP process or endpoint. Add one and agenthub will offer its tools to every client you connect.",
            actions: [
              button("Add your first server", "btn btn-primary", () =>
                form.show("Add server", editor("", null)),
              ),
            ],
          }),
        );
      } else if (notice === "settled") {
        noticeHost.append(
          emptyState({
            kind: "empty",
            title: "Nothing needs attention.",
            body: "Every enabled server answered its last handshake. This view is filtered to the ones that did not.",
            actions: [button("Show all servers", "btn", clearFilters)],
          }),
        );
      } else if (notice === "filtered") {
        const both = attentionOnly && filter.trim() !== "";
        noticeHost.append(
          emptyState({
            kind: "empty",
            title: both
              ? `Nothing that needs attention contains \u201C${filter.trim()}\u201D.`
              : attentionOnly
                ? "Nothing needs attention."
                : `No server id contains \u201C${filter.trim()}\u201D.`,
            body: `All ${servers.length} configured servers are still there — this view is filtered.`,
            actions: [button("Clear filters", "btn", clearFilters)],
          }),
        );
      }
    }

    // One order, by id, for both groups: the same order `server ls` prints,
    // and one a reader can predict before the page has finished loading.
    const byID = (a: Server, b: Server): number => a.id.localeCompare(b.id);
    const inService = shown.filter((s) => !isDisabled(s)).sort(byID);
    const switchedOff = shown.filter(isDisabled).sort(byID);

    // A row that has left the fleet takes its node with it, or the cache
    // would keep a growing set of servers that no longer exist.
    const present = new Set(servers.map((s) => s.id));
    for (const id of Array.from(rowNodes.keys())) {
      if (!present.has(id)) rowNodes.delete(id);
    }

    // BOTH GROUPS ARE ALWAYS RENDERED, empty or not — see
    // sectionPlaceholder(). An empty one shows its count and one sentence
    // saying which of the two reasons it is empty for.
    const anyEnabled = servers.some((s) => !isDisabled(s));
    const anyDisabled = servers.some(isDisabled);
    fill(enabledHost, "enabled", inService, anyEnabled);
    fill(disabledHost, "disabled", switchedOff, anyDisabled);
  }

  /** Puts a group's rows in its body, or the placeholder when it has none.
   *  `existsUnfiltered` separates "there are none" from "this view hides
   *  them", which are different sentences to the reader. */
  function fill(host: SectionHost, name: SectionName, rows: Server[], existsUnfiltered: boolean): void {
    host.count.textContent = String(rows.length);
    if (rows.length > 0) {
      reconcile(host.body, rows.map(rowFor));
      return;
    }
    host.placeholder.textContent = sectionPlaceholder(name, existsUnfiltered);
    reconcile(host.body, [host.placeholder]);
  }

  async function draw(
    forceProbe = false,
    waitForProbes = false,
    showChecking = false,
  ): Promise<void> {
    if (!root) return;
    if (!listRoot) {
      // First paint: build the frame once so the notice slot, the filter and
      // any open editor survive every later redraw.
      clear(root);
      listRoot = el("div", { class: "server-list" }, [loadingState("Reading the registry…")]);
      const box = textInput(filter, "Search servers…");
      box.classList.add("search-input");
      box.addEventListener("input", () => {
        filter = box.value;
        void draw();
      });
      search = box;
      const refresh = button("Refresh", "btn", () => {
        refresh.setAttribute("aria-busy", "true");
        void draw(true, true, true).finally(() => refresh.removeAttribute("aria-busy"));
      });
      root.append(
        pageHeader(
          "Servers",
          "Configure every downstream MCP process and endpoint, then act on the ones that need attention.",
          refresh,
          el("a", { class: "btn", href: "#/catalog", text: "Browse Catalog" }),
          button("Add server", "btn btn-primary", () => form.show("Add server", editor("", null))),
        ),
        el("div", { class: "page-toolbar" }, [
          el("div", { class: "toolbar-search" }, [
            icon("search", "search-glyph"),
            box,
          ]),
          el("span", {
            class: "toolbar-hint",
            text: "Health is checked directly from this page.",
          }),
        ]),
        slot.node,
        listRoot,
      );
    }

    let servers: Server[];
    try {
      servers = (await hub.listServers()) ?? [];
    } catch (err) {
      clear(listRoot);
      listRoot.append(failureState(err, "the server list", () => void draw()));
      return;
    }
    listedServers = servers;
    if (forceProbe || !authStatusLoaded) await loadAuthStatuses();
    const probing = probeFleet(servers, forceProbe, showChecking);
    repaint();
    if (waitForProbes) await probing;
  }

  return {
    async render(node) {
      root = node;
      // Runtime reports and registry changes share the servers topic. Both
      // trigger a configuration re-read, but only a newer registry revision
      // starts fresh page-owned handshakes; gateway churn cannot overwrite
      // or continuously retrigger this page's status observations.
      off = on<TopicEvent>(EVT.servers, (event) => {
        const registryChanged = lastServerRevision !== 0 && event.rev > lastServerRevision;
        lastServerRevision = Math.max(lastServerRevision, event.rev);
        void draw(registryChanged);
      });
      ticker = window.setInterval(tick, 1000);
      const requestedSecrets = consumeServerSecrets();
      // A cross-page handoff opens one Server's manager after the registry is
      // present. It does not turn navigation into a forced fleet probe.
      await draw(requestedSecrets === null);
      if (requestedSecrets && listedServers.some((server) => server.id === requestedSecrets.server)) {
        secretManager.open(requestedSecrets.server, requestedSecrets.keys);
      }
    },
    dispose() {
      off?.();
      off = null;
      if (ticker !== undefined) window.clearInterval(ticker);
      ticker = undefined;
      fleetProbeEpoch++;
      connectingSince.clear();
      commands.clear();
      probes.clear();
      probeVersions.clear();
      for (const request of inFlightProbes.values()) request.cancel();
      inFlightProbes.clear();
      listedServers = [];
      lastServerRevision = 0;
      // The surviving nodes belong to a mount that is over. Dropping them
      // here — rather than letting the next mount find stale hosts pointing
      // at a detached tree — is what keeps re-entering the page a clean
      // build rather than a repair.
      rowNodes.clear();
      sectionHosts.clear();
      chipsHost = null;
      noticeHost = null;
      chipsSignature = "";
      noticeSignature = "";
      root = null;
      listRoot = null;
      search = null;
      form.hide();
      secretManager.hide();
    },
  };
}
