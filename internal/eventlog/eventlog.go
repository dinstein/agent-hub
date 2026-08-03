package eventlog

import (
	"encoding/json"
	"os"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/dinstein/agent-hub/internal/jsonl"
)

// FileName is the stream's file name under <data>/logs.
const FileName = "events.jsonl"

// Scope names which kind of subject a record is about. It is the field that
// lets one file hold three vocabularies without them being confusable.
type Scope string

const (
	// ScopeServer: a downstream server entry in the registry.
	ScopeServer Scope = "server"
	// ScopeGateway: one `agenthub connect` process serving one client.
	ScopeGateway Scope = "gateway"
	// ScopeDaemon: the coordination plane process.
	ScopeDaemon Scope = "daemon"
)

// Kind is the closed vocabulary of what happened.
//
// CLOSED is the point. A consumer may switch on these values, colour a
// timeline by them or alert on them, none of which is safe against a
// free-text log message. Adding one means editing THREE places — the
// constant, allKinds below, and the table in docs/modules/foundation.md —
// and then WRITING it somewhere. test/buildrules fails until all four are
// true. The first three catch a kind a consumer cannot learn about: the
// event still gets written and only the reader meant to recognize it
// silently does not. The fourth catches the opposite and less obvious
// failure — a kind all three agree on that nothing emits, which is still
// offered as a `--kind` selector and answers "no events", the same answer
// as "this has not happened".
type Kind string

// Server-scope kinds: the life of one downstream connection.
const (
	// KindConnected: the handshake completed and tools were listed.
	KindConnected Kind = "connected"
	// KindConnectFailed: the connection attempt did not complete. Detail
	// carries the error.
	KindConnectFailed Kind = "connect_failed"
	// KindDisconnected: an established connection ended.
	KindDisconnected Kind = "disconnected"
	// KindRespawned: a stdio child died and was restarted. Count is the
	// respawn number.
	KindRespawned Kind = "respawned"
	// KindRespawnFailed: the restart itself failed; the server stays down.
	KindRespawnFailed Kind = "respawn_failed"
	// KindCircuitOpen: the breaker opened and is rejecting every call.
	KindCircuitOpen Kind = "circuit_open"
	// KindCircuitHalfOpen: the cooldown elapsed; one probe is admitted.
	KindCircuitHalfOpen Kind = "circuit_half_open"
	// KindCircuitClosed: the server answered again and calls resume.
	KindCircuitClosed Kind = "circuit_closed"
	// KindHealthDown: the health tracker flipped to not-answering. From/To
	// carry the states, because a flip is the fact and a level is not.
	KindHealthDown Kind = "health_down"
	// KindHealthUp: the health tracker flipped back.
	KindHealthUp Kind = "health_up"
	// KindToolsChanged: the server announced a new tool catalog.
	KindToolsChanged Kind = "tools_changed"
	// KindOAuthRefreshFailed: a token refresh failed; calls will start
	// failing when the current token expires.
	KindOAuthRefreshFailed Kind = "oauth_refresh_failed"
	// KindSecretsMissing: the connection is blocked on unresolved secrets.
	KindSecretsMissing Kind = "secrets_missing"
	// KindOAuthLoginStarted: an interactive login for this server began.
	KindOAuthLoginStarted Kind = "oauth_login_started"
	// KindOAuthLoginWaiting: the flow picked a mode and is now waiting on the
	// human — the URL to open, or the code to type, is in Detail.
	//
	// It is a kind of its own rather than elaboration on `started` because
	// the wait is the part that goes wrong: a login that never reaches this
	// point failed at discovery, and one that sits here for ten minutes is
	// waiting on somebody who never saw the browser tab. Those are different
	// problems and a timeline that cannot separate them is no help with
	// either.
	KindOAuthLoginWaiting Kind = "oauth_login_waiting"
	// KindOAuthLoginCompleted: a credential was obtained and stored.
	KindOAuthLoginCompleted Kind = "oauth_login_completed"
	// KindOAuthLoginFailed: the flow errored, timed out, or was cancelled.
	KindOAuthLoginFailed Kind = "oauth_login_failed"
)

// Gateway-scope kinds: the life of one client-serving process.
const (
	// KindGatewayStarted: the process is serving.
	KindGatewayStarted Kind = "started"
	// KindGatewayStopped: the process is shutting down.
	KindGatewayStopped Kind = "stopped"
	// KindClientAttached: the upstream MCP client completed initialize. There
	// is deliberately no client_detached to pair with it — a stdio gateway
	// serves exactly one client and ends when it does, so the departure is
	// already KindGatewayStopped, and a second kind for the same instant
	// would make one event look like two.
	KindClientAttached Kind = "client_attached"
	// KindRegistryReloadFailed: a configuration change could not be adopted,
	// so the process keeps serving the previous generation.
	KindRegistryReloadFailed Kind = "registry_reload_failed"
	// KindSessionOpened / KindSessionClosed bracket one MCP session on the
	// HTTP face, where a single process serves many of them at once.
	//
	// The stdio gateway has no counterpart and needs none: it serves exactly
	// one client over one pipe, so its session IS the process and `started`
	// and `stopped` already bracket it. Detail on the close says which way it
	// ended — the client said so, or it timed out — because a session nobody
	// closed and a session nobody used look identical afterwards.
	KindSessionOpened Kind = "session_opened"
	KindSessionClosed Kind = "session_closed"
)

// Daemon-scope kinds.
const (
	// KindDaemonStarted / KindDaemonStopping bracket the process.
	KindDaemonStarted  Kind = "started"
	KindDaemonStopping Kind = "stopping"
	// KindListenerBound: the HTTP data plane bound an address.
	KindListenerBound Kind = "listener_bound"
	// KindConfigReloaded: a registry generation was adopted.
	KindConfigReloaded Kind = "config_reloaded"
)

// scopeOrder is how scopes are offered in help text, hints and the published
// table: outermost subject first. Alphabetical order would put `daemon`
// before `gateway` before `server`, which reads as a containment hierarchy
// running the wrong way.
var scopeOrder = []Scope{ScopeServer, ScopeGateway, ScopeDaemon}

// allKinds is the closed set, grouped by the scope each kind belongs to.
//
// It is a map rather than a flat list because two scopes legitimately share
// a spelling — a gateway and the daemon both "start" — and a flat set could
// not say that `started` is meaningless at server scope. A reader validating
// a record checks the pair, never the kind alone.
//
// UNEXPORTED, with the functions below as the entire public answer to "what
// may appear here". Handing callers the raw map produced exactly what a
// closed vocabulary exists to prevent: two of them walked it themselves and
// wrote the same scope-or-any search twice, and the copy that could not see
// this list grew a hardcoded prose list of scopes with nothing left to keep
// the two in agreement.
var allKinds = map[Scope][]Kind{
	ScopeServer: {
		KindConnected, KindConnectFailed, KindDisconnected,
		KindRespawned, KindRespawnFailed,
		KindCircuitOpen, KindCircuitHalfOpen, KindCircuitClosed,
		KindHealthDown, KindHealthUp, KindToolsChanged,
		KindOAuthRefreshFailed, KindSecretsMissing,
		KindOAuthLoginStarted, KindOAuthLoginWaiting,
		KindOAuthLoginCompleted, KindOAuthLoginFailed,
	},
	ScopeGateway: {
		KindGatewayStarted, KindGatewayStopped,
		KindClientAttached, KindRegistryReloadFailed,
		KindSessionOpened, KindSessionClosed,
	},
	ScopeDaemon: {
		KindDaemonStarted, KindDaemonStopping,
		KindListenerBound, KindConfigReloaded,
	},
}

// ScopeNames lists every scope in presentation order, as strings, for help
// text and error hints. Callers want the names: the typed values are already
// reachable as the exported constants, and a []Scope accessor had no caller.
func ScopeNames() []string {
	out := make([]string, 0, len(scopeOrder))
	for _, s := range scopeOrder {
		out = append(out, string(s))
	}
	return out
}

// KnownScope reports whether scope is one this package defines.
func KnownScope(scope Scope) bool { return len(allKinds[scope]) > 0 }

// KnownKind reports whether kind may appear at scope.
//
// The EMPTY scope means "at any scope" — the question a reader that narrowed
// by kind alone is asking — and never "a scope that failed to validate".
// Scope is checked separately, by KnownScope, so a typo in one selector is
// never reported as a fault in the other.
func KnownKind(scope Scope, kind Kind) bool {
	if scope != "" {
		return slices.Contains(allKinds[scope], kind)
	}
	for _, kinds := range allKinds {
		if slices.Contains(kinds, kind) {
			return true
		}
	}
	return false
}

// KindNames lists the kinds valid at scope, or the whole vocabulary for the
// empty scope. Sorted and deduplicated: two scopes share the spelling
// `started`, and a hint printing it twice reads as two different things.
func KindNames(scope Scope) []string {
	seen := map[Kind]bool{}
	var out []string
	add := func(kinds []Kind) {
		for _, k := range kinds {
			if !seen[k] {
				seen[k] = true
				out = append(out, string(k))
			}
		}
	}
	if scope != "" {
		add(allKinds[scope])
	} else {
		for _, s := range scopeOrder {
			add(allKinds[s])
		}
	}
	slices.Sort(out)
	return out
}

// Record is one line of events.jsonl. Field order is frozen: the file is
// meant to be greppable and diffable across releases.
type Record struct {
	TS    time.Time `json:"ts"`
	Scope Scope     `json:"scope"`
	Kind  Kind      `json:"kind"`
	// Server is the registry server id (never a derived-instance key), set
	// at server scope. Inst names the derived instance, empty for the base
	// connection — the same split internal/logx's FieldServer/FieldInstance
	// make, and for the same reason.
	Server string `json:"server,omitempty"`
	Inst   string `json:"inst,omitempty"`
	// Client is the client whose gateway observed this. Empty for daemon
	// records, which belong to no client.
	Client string `json:"client,omitempty"`
	// PID is never omitted. A record written by no process is not a state
	// that exists, and with N gateways sharing this file it is the only way
	// to tell two writers apart.
	PID int `json:"pid"`
	// From/To carry a state transition where there is one. A flip is the
	// fact worth recording; a level sampled at some instant is not.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Detail is free text elaborating a failure. It is the only unbounded
	// field and is fitted to the line budget on write.
	Detail string `json:"detail,omitempty"`
	// Count is the one number a kind carries, and WHAT it counts is decided
	// by the kind — CountNoun is the whole list. It was called Attempt while
	// three writers put three different quantities in it, which made a
	// connect that listed thirteen tools render as a thirteenth attempt.
	//
	// One field rather than one per meaning: they are mutually exclusive by
	// kind, so separate fields would be a set of columns that are empty
	// except for the one the kind selects.
	Count int `json:"count,omitempty"`
	// Rev is a registry generation, and is NOT a count — it identifies a
	// config revision rather than tallying anything, so joining two records
	// on it is meaningful and adding two of them is not. internal/logx draws
	// the same line with FieldRev, and this field was folded into the count
	// until `config_reloaded` started rendering generation 17 as seventeen
	// of something.
	Rev uint64 `json:"rev,omitempty"`
	// DurMs is how long the thing being reported took.
	DurMs int64 `json:"durMs,omitempty"`
	// Session is the MCP session a record belongs to, on the face that has
	// them. Empty everywhere else, and last so the frozen field order above
	// is untouched — a record written before this field existed is still
	// byte-identical.
	//
	// The spelling matches logx.FieldSession for the reason Inst matches
	// TraceFrame's: a reader joining the two streams must not have to know
	// two names for one thing.
	Session string `json:"session,omitempty"`
}

// CountNoun names what Count holds for one kind, plural, or "" for a kind
// that carries no count.
//
// It lives here rather than in a renderer because it is the field's MEANING,
// not its presentation, and a meaning stated only in a doc comment is what
// let three writers disagree about it. Every consumer that prints the number
// reads this list, so none of them can label it differently.
func CountNoun(kind Kind) string {
	switch kind {
	case KindConnected, KindToolsChanged:
		return "tools"
	case KindRespawned, KindRespawnFailed:
		return "respawns"
	case KindDisconnected:
		return "reconnects"
	case KindCircuitOpen, KindHealthDown, KindHealthUp:
		return "failures"
	}
	return ""
}

const (
	// lineBudget bounds the SERIALIZED line. jsonl.Writer replaces anything
	// longer with an oversize marker and drops the record, so a field cap
	// alone does not honour it — escaping doubles quotes and expands control
	// bytes sixfold. Detail is fitted to this instead (see Append), which is
	// the fix internal/downstream's trace log needed after capping the raw
	// payload quietly did not work. -1 leaves room for the newline.
	lineBudget = jsonl.DefaultMaxLineBytes - 1
	// detailCap is the first, cheap cut: enough of an error to recognize it,
	// little enough that no single event can dominate the file.
	detailCap = 512
	// keepSegments is how many rotated segments survive a prune, newest
	// first, on top of the active file.
	keepSegments = jsonl.DefaultKeepSegments
)

// Stream appends Records. A nil *Stream is valid and does nothing.
type Stream struct {
	w     *jsonl.Writer
	clock func() time.Time
	pid   int
}

// Options configures Open.
type Options struct {
	// Clock overrides time.Now (tests).
	Clock func() time.Time
	// PID overrides os.Getpid (tests).
	PID int
}

// Open opens (creating if needed) the event stream at path. The parent
// directory must already exist.
//
// It also prunes old rotated segments. Pruning here rather than on a timer
// is deliberate: rotation happens at 32 MiB of state-change records, which
// is rare, while gateway processes open this file constantly — one per
// `agenthub connect`. So the check runs often in practice and costs one
// directory listing when it does. The bound it gives is "keepSegments plus
// whatever a single process rotated during its own lifetime", which for a
// stream of this shape is keepSegments.
func Open(path string, opts Options) (*Stream, error) {
	w, err := jsonl.NewWriter(path, jsonl.WriterOptions{Clock: opts.Clock})
	if err != nil {
		return nil, err
	}
	s := &Stream{w: w, clock: w.Clock(), pid: opts.PID}
	if s.pid == 0 {
		s.pid = os.Getpid()
	}
	jsonl.Prune(path, keepSegments)
	return s, nil
}

// Append enqueues one record. A zero TS is filled from the stream clock and
// normalized to UTC; PID is always stamped here rather than by the caller,
// so no call site can forget it.
//
// Never blocks and never fails: an unwritable record is dropped and counted.
func (s *Stream) Append(r Record) {
	if s == nil {
		return
	}
	if r.TS.IsZero() {
		r.TS = s.clock()
	}
	r.TS = r.TS.UTC()
	r.PID = s.pid
	if len(r.Detail) > detailCap {
		r.Detail = trimValidUTF8(r.Detail, detailCap)
	}
	line, err := json.Marshal(r)
	if err != nil {
		return
	}
	// Fit the serialized line by shrinking Detail. Each pass cuts by the
	// overflow measured in serialized bytes, so it converges in a couple of
	// rounds despite escaping making the relationship non-linear, and it
	// stops when Detail is empty rather than spinning on an envelope that
	// cannot fit on its own.
	for len(line) > lineBudget && r.Detail != "" {
		keep := len(r.Detail) - (len(line) - lineBudget)
		if keep >= len(r.Detail) {
			keep = len(r.Detail) - 1 // guarantee progress
		}
		if keep < 0 {
			keep = 0
		}
		r.Detail = trimValidUTF8(r.Detail, keep)
		if line, err = json.Marshal(r); err != nil {
			return
		}
	}
	s.w.AppendLine(line)
}

// Dropped reports records discarded by writer backpressure.
func (s *Stream) Dropped() uint64 {
	if s == nil {
		return 0
	}
	return s.w.Dropped()
}

// Sync blocks until everything queued so far has been written. Tests and
// shutdown use it; the data path never does.
func (s *Stream) Sync() {
	if s == nil {
		return
	}
	s.w.Sync()
}

// Close flushes and releases the file.
func (s *Stream) Close() error {
	if s == nil {
		return nil
	}
	return s.w.Close()
}

// trimValidUTF8 cuts a string to at most max bytes without splitting a rune.
// A truncated multi-byte sequence would make the line invalid JSON's problem
// rather than the reader's, so the cut backs off to a boundary.
func trimValidUTF8(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
