// Bridge to the Go side: one typed wrapper per bound HubService method plus
// the event subscription helper.
//
// Methods are called BY NAME rather than through generated bindings. The
// name is the Go fully-qualified method name (package path + type + method),
// which is what application.NewService binds — see cmd/agenthub-gui/services.
// Consequence: renaming a Go method without renaming it here fails at
// runtime with a ReferenceError, so the names live in exactly one place.
//
// Every configuration and runtime method here maps to exactly one
// control-plane call. There is no method that composes two writes, because a
// composite the CLI cannot perform would be a GUI privilege — and "the GUI
// is one control-plane client among several" is the property this file exists
// to keep true. ApplicationVersion is the sole local value: immutable build
// identity supplied by the GUI process itself.
//
// PRECONDITIONS. Writes against the registry take the generation the caller
// last read (api/write.go): 0 means "do not check", a non-zero value is
// compared under the registry lock and a mismatch writes NOTHING and answers
// 409. Pages read the generation from the same call that gave them the data
// (ServerDetail, ProfileList, GovernanceList, ...) and hand it straight back.

import { Browser, Call, Clipboard, Events } from "@wailsio/runtime";
import type {
  ProcLogPage,
  CallDetail,
  AuditCalls,
  AuditKeyRotation,
  AuditPrune,
  CallsStats,
  CallsStatus,
  AuditVerify,
  AuthLoggedOut,
  AuthLogin,
  AuthRefreshed,
  AuthStatus,
  CallError,
  CatalogAddRequest,
  CatalogAdded,
  CatalogList,
  ClientBinding,
  ClientConnectRequest,
  ClientConnection,
  ClientDetectResult,
  ClientDisconnected,
  ClientInspection,
  ConfigWrite,
  EventLog,
  GovernanceList,
  ParsedClientConfig,
  ProfileList,
  ProfileTools,
  ProfileWrite,
  ScopeDetail,
  ScopeWrite,
  SecretChange,
  SecretRef,
  Server,
  ServerDetail,
  ServerSetEdit,
  ServerSpec,
  ServerTestRequest,
  ServerTestResult,
  ServerWrite,
  SessionInfo,
  Skill,
  SkillInstall,
  SkillInstallRequest,
  Status,
  Token,
  TokenCreated,
  TokenRevoked,
  TokenSpec,
  WindowPrefs,
} from "./types";
import { ErrCode, ErrorKindConflict } from "./types";

const SERVICE = "github.com/dinstein/agent-hub/cmd/agenthub-gui/services.HubService";

function call<T>(method: string, ...args: unknown[]): Promise<T> {
  return Call.ByName(`${SERVICE}.${method}`, ...args) as unknown as Promise<T>;
}

/**
 * A call that can be abandoned while it is still in flight.
 *
 * Wails' binding calls are already cancellable, and the cancellation is REAL
 * rather than cosmetic: it cancels the bound method's context, which cancels
 * the control-plane HTTP request, which cancels the daemon handler's request
 * context — and that is the context the self-test hands to the downstream
 * call. So a cancel here propagates all the way down.
 *
 * What it still cannot promise is that nothing happened: a tool that had
 * already begun its work downstream is not un-called by anyone hanging up.
 * Every surface that offers cancellation has to say that out loud rather than
 * implying an undo (see CANCEL_CAVEAT).
 */
export interface Cancellable<T> extends Promise<T> {
  cancel(cause?: unknown): void;
}

function cancellableCall<T>(method: string, ...args: unknown[]): Cancellable<T> {
  return Call.ByName(`${SERVICE}.${method}`, ...args) as unknown as Cancellable<T>;
}

/** What a cancelled call is allowed to claim, in the one wording every
 *  surface uses. Cancelling stops the WAIT; it does not undo the work. */
export const CANCEL_CAVEAT =
  "agenthub stopped waiting and asked the daemon to abandon the call. A tool that had already " +
  "started downstream may still finish — hanging up is not an undo.";

/** True when a promise rejected because it was cancelled, rather than because
 *  anything failed. Wails names the rejection reason "CancelError". */
export function isCancelled(err: unknown): boolean {
  return err instanceof Error && err.name === "CancelError";
}

export const hub = {
  /** GUI build identity stamped from VERSION plus the source commit. */
  applicationVersion: () => call<string>("ApplicationVersion"),
  /** Last known daemon connection state; does no I/O. */
  status: () => call<Status>("Status"),
  /** Force a connection attempt, starting the daemon if necessary. */
  connect: () => call<Status>("Connect"),

  // -- this window ----------------------------------------------------------
  // The only bound calls with no control-plane endpoint behind them, and not
  // a GUI privilege for it: a CLI is not missing anything by being unable to
  // hide a window.
  /** Whether a tray icon actually came up. False means the close button
   *  quits, whatever the preference says. */
  trayAvailable: () => call<boolean>("TrayAvailable"),
  /** Whether THIS process started the daemon — i.e. whether quitting stops
   *  the hub with it. */
  ownsDaemon: () => call<boolean>("OwnsDaemon"),
  /** Hands the Go side the preferences it acts on when the close button is
   *  pressed. Storing them is this window's job (window-prefs.ts). */
  setWindowPreferences: (prefs: WindowPrefs) => call<WindowPrefs>("SetWindowPreferences", prefs),
  /** Minimises to the tray. */
  hideWindow: () => call<void>("HideWindow"),
  /** Ends the application through the shutdown path that also stops a daemon
   *  this process started. */
  quitApplication: () => call<void>("QuitApplication"),

  // -- encrypted access ledger ---------------------------------------------
  callsStatus: () => call<CallsStatus>("CallsStatus"),
  callList: (
    sinceMillis: number,
    limit: number,
    cursor = "",
    client = "",
    server = "",
    tool = "",
    outcome = "",
  ) => call<AuditCalls>("CallList", sinceMillis, limit, cursor, "", client, server, tool, outcome),
  /** Selecting a row is the disclosure action: payload previews are returned
   *  immediately, with no second decrypt control. */
  callDetail: (id: string) => call<CallDetail>("CallDetail", id),
  callsStats: (sinceMillis: number) => call<CallsStats>("CallsStats", sinceMillis),
  setAuditEnabled: (enabled: boolean, generation: number) =>
    call<CallsStatus>("SetCallsEnabled", enabled, generation),
  rotateAuditKey: (generation: number) =>
    call<AuditKeyRotation>("RotateCallsKey", generation),
  verifyAudit: () => call<AuditVerify>("VerifyCalls"),
  pruneAudit: (dryRun: boolean) => call<AuditPrune>("PruneCalls", dryRun),

  // -- control-plane event log ---------------------------------------------
  /** What HAPPENED, in a closed vocabulary — as opposed to EVT.servers, which
   *  only says that something did. Empty selectors mean "no rule". */
  eventLog: (
    sinceMillis: number,
    limit: number,
    scope = "",
    server = "",
    client = "",
    /** "routine" | "disruption" | "" for both. */
    cls = "",
    kinds: string[] = [],
    cursor = "",
  ) => call<EventLog>("EventLog", sinceMillis, limit, scope, server, client, cls, kinds, cursor),

  // -- process logs ---------------------------------------------------------
  /** What the daemon and the gateways DID, merged and newest first — the
   *  prose beside EventLog's closed vocabulary. Empty selectors mean "no
   *  rule"; cursor resumes a page. */
  procLogs: (
    sinceMillis: number,
    limit: number,
    source = "",
    level = "",
    client = "",
    server = "",
    cursor = "",
  ) => call<ProcLogPage>("ProcLogs", sinceMillis, limit, source, level, client, server, cursor),

  // -- servers --------------------------------------------------------------
  listServers: () => call<Server[]>("ListServers"),
  /** Stored definition + the generation it was read at. */
  getServer: (id: string) => call<ServerDetail>("GetServer", id),
  /** Refuses to overwrite: an existing id is a name conflict, never a silent
   *  replacement. */
  createServer: (spec: ServerSpec, generation: number) =>
    call<ServerWrite>("CreateServer", spec, generation),
  /** WHOLESALE replacement — every key of the entry is sent, so no stored
   *  field survives unmentioned. */
  updateServer: (spec: ServerSpec, generation: number) =>
    call<ServerWrite>("UpdateServer", spec, generation),
  deleteServer: (id: string, generation: number) =>
    call<ServerWrite>("DeleteServer", id, generation),
  setServerEnabled: (id: string, enabled: boolean, generation: number) =>
    call<ServerWrite>("SetServerEnabled", id, enabled, generation),
  /** Real connection, real handshake: the only way this UI ever verifies a
   *  credential (it cannot read one back). It changes no configuration, so it
   *  carries no precondition.
   *
   *  This is the ONE call in this file that is returned as a Cancellable: it
   *  is also the only one that can legitimately take minutes (a cold npx
   *  cache, a slow tool), so it is the only one a user ever needs to abandon.
   *  Cancelling still does not undo a call that already ran downstream —
   *  see CANCEL_CAVEAT. */
  testServer: (id: string, req: ServerTestRequest) =>
    cancellableCall<ServerTestResult>("TestServer", id, req),

  // -- the curated catalog, and reading someone else's configuration --------
  /** Entries matching `query`, best match first; "" is the whole directory.
   *  The answer echoes the query that produced it, so a search box can drop
   *  a response that arrived after the user moved on. */
  searchCatalog: (query: string) => call<CatalogList>("SearchCatalog", query),
  /** Stores a catalog entry as a server definition. An ORDINARY registry
   *  write: same validation, same 409 on an id already taken, same
   *  precondition. Being curated buys the entry no shortcut through the
   *  rules — it only saves the typing. */
  addFromCatalog: (id: string, req: CatalogAddRequest, generation: number) =>
    call<CatalogAdded>("AddFromCatalog", id, req, generation),
  /** Parses pasted client-configuration text into a PREVIEW. Nothing is
   *  written and nothing is validated yet: the entries the user keeps are
   *  stored afterwards with createServer, which is where a bad one is
   *  refused. */
  parseClientConfig: (text: string) => call<ParsedClientConfig>("ParseClientConfig", text),

  // -- profiles -------------------------------------------------------------
  listProfiles: () => call<ProfileList>("ListProfiles"),
  /** `servers`: null = every registered server, [] = block-all. The two are
   *  never collapsed. */
  createProfile: (name: string, servers: string[] | null, generation: number) =>
    call<ProfileWrite>("CreateProfile", name, servers, generation),
  /** A rename repoints every client reference: it is an operation, not a
   *  delete-then-create. */
  renameProfile: (name: string, newName: string, generation: number) =>
    call<ProfileWrite>("RenameProfile", name, newName, generation),
  deleteProfile: (name: string, generation: number) =>
    call<ProfileWrite>("DeleteProfile", name, generation),
  setProfileServers: (name: string, edit: ServerSetEdit, generation: number) =>
    call<ProfileWrite>("SetProfileServers", name, edit, generation),
  setProfileTools: (name: string, server: string, sel: ProfileTools, generation: number) =>
    call<ProfileWrite>("SetProfileTools", name, server, sel, generation),
  setActiveProfile: (name: string, generation: number) =>
    call<ProfileWrite>("SetActiveProfile", name, generation),
  clearActiveProfile: (name: string, generation: number) =>
    call<ProfileWrite>("ClearActiveProfile", name, generation),

  // -- client scope binding -------------------------------------------------
  getScope: (client: string) => call<ScopeDetail>("GetScope", client),
  setScope: (client: string, binding: ClientBinding, generation: number) =>
    call<ScopeWrite>("SetScope", client, binding, generation),
  clearScope: (client: string, generation: number) =>
    call<ScopeWrite>("ClearScope", client, generation),

  // -- governance -----------------------------------------------------------
  listConfig: () => call<GovernanceList>("ConfigKeys"),
  setConfig: (key: string, value: string, generation: number) =>
    call<ConfigWrite>("SetConfig", key, value, generation),

  // -- credential vault (names only, never values) --------------------------
  listSecrets: (server: string) => call<SecretRef[]>("ListSecrets", server),
  /** The plaintext travels as an argument and lives only inside this call:
   *  nothing it returns can carry a value back. */
  setSecret: (server: string, scope: string, key: string, value: string) =>
    call<SecretChange>("SetSecret", server, scope, key, value),
  deleteSecret: (server: string, scope: string, key: string) =>
    call<SecretChange>("DeleteSecret", server, scope, key),

  // -- skills ---------------------------------------------------------------
  listSkills: () => call<Skill[]>("ListSkills"),
  setSkillEnabled: (id: string, enabled: boolean) =>
    call<Skill>("SetSkillEnabled", id, enabled),
  installSkill: (id: string, req: SkillInstallRequest) =>
    call<SkillInstall>("InstallSkill", id, req),

  // -- agent tokens ---------------------------------------------------------
  listTokens: () => call<Token[]>("ListTokens"),
  /** The response is the ONLY place the plaintext ever appears. */
  createToken: (spec: TokenSpec) => call<TokenCreated>("CreateToken", spec),
  revokeToken: (name: string) => call<TokenRevoked>("RevokeToken", name),

  // -- client wiring --------------------------------------------------------
  detectClients: () => call<ClientDetectResult>("DetectClients"),
  /** Opens one named client's configuration. This may trigger a host privacy
   *  prompt, so pages call it only from an explicit check action. */
  inspectClient: (client: string) => call<ClientInspection>("InspectClient", client),
  connectClient: (client: string, req: ClientConnectRequest) =>
    call<ClientConnection>("ConnectClient", client, req),
  disconnectClient: (client: string) => call<ClientDisconnected>("DisconnectClient", client),

  // -- OAuth credentials ----------------------------------------------------
  authStatus: (server: string) => call<AuthStatus[]>("AuthStatus", server),
  refreshAuth: (server: string) => call<AuthRefreshed>("AuthRefresh", server),
  /** Removes the credential FROM THIS MACHINE; it does not revoke it at the
   *  provider — agenthub cannot promise that. */
  logoutAuth: (server: string) => call<AuthLoggedOut>("AuthLogout", server),
  /** Starts an interactive login. It returns BEFORE there is anything to show
   *  — choosing between the device and loopback flows needs the authorization
   *  server's metadata — so the caller polls `loginStatus` until the session
   *  is actionable, then opens whatever it carries.
   *
   *  Starting one for a server that already has a login running returns THAT
   *  session, so a double-clicked button cannot open two browser dances. */
  startLogin: (server: string) => call<AuthLogin>("AuthLoginStart", server),
  loginStatus: (id: string) => call<AuthLogin>("AuthLogin", id),
  /** Stops the WAIT, not the authorization: a consent already granted at the
   *  provider stays granted, and a login that had already stored a credential
   *  keeps it. */
  cancelLogin: (id: string) => call<AuthLogin>("AuthLoginCancel", id),

  // -- sessions ---------------------------------------------------------------
  listSessions: () => call<SessionInfo[]>("ListSessions"),
  /** Narrow-only: the daemon rejects anything that would widen scope. */
};

/** Tool names of one server, for the three-state selector.
 *
 *  The daemon no longer serves a tool listing of its own: what a tool IS
 *  belongs to the server, and what is offered is decided in the registry. The
 *  selector therefore offers free text and this returns nothing — a form that
 *  cannot enumerate is honest, one that enumerates from a stale cache is not.
 */
export async function knownTools(_server: string): Promise<string[]> {
  return [];
}

/**
 * Normalises a rejected binding call into the structured error the Go side
 * produced (services.MarshalError puts it on `cause`). Anything else — a
 * ReferenceError for an unknown method, a runtime failure — degrades to a
 * message with the E_GUI code, never to a silent success.
 */
export function asCallError(err: unknown): CallError {
  const cause = (err as { cause?: unknown } | undefined)?.cause;
  if (cause && typeof cause === "object" && typeof (cause as CallError).code === "string") {
    return cause as CallError;
  }
  const message = err instanceof Error ? err.message : String(err);
  return { code: ErrCode.Gui, message };
}

/** True when the failure means "this daemon does not serve that endpoint". */
export function isUnavailable(err: CallError): boolean {
  return err.code === ErrCode.NotFound;
}

/** True when the daemon could not be reached at all. */
export function isOffline(err: CallError): boolean {
  return err.code === ErrCode.Offline || err.offline === true;
}

/**
 * True when a write lost the optimistic-concurrency check: the registry moved
 * between the read and the write and NOTHING was written.
 *
 * It tests the `kind` the Go side stamps (services.ErrorKindConflict), not
 * the status and not "some 409". The daemon also answers 409 for a name
 * already taken, for a login already in flight and for a skills target that
 * drifted; re-reading and retrying fixes none of those, and treating them
 * as "your view was stale" would send the page into a loop that can never
 * succeed (api/write.go, asConflict). The code is accepted as an equivalent
 * spelling for a services build that predates the kind field.
 */
export function isStalePrecondition(err: CallError): boolean {
  return err.kind === ErrorKindConflict || err.code === ErrCode.StalePrecondition;
}

/** Subscribe to a bridged daemon event; returns the unsubscribe function. */
export function on<T>(name: string, cb: (data: T) => void): () => void {
  return Events.On(name, (ev: { data: unknown }) => {
    // Wails delivers Emit's variadic data as an array when there are several
    // values; the Go side always emits exactly one.
    const data = Array.isArray(ev.data) ? ev.data[0] : ev.data;
    cb(data as T);
  });
}

/**
 * Opens a URL in the user's real browser.
 *
 * This is the frontend's half of an interactive login: the daemon returns the
 * authorization URL rather than visiting it, because it may be headless and
 * may not be where the user is sitting — this window is.
 *
 * It goes through the HOST browser, never `window.open` and never a
 * navigation: an authorization page rendered inside the application's own
 * webview would be agenthub asking for the user's provider password in a
 * window agenthub controls, which is the exact shape of a credential-phishing
 * screen and defeats every visual check the user has (the address bar, the
 * lock, the password manager's origin match).
 *
 * Failure is reported, never swallowed. A login whose browser never opened
 * looks identical to one the user has not got round to approving yet, and the
 * caller has a deadline counting down on screen either way.
 */
export async function openExternal(url: string): Promise<void> {
  await Browser.OpenURL(url);
}

/** Copies text through the host clipboard, falling back to the DOM API when
 *  the page runs outside the desktop app (vite dev server). */
export async function copyText(text: string): Promise<void> {
  try {
    await Clipboard.SetText(text);
  } catch {
    await navigator.clipboard?.writeText(text);
  }
}

/** Event names emitted by the Go side (services.Event*). */
export const EVT = {
  daemon: "agenthub:daemon",
  servers: "agenthub:servers",
  sessions: "agenthub:sessions",
  skills: "agenthub:skills",
  /** The tray changed a window preference; this window stores it. */
  windowPrefs: "agenthub:window-prefs",
  /** The tray asked for a page. */
  navigate: "agenthub:navigate",
  /** The close button was pressed for the first time. */
  confirmClose: "agenthub:confirm-close",
} as const;
