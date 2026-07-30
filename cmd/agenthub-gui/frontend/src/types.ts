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
  deny?: string[];
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
export const Binding = {
  Named: "named",
  FollowActive: "followActive",
  Inherit: "inherit",
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

/**
 * The governance switches whose "off" position WEAKENS enforcement (mirrors
 * api.IsSafetyKey). Restated here on purpose: the wire carries no "this is a
 * safety gate" flag, and deriving the warning from a field the daemon may
 * omit would make the loud path the one that can silently go quiet.
 */
const SAFETY_KEYS = new Set([
  "denyDestructive",
  "blockOnInjection",
  "humanApproval",
  "deny_destructive",
  "block_on_injection",
  "human_approval",
]);

/** True when relaxing this key weakens a safety gate. */
export function isSafetyKey(key: string): boolean {
  return SAFETY_KEYS.has(key);
}

/** True when this write moves a safety gate in the LOOSE direction, i.e. the
 *  one case that has to be marked red and confirmed separately. */
export function relaxesSafety(key: string, from: string, to: string): boolean {
  return isSafetyKey(key) && from === "true" && to !== "true";
}

// ---------------------------------------------------------------------------
// Tool-level governance and the integrity quarantine
// ---------------------------------------------------------------------------

/** One tool's governance state, keyed by (server, RAW tool name). */
export interface Tool {
  server: string;
  tool: string;
  status?: string;
  disabled: boolean;
  approved_hash?: string;
  current_hash?: string;
  override_name?: string;
  override_description?: string;
}

/** Mirrors api.Tool.Drifted: both hashes must be known — an unknown one is
 *  "we cannot tell", which must not read as "unchanged". */
export function toolDrifted(t: Tool): boolean {
  return !!t.approved_hash && !!t.current_hash && t.approved_hash !== t.current_hash;
}

export interface ToolList {
  generation: number;
  tools: Tool[];
}

/** An override edit: an absent field is left untouched, `clear` drops the
 *  override entirely and is exclusive with the two field edits. */
export interface ToolOverride {
  name?: string;
  description?: string;
  clear?: boolean;
}

export interface ToolOverrideValue {
  name?: string;
  description?: string;
}

export interface ToolWrite extends WriteResult {
  server: string;
  tool: string;
  enabled?: boolean;
  status?: string;
  override_cleared?: boolean;
  override?: ToolOverrideValue;
}

/** One quarantined tool, keyed by the CLIENT-VISIBLE exposed name. */
export interface QuarantineEntry {
  exposed: string;
  server: string;
  tool: string;
  reason?: string;
  pinned_hash?: string;
  current_hash?: string;
  at: string;
}

export interface QuarantineList {
  generation: number;
  entries: QuarantineEntry[];
}

export interface QuarantineRelease extends WriteResult {
  exposed: string;
  entry: QuarantineEntry;
  released: boolean;
}

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
// Sessions / approvals / audit
// ---------------------------------------------------------------------------

export interface SessionInfo {
  id: string;
  client_id: string;
  origin: string;
  root?: string;
  profile_name: string;
  last_seen: string;
}

export interface Approval {
  token: string;
  server: string;
  tool: string;
  /** Present only on SSE pending frames; displayed, then dropped. */
  args?: unknown;
  args_hash?: string;
  fingerprint?: string;
  gate_reason?: string;
  client?: string;
  session_id?: string;
  deadline: string;
  /** "" / absent while pending. */
  decision?: string;
  decided_at?: string;
  decided_by?: string;
}

export interface ApprovalResolution {
  token: string;
  decision: string;
  decided_by?: string;
}

export interface ApprovalDecision {
  decision: string;
  remember_error?: string;
}

/** Daemon connection state (services.Status). */
export interface Status {
  connected: boolean;
  socket: string;
  version: string;
  pid: number;
  generation: number;
  error?: string;
}

/** One bridged daemon event (services.TopicEvent). */
export interface TopicEvent<T = unknown> {
  topic: string;
  kind: string;
  rev: number;
  payload?: T;
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
  AlreadyDecided: "E_ALREADY_DECIDED",
  Expired: "E_EXPIRED",
  Stale: "E_STALE",
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
