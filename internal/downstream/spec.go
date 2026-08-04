package downstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/eventlog"
	"github.com/dinstein/agent-hub/internal/guard/spawnguard"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/secrets"
	"github.com/dinstein/agent-hub/internal/secrets/secureenv"
)

// Sentinel errors of this package.
var (
	// ErrCircuitOpen is returned by Call while the circuit breaker is open
	// (cooldown) or a half-open probe is already in flight. Callers fail
	// fast; nothing is queued.
	ErrCircuitOpen = errors.New("downstream: circuit open")
	// ErrServerClosed is returned by Call/RefreshTools after Close.
	ErrServerClosed = errors.New("downstream: server closed")
	// ErrNoRefresher reports a 401/403 on a server whose TokenSource has no
	// renewal half wired (no OAuth state, or a caller that opted out).
	ErrNoRefresher = errors.New("downstream: no token refresher wired")
)

// envPrefix is stripped from the child environment on spawn
// (canonical.md §1: AGENTHUB_* is stripped wholesale for downstreams).
const envPrefix = "AGENTHUB_"

// DefaultConnectTimeout bounds the first connection (dial + initialize +
// tools/list). 120s is deliberately generous: npx/uvx launchers with a cold
// cache can take minutes to first byte.
const DefaultConnectTimeout = 120 * time.Second

// refreshTimeout bounds an owner-initiated tools/list refresh triggered by
// a list_changed notification.
const refreshTimeout = 30 * time.Second

// Provenance values for Spec.Provenance — the stable trust flag SSRF
// screening derives from (docs/flows.md: "block_private is a
// provenance-derived stable flag"). They mirror registry.Provenance*.
const (
	// ProvenanceRemote (also the empty default) screens every outbound
	// connection: a private or non-public address is refused.
	ProvenanceRemote = "remote"
	// ProvenanceLocal marks an operator-declared local endpoint. It
	// unblocks LITERAL loopback addresses only — never RFC1918, never a
	// hostname whose DNS answer claims to be local.
	ProvenanceLocal = "local"
)

// Spec is the runtime description of one downstream server, resolved from
// registry.ServerEntry (see SpecFromEntry). Placeholders in Env and Headers
// are still unresolved here: they are expanded against the vault at dial
// time so a rotated secret takes effect on the next (re)connect and so no
// resolved credential is ever held in a long-lived configuration value.
type Spec struct {
	ID   string
	Kind transport.Kind

	// stdio fields.
	Command string
	Args    []string
	// Docker, when non-nil, runs the stdio child inside a container instead
	// of on this host (registry `runtime: docker`). It is a POINTER on
	// purpose: a nil value is the only thing that means "host", so a config
	// asking for isolation can never degrade to a host spawn by defaulting.
	Docker *transport.DockerConfig
	// Env entries are overlaid on the parent environment after AGENTHUB_*
	// stripping. Values may carry ${SECRET_X} placeholders.
	Env map[string]string
	Cwd string

	// URL is the MCP endpoint for the http and sse transports.
	URL string
	// Headers are caller-owned request headers for the HTTP transports.
	// Values may carry ${SECRET_X} placeholders. An explicit Authorization
	// header suppresses the vault/OAuth bearer injection.
	Headers map[string]string

	// Provenance drives SSRF screening (see the Provenance* constants).
	Provenance string

	// Derive is the server's derivation POLICY (registry `derive`), read by
	// the assembling gateway/daemon to decide whether a session gets its own
	// instance. The connection layer itself never interprets it: by the time
	// a Spec reaches Connect the decision is already expressed by DeriveKey.
	Derive DeriveMode
	// DeriveKey identifies THIS instance's derivation; "" is the base
	// instance shared by every session. It never changes ID — see derive.go
	// invariant 1.
	DeriveKey DeriveKey
	// ScopeName is the vault scope component of every secret this instance
	// resolves: the composite key (ServerID, ScopeName). Empty means
	// secrets.DefaultScope ("_global"). Derive sets it to the derive key so
	// a per-root/per-session identity is storable without a migration
	// (docs/modules/dataplane.md early warning).
	ScopeName string
}

// IsHTTP reports whether the spec uses one of the two HTTP transports.
func (s Spec) IsHTTP() bool {
	return s.Kind == transport.StreamableHTTP || s.Kind == transport.SSE
}

// DialFunc creates a connected (but not yet initialized) transport for a
// spec. It doubles as the respawn factory used when a half-open probe fails
// (the respawn factory is injected; the slot owns no spawn
// logic).
type DialFunc func(ctx context.Context, spec Spec) (transport.Transport, error)

// TokenSource supplies and renews the bearer credential of an HTTP
// downstream. It is the only OAuth surface this package knows: the flow,
// the vault and the refresh coordination all live behind it, so
// internal/downstream never imports internal/oauthflow.
//
// NewVaultTokenSource builds the standard implementation.
type TokenSource interface {
	// Token returns the current access token. ok=false means "this server
	// has no stored credential"; the connection is then attempted
	// anonymously (several servers allow initialize and tools/list without
	// auth and only 401 on tools/call — docs/modules/oauth.md).
	Token(ctx context.Context) (tok string, ok bool, err error)
	// Refresh renews the credential after a downstream answered 401/403 and
	// returns the new access token. Implementations MUST serialize
	// (singleflight online, the sibling file lock offline): a one-time
	// refresh token spent twice concurrently locks the user out.
	Refresh(ctx context.Context) (string, error)
}

// RefreshFunc is the renewal half of a TokenSource, adapted by the wiring
// from oauthflow.Refresher (which the daemon backs with a singleflight
// coordinator and the CLI/standalone gateway with the sibling file lock).
type RefreshFunc func(ctx context.Context) (string, error)

// BreakerConfig tunes the circuit breaker. Zero fields take the frozen
// defaults (3 consecutive health failures to open, 20s cooldown).
type BreakerConfig struct {
	FailureThreshold int
	Cooldown         time.Duration
}

// RetryConfig tunes the retry loop for ClassRetry errors. Zero fields take
// the defaults: 3 attempts total, 25ms base backoff (doubling, jittered),
// 1s cap.
type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

func (c RetryConfig) withDefaults() RetryConfig {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.BaseDelay <= 0 {
		c.BaseDelay = 25 * time.Millisecond
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = time.Second
	}
	return c
}

// withReconnectDefaults fills the RECONNECT ladder (a separate, much slower
// ladder than the in-call retry above: re-spawning a downstream costs a
// process launch, so the base delay is measured in hundreds of ms and the
// cap in tens of seconds). MaxAttempts is unused on this path — the ladder
// is bounded by the breaker, not by an attempt count.
func (c RetryConfig) withReconnectDefaults() RetryConfig {
	if c.BaseDelay <= 0 {
		c.BaseDelay = 250 * time.Millisecond
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = 30 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 1
	}
	return c
}

// Deps carries the collaborators Connect needs. All fields may be nil/zero;
// a zero Deps still connects a stdio server that needs no secrets.
type Deps struct {
	Log *slog.Logger

	// Secrets resolves ${SECRET_X} placeholders in Env and Headers. nil
	// makes any placeholder a hard error (ErrNoResolver) rather than a
	// silent passthrough.
	Secrets secrets.Resolver

	// Auth supplies the bearer credential of an HTTP downstream and renews
	// it once after a 401/403. nil means no credential is attached and no
	// passive refresh is attempted.
	Auth TokenSource

	// AuthFor builds the TokenSource of ONE instance, so a derived instance
	// can carry its own identity (the vault scope name is spec.ScopeName).
	// It exists because Deps is shared by every instance of a Pool while the
	// credential is per (server, scope). Returning nil falls back to Auth —
	// which is also what a nil AuthFor does, keeping every pre-M2 assembly
	// byte-for-byte unchanged.
	AuthFor func(spec Spec) TokenSource

	// Spawn vets the FINAL host command line — after secret expansion and
	// after any docker rewriting — immediately before the child is spawned,
	// and refuses the spawn when it returns an error.
	//
	// nil selects the built-in spawnguard. It does NOT mean "do not screen".
	// This field spent the whole of M1 declared as `Spawn any // reserved`
	// with no reader, so transport.StdioConfig.Screen was never set by any
	// production assembly and every host-runtime command spawned unscreened:
	// spawnguard's only production caller was confops, which sees docker
	// entries at `server add` time and nothing else. A screen each assembly
	// has to remember to attach is one that will be missing from the next
	// assembly, so the default here is the guard and the opt-out is explicit.
	Spawn func(command string, args, env []string) error

	// LoginPATH supplies the login shell's PATH, which buildEnv folds into
	// the one a stdio child is given (see widenPATH). nil selects
	// secureenv.LoginPATH, which is what production wants; this field exists
	// because that one is a process-wide sync.Once, so a test cannot ask it
	// for two different answers — and this path only ever runs for real in a
	// packaged GUI, which is precisely where an untested regression would sit
	// unnoticed.
	LoginPATH func() string

	// SpawnUnscreened disables Spawn entirely. It exists for the tests that
	// spawn deliberately guard-tripping shapes to prove the guard is what
	// stops them elsewhere; production never sets it.
	SpawnUnscreened bool

	// DialContext overrides the SSRF-screening dialer used by the HTTP
	// transports. nil selects the netguard-screened dialer derived from
	// Spec.Provenance — which is what production always wants; this field
	// exists for tests that must reach an httptest server.
	DialContext transport.DialContextFunc

	// NotificationStream opens the optional server→client GET SSE stream of
	// the streamable-http transport after initialize. Servers that do not
	// offer it answer 405 and are left alone.
	NotificationStream bool

	// Dial overrides transport creation (tests, in-process fakes). nil
	// selects the built-in dialer for Spec.Kind. The same function is the
	// respawn factory after a failed half-open probe.
	Dial DialFunc

	// ConnectTimeout bounds first connect and every respawn; 0 means
	// DefaultConnectTimeout.
	ConnectTimeout time.Duration

	Breaker BreakerConfig
	Retry   RetryConfig

	// Reconnect tunes the respawn backoff ladder (see
	// withReconnectDefaults). Its exponent is Server.Reconnects(), which
	// survives successful reconnects by design.
	Reconnect RetryConfig

	// PingInterval starts a background MCP ping health probe at this period
	// (0 = no background probing; Server.Ping stays available for on-demand
	// probes). Three consecutive transient failures — or one hard failure
	// such as connection refused — flip Server.Health() to ConnError.
	PingInterval time.Duration

	// TraceFor returns the JSON-RPC frame log of ONE server (OpenServerLog).
	// nil, or a nil return, means no tracing; a non-nil log still records
	// nothing until SetEnabled(true).
	//
	// A function rather than a *ServerLog for the same reason AuthFor is a
	// function rather than Auth: Deps is shared by every server and every
	// derived instance of one gateway, while a ServerLog carries the server
	// id it was opened with — into its file name AND into every frame's
	// `server` field. One shared log would therefore file every server's
	// frames under whichever server happened to open it, and label them as
	// that server. There is no assembly for which that is correct, so the
	// plain field is not kept as a fallback: keeping it would preserve the
	// ability to express the bug.
	FramesFor func(spec Spec) *FrameLog

	// Events returns the shared control-plane event stream
	// (internal/eventlog), or nil. A nil *Stream's Append is a no-op, so no
	// call site needs a check, and "the switch is off" and "the file would
	// not open" are deliberately the same answer.
	//
	// A function for a different reason from TraceFor and AuthFor above,
	// which are functions because their value is PER SPEC. This one is
	// shared by every server — one file, one timeline — but it is decided by
	// GOVERNANCE, which a gateway loads after it has built its pool. Deps is
	// captured once at NewPool, so a plain field would be read before the
	// switch exists and every derived instance would be silent forever.
	Events func() *eventlog.Stream

	// ClientID names the client this gateway serves. It is stamped on every
	// server event, so a record says which process observed it — the
	// question a shared file forces, and the one the trace log's `pid` had
	// to be added to answer. Empty for an assembly serving no single client.
	ClientID string
}

// serverEvents binds the shared stream to one connection's identity, so a
// call site passes only what varies. The zero value is usable and silent.
//
// It exists for the reason srvLog is bound once below rather than stamped
// per call site: an identity each emit has to remember is an identity one
// emit eventually forgets.
type serverEvents struct {
	stream *eventlog.Stream
	log    *slog.Logger
	server string
	inst   string
	client string
}

// emit fills in scope and identity, appends the record, and writes the same
// fact as prose to the connection's logger. Callers set Kind and whatever
// that kind carries, plus the sentence a human reads.
//
// One call rather than two: see eventlog.Emit. The logger is the bound one,
// so the identity is already on the line and is not stamped again.
func (e serverEvents) emit(r eventlog.Record, msg string, attrs ...any) {
	r.Scope = eventlog.ScopeServer
	r.Server, r.Inst, r.Client = e.server, e.inst, e.client
	e.stream.Emit(e.log, r, msg, attrs...)
}

// boundServerLog binds one connection's identity onto a logger: the server
// id always, the derive key when this is a derived instance.
//
// One function because two callers need the identical binding — Connect for
// the connection's own logger, eventsFor for the prose half of every event —
// and two spellings of "which connection is this" is how one of them ends up
// missing `inst` and its lines become unattributable.
//
// Spec.ID deliberately does NOT change under derivation, so without the
// derive key four instances of one server would log under one name and a
// respawn could not be pinned to the connection it happened on.
func boundServerLog(base *slog.Logger, spec Spec) *slog.Logger {
	if base == nil {
		base = slog.New(slog.DiscardHandler)
	}
	out := base.With(logx.FieldServer, spec.ID)
	if spec.DeriveKey != "" {
		out = out.With(logx.Instance(string(spec.DeriveKey)))
	}
	return out
}

// eventsFor binds this Deps' stream and a logger carrying the connection's
// identity to one spec.
func (d Deps) eventsFor(spec Spec) serverEvents {
	var stream *eventlog.Stream
	if d.Events != nil {
		stream = d.Events()
	}
	return serverEvents{
		stream: stream,
		log:    boundServerLog(d.Log, spec),
		server: spec.ID,
		inst:   string(spec.DeriveKey),
		client: d.ClientID,
	}
}

// traceFor returns the frame log of one instance, or nil when the assembly
// wired none.
func (d Deps) framesFor(spec Spec) *FrameLog {
	if d.FramesFor == nil {
		return nil
	}
	return d.FramesFor(spec)
}

// authFor returns the TokenSource of one instance: the per-instance source
// when the assembly supplies one, otherwise the shared Auth.
func (d Deps) authFor(spec Spec) TokenSource {
	if d.AuthFor != nil {
		if ts := d.AuthFor(spec); ts != nil {
			return ts
		}
	}
	return d.Auth
}

// dialer returns the DialFunc Connect uses: the caller's override when set,
// otherwise the built-in per-kind dialer closed over these deps.
func (d Deps) dialer() DialFunc {
	if d.Dial != nil {
		return d.Dial
	}
	return func(ctx context.Context, spec Spec) (transport.Transport, error) {
		switch spec.Kind {
		case transport.Stdio:
			return d.dialStdio(ctx, spec)
		case transport.StreamableHTTP, transport.SSE:
			return d.dialHTTP(ctx, spec)
		default:
			return nil, fmt.Errorf("downstream %q: unknown transport kind %q", spec.ID, spec.Kind)
		}
	}
}

// dialStdio spawns the child described by spec and speaks stdio to it. The
// child environment is the parent environment with every AGENTHUB_*
// variable stripped, overlaid with spec.Env after secret expansion, and its
// PATH widened to the login shell's if the command cannot be found without
// that (see widenPATHIfNeeded).
func (d Deps) dialStdio(ctx context.Context, spec Spec) (transport.Transport, error) {
	if spec.Command == "" {
		return nil, fmt.Errorf("downstream %q: empty command", spec.ID)
	}
	env, err := expandSecretMap(ctx, spec.ID, spec.ScopeName, spec.Env, d.Secrets)
	if err != nil {
		return nil, err
	}
	childEnv := buildEnv(env)
	if spec.Docker == nil {
		// Host runtime only: the docker path spawns the docker CLI, which
		// finds itself through transport.DockerBinary's own fallback table.
		childEnv = widenPATHIfNeeded(childEnv, spec.Command, env, d.LoginPATH)
	}
	cfg := transport.StdioConfig{
		Command: spec.Command,
		Args:    spec.Args,
		Env:     childEnv,
		Cwd:     spec.Cwd,
		Screen:  d.spawnScreen(),
	}
	if spec.Docker != nil {
		// Container isolation: the env the child sees is passed through the
		// docker CLI's own environment rather than argv, so secrets never
		// appear in ps(1) output.
		docker := *spec.Docker
		docker.Env = env
		docker.ServerID = spec.ID
		cfg.Docker = &docker
	}
	tr, err := transport.SpawnStdio(cfg)
	if err != nil {
		return nil, explainBlockedEnv(err, env)
	}
	return tr, nil
}

// explainBlockedEnv finishes a diagnosis spawnguard can only half-answer. The
// guard is handed one flat environment and reports which variable it refused;
// it cannot see that childEnv is the agenthub process's own environment with
// the entry's env block laid over it, so it cannot say which of the two the
// variable came from. stated is that env block, after secret expansion.
//
// The distinction is the whole answer. A declared variable is edited out of
// the registry entry; an inherited one is not in any AgentHub file at all, and
// is fixed in whatever started the process — which, for a gateway, is the
// client, and means restarting it. Without this, the operator greps the
// registry, finds nothing, and concludes the message is wrong.
//
// Everything that is not an env_smuggling block, and every such block that
// names no variable (env -S is a shape, not a variable), passes through
// untouched: a wrong provenance claim is worse than none.
func explainBlockedEnv(err error, stated map[string]string) error {
	var b *spawnguard.Blocked
	if !errors.As(err, &b) || b.EnvVar == "" {
		return err
	}
	if _, ok := stated[b.EnvVar]; ok {
		return fmt.Errorf("%w: this server's own env block sets %s — remove it from the entry", err, b.EnvVar)
	}
	return fmt.Errorf("%w: %s is not in this server's env block, so it was inherited from the environment "+
		"agenthub itself was started in — unset it there (a running gateway keeps the environment its client "+
		"gave it, so restart that client afterwards)", err, b.EnvVar)
}

// spawnScreen resolves the screen handed to the transport.
//
// The default is the shared spawnguard rather than nil, so that an assembly
// which simply does not mention screening still gets it. transport screens
// the final host command line in both spawn paths — plain stdio and the
// rewritten `docker run` line — so one screen here covers both runtimes.
func (d Deps) spawnScreen() func(command string, args, env []string) error {
	switch {
	case d.SpawnUnscreened:
		return nil
	case d.Spawn != nil:
		return d.Spawn
	default:
		return defaultSpawnGuard.Check
	}
}

// defaultSpawnGuard is shared: Guard is immutable after New and its Check is
// safe for concurrent use, so every instance of every pool screens through
// one policy. A per-Deps guard would let two assemblies disagree about what
// a container escape is.
var defaultSpawnGuard = spawnguard.New(spawnguard.Config{})

// buildEnv assembles the child environment: parent env minus AGENTHUB_*
// and minus any key overridden by extra, plus extra in sorted key order
// (deterministic output for a given input).
func buildEnv(extra map[string]string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		if strings.HasPrefix(kv, envPrefix) {
			continue
		}
		name, _, _ := strings.Cut(kv, "=")
		if _, ok := extra[name]; ok {
			continue
		}
		out = append(out, kv)
	}
	keys := slices.Sorted(maps.Keys(extra))
	for _, k := range keys {
		out = append(out, k+"="+extra[k])
	}
	return out
}

// pathVar is spelled PATH on every platform this widens on; Windows' Path is
// left to exec's own casing rules, which is where transport stops too.
const pathVar = "PATH"

// widenPATHIfNeeded returns env with its PATH extended by the directories of
// the login shell's PATH, but ONLY when command cannot already be found under
// the PATH env carries. stated is the server's own `env` block.
//
// The problem it solves: a process started by launchd or systemd inherits a
// PATH with four entries in it, and cannot tell from the inside that it was.
// An agenthub daemon spawned by the GUI — itself an app bundle launchd
// opened — sees /usr/bin:/bin:/usr/sbin:/sbin, while the same daemon started
// from a terminal sees everything the user has, so every server whose command
// is a package-manager shim (npx, uvx, bunx — the common case) is unspawnable
// from the GUI and fine from the CLI.
//
// **The precondition is what keeps this off the hot path.** Capturing a login
// PATH costs a shell — an interactive one, which sources an rc file that may
// do real work — and the first stdio dial is the most timing-sensitive moment
// the gateway has: until it finishes, calls are answered "downstream servers
// are still connecting". Paying that on every machine to help the launchd ones
// is the wrong trade when the launchd ones are exactly the ones a lookup
// identifies for free. A PATH that already resolves the command is left alone
// and no shell is ever spawned, so a CLI gateway does no work at all here.
//
// Widening rather than replacing keeps the result a strict superset, so even
// in the repair case every command that already resolved resolves to the same
// file. An explicit PATH in the server's `env` is never touched and never
// probed: a configuration that states a PATH has said what it means.
func widenPATHIfNeeded(env []string, command string, stated map[string]string, loginPATH func() string) []string {
	if _, ok := stated[pathVar]; ok {
		return env
	}
	// The presence check cannot be folded into the lookup. transport.LookPath
	// passes a PATH-less environment through untouched — declining to invent a
	// policy for a caller that gave it nothing — which is right there and
	// wrong here: an environment with no PATH at all is the case that most
	// needs repairing, not the case that needs none.
	if _, hasPATH := envValueOf(env, pathVar); hasPATH {
		if _, err := transport.LookPath(command, env); err == nil {
			return env
		}
	}
	if loginPATH == nil {
		loginPATH = secureenv.LoginPATH
	}
	login := loginPATH()
	if login == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	widened := false
	for _, kv := range env {
		if name, val, ok := strings.Cut(kv, "="); ok && name == pathVar {
			kv, widened = name+"="+secureenv.MergePATH(val, login), true
		}
		out = append(out, kv)
	}
	if !widened {
		// A parent with no PATH at all: launchd can produce one, and the
		// child would then have nothing to resolve a command against.
		out = append(out, pathVar+"="+login)
	}
	return out
}

// envValueOf returns the value of name in a "KEY=value" slice; the last
// occurrence wins, as os/exec's own deduplication does.
func envValueOf(env []string, name string) (string, bool) {
	val, found := "", false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			val, found = v, true
		}
	}
	return val, found
}
