// Servers page: the dashboard, the definition editor and the live self-test.
//
// The listing NEVER derives status itself: level, admin_state and action come
// from the daemon as one pure-function result (docs/modules/controlplane.md), and the
// constants they are compared against are generated from the Go api package
// (src/generated/health.ts). What this file DOES decide is presentation:
// which of three buckets a row belongs in, and which of the five status
// shapes its status cell takes.
//
// INFORMATION ARCHITECTURE (docs/modules/gui.md / 2.2)
//
//   - Rows are grouped by "does this need you", not alphabetically. Needs
//     attention / Active / Disabled, empty buckets not rendered at all, the
//     disabled bucket collapsed by default and its collapse state remembered.
//     Alphabetical order is only useful to someone who already knows which
//     name they are looking for; the operator opening this window usually
//     does not, which is why they opened it.
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

import { EVT, hub, on, openExternal } from "../bridge";
import { chip, chipRow, clear, el, emptyState, icon, loadingState, pageHeader } from "../dom";
import { AdminState, HealthAction, HealthLevel } from "../generated/health";
import type { Page } from "../page";
import { failureBox, failureState, noticeSlot, runWrite } from "../page";
import {
  advanced,
  button,
  checkboxInput,
  cliBlock,
  cliHint,
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
  shellArg,
  textInput,
  toggleSwitch,
} from "../ui";
import type {
  AuthLogin,
  DockerMount,
  DockerRuntime,
  ParsedClientConfig,
  ParsedSkip,
  Server,
  ServerDetail,
  ServerEntry,
  ServerTestResult,
  TopicEvent,
} from "../types";
import { LoginMode, LoginPhase, Provenance, Runtime, Transport } from "../types";

// ---------------------------------------------------------------------------
// Equivalent CLI commands (docs/modules/gui.md)
// ---------------------------------------------------------------------------
//
// Every command below exists in internal/cli. Nothing is invented: a command
// that looks plausible and is not real would be worse than showing none,
// because the operator finds out only after pasting it somewhere that matters.

const cliRemove = (id: string): string => `agenthub server rm ${shellArg(id)}`;
const cliEnable = (id: string): string => `agenthub server enable ${shellArg(id)}`;
const cliDisable = (id: string): string => `agenthub server disable ${shellArg(id)}`;
const cliTest = (id: string): string => `agenthub server test ${shellArg(id)}`;

/**
 * The `server add` line that reproduces one stored definition.
 *
 * Rendered from the SAME entry the form is about to send, so the two cannot
 * drift: if a field is not representable as a flag it does not silently
 * disappear from the command, it is simply not in the entry either.
 */
function cliAdd(id: string, e: ServerEntry): string {
  const parts = ["agenthub server add", shellArg(id || "<id>")];
  const push = (flag: string, value: string): void => {
    parts.push(flag, shellArg(value));
  };
  if (e.transport && e.transport !== Transport.Stdio) push("--transport", e.transport);
  if (e.command) push("--cmd", e.command);
  if (e.args && e.args.length > 0) push("--args", e.args.join(","));
  if (e.cwd) push("--cwd", e.cwd);
  if (e.url) push("--url", e.url);
  for (const [k, v] of Object.entries(e.env ?? {})) push("--env", `${k}=${v}`);
  for (const [k, v] of Object.entries(e.headers ?? {})) push("--header", `${k}=${v}`);
  if (e.provenance === Provenance.Local) parts.push("--local");
  if (e.runtime === Runtime.Docker) {
    push("--runtime", Runtime.Docker);
    const d = e.docker;
    if (d) {
      if (d.image) push("--image", d.image);
      if (d.network) push("--network", d.network);
      for (const m of d.mounts ?? []) {
        push("--mount", [m.source, m.target ?? "", m.write ? "rw" : "ro"].join(":"));
      }
      if (d.memory) push("--memory", d.memory);
      if (d.cpus) push("--cpus", d.cpus);
      if (d.user) push("--container-user", d.user);
      if (d.workdir) push("--container-workdir", d.workdir);
      for (const a of d.extraArgs ?? []) push("--docker-arg", a);
    }
  }
  if (e.oauth?.issuer) push("--oauth-issuer", e.oauth.issuer);
  for (const s of e.oauth?.scopes ?? []) push("--oauth-scope", s);
  if (e.oauth?.resourceMetadataUrl) push("--oauth-resource-metadata", e.oauth.resourceMetadataUrl);
  return parts.join(" ");
}

/**
 * There is no `server edit`: the CLI has add / rm / enable / disable /
 * test / inspect / logs and nothing else, so the honest equivalent of the
 * GUI's wholesale update is a remove followed by an add.
 *
 * Saying so is the point of this feature. Papering over the gap with a
 * pretend `server edit` would turn a teaching aid into a source of commands
 * that fail when pasted.
 */
function cliUpdate(id: string, e: ServerEntry): string {
  return `${cliRemove(id)} && ${cliAdd(id, e)}`;
}

// ---------------------------------------------------------------------------
// Health presentation
// ---------------------------------------------------------------------------

/** Actions the daemon can suggest that this page cannot perform itself. The
 *  honest form of "GUI is optional": an affordance may not exist before the
 *  endpoint does, so the exact CLI command is offered instead of a button
 *  that pretends to work. */
const CLI_ACTIONS: Record<string, { label: string; command: string; note?: string }> = {
  [HealthAction.Login]: {
    label: "Sign in",
    command: "agenthub auth login <id>",
    note: "interactive login is a CLI flow; refresh and logout are on the Auth page",
  },
  [HealthAction.ViewLogs]: { label: "View logs", command: "agenthub server logs <id> --follow" },
  [HealthAction.Restart]: {
    label: "Restart",
    command: "agenthub daemon restart",
    note: "restarts the hub; there is no per-server restart endpoint",
  },
};

/** Actions that are just another page of this window. */
const ROUTE_ACTIONS: Record<string, { label: string; route: string }> = {
  [HealthAction.SetSecret]: { label: "Set secret", route: "#/secrets" },
};

function suggestion(server: Server): Node | null {
  const action = server.health.action ?? HealthAction.None;
  if (action === HealthAction.None || action === HealthAction.Enable) return null;
  const route = ROUTE_ACTIONS[action];
  if (route) return el("a", { class: "btn btn-secondary", href: route.route, text: route.label });
  const spec = CLI_ACTIONS[action];
  if (!spec) return el("span", { class: "meta", text: action });
  const command = spec.command.replace("<id>", server.id);
  return el("div", { class: "action" }, [cliHint(command, spec.note ? { note: spec.note } : {})]);
}

// ---------------------------------------------------------------------------
// Buckets
// ---------------------------------------------------------------------------

/** Which of the three groups a row belongs to. */
type Bucket = "attention" | "active" | "disabled";

/**
 * Bucketing is derived from the Health contract alone, never from the raw
 * connection state: "disabled" reports level=healthy on purpose, and
 * re-deriving severity here is exactly the frontend-invented status
 * docs/modules/controlplane.md forbids.
 */
function bucketOf(s: Server): Bucket {
  if (s.health.admin_state === AdminState.Disabled) return "disabled";
  return s.health.level === HealthLevel.Healthy ? "active" : "attention";
}

/** Severity rank inside the attention bucket: the unusable before the merely
 *  degraded, so the first row is always the most expensive one to ignore. */
function severityRank(s: Server): number {
  if (s.health.level === HealthLevel.Unhealthy) return 0;
  if (s.health.level === HealthLevel.Degraded) return 1;
  return 2; // quarantined (level=healthy by contract)
}

/** Connection rank inside the active bucket: what is actually serving tools
 *  first, what nobody is watching last. */
function activeRank(s: Server): number {
  if (s.state === "connected") return 0;
  if (s.state === "connecting") return 1;
  return 2;
}

const BUCKET_TITLES: Record<Bucket, string> = {
  attention: "Needs attention",
  active: "Active",
  disabled: "Disabled",
};

/** localStorage key for one bucket's collapse state. */
const collapseKey = (b: Bucket): string => `agenthub.servers.bucket.${b}.collapsed`;

/** Disabled is collapsed by default: it is the bucket the operator already
 *  decided about. The other two open. */
function collapsedByDefault(b: Bucket): boolean {
  return b === "disabled";
}

function isCollapsed(b: Bucket): boolean {
  try {
    const v = localStorage.getItem(collapseKey(b));
    if (v === "1") return true;
    if (v === "0") return false;
  } catch {
    // Storage unavailable: fall through to the default.
  }
  return collapsedByDefault(b);
}

function setCollapsed(b: Bucket, collapsed: boolean): void {
  try {
    localStorage.setItem(collapseKey(b), collapsed ? "1" : "0");
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

/** The empty definition a new server starts from. Enabled and stdio are the
 *  defaults because they are what the registry's zero value already means. */
function blankEntry(): ServerEntry {
  return {
    transport: Transport.Stdio,
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

/** The whole invocation on one line: the thing a user recognises a pasted
 *  server by. Truncating it would hide exactly the argument worth reading. */
function parsedCommandLine(entry: Partial<ServerEntry>): string {
  if (entry.url) return entry.url;
  return [entry.command ?? "", ...(entry.args ?? [])].filter(Boolean).join(" ");
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
  if (!entry.transport) entry.transport = Transport.Stdio;
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
  const slot = noticeSlot();
  const form = modalHost();

  /** When each currently-connecting server was first seen connecting. Reset
   *  whenever it leaves that state, so a reconnect starts the clock again. */
  const connectingSince = new Map<string, number>();
  /** id -> spawn command, fetched lazily and ONLY for connecting stdio
   *  servers. The dashboard payload does not carry the command and there is
   *  no reason to pull every definition to label one wait. */
  const commands = new Map<string, string>();

  function noteConnecting(servers: Server[]): void {
    const now = Date.now();
    const live = new Set<string>();
    for (const s of servers) {
      if (s.state !== "connecting") continue;
      live.add(s.id);
      if (!connectingSince.has(s.id)) connectingSince.set(s.id, now);
      if (!commands.has(s.id)) {
        commands.set(s.id, ""); // claim the slot so one fetch runs per id
        hub
          .getServer(s.id)
          .then((d) => commands.set(s.id, d.entry.command ?? ""))
          .catch(() => {
            // Not knowing the command only costs the "Installing…" label.
          });
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

    /** The equivalent command, on every state of this dialog — the same rule
     *  every other action on this page follows. */
    const equivalent = (): HTMLElement => cliHint(`agenthub auth login ${shellArg(id)}`);

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

    show(el("p", { class: "muted", text: `Contacting ${id}…` }), equivalent());

    try {
      const started = await hub.startLogin(id);
      session = started.id;
    } catch (err) {
      show(failureBox(err), equivalent());
      return;
    }

    for (;;) {
      if (stopped) return;
      let st: AuthLogin;
      try {
        st = await hub.loginStatus(session);
      } catch (err) {
        show(failureBox(err), equivalent());
        return;
      }
      if (stopped) return;

      if (st.phase === LoginPhase.Complete) {
        finished = true;
        close();
        await draw();
        slot.say(
          `Signed in to ${id}.` +
            (st.has_refresh_token
              ? " agenthub will renew it on its own from here."
              : " The provider issued no refresh token, so this will need signing in again when it expires."),
        );
        return;
      }

      if (st.phase === LoginPhase.Failed) {
        finished = true;
        show(
          el("div", { class: "error" }, [
            el("strong", { text: st.error || "The sign-in did not complete." }),
            st.hint ? el("span", { class: "hint", text: st.hint }) : null,
          ]),
          el("p", {
            class: "hint",
            text: "Nothing was stored. The server is unchanged and can be signed in to again.",
          }),
          controls(
            button("Try again", "btn btn-primary", () => {
              close();
              void login(id);
            }),
            button("Close", "btn btn-secondary", () => close()),
          ),
          equivalent(),
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
          equivalent(),
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
          equivalent(),
        );
      } else {
        show(
          el("p", { class: "muted", text: "Working out how this provider signs you in…" }),
          equivalent(),
        );
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

    // disabled: a grey dot and nothing else. There is no status to report
    // about a server that was switched off on purpose, and writing one makes
    // the six disabled rows compete with the one broken row.
    if (admin === AdminState.Disabled) {
      return el("div", { class: "srv-status" }, [
        el("div", { class: "state-line" }, [dot("neutral")]),
      ]);
    }

    // needs-auth: the status position BECOMES the action, and the action now
    // signs the user in rather than handing them a command to go and type.
    if (s.health.action === HealthAction.Login) {
      const authenticate = button("Authenticate", "btn btn-primary", () => void login(s.id));
      return el("div", { class: "srv-status" }, [
        el("div", { class: "state-line" }, [dot("warning"), authenticate]),
        s.health.summary
          ? el("span", { class: "meta", text: s.health.summary })
          : null,
      ]);
    }

    // checking: a wait re-told as progress once it is long enough to need one.
    if (s.state === "connecting") {
      return el("div", { class: "srv-status" }, [
        el("div", { class: "state-line" }, [
          dot("warning", "pulse"),
          el("span", {
            class: "state-text t-warning state-checking",
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

    // connected: the TOOL COUNT, not the word "connected". "connected" is
    // already implied by the bucket the row is in and by the green dot; the
    // number is the only thing in this position that answers a question the
    // operator actually has.
    if (s.state === "connected") {
      return el("div", { class: "srv-status" }, [
        el("div", { class: "state-line" }, [
          dot("success"),
          el("span", {
            class: "state-text t-success",
            text: s.tools === 1 ? "1 tool" : `${s.tools} tools`,
          }),
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

  /** Full diagnostics belong to the flexible body column, never the compact
   *  status/action column. Keeping this collapsed preserves scan density for
   *  the common case while making the daemon's exact answer one click away. */
  function statusDetails(s: Server): Node | null {
    if (s.health.admin_state === AdminState.Disabled || s.state === "connecting") return null;
    const detail = s.health.detail?.trim() ?? "";
    // Login already turns the status itself into the Authenticate action; a
    // second CLI login instruction underneath would be a duplicate path.
    const action = s.health.action === HealthAction.Login ? null : suggestion(s);
    // Healthy-but-not-observed servers can carry explanatory daemon detail,
    // but repeating a disclosure on every normal row defeats the purpose of
    // keeping this list scan-friendly. The compact neutral status is enough.
    if (s.health.level === HealthLevel.Healthy && action === null) return null;
    if (!detail && action === null) return null;

    const details = el("details", { class: "server-health-detail" });
    details.append(
      el("summary", { text: "Connection details" }),
      el("div", { class: "server-health-detail-body" }, [
        detail ? el("p", { class: "meta", text: detail, title: detail }) : null,
        action,
      ]),
    );
    return details;
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

  function editor(id: string, detail: ServerDetail | null): Node {
    const creating = detail === null;
    const idInput = textInput(id, "server id");
    idInput.disabled = !creating;
    const fields = entryForm(detail ? detail.entry : blankEntry());
    const errors = el("div", { class: "notice-slot" });

    // The equivalent command, kept in step with the form as it is typed.
    const cliSlot = el("div", { class: "cli-list" });
    const refreshCli = (): void => {
      clear(cliSlot);
      const collected = fields.collect();
      const entry = collected.ok ? collected.entry : blankEntry();
      const name = idInput.value.trim() || "<id>";
      cliSlot.append(
        cliHint(creating ? cliAdd(name, entry) : cliUpdate(name, entry), {
          note: creating
            ? "same definition, from a terminal"
            : "there is no `server edit`: an update is a remove plus an add",
        }),
      );
      if (!collected.ok) {
        cliSlot.append(el("span", { class: "hint", text: `incomplete: ${collected.message}` }));
      }
    };
    fields.node.addEventListener("input", refreshCli);
    fields.node.addEventListener("change", refreshCli);
    idInput.addEventListener("input", refreshCli);

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
      ).then((ok) => {
        if (ok) form.hide();
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
      cliSlot,
      controls(save, button("Cancel", "btn btn-secondary", () => form.hide())),
    ]);
    refreshCli();
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
      cli: cliRemove(s.id),
      perform: async () => {
        // The generation is re-read INSIDE the attempt, so a 409 caused by
        // an edit elsewhere is fixed simply by pressing Remove again.
        const detail = await hub.getServer(s.id);
        await hub.deleteServer(s.id, detail.generation);
      },
    });
    if (!ok) return;
    await draw();
    slot.say(`${s.id} removed.`);
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
    await draw();
    slot.say(`${s.id} ${next ? "enabled" : "disabled"}.`);
  }

  async function test(s: Server): Promise<void> {
    const body = el("div", {}, [el("p", { class: "muted", text: "Connecting…" })]);
    openModal(`Test ${s.id}`, [body]);
    try {
      // Handshake only: naming a tool here would CALL it, and a dashboard
      // button must not have side effects on a downstream system.
      const res = await hub.testServer(s.id, {});
      clear(body);
      body.append(
        el("p", {
          class: "hint",
          text:
            "A real connection and a real handshake. This is also how a credential is verified — " +
            "agenthub sends secrets, it never reads them back.",
        }),
        testResultView(res),
        cliHint(cliTest(s.id)),
      );
    } catch (err) {
      clear(body);
      body.append(failureBox(err), cliHint(cliTest(s.id)));
    }
  }

  // -- rendering -------------------------------------------------------------

  /** The spine's colour channel: health, with admin state winning — a server
   *  switched off on purpose is not broken. */
  function spineTone(s: Server): string {
    const admin = s.health.admin_state;
    if (admin === AdminState.Disabled) return "off";
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
    menu.addEventListener("keydown", (ev) => {
      if (ev.key !== "Escape") return;
      ev.preventDefault();
      menu.open = false;
      summary.focus();
    });
    menu.addEventListener("toggle", () => {
      menu.closest(".rec")?.classList.toggle("menu-open", menu.open);
      if (!menu.open) return;
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
      el("div", { class: "server-row-menu-popover", role: "menu" }, [removeButton]),
    );
    return menu;
  }

  function row(s: Server): HTMLElement {
    const toggleLabel = s.enabled ? "Disable" : "Enable";
    // The spine's second channel: dashed means this definition is reached
    // over the network rather than run here. Same convention as the Catalog
    // ledger, because it is the same distinction.
    const remote = s.transport === Transport.HTTP || s.transport === Transport.SSE;
    const overview = el("button", {
      class: "rec-overview",
      type: "button",
      "aria-label": `Edit ${s.id}`,
      title: `Edit ${s.id}`,
    }, [
      el("span", { class: "rec-title" }, [
        el("span", { class: "rec-name", text: s.id }),
        // Metadata is NEUTRAL: a green "stdio" next to a green health dot
        // would put two unrelated greens on one row (see style.css).
        el("span", { class: "id-chip", text: s.transport || "stdio" }),
      ]),
      el("span", { class: "rec-overview-cue", text: "Edit" }),
    ]) as HTMLButtonElement;
    overview.addEventListener("click", () => void openEditor(s.id));

    return el("div", { class: "rec has-lead" }, [
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
      el("div", { class: "rec-body" }, [
        overview,
        statusDetails(s),
        cliBlock(
          [
            { label: toggleLabel, command: s.enabled ? cliDisable(s.id) : cliEnable(s.id) },
            { label: "Test", command: cliTest(s.id) },
            { label: "Remove", command: cliRemove(s.id) },
            {
              label: "Edit",
              command: `agenthub server inspect ${shellArg(s.id)}`,
              note: "read the definition; writing it back is rm + add (see the editor)",
            },
          ],
          "⌘ CLI",
        ),
      ]),
      el("div", { class: "rec-act" }, [
        statusCell(s),
        controls(
          button("Test", "btn btn-sm", () => void test(s)),
          rowMenu(s),
        ),
      ]),
    ]);
  }

  function bucketNode(b: Bucket, servers: Server[]): HTMLElement {
    const details = el("details", {
      class: b === "attention" ? "bucket b-attention" : "bucket",
    }) as HTMLDetailsElement;
    details.open = !isCollapsed(b);
    // Setting `open` above queues a toggle event of its own; ignore that one
    // so a redraw never rewrites the stored preference with what it just
    // read. Only a real click is a choice.
    let settled = false;
    details.addEventListener("toggle", () => {
      if (!settled) {
        settled = true;
        return;
      }
      setCollapsed(b, !details.open);
    });
    if (!details.open) settled = true;
    details.append(
      el("summary", {}, [
        el("span", { text: BUCKET_TITLES[b] }),
        el("span", { class: "bucket-count", text: String(servers.length) }),
      ]),
      el("div", { class: "bucket-body ledger" }, servers.map(row)),
    );
    return details;
  }

  function paint(servers: Server[]): void {
    if (!listRoot) return;
    clear(listRoot);

    const needle = filter.trim().toLowerCase();
    const shown = needle
      ? servers.filter((s) => s.id.toLowerCase().includes(needle))
      : servers;

    // Chips describe the WHOLE fleet, not the filtered view: a summary that
    // silently means "of what you can currently see" is the same lie as a
    // global action under a filter.
    const connected = servers.filter((s) => s.state === "connected").length;
    const attention = servers.filter((s) => bucketOf(s) === "attention").length;
    const disabled = servers.filter((s) => bucketOf(s) === "disabled").length;
    const chips = chipRow(
      chip(servers.length, servers.length === 1 ? "server" : "servers"),
      chip(connected, "connected", "success"),
      chip(attention, attention === 1 ? "needs attention" : "need attention", "warning"),
      chip(disabled, "disabled"),
    );
    if (chips) listRoot.append(chips);

    if (servers.length === 0) {
      listRoot.append(
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
      return;
    }
    if (shown.length === 0) {
      listRoot.append(
        emptyState({
          kind: "empty",
          title: `No server id contains “${filter.trim()}”.`,
          body: `All ${servers.length} configured servers are still there — this view is filtered.`,
          actions: [
            button("Clear filter", "btn", () => {
              filter = "";
              if (search) search.value = "";
              void draw();
            }),
          ],
        }),
      );
      return;
    }

    const buckets: Record<Bucket, Server[]> = { attention: [], active: [], disabled: [] };
    for (const s of shown) buckets[bucketOf(s)].push(s);
    buckets.attention.sort((a, b) => severityRank(a) - severityRank(b) || a.id.localeCompare(b.id));
    buckets.active.sort((a, b) => activeRank(a) - activeRank(b) || a.id.localeCompare(b.id));
    buckets.disabled.sort((a, b) => a.id.localeCompare(b.id));

    // An empty bucket is not rendered at all. A permanent "Needs attention
    // (0)" header trains the eye to skip the exact region that matters on the
    // one day it is not zero.
    for (const b of ["attention", "active", "disabled"] as Bucket[]) {
      if (buckets[b].length > 0) listRoot.append(bucketNode(b, buckets[b]));
    }
  }

  async function draw(): Promise<void> {
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
      root.append(
        pageHeader(
          "Servers",
          "Configure every downstream MCP process and endpoint, then act on the ones that need attention.",
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
            text: "Health comes from gateways that are actually using each server.",
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
    noteConnecting(servers);
    paint(servers);
    tick();
  }

  return {
    render(node) {
      root = node;
      // The `servers` SSE payload is byte-identical to the list call, so a
      // notification is simply a cue to re-read: one code path, one shape.
      // Our own writes come back the same way, so "someone else changed it"
      // and "I changed it" look identical on screen.
      off = on<TopicEvent>(EVT.servers, () => void draw());
      ticker = window.setInterval(tick, 1000);
      return draw();
    },
    dispose() {
      off?.();
      off = null;
      if (ticker !== undefined) window.clearInterval(ticker);
      ticker = undefined;
      connectingSince.clear();
      commands.clear();
      root = null;
      listRoot = null;
      search = null;
      form.hide();
    },
  };
}
