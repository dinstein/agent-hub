// Wire types mirroring the Go package github.com/dinstein/agent-hub/api.
//
// Only the Health constants are generated (src/generated/health.ts): those
// are matched values and drift there is silent, while a missing field here
// shows up the moment a page renders. Keep the field names identical to the
// JSON tags on the Go side.
//
// THREE-STATE FIELDS. Several fields here are three-state and their empty
// case is the DANGEROUS one: `allow` absent means "every tool", `allow: []`
// means "no tool at all". TypeScript's optional field models exactly that
// distinction (undefined vs []), so every such field is declared optional and
// no page may normalise one into the other — collapsing them is how block-all
// turns into allow-all (api/profiles.go, ToolSelector).

import type { HealthLevel, AdminState, HealthAction } from "./generated/health";

export interface Health {
  level: HealthLevel;
  admin_state: AdminState;
  summary: string;
  detail?: string;
  action?: HealthAction;
}

/** One row of the servers dashboard: runtime state plus the Health contract. */
export interface Server {
  id: string;
  transport: string;
  enabled: boolean;
  state: string;
  tools: number;
  source: string;
  health: Health;
}

// ---------------------------------------------------------------------------
// Server definitions (the registry document shape, camelCase like the Go side)
// ---------------------------------------------------------------------------

/** api.Transport* */
export const Transport = {
  Stdio: "stdio",
  HTTP: "http",
  SSE: "sse",
} as const;
export type Transport = (typeof Transport)[keyof typeof Transport];

/** api.Runtime* */
export const Runtime = { Host: "host", Docker: "docker" } as const;
export type Runtime = (typeof Runtime)[keyof typeof Runtime];

/** api.Provenance* */
export const Provenance = { Remote: "remote", Local: "local" } as const;
export type Provenance = (typeof Provenance)[keyof typeof Provenance];

export interface DockerMount {
  source: string;
  target?: string;
  /** Read-only is the zero value: a form that never thinks about write
   *  access lands on the safe side. */
  write?: boolean;
}

export interface DockerRuntime {
  image: string;
  network?: string;
  mounts?: DockerMount[];
  memory?: string;
  cpus?: string;
  user?: string;
  workdir?: string;
  extraArgs?: string[];
}

export interface OAuthHint {
  issuer?: string;
  scopes?: string[];
  resourceMetadataUrl?: string;
}

/**
 * One server's stored definition.
 *
 * Credentials red line: env and header VALUES are stored verbatim and may
 * hold `${SECRET_X}` placeholders. A frontend must never resolve one before
 * sending — a registry document must not hold a credential.
 */
export interface ServerEntry {
  transport: string;
  command: string;
  args: string[] | null;
  env: Record<string, string> | null;
  cwd: string;
  url: string;
  headers: Record<string, string> | null;
  oauth: OAuthHint | null;
  provenance: string;
  derive: string;
  runtime: string;
  docker: DockerRuntime | null;
  enabled: boolean;
  source: string;
}

export interface ServerSpec {
  id: string;
  entry: ServerEntry;
}

/** Stored definition plus the generation it was read at — the read half of a
 *  read-modify-write. The two travel together on purpose. */
export interface ServerDetail {
  generation: number;
  id: string;
  entry: ServerEntry;
}

/** api.WriteResult: where the registry now stands after a mutation. */
export interface WriteResult {
  generation: number;
  changed: boolean;
  /** Healed corruption and fail-closed side effects. Surfaced, never swallowed. */
  warnings?: string[];
}

export interface ServerWrite extends WriteResult {
  id: string;
  entry?: ServerEntry;
  deleted?: boolean;
}

/** Asks the daemon to connect to a configured server and report what it
 *  finds. `tool`, when set, is also called after the handshake — and it is
 *  the ORIGINAL downstream name, not the exposed one. */
export interface ServerTestRequest {
  tool?: string;
  args?: unknown;
  /** 0 selects the downstream default. */
  timeout_ms?: number;
  /** Asks for `tool_defs` — signature, description and raw input schema —
   *  alongside the bare name list. Opt-in because schemas are unbounded and
   *  the usual question ("does this connect") does not need them. */
  defs?: boolean;
  /** Raises the byte limit on `call.text`. 0 selects the daemon's default,
   *  which is small on purpose — it is sized for "does this connect", not
   *  for rendering a tool's answer to a person. There is no cursor to fetch
   *  a remainder from, so anything over this limit is lost for good. */
  max_text_bytes?: number;
}

/** One tool of the live handshake, present when the request set `defs`. */
export interface ServerTestTool {
  name: string;
  description?: string;
  /** The same compact grammar an agent is shown (internal/discovery/toolsig),
   *  not a second format invented for the GUI. */
  signature?: string;
  /** The signature dropped information; `input_schema` has the rest. A lossy
   *  signature must never be presented as the whole truth. */
  lossy?: boolean;
  /** The downstream's own JSON Schema, verbatim. ABSENT is a fact about the
   *  server — it is not the same as `{}`. */
  input_schema?: unknown;
}

export interface ServerTestCall {
  tool: string;
  is_error: boolean;
  text?: string;
  /** `text` is not the whole answer. A FIELD rather than the `… (truncated)`
   *  trailer inside the text, which is prose a tool could have written
   *  itself — and this is what decides whether a failed JSON parse means
   *  "not JSON" or "cut before it could be". */
  truncated?: boolean;
  millis: number;
}

export interface ServerTestResult {
  server: string;
  transport: string;
  server_info?: string;
  protocol_version?: string;
  connect_ms: number;
  tool_count: number;
  tools: string[];
  tool_defs?: ServerTestTool[];
  call?: ServerTestCall;
}

// ---------------------------------------------------------------------------
// The curated catalog (api/catalog.go, docs/modules/controlplane.md)
// ---------------------------------------------------------------------------

/**
 * api.Provenance* — where a catalog definition came from.
 *
 * RED LINE for every renderer: this is a SOURCE SIGNAL, not a cryptographic
 * proof. Nothing in the catalog is signed and nothing is verified at add
 * time, so it may be shown as an origin ("curated by the agenthub
 * maintainers") and must never be shown as "verified", "trusted" or "safe".
 */
export const CatalogProvenance = {
  Curated: "curated",
  Registry: "registry",
  User: "user",
} as const;
export type CatalogProvenance = (typeof CatalogProvenance)[keyof typeof CatalogProvenance];

/** api.CatalogAuthOAuth: the server needs a login AFTER it is added. It does
 *  not make the entry harder to add — the login is a separate, later step —
 *  so such an entry is still one-click addable. */
export const CatalogAuthOAuth = "oauth";

/** One credential an entry needs. `key` is the VAULT key, which the entry
 *  references as ${SECRET_<KEY>}. No value ever travels here. */
export interface CatalogCredential {
  key: string;
  description?: string;
  /** An optional credential does NOT make an entry need configuration. */
  optional?: boolean;
}

/** One plain (non-secret) value the user must supply. The entry references
 *  it as {{name}} and the daemon substitutes it at ADD time — unlike
 *  ${SECRET_X}, which must survive into the stored entry. */
export interface CatalogParam {
  name: string;
  description?: string;
  example?: string;
}

export interface CatalogEntry {
  id: string;
  name: string;
  description: string;
  publisher?: string;
  homepage?: string;
  /** One of CatalogProvenance. */
  provenance: string;
  transport: string;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  url?: string;
  headers?: Record<string, string>;
  keys?: CatalogCredential[];
  params?: CatalogParam[];
  auth?: string;
  tags?: string[];
  /** False when the entry can be added with a single click. The DAEMON
   *  computes it so every frontend splits the list identically — a GUI that
   *  re-derived it would eventually disagree with the CLI. */
  needs_config: boolean;
  /** The credentials to store afterwards. */
  required_keys?: string[];
}

/** The answer to a catalog listing or search. `query` echoes what produced
 *  the entries, which is how a search box drops an answer that arrived after
 *  the user moved on. */
export interface CatalogList {
  query?: string;
  entries: CatalogEntry[];
}

export interface CatalogAddRequest {
  /** Overrides the registry id ("" / absent = the catalog id). */
  name?: string;
  /** A missing or unknown parameter is refused, never guessed. */
  params?: Record<string, string>;
}

export interface CatalogAdded extends WriteResult {
  id: string;
  catalog_id: string;
  entry?: Partial<ServerEntry>;
  /** The commands that finish the job — storing a credential, logging in.
   *  Adding the definition is not the same as making the server work, and a
   *  page that hides this makes a half-done setup look finished. */
  next_steps?: string[];
}

// ---------------------------------------------------------------------------
// Pasted client configuration (api/catalog.go, docs/modules/controlplane.md)
// ---------------------------------------------------------------------------

/** api catalog.Shape: which wrapper a pasted document was recognized as. */
export const PasteShape = {
  Wrapped: "wrapped",
  EntryMap: "entry-map",
  SingleEntry: "single-entry",
} as const;

/**
 * One server a pasted configuration proposes. A CANDIDATE, not a stored
 * entry: `name` may be empty (a single pasted entry names nothing), nothing
 * has been written, and the definition has not been through the registry's
 * validation yet — so a preview may contain an entry the daemon will later
 * refuse.
 *
 * `entry` is the REGISTRY document shape, where most fields are omitempty:
 * an absent field is one the pasted configuration did not mention, which is
 * why it is Partial and not ServerEntry.
 */
export interface ParsedServer {
  name: string;
  entry: Partial<ServerEntry>;
  /** Dropped fields, a value that looks like a pasted credential, a missing
   *  name. Shown, never swallowed: this is the whole point of a preview. */
  warnings?: string[];
}

/** One recognized entry that is deliberately not proposed — agenthub's own
 *  gateway entry, or an entry naming neither command nor url. */
export interface ParsedSkip {
  name: string;
  reason: string;
}

export interface ParsedClientConfig {
  /** One of PasteShape. */
  shape: string;
  /** The JSON key path the servers were found under; absent for the naked
   *  shapes. */
  section?: string[];
  servers: ParsedServer[];
  skipped?: ParsedSkip[];
}

// ---------------------------------------------------------------------------
// Profiles
// ---------------------------------------------------------------------------

/** api.ToolSelect* — the three states of a tool selector EDIT. */
export const ToolSelect = { All: "all", Only: "only", None: "none" } as const;
export type ToolSelect = (typeof ToolSelect)[keyof typeof ToolSelect];

/** One three-state selector edit. `mode` is required: an empty mode is
 *  refused by the daemon rather than guessed, so a forgotten field can never
 *  widen a selector. */
export interface ProfileTools {
  mode: ToolSelect;
  tools?: string[];
}

/**
 * A STORED selector as read back:
 *   allow === undefined -> the server's full tool set (no rule)
 *   allow === []        -> block-all
 *   allow === [...]     -> exactly those tools
 */
export interface ToolSelector {
  allow?: string[];
}

export interface Profile {
  name: string;
  /** Same three states as ToolSelector.allow, for the member-server set. */
  servers?: string[];
  tools?: Record<string, ToolSelector>;
}

export interface ProfileList {
  generation: number;
  profiles: Profile[];
  active: string;
  /** False when this daemon cannot answer the question at all — which is not
   *  the same as "there is no active profile". */
  active_known: boolean;
}

/** api.ServerSet* */
export const ServerSet = { Replace: "replace", Add: "add", Remove: "remove" } as const;
export type ServerSet = (typeof ServerSet)[keyof typeof ServerSet];

/** One edit of a profile's member set. Under `replace`, `null` clears the
 *  narrowing and `[]` is block-all — the field is never omitted. */
export interface ServerSetEdit {
  mode: ServerSet;
  servers: string[] | null;
}

export interface ProfileWrite extends WriteResult {
  name: string;
  old_name?: string;
  profile?: Profile;
  repointed?: string[];
  /** Client ids left pointing at a REMOVED profile: they now resolve to an
   *  EMPTY scope. Reported so the UI can say so out loud. */
  dangling?: string[];
  active_cleared?: boolean;
  deleted?: boolean;
}

// ---------------------------------------------------------------------------
// Client scope binding
// ---------------------------------------------------------------------------

/** api.Binding* — "no profile" is spelled followActive, never an empty name. */
/** registry.ProfileBindingKind: the two spellings a stored binding has.
 *  There is no third — "no profile" is followActive, never an empty name. */
export const Binding = {
  Named: "named",
  FollowActive: "followActive",
} as const;
export type Binding = (typeof Binding)[keyof typeof Binding];

export interface ProfileBinding {
  kind: string;
  name?: string;
}

export interface ClientEntry {
  profile?: string;
  profileRef?: ProfileBinding;
  discovery?: string;
  servers?: string[];
  tools?: Record<string, ToolSelector>;
}

/**
 * An EDIT of one client's binding: an absent field is left untouched.
 *
 * `servers` is the load-bearing one: null/absent = leave the rule alone,
 * [] = block-all, [...] = exactly those.
 */
export interface ClientBinding {
  profile?: ProfileBinding;
  servers?: string[] | null;
  tools?: Record<string, ProfileTools>;
  discovery?: string;
}

export interface ScopeDetail {
  generation: number;
  client: string;
  /** False when this client has no binding at all — NOT the same as an empty
   *  binding (the former follows the active profile). */
  exists: boolean;
  entry?: ClientEntry;
  dangling?: boolean;
  dangling_profile?: string;
}

export interface ScopeWrite extends WriteResult {
  client: string;
  entry?: ClientEntry;
  exists: boolean;
  dangling?: boolean;
  dangling_profile?: string;
  cleared?: boolean;
}

/** Discovery modes accepted by the governance key and the scope override. */
export const DiscoveryModes = ["lazy", "grouped", "full"] as const;

// ---------------------------------------------------------------------------
// Governance
// ---------------------------------------------------------------------------

export const GovernanceKind = { Bool: "bool", Enum: "enum", Bytes: "bytes" } as const;

export interface GovernanceValue {
  key: string;
  kind: string;
  doc?: string;
  /** Rendered value for every kind ("true", "grouped", "65536"). "" = unset. */
  value: string;
}

export interface GovernanceList {
  generation: number;
  entries: GovernanceValue[];
}

export interface ConfigWrite extends WriteResult {
  key: string;
  value: string;
  previous?: string;
}

/** api.ResultBudgetPrefix — the dynamic `resultBudget.<serverID|*>` family. */
export const ResultBudgetPrefix = "resultBudget.";

// ---------------------------------------------------------------------------
// Secrets — names only, never values
// ---------------------------------------------------------------------------

/** api.SecretScopeGlobal */
export const SecretScopeGlobal = "_global";

/**
 * One stored secret's identity.
 *
 * RED LINE: there is no value field and there never will be. Not "we do not
 * populate it" — the type cannot carry one, so no listing and no frontend
 * cache can ever hold a credential.
 */
export interface SecretRef {
  server: string;
  scope: string;
  key: string;
  backend: string;
  set: boolean;
}

export interface SecretChange {
  action: string;
  server: string;
  key: string;
  scope: string;
}

// ---------------------------------------------------------------------------
// Skills
// ---------------------------------------------------------------------------

export interface SkillInstall {
  client_id: string;
  scope: string;
  project_root?: string;
  path: string;
  state: string;
  detail?: string;
}

export interface Skill {
  id: string;
  name: string;
  description?: string;
  kind?: string;
  enabled: boolean;
  fingerprint?: string;
  updated_at?: string;
  installs?: SkillInstall[];
}

export interface SkillInstallRequest {
  client_id: string;
  scope?: string;
  project_root?: string;
  dir?: string;
  /** Permits overwriting a copy edited outside agenthub. Without it a drifted
   *  target refuses with a 409 — drift is the user telling us something. */
  allow_drift?: boolean;
}

// ---------------------------------------------------------------------------
// Agent tokens
// ---------------------------------------------------------------------------

/** api.Tier* — the operation class a credential may reach. */
export const Tier = { Read: "read", Write: "write", Destructive: "destructive" } as const;
export type Tier = (typeof Tier)[keyof typeof Tier];

/** api.TokenServerWildcard */
export const TokenServerWildcard = "*";

export interface Token {
  name: string;
  /** 12 characters: enough to identify a row, useless as a credential. */
  prefix: string;
  tier: string;
  /** null = every server, ["*"] = every server explicitly, [] = NOTHING. */
  servers: string[] | null;
  profile?: string;
  state: string;
  created_at: string;
  expires_at?: string;
  revoked_at?: string;
}

export interface TokenSpec {
  name: string;
  tier?: string;
  servers: string[] | null;
  profile?: string;
  expires_in_seconds?: number;
}

/** The ONLY place a token value ever appears. Show it once, offer a copy
 *  button, say plainly that closing the dialog loses it forever, never
 *  persist it — agenthub itself cannot print it again. */
export interface TokenCreated {
  token: Token;
  value: string;
}

export interface TokenRevoked {
  name: string;
  prefix: string;
  revoked_at: string;
}

// ---------------------------------------------------------------------------
// Client wiring
// ---------------------------------------------------------------------------

export interface ClientDetected {
  client: string;
  name: string;
  placement: string;
  shape: string;
  path: string;
  writable: boolean;
  size: number;
  modified: string;
  note?: string;
  /** A location that exists but may not be inspected. "Not installed" and
   *  "you may not look" call for opposite user actions. */
  denied?: boolean;
  remediation?: string;
}

export interface ClientDetectResult {
  found: ClientDetected[];
  /** Every client agenthub knows about — NOT the writable subset. Presenting
   *  it as the set agenthub can write contradicts the `writable` field on the
   *  rows beside it; `indirect` names the difference. */
  supported: string[];
  /** The supported clients agenthub does not write itself: connect delegates
   *  to the client's own CLI, or hands back a snippet to paste. */
  indirect?: string[];
}

export type ClientConnectState =
  | "connected"
  | "not_connected"
  | "denied"
  | "unreadable"
  | "unknown";

export interface ClientInspectedServer {
  name: string;
  transport?: string;
  command?: string;
  url?: string;
  disabled?: boolean;
  owned: boolean;
}

export interface ClientInspectedFile {
  path: string;
  placement: string;
  exists: boolean;
  parsed: boolean;
  connected: boolean;
  servers?: ClientInspectedServer[];
  error?: string;
}

export interface ClientInspection {
  client: string;
  name?: string;
  shape?: string;
  state: ClientConnectState;
  connected: boolean;
  placements?: string[];
  files: ClientInspectedFile[];
  note?: string;
  manual?: string;
}

export interface GatewayEntry {
  command: string;
  args: string[];
}

export interface ClientConnectRequest {
  profile?: string;
  path?: string;
  /** "user" (the default) or "project". Not combinable with `path`. */
  placement?: string;
  bin?: string;
  dry_run?: boolean;
}

export interface ClientConnection {
  client: string;
  profile?: string;
  dry_run: boolean;
  entry: GatewayEntry;
  path?: string;
  /** The copy taken before the file was rewritten: the operator's undo. */
  backup?: string;
  changed?: boolean;
}

export interface ClientDisconnected {
  client: string;
  path: string;
  removed: string[];
  backup?: string;
}

// ---------------------------------------------------------------------------
// OAuth credentials
// ---------------------------------------------------------------------------

/** api.AuthState* */
export const AuthState = {
  Authorized: "authorized",
  Expiring: "expiring",
  Expired: "expired",
  None: "none",
  Error: "error",
} as const;

/** RED LINE: no token, no client secret, no refresh token — has_refresh_token
 *  is a boolean, not the token. */
export interface AuthStatus {
  server: string;
  state: string;
  issuer?: string;
  scope?: string;
  /** Unix seconds; 0 means the provider advertised no expiry at all, which is
   *  "never expires" — NOT "expired". */
  expires_at: number;
  expires_in: number;
  has_refresh_token: boolean;
  client_registrar?: string;
  detail?: string;
}

export interface AuthRefreshed {
  server: string;
  expires_at: number;
  expires_in: number;
  /** Another writer refreshed first and this call adopted its result: a
   *  success with a different provenance, not a race lost. */
  superseded: boolean;
}

export interface AuthLoggedOut {
  server: string;
}

/** Phases of AuthLogin.phase. */
export const LoginPhase = {
  Pending: "pending",
  Complete: "complete",
  Failed: "failed",
} as const;

/** Interactive modes of AuthLogin.mode. */
export const LoginMode = {
  /** The frontend opens authorization_url; the daemon catches the redirect. */
  Loopback: "loopback",
  /** The frontend shows user_code and verification_uri; the daemon polls. */
  Device: "device",
} as const;

/**
 * One interactive login session.
 *
 * RED LINE, as everywhere: no access token, no refresh token, no authorization
 * code and no device code. `user_code` is the short string the human types
 * into the provider's own site and is meant to be displayed; the device code
 * polled with has no field here.
 *
 * THE FRONTEND OPENS THE BROWSER. `authorization_url` is returned rather than
 * visited, because the daemon may be headless, may have been started by a
 * service manager with no session to draw into, and may not be where the user
 * is sitting. A page that renders it and waits will wait forever.
 */
export interface AuthLogin {
  id: string;
  server: string;
  phase: string;
  /** Empty until the flow has chosen: that needs the authorization server's
   *  metadata, so the first poll commonly has none. */
  mode?: string;
  authorization_url?: string;
  verification_uri?: string;
  verification_uri_complete?: string;
  user_code?: string;
  /** When the SESSION gives up, in Unix seconds. Not the credential's expiry. */
  deadline?: number;
  issuer?: string;
  scope?: string;
  /** Unix seconds; 0 means the provider advertised no expiry — "never
   *  expires", NOT "expired". */
  token_expires_at?: number;
  has_refresh_token?: boolean;
  error?: string;
  hint?: string;
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

export interface SessionInfo {
  id: string;
  client_id: string;
  origin: string;
  root?: string;
  profile_name: string;
  last_seen: string;
}

/** Daemon connection state (services.Status). */
export interface Status {
  connected: boolean;
  socket: string;
  version: string;
  pid: number;
  generation: number;
  /** This application is running the hub, and quitting stops it. */
  owned: boolean;
  /** The hub belongs to another AgentHub window: everything on the page
   *  works, but quitting here leaves it running. A hub an operator started
   *  headless is neither owned nor guest — it belongs to them. */
  guest: boolean;
  error?: string;
}

/**
 * The window-local preferences (services.WindowPrefs).
 *
 * Not hub state: they describe this window on this machine, live in
 * localStorage rather than in the registry, and are pushed to the Go side only
 * because the close button is handled natively.
 */
export interface WindowPrefs {
  closeToTray: boolean;
  hideNoticeSeen: boolean;
}

/** One bridged daemon event (services.TopicEvent). */
export interface TopicEvent<T = unknown> {
  topic: string;
  kind: string;
  rev: number;
  payload?: T;
}

// ---------------------------------------------------------------------------
// Encrypted access ledger
// ---------------------------------------------------------------------------

export interface CallsUsage {
  bytes: number;
  days: number;
  eventFiles: number;
  packFiles: number;
}

export interface CallsStatus {
  generation: number;
  enabled: boolean;
  arguments: string;
  results: string;
  resultBytes: number;
  durability: string;
  retentionDays: number;
  maxBytes: number;
  minFreeBytes: number;
  pressure: string;
  keyId?: string;
  storage: CallsUsage;
}

export interface CallSummary {
  callId: string;
  time: string;
  client?: string;
  face?: string;
  /** What the client asked agenthub FOR — tools/call, tools/list, initialize,
   *  ping — and which of the hub's own faces the name reached: "meta" for one
   *  of agenthub's own tools, "group" for a grouped listing, "tool" for a name
   *  routed straight through. The pair distinguishes "the client called the
   *  server" from "the client asked the hub, which called the server". */
  method?: string;
  surface?: string;
  exposedTool?: string;
  server?: string;
  tool?: string;
  /** What the call REACHED, one groupable value each: the routed server and
   *  tool where routing happened, and agenthub's own "(agenthub)" where the
   *  hub answered the call itself. The statistics count these and the daemon
   *  filters on them, so this page must LABEL rows with them too — a label
   *  derived here instead is a dropdown option that selects rows rendered
   *  under another name. Both empty means unrouted. */
  targetServer?: string;
  targetTool?: string;
  outcome?: string;
  durationMs?: number;
  code?: string;
  resultCapture?: string;
  complete: boolean;
}

export interface CallPage {
  since?: string;
  calls: CallSummary[];
  total: number;
  nextCursor?: string;
  skippedMalformed: number;
}

/** One control-plane state change (api.EventRecord). `scope` and `kind` are
 *  CLOSED sets published in docs/subsystems/records.md — safe to switch on,
 *  unlike a log message. An unknown value means this frontend is older than
 *  the daemon, not that the field is free text. */
export interface EventRecord {
  ts: string;
  scope: string;
  kind: string;
  /** "routine" or "disruption": the hub running as intended versus the hub
   *  reacting to something that went wrong, the recovery that ends an outage
   *  included. Derived from `kind` by the daemon, so this frontend never has
   *  to hold a second copy of which kinds are trouble. */
  class: string;
  server?: string;
  inst?: string;
  client?: string;
  /** The MCP session a record is about, on the HTTP face. Its callers are
   *  tokens rather than configured clients, so for the session kinds this is
   *  the only identity there is. */
  session?: string;
  pid: number;
  from?: string;
  to?: string;
  detail?: string;
  /** The one number the kind carries; COUNT_NOUN in pages/events.ts says what
   *  it counts. Rendered unlabelled, a thirteen-tool connect reads as a
   *  thirteenth attempt. */
  count?: number;
  /** A registry generation. It identifies a revision rather than counting
   *  anything, so it is not folded into `count`. */
  rev?: number;
  durMs?: number;
}

/** One line of a process log (api.ProcLogRecord). The three join keys are
 *  columns; everything else the line carried stays in `fields`, because slog
 *  attrs are open-ended and a UI that showed only the named ones would
 *  silently drop whatever the next log line adds. */
export interface ProcLogRecord {
  time: string;
  origin: string;
  level: string;
  msg: string;
  client?: string;
  server?: string;
  pid?: number;
  fields?: Record<string, string>;
}

/** One page of process logs, newest first (api.ProcLogPage). */
export interface ProcLogPage {
  records: ProcLogRecord[];
  /** How many matched from the cursor onward — what "of N" needs. */
  total?: number;
  /** The position to resume from; absent at the end of the list. */
  nextCursor?: string;
}

export interface EventLog {
  events: EventRecord[];
  /** How many matched from the cursor onward — what "of N" needs. */
  total?: number;
  /** The position to resume from; absent at the end of the list. */
  nextCursor?: string;
  /** How many segments were read. 0 means nothing has ever been written,
   *  which is a different fact from an empty `events` over four files. */
  files: number;
  skipped?: number;
}

export interface CallEvent {
  time: string;
  event: string;
  requestId?: string;
  session?: string;
  policyRev?: number;
  server?: string;
  tool?: string;
  outcome?: string;
  durationMs?: number;
  gate?: string;
  rule?: string;
  code?: string;
  error?: string;
  toolError?: boolean;
  /** Set on a FRAME (event "sent" or "recv"): what crossed the downstream
   *  boundary, why, which attempt it was, and how big it was. A call that
   *  retried twice has three sent/recv pairs under one `routed`. */
  method?: string;
  cause?: string;
  seq?: number;
  bytes?: number;
}

export interface CallPayload {
  text?: string;
  bytes?: number;
  truncated?: boolean;
}

export interface CallDetail extends CallSummary {
  events: CallEvent[];
  error?: string;
  request: CallPayload;
  effectiveArguments: CallPayload;
  result: CallPayload;
}

export interface CallsStats {
  since?: string;
  events: number;
  calls: number;
  incomplete: number;
  skippedMalformed: number;
  payloadRawBytes: number;
  payloadStoredBytes: number;
  outcomes: Record<string, number>;
  clients: Record<string, number>;
  servers: Record<string, number>;
  tools: Record<string, number>;
  serverTools?: Record<string, Record<string, number>>;
}

export interface CallsVerify {
  ok: boolean;
  events: number;
  payloads: number;
  skippedMalformed: number;
  failures: number;
  issues?: string[];
}

export interface CallsPrune {
  dryRun: boolean;
  before: string;
  days: number;
  bytes: number;
  names?: string[];
}

export interface CallsKeyRotation {
  previousKeyId: string;
  keyId: string;
  enabled: boolean;
}

/** Remember scopes accepted by Answer (api.Remember*). */
export const Remember = {
  None: "none",
  Session: "session",
  Forever: "forever",
} as const;
export type Remember = (typeof Remember)[keyof typeof Remember];

/** Error codes a page branches on (api.ErrCode*, plus the GUI-local ones). */
export const ErrCode = {
  Offline: "E_OFFLINE",
  NotFound: "E_NOT_FOUND",
  BadRequest: "E_BAD_REQUEST",
  Forbidden: "E_FORBIDDEN",
  /** A write carried an expectedGeneration the registry has moved past.
   *  NOTHING was written: re-read, re-apply the intent, retry. */
  StalePrecondition: "E_STALE_PRECONDITION",
  /** A different 409: a name already taken, a drifted skill target. Re-reading
   *  fixes none of those, so it must not trigger the retry path. */
  Conflict: "E_CONFLICT",
  TightenOnly: "E_TIGHTEN_ONLY",
  /** ctlapi.CodeUnsupportedFormat: a configuration format agenthub
   *  RECOGNIZES but deliberately does not parse (TOML, YAML). It is not
   *  "your paste is broken" — the hint carries the manual route, and the
   *  refusal is permanent by design (docs/modules/controlplane.md). */
  UnsupportedFormat: "E_UNSUPPORTED_FORMAT",
  /** A live server self-test reached the downstream and its initial
   *  handshake was rejected with 401/403. Offer the OAuth login action. */
  AuthRequired: "E_AUTH_REQUIRED",
  /** The server definition references a vault key that has not been stored.
   *  missingSecrets carries safe key names for a prefilled writer. */
  SecretRequired: "E_SECRET_REQUIRED",
  /** CLI-shaped authentication failures can cross a mixed-version desktop
   *  boundary. They require the same persistent login action. */
  AuthFailed: "E_AUTH_FAILED",
  Gui: "E_GUI",
} as const;
export type ErrCode = (typeof ErrCode)[keyof typeof ErrCode];

/** services.ErrorKindConflict: the `kind` stamped on a lost
 *  optimistic-concurrency check, and the ONLY failure a page answers by
 *  re-reading rather than by reporting. It is a kind and not a code because
 *  E_STALE_PRECONDITION is one of several 409s and none of the others gets
 *  better by retrying. */
export const ErrorKindConflict = "conflict";

/** The `cause` payload of a rejected binding call (services.MarshalError). */
export interface CallError {
  code: string;
  message: string;
  hint?: string;
  /** Vault key names required by a failed self-test. Values never cross this
   *  interface in either direction. */
  missingSecrets?: string[];
  status?: number;
  offline?: boolean;
  /** ErrorKindConflict, or absent. Classifies the failure by the RESPONSE it
   *  calls for, where the code alone is ambiguous. */
  kind?: string;
  /** Where the registry stands after a conflict. ABSENT when the daemon did
   *  not report one — 0 is the wire spelling of "do not check", so a
   *  defaulted 0 fed back as the next expectedGeneration would become the
   *  blind overwrite the precondition exists to prevent. */
  currentGeneration?: number;
}
