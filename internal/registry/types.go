package registry

import (
	"fmt"
	"strings"
)

// DocKind names one of the registry's documents. The kind doubles as the
// file basename: <kind>.json.
type DocKind string

// The five M0 documents. meta.json is store-internal (it holds the monotonic
// generation) and is not exposed for mutation through Tx.
const (
	DocMeta       DocKind = "meta"
	DocServers    DocKind = "servers"
	DocProfiles   DocKind = "profiles"
	DocClients    DocKind = "clients"
	DocGovernance DocKind = "governance"
)

// MetaDoc is the typed view of meta.json. Generation is bumped by exactly 1,
// inside the cross-process lock, on every Update that actually changed at
// least one document (the no-op guard skips both the write and the bump).
type MetaDoc struct {
	Generation uint64 `json:"generation"`
}

// Transport identifiers of a ServerEntry. They are the on-disk spelling of
// the three read-side transports (canonical.md §5b) and are mirrored by
// mcp/transport.Kind; an empty value means TransportStdio.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http" // MCP Streamable HTTP (2025-11-25)
	TransportSSE   = "sse"  // legacy HTTP+SSE (deprecated 2025-03-26, read side only)
)

// Runtime identifiers of a ServerEntry: WHERE a stdio child runs. An empty
// value means RuntimeHost, so every entry written before M2 keeps its exact
// previous behaviour (the field is additive and backward compatible in both
// directions — see the Doc[T] unknown-field contract).
const (
	// RuntimeHost spawns the command directly on this machine. The spawn
	// guard still applies; it is anti-smuggling, not a sandbox.
	RuntimeHost = "host"
	// RuntimeDocker runs the command inside a container (docs/modules/foundation.md).
	RuntimeDocker = "docker"
)

// DockerMount is one host directory exposed to a RuntimeDocker server.
//
// Failure direction: read-only is the zero value. A config that forgets to
// think about write access lands on the safe side, and `omitempty` on Write
// keeps that default stable on disk.
type DockerMount struct {
	// Source is the absolute host path. Required.
	Source string `json:"source"`
	// Target is the absolute container path; empty means "same as Source".
	Target string `json:"target,omitempty"`
	// Write mounts read-write instead of read-only.
	Write bool `json:"write,omitempty"`
}

// DockerRuntime is the container configuration of a RuntimeDocker server.
// Everything not stated here is denied by the spawner's defaults: no
// network, no mounts, no capabilities, no privileged mode.
//
// Env is deliberately absent: the container's environment IS ServerEntry.Env
// (one place to look, one place to put a ${SECRET_X} placeholder). The
// spawner forwards those variables by NAME so their values never reach argv.
//
// Like OAuthHint this is a plain struct, not a Doc[T]: unknown members
// INSIDE the block do not survive a rewrite by an older binary. The
// entry-level envelope still preserves the whole `docker` object for a
// binary that does not know the field at all.
type DockerRuntime struct {
	// Image is the container image reference. Required for RuntimeDocker.
	Image string `json:"image"`
	// Network is the docker network. Empty means "none" — an MCP server
	// that needs the network has to say so.
	Network string `json:"network,omitempty"`
	// Mounts are the only host paths the container can see.
	Mounts []DockerMount `json:"mounts,omitempty"`
	// Memory is the `--memory` limit (e.g. "512m", "2g"); empty = unset.
	Memory string `json:"memory,omitempty"`
	// CPUs is the `--cpus` limit (e.g. "1.5"); empty = unset.
	CPUs string `json:"cpus,omitempty"`
	// User and Workdir map to `--user` / `--workdir`.
	User    string `json:"user,omitempty"`
	Workdir string `json:"workdir,omitempty"`
	// ExtraArgs are appended to `docker run` verbatim. They cannot
	// re-specify a flag the isolation defaults own, and they are screened
	// by the spawn guard like every other spawn.
	ExtraArgs []string `json:"extraArgs,omitempty"`
}

// OAuthHint is the *configuration* half of a server's OAuth setup: what a
// future `agenthub auth login` should discover against. It is optional —
// the discovery chain works from the server URL alone (RFC 9728) — and it
// exists so an operator can pin an issuer or a scope set that a provider
// does not advertise.
//
// What deliberately does NOT live here: `needsAuth`. Whether a server
// currently requires (or has lost) authorization is RUNTIME state,
// discovered by a 401/403 on a live connection and reported through the
// Health contract. Persisting it would create a second source of truth that
// goes stale the moment a token expires or a provider changes its mind, and
// a stale "needsAuth": false is exactly the failure that shows a Ready badge
// on a server whose every call 401s (docs/modules/oauth.md).
type OAuthHint struct {
	// Issuer pins the authorization server, skipping RFC 9728 discovery.
	Issuer string `json:"issuer,omitempty"`
	// Scopes is the scope set to request. Sent verbatim; agenthub never
	// appends offline_access.
	Scopes []string `json:"scopes,omitempty"`
	// ResourceMetadataURL pins the RFC 9728 protected-resource document
	// for servers that do not emit a usable WWW-Authenticate hint.
	ResourceMetadataURL string `json:"resourceMetadataUrl,omitempty"`
	// AuthorizationEndpoint replaces the discovered authorization_endpoint
	// for providers that serve one they never advertise (RFC 8414 gives
	// that endpoint exactly one legal source, so such an endpoint is
	// unreachable by a conforming client). Off-spec escape hatch: prefer
	// getting the provider's metadata fixed. See oauthflow.LoginRequest.
	AuthorizationEndpoint string `json:"authorizationEndpoint,omitempty"`
}

// ServerEntry describes one downstream MCP server: a spawned stdio child
// (Command/Args/Env/Cwd) or an HTTP endpoint (URL/Headers), selected by
// Transport.
//
// Env and Headers values may contain ${SECRET_X} placeholders; the registry
// stores them VERBATIM — resolution against the vault happens at connect
// time in internal/downstream. A registry document is world-readable-ish
// configuration and must never hold a credential.
//
// Unknown fields survive round-trips because entries are wrapped in Doc[T];
// adding a field here is backward compatible in both directions (an old
// binary preserves it, a new binary defaults it).
type ServerEntry struct {
	Transport string            `json:"transport"` // "" == stdio
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	// URL is the MCP endpoint for the http and sse transports. Ignored by
	// stdio.
	URL string `json:"url,omitempty"`
	// Headers are caller-owned request headers applied to every HTTP
	// request (http/sse transports). Values may hold ${SECRET_X}.
	//
	// Authorization is NOT set here by the OAuth path: the access token is
	// injected per request from the vault so a token refreshed by the
	// daemon takes effect without reconnecting. An explicit Authorization
	// header (a hand-pasted token) still wins for servers that never do
	// an OAuth login.
	Headers map[string]string `json:"headers,omitempty"`
	// OAuth carries optional login hints (see OAuthHint). nil is the norm.
	OAuth *OAuthHint `json:"oauth,omitempty"`
	// Provenance is the stable trust flag SSRF screening derives from
	// (docs/flows.md: "block_private is a provenance-derived stable flag").
	// Empty and ProvenanceRemote both mean "screen the destination"; only
	// ProvenanceLocal unblocks a LITERAL loopback endpoint.
	Provenance string `json:"provenance,omitempty"`
	// Derive is the derived-instance policy of docs/modules/dataplane.md:
	// "none" (default, and what an entry written before this field existed
	// means) shares one connection across every session; "root" gives each
	// project root its own instance; "session" gives each session one.
	//
	// It is a CONNECTION-plane field only: a derived instance exposes the
	// same tools under the same names and is subject to the same scope —
	// deriving changes which process a call runs on, never what a session
	// can see (docs/architecture.md §7 invariant 2).
	Derive string `json:"derive,omitempty"`
	// Runtime selects WHERE a stdio child runs: RuntimeHost (default, and
	// what every entry written before M2 means) or RuntimeDocker. It is
	// orthogonal to Transport — a docker-runtime server still speaks stdio;
	// only the process on this host changes from the command itself to
	// `docker run`.
	Runtime string `json:"runtime,omitempty"`
	// Docker holds the container configuration; required when Runtime is
	// RuntimeDocker, ignored otherwise.
	Docker  *DockerRuntime `json:"docker,omitempty"`
	Enabled bool           `json:"enabled"`
	// Tools is the GLOBAL tool allow list for this server: nil exposes every
	// tool the server offers, [...] exposes exactly those, [] exposes none.
	// Keyed by ORIGINAL tool names, like every selector here.
	//
	// It sits on the server rather than in a separate state file because it
	// answers the same question `Enabled` does — what this machine offers at
	// all, before any profile narrows it — and because one document means
	// one watch: a change here reaches a running gateway through the
	// registry reload that already exists.
	//
	// The same nil-vs-empty rule as ToolSelector.Allow applies, and for the
	// same reason: omitzero keeps an empty list on disk, because dropping it
	// would turn expose-nothing into expose-everything.
	Tools  []string `json:"tools,omitzero"`
	Source string   `json:"source,omitempty"` // where the entry came from (cli/import/…)
	// Trace records every JSON-RPC frame exchanged with this server to
	// <data>/logs/server-<id>.log, which `agenthub server logs` reads back.
	// Absent (false) is the default: payload capture is opt-in, per server.
	//
	// It is a CONNECTION-plane field, sitting beside Derive and Runtime, and
	// deliberately NOT a profile or client field. One connection is shared by
	// every session reaching this server (Derive "none"), so a per-client
	// switch could not deliver what its name promises: it would have to split
	// connections for the sake of a debugging feature, or record the frames
	// of clients that never asked for it. An isolation a field cannot honor
	// is refused here rather than approximated.
	//
	// The capture point is the downstream boundary, so a trace holds raw
	// results. That is the cost of turning it on, and the reason it stays off
	// until someone says otherwise — and it is the whole cost, because nothing
	// downstream of the capture redacts them either. This used to say "BEFORE
	// leakguard redacts anything", which read as though a trace were the one
	// unredacted copy; there is no leakguard, and a delivered result is as raw
	// as a traced one.
	Trace bool `json:"trace,omitempty"`
}

// Provenance values of ServerEntry.Provenance.
const (
	// ProvenanceRemote is the default: the endpoint is screened, and a
	// private/non-public address is refused.
	ProvenanceRemote = "remote"
	// ProvenanceLocal marks an operator-declared local endpoint (a server
	// running on this machine). It unblocks literal loopback addresses ONLY
	// — never RFC1918, never a hostname whose DNS answer claims to be
	// local, because a DNS answer is a claim its owner can change at will.
	ProvenanceLocal = "local"
)

// TransportName returns the entry's transport with the stdio default
// applied, so callers never have to spell the empty-string case.
func (e ServerEntry) TransportName() string {
	if e.Transport == "" {
		return TransportStdio
	}
	return e.Transport
}

// RuntimeName returns the entry's runtime with the host default applied.
func (e ServerEntry) RuntimeName() string {
	if e.Runtime == "" {
		return RuntimeHost
	}
	return e.Runtime
}

// IsDocker reports whether the entry runs its stdio child in a container.
func (e ServerEntry) IsDocker() bool { return e.RuntimeName() == RuntimeDocker }

// ValidateRuntime checks the runtime half of an entry: an unknown runtime
// name, a docker runtime without an image, or a docker block on a transport
// that spawns nothing.
//
// Failure direction: an unknown runtime is REFUSED, never silently treated
// as host — a typo like "dcoker" must not quietly drop the isolation the
// operator asked for.
func (e ServerEntry) ValidateRuntime() error {
	switch e.RuntimeName() {
	case RuntimeHost:
		if e.Docker != nil {
			return fmt.Errorf("runtime %q ignores the docker block; set runtime to %q", RuntimeHost, RuntimeDocker)
		}
		return nil
	case RuntimeDocker:
		if e.TransportName() != TransportStdio {
			return fmt.Errorf("runtime %q applies to the stdio transport only (this entry is %q)",
				RuntimeDocker, e.TransportName())
		}
		if e.Docker == nil || strings.TrimSpace(e.Docker.Image) == "" {
			return fmt.Errorf("runtime %q needs a docker image", RuntimeDocker)
		}
		return nil
	default:
		return fmt.Errorf("unknown runtime %q (want %q or %q)", e.Runtime, RuntimeHost, RuntimeDocker)
	}
}

// IsHTTP reports whether the entry uses one of the two HTTP transports.
func (e ServerEntry) IsHTTP() bool {
	switch e.TransportName() {
	case TransportHTTP, TransportSSE:
		return true
	default:
		return false
	}
}

// ServersDoc is the typed view of servers.json. Entries are wrapped in Doc so
// unknown fields inside each server entry survive round-trips too.
type ServersDoc struct {
	Servers map[string]Doc[ServerEntry] `json:"servers"`
}

// ToolSelector narrows the tool set of one server (docs/architecture.md §7; owned by
// registry, consumed by internal/scope). Three-state semantics, keyed by
// ORIGINAL tool names (never exposed/renamed names):
//
//   - selector absent for a server (no map entry)  = no intervention
//   - Allow == nil                                 = full server tool set
//   - Allow == []                                  = block-all
//   - Allow == [...]                               = narrow to that subset
//
// It is an ALLOW list and nothing else. A deny list answers the arrival of a
// tool the downstream added after the rule was written in the opposite
// direction — allow hides it, deny exposes it — so carrying both would make
// one configuration file give two different answers to the same question
// depending on which field the operator happened to use.
//
// `omitzero` (not omitempty) is load-bearing: it keeps the nil-vs-empty
// distinction on disk — dropping an empty Allow would silently turn
// block-all into allow-all (fail-open); with omitzero the empty list
// round-trips and block-all stays closed.
type ToolSelector struct {
	Allow []string `json:"allow,omitzero"`
}

// ProfileBindingKind enumerates the explicit profile-reference semantics that
// replace toolport's `"profile": ""` empty-string magic (docs/architecture.md §7).
type ProfileBindingKind string

const (
	// BindingNamed references a profile by name.
	BindingNamed ProfileBindingKind = "named"
	// BindingFollowActive follows the global active profile.
	BindingFollowActive ProfileBindingKind = "followActive"
)

// ProfileBinding is the explicit form of a profile reference. The plain
// `profile` string field on ClientEntry is shorthand for
// {Kind: named, Name: <value>}; an explicit ProfileRef wins over the
// shorthand when both are present.
type ProfileBinding struct {
	Kind ProfileBindingKind `json:"kind"`
	Name string             `json:"name,omitempty"` // required iff Kind == named
}

// Profile is one named tier in profiles.json: an enabled-server set, the
// per-server tool selectors, and how that surface is presented.
//
// Servers three-state mirrors ToolSelector.Allow: nil = no intervention
// (all registered servers), [] = none, [...] = that set. omitzero keeps the
// nil-vs-empty distinction (empty must stay closed, see ToolSelector).
type Profile struct {
	Servers []string                     `json:"servers,omitzero"`
	Tools   map[string]Doc[ToolSelector] `json:"tools,omitempty"` // serverID -> selector
	// Discovery is how this profile's tools are surfaced ("lazy", "grouped",
	// "full"); empty inherits the global default. It lives with the tool set
	// rather than on the client because it describes THAT set: a profile
	// narrowed to two servers wants a different presentation than one holding
	// forty, and binding a client to a profile should settle both questions at
	// once rather than leave presentation to be configured a second time.
	Discovery string `json:"discovery,omitempty"`
}

// ProfilesDoc is the typed view of profiles.json.
type ProfilesDoc struct {
	Profiles map[string]Doc[Profile] `json:"profiles"`
}

// Budget bounds result payloads. Forced marks an org/tighten-only rule: when
// set, merge takes the minimum instead of most-specific-wins.
type Budget struct {
	Bytes  int  `json:"bytes"`
	Forced bool `json:"forced,omitempty"`
}

// RateLimitRule is one cooperative call quota, enforced by
// internal/ratelimit: at most Limit calls per Window for the calls matching
// the (Client, Server, Tool) pattern. An empty dimension — or "*" — matches
// anything; there is deliberately no prefix or glob syntax, because a
// half-understood pattern language is how a quota ends up applying to
// nothing.
//
// Server and Tool name the ROUTED values (router.RouteOf provenance), never
// an exposed or renamed name: renaming a tool must not change which quota a
// call spends from.
//
// The registry stores every field VERBATIM (the same discipline as the
// ${SECRET_X} placeholders): parsing and validation live in
// internal/ratelimit, which refuses the whole rule set rather than dropping
// a rule it cannot understand.
type RateLimitRule struct {
	Client string `json:"client,omitempty"`
	Server string `json:"server,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Limit  int    `json:"limit"`
	// Window is a Go duration STRING ("30s", "1m", "1h"). A bare number is
	// refused at load: 60 is ambiguous between seconds, millis and nanos,
	// and the ambiguity would be discovered as a 1000x wrong quota in
	// production.
	Window string `json:"window"`
	// Scope counts the limit per matching (client, server, tool) triple
	// ("key", the default) or once for the whole rule ("rule" — the way to
	// write a machine-wide, server-wide or client-wide cap). An unknown
	// value is refused at load, never silently read as the default: the two
	// spellings are different quotas.
	Scope string `json:"scope,omitempty"`
}

// ClientEntry binds one AI client to a profile, and does nothing else
// (docs/architecture.md §7).
//
// It used to carry its own servers / tools / discovery / approval /
// resultBudget narrowing on top of the profile. That made "which profile is
// this client on" an incomplete answer to "what can this client see", which
// is the question the whole model exists to answer: an operator had to check
// two places and intersect them by hand. Narrowing now has exactly one home
// (the profile), and a client that needs a different surface is bound to a
// different profile.
type ClientEntry struct {
	Profile    string               `json:"profile,omitempty"`    // shorthand for ProfileRef named
	ProfileRef *Doc[ProfileBinding] `json:"profileRef,omitempty"` // explicit form, wins over Profile
}

// Binding resolves the effective profile reference of a client entry.
// Default (nothing set) is followActive: the client follows the global
// active profile.
func (c ClientEntry) Binding() ProfileBinding {
	return resolveBinding(c.ProfileRef, c.Profile, BindingFollowActive)
}

// resolveBinding implements the shared precedence: explicit ProfileRef >
// `profile` shorthand > layer default.
func resolveBinding(ref *Doc[ProfileBinding], shorthand string, def ProfileBindingKind) ProfileBinding {
	if ref != nil && ref.V.Kind != "" {
		return ref.V
	}
	if shorthand != "" {
		return ProfileBinding{Kind: BindingNamed, Name: shorthand}
	}
	return ProfileBinding{Kind: def}
}

// ClientsDoc is the typed view of clients.json.
type ClientsDoc struct {
	Clients map[string]Doc[ClientEntry] `json:"clients"`
}

const (
	// Audit policy defaults are conservative on both sides of the ledger:
	// request arguments are always complete, result capture is bounded, and
	// storage pressure refuses new calls instead of silently losing history.
	DefaultAuditDurability          = "sync"
	DefaultAuditResultMode          = "truncated"
	DefaultAuditResultBytes         = 64 << 10
	DefaultAuditRetentionDays       = 30
	DefaultAuditMaxBytes      int64 = 5 << 30
	DefaultAuditMinFree       int64 = 1 << 30
)

// AuditPolicy is the persisted access-ledger policy. Zero values other than
// Enabled mean "use the built-in default" so an older governance document
// remains bounded when a newer binary first reads it.
//
// Request arguments deliberately have no switch: an enabled ledger always
// records them completely. Results may be omitted, limited to tool errors,
// truncated, or stored in full up to the MCP frame bound.
type AuditPolicy struct {
	Enabled       bool   `json:"enabled,omitempty"`
	Durability    string `json:"durability,omitempty"`
	ResultMode    string `json:"results,omitempty"`
	ResultBytes   int    `json:"resultBytes,omitempty"`
	RetentionDays int    `json:"retentionDays,omitempty"`
	MaxBytes      int64  `json:"maxBytes,omitempty"`
	MinFreeBytes  int64  `json:"minFreeBytes,omitempty"`
	KeyID         string `json:"keyId,omitempty"`
}

// ResolvedAuditPolicy is a complete immutable policy snapshot used by a
// gateway and rendered by the CLI.
type ResolvedAuditPolicy struct {
	Enabled       bool
	Durability    string
	ResultMode    string
	ResultBytes   int
	RetentionDays int
	MaxBytes      int64
	MinFreeBytes  int64
	KeyID         string
}

// GovernanceDoc is the typed view of governance.json: the global root layer
// of the scope chain.
//
// It carries no permission switches. It used to hold three — humanApproval,
// denyDestructive, blockOnInjection — and each went with the stage that read
// it. What is left decides PRESENTATION (the default discovery mode, result
// budgets) and which profile is active; nothing here can widen or narrow what
// a client may reach, because that is settled by servers.json and
// profiles.json.
type GovernanceDoc struct {
	Discovery string `json:"discovery,omitempty"` // global default discovery mode
	// ActiveProfile is the globally active profile name, the fallback every
	// client that does not name one follows (docs/architecture.md §7). "" = none, so
	// followActive applies no profile narrowing at all.
	//
	// It lives here rather than in a state file because scope resolution is
	// PURE — FromRegistry takes a snapshot and reads no files — so a marker
	// the registry does not carry is a marker the scope chain cannot see.
	// It previously lived in <state>/active-profile.json, which the CLI and
	// the control plane read and wrote while FromRegistry hardcoded "": the
	// value could be set and displayed, but never applied.
	ActiveProfile string                 `json:"activeProfile,omitempty"`
	ResultBudget  map[string]Doc[Budget] `json:"resultBudget,omitempty"` // serverID or "*": global default budget
	// IntentVariants switches lazy mode's single call_tool into the three
	// intent variants call_tool_read / _write / _destructive (docs/architecture.md §9,
	// ruling #18). It is the ONE governance switch whose default is ON, so
	// it is a *bool rather than a plain bool: absent means "the default"
	// (true), false is an explicit opt-out into the compatibility shape for
	// clients whose tool-allowlist UI cannot handle per-tool entries.
	// Read it through IntentVariantsEnabled, never directly.
	//
	// NOT WIRED, IN BOTH DIRECTIONS. Nothing writes it — there is no
	// `agenthub config` key and no control-plane route, so the only way to
	// set it is editing governance.json by hand — and the stdio gateway
	// never carries the resolved value into discovery.Options, so setting
	// it changes nothing today. `internal/discovery` implements and tests
	// the behaviour; only the assembly is missing (docs/architecture.md §8
	// and the unwired-faces appendix in docs/modules/dataplane.md).
	IntentVariants *bool `json:"intentVariants,omitempty"`
	// RateLimits is the cooperative call quota rule set (internal/ratelimit).
	// Absent = no quotas at all; the package is opt-in like every other M2
	// switch.
	//
	// It sits at the GLOBAL layer only — it is deliberately NOT one of the
	// three-layer scope-chain fields — for three reasons:
	//
	//  1. The rule pattern already carries the (client, server, tool)
	//     dimension a per-client or per-project layer would add. A client
	//     layer holding a rule with `client: someone-else` would spell the
	//     same fact twice, in two places that can disagree.
	//  2. The counters are SHARED ACROSS PROCESSES and keyed by the rule
	//     pattern. The same pattern defined at several layers would either
	//     split one quota into one bucket per layer (three layers = three
	//     times the limit — the opposite of the tighten-only merge every
	//     other governance field obeys) or need a per-pattern min-merge that
	//     exists nowhere else in this codebase.
	//  3. A quota is a budget on this machine's shared resources, not a
	//     visibility or permission decision. The scope chain answers "may
	//     this session see and call it"; a quota answers "does this allowed
	//     call happen now or in a few seconds", and it is enforced as an
	//     admission wrapper around the call, after every gate has passed.
	//
	// Tightening within the global layer is still expressive: ALL matching
	// rules are enforced (logical AND), so a narrow rule can only ever
	// restrict further, never unlock what a broad rule forbids.
	RateLimits []Doc[RateLimitRule] `json:"rateLimits,omitempty"`
	// SkillsOverMCP exposes the enabled skill library as read-only MCP tools
	// under the "skills" pseudo-server (docs/modules/config.md). Default OFF: it adds
	// a new supply channel for untrusted text into the model's context, so it
	// is opted INTO, never inherited by an upgrade. Once on, the pseudo-server
	// is an ordinary scope subject — a profile or client layer that lists its
	// servers explicitly hides it again.
	SkillsOverMCP bool `json:"skillsOverMcp,omitempty"`
	// Audit is global machine policy, not a permission layer. When enabled,
	// every tools/call attempt is written before execution and storage
	// failure blocks the call; it never changes which server or tool is in
	// scope. The payload key lives in the secret vault, never this document.
	Audit *Doc[AuditPolicy] `json:"audit,omitempty"`
	// HTTP is the MCP data plane's stored opt-in: the address the daemon
	// serves Streamable HTTP on, and the two confirmations that address
	// needs. Absent — the default — means no listener at all.
	//
	// It is stored rather than only passed on a command line because the
	// desktop application is now what starts a hub, and an application does
	// not type flags. The opt-in it replaces had to be repeated on every
	// start, which is a weaker record than a file the operator can read: the
	// alternative to writing it down is not "confirm it each time", it is
	// "the HTTP face cannot be turned on at all".
	//
	// It is not a scope decision — nothing here says what a client may reach
	// — and the guards that make a bind safe are unchanged: a non-loopback
	// address still needs AllowRemote, and the credential-less endpoint still
	// needs an explicit InsecureLoopback. Storing an answer does not lower
	// the bar for it.
	HTTP *Doc[HTTPFace] `json:"http,omitempty"`
}

// HTTPFace is the MCP data plane's listener, as configured.
type HTTPFace struct {
	// Addr is "host:port". EMPTY MEANS NO LISTENER AT ALL, never a default
	// port: binding a network socket because nobody said otherwise is a
	// decision only an operator may make.
	Addr string `json:"addr,omitempty"`
	// AllowRemote is the explicit confirmation a non-loopback Addr requires.
	// Without it a non-loopback address fails the daemon rather than
	// silently downgrading to loopback.
	AllowRemote bool `json:"allowRemote,omitempty"`
	// InsecureLoopback accepts unauthenticated loopback callers. It never
	// covers a non-loopback bind.
	InsecureLoopback bool `json:"insecureLoopback,omitempty"`
}

// ResolvedHTTP returns the stored data-plane opt-in, or the zero value — no
// listener — when there is none.
func (g GovernanceDoc) ResolvedHTTP() HTTPFace {
	if g.HTTP == nil {
		return HTTPFace{}
	}
	return g.HTTP.V
}

// IntentVariantsEnabled resolves the tri-state IntentVariants switch:
// absent = the ruling #18 default (three variants), otherwise its value.
func (g GovernanceDoc) IntentVariantsEnabled() bool {
	if g.IntentVariants == nil {
		return true
	}
	return *g.IntentVariants
}

// ResolvedAudit returns the complete audit policy with bounded defaults.
func (g GovernanceDoc) ResolvedAudit() ResolvedAuditPolicy {
	p := ResolvedAuditPolicy{
		Durability: DefaultAuditDurability, ResultMode: DefaultAuditResultMode,
		ResultBytes: DefaultAuditResultBytes, RetentionDays: DefaultAuditRetentionDays,
		MaxBytes: DefaultAuditMaxBytes, MinFreeBytes: DefaultAuditMinFree,
	}
	if g.Audit == nil {
		return p
	}
	p.Enabled, p.KeyID = g.Audit.V.Enabled, g.Audit.V.KeyID
	if g.Audit.V.Durability != "" {
		p.Durability = g.Audit.V.Durability
	}
	if g.Audit.V.ResultMode != "" {
		p.ResultMode = g.Audit.V.ResultMode
	}
	if g.Audit.V.ResultBytes > 0 {
		p.ResultBytes = g.Audit.V.ResultBytes
	}
	if g.Audit.V.RetentionDays > 0 {
		p.RetentionDays = g.Audit.V.RetentionDays
	}
	if g.Audit.V.MaxBytes > 0 {
		p.MaxBytes = g.Audit.V.MaxBytes
	}
	if g.Audit.V.MinFreeBytes > 0 {
		p.MinFreeBytes = g.Audit.V.MinFreeBytes
	}
	return p
}

// Snapshot is an immutable view of the registry as of the last successful
// Open or Update performed by this Store. Callers must treat it as read-only;
// mutating it never affects persisted state (Update reloads from disk inside
// the lock), but it would corrupt the view for other readers.
type Snapshot struct {
	Generation uint64
	Servers    Doc[ServersDoc]
	Profiles   Doc[ProfilesDoc]
	Clients    Doc[ClientsDoc]
	Governance Doc[GovernanceDoc]
}

// Default document values used when a file is missing (first Open) or has
// been quarantined. Maps are pre-allocated so empty docs marshal as {} not
// null.

func defaultMetaDoc() Doc[MetaDoc] { return Doc[MetaDoc]{} }

func defaultServersDoc() Doc[ServersDoc] {
	return Doc[ServersDoc]{V: ServersDoc{Servers: map[string]Doc[ServerEntry]{}}}
}

func defaultProfilesDoc() Doc[ProfilesDoc] {
	return Doc[ProfilesDoc]{V: ProfilesDoc{Profiles: map[string]Doc[Profile]{}}}
}

func defaultClientsDoc() Doc[ClientsDoc] {
	return Doc[ClientsDoc]{V: ClientsDoc{Clients: map[string]Doc[ClientEntry]{}}}
}

func defaultGovernanceDoc() Doc[GovernanceDoc] { return Doc[GovernanceDoc]{} }
