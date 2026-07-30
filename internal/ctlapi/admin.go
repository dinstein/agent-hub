package ctlapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/audit"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/integrity"
	"github.com/dinstein/agent-hub/internal/registry"
)

// The configuration surface (docs/modules/controlplane.md) is the control
// plane's half of the "one semantic layer, two front ends" split: the CLI
// calls internal/confops in-process, the GUI calls it through these
// endpoints. NOTHING here validates a value, decides a side effect or writes
// a document — every rule lives in confops, so the two front ends cannot
// drift. This file owns transport concerns only: decoding, the optimistic-
// concurrency guard, the error-to-status mapping, the audit record and the
// change notification.
//
// Uniform 404 (anti-probing, same rule as the rest of the package): an
// unknown route, an unknown method AND an unknown resource id all answer the
// identical body. A confops KindNotFound therefore loses its specific code
// on the way out — the CLI still gets E_SERVER_NOT_FOUND in-process, the
// wire does not.
//
// Endpoints whose subject is NOT the registry (tool governance, quarantine)
// answer the uniform 404 when their state directory was not injected. That
// is the same "this daemon does not serve it" shape a frontend already
// handles for an older daemon, and it keeps a half-wired server from writing
// to a directory nobody chose.

// CodeStalePrecondition rejects a write whose expected_generation no longer
// matches the registry: someone else wrote in between. The error body
// carries the CURRENT generation so the client can re-read and retry against
// a known version instead of guessing (docs/flows.md §4).
//
// It is the same string internal/confops freezes; adminErrorCodeContract
// asserts the two agree.
const CodeStalePrecondition = "E_STALE_PRECONDITION"

// maxAuditTail bounds one /v1/audit or /v1/security read, and
// defaultAuditTail is what an unspecified limit selects. The api client
// clamps to the same ceiling; the server clamps again because a client is
// not a trusted bound.
const (
	defaultAuditTail = 50
	maxAuditTail     = 1000
)

// auditValueLimit truncates the old/new values recorded for a governance
// write. The audit line must stay a line: a result-budget value is short,
// but the field is operator input and nothing else bounds it.
const auditValueLimit = 64

// pathSegments matches prefix on the ESCAPED path and returns exactly want
// unescaped segments.
//
// Working on the escaped path is the anti-smuggling invariant shared with
// sessionPathID: a %2F inside an id must not become a path separator, so the
// split happens BEFORE unescaping and a segment that unescapes to something
// containing a slash is rejected outright.
func pathSegments(r *http.Request, prefix string, want int) ([]string, bool) {
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), prefix)
	if !ok || rest == "" {
		return nil, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != want {
		return nil, false
	}
	out := make([]string, 0, want)
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
		v, err := url.PathUnescape(p)
		if err != nil || v == "" || strings.Contains(v, "/") {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// preconditionWire is the optimistic-concurrency guard every write body may
// carry. Absent (or 0) means "do not check" — the scripting path, whose
// behaviour matches the CLI's.
//
// Two spellings are accepted because the control-plane DTOs are snake_case
// while the design document spells the field expectedGeneration. They are
// aliases of ONE value, never two settings: a body carrying both with
// different numbers is refused rather than silently resolved, because
// guessing which one the caller meant is guessing about a concurrency guard.
type preconditionWire struct {
	Snake *uint64 `json:"expected_generation,omitempty"`
	Camel *uint64 `json:"expectedGeneration,omitempty"`
}

// preconditionFrom resolves the guard from the body and the query string.
// DELETE carries no body, so the query spelling is not a convenience: it is
// the only way those endpoints can be guarded at all.
func preconditionFrom(r *http.Request, body []byte) (confops.Precondition, error) {
	var seen []uint64
	if len(body) > 0 {
		var wire preconditionWire
		if err := json.Unmarshal(body, &wire); err != nil {
			return confops.Precondition{}, fmt.Errorf("decoding expected_generation: %w", err)
		}
		if wire.Snake != nil {
			seen = append(seen, *wire.Snake)
		}
		if wire.Camel != nil {
			seen = append(seen, *wire.Camel)
		}
	}
	q := r.URL.Query()
	for _, key := range []string{"expected_generation", "expectedGeneration"} {
		raw := q.Get(key)
		if raw == "" {
			continue
		}
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return confops.Precondition{}, fmt.Errorf("%s must be a generation number, got %q", key, raw)
		}
		seen = append(seen, n)
	}
	for _, v := range seen {
		if v != seen[0] {
			return confops.Precondition{}, errors.New(
				"expected_generation given more than once with different values")
		}
	}
	if len(seen) == 0 {
		return confops.Precondition{}, nil
	}
	return confops.Precondition{Generation: seen[0]}, nil
}

// writeResultWire is the common tail of every write response: where the
// registry now stands (so the next write can be guarded without a re-read),
// whether anything actually changed, and the healed-quarantine reports
// confops demotes to warnings.
type writeResultWire struct {
	Generation uint64   `json:"generation"`
	Changed    bool     `json:"changed"`
	Warnings   []string `json:"warnings,omitempty"`
}

func resultWire(res confops.Result) writeResultWire {
	return writeResultWire{Generation: res.Generation, Changed: res.Changed, Warnings: res.Warnings}
}

// writeOpsError maps a confops (or state-store) failure onto the wire.
//
// The mapping is the control plane's half of the table internal/cli owns for
// exit codes — same Kind vocabulary, different target:
//
//	KindUsage    400  the caller's code, verbatim
//	KindNotFound 404  the UNIFORM body (the specific code is dropped)
//	KindConflict 409  the caller's code (a name already taken)
//	KindDenied   403  a guard refused the shape, not the argument
//	KindState    500  a state file that must not be read as empty
//	KindStale    409  plus the current generation, see CodeStalePrecondition
//
// Failure direction: anything unclassified becomes a 500. An unrecognized
// error is a bug or an I/O fault, never a "probably fine" 200.
func (s *Server) writeOpsError(w http.ResponseWriter, r *http.Request, err error) {
	reqID := requestIDFrom(r.Context())
	if stale, ok := confops.AsStale(err); ok {
		writeStale(w, stale.Got, reqID)
		return
	}
	var oe *confops.Error
	if errors.As(err, &oe) {
		switch oe.Kind {
		case confops.KindUsage:
			writeErr(w, http.StatusBadRequest, oe.Code, oe.Message, oe.Hint, reqID)
		case confops.KindNotFound:
			writeNotFound(w, r)
		case confops.KindConflict:
			writeErr(w, http.StatusConflict, oe.Code, oe.Message, oe.Hint, reqID)
		case confops.KindDenied:
			writeErr(w, http.StatusForbidden, oe.Code, oe.Message, oe.Hint, reqID)
		case confops.KindState:
			writeErr(w, http.StatusInternalServerError, oe.Code, oe.Message, oe.Hint, reqID)
		case confops.KindStale:
			// Kind without the typed StaleError (defensive): still a 409, but
			// with no generation to echo.
			writeErr(w, http.StatusConflict, CodeStalePrecondition, oe.Message, oe.Hint, reqID)
		default:
			writeErr(w, http.StatusInternalServerError, CodeInternal, oe.Message, oe.Hint, reqID)
		}
		return
	}
	// The state stores below confops speak their own sentinels.
	switch {
	case errors.Is(err, integrity.ErrNotFound):
		writeNotFound(w, r)
	case errors.Is(err, integrity.ErrStoreCorrupt):
		writeErr(w, http.StatusInternalServerError, confops.CodeStateCorrupt, err.Error(),
			"the state file is left in place for inspection; agenthub refuses to read it as empty", reqID)
	case errors.Is(err, integrity.ErrLockTimeout), errors.Is(err, registry.ErrLockTimeout):
		writeErr(w, http.StatusInternalServerError, CodeInternal, err.Error(),
			"another process holds the cross-process lock; retry", reqID)
	default:
		s.log.Warn("ctlapi: configuration write failed", "path", r.URL.Path, "error", err)
		writeErr(w, http.StatusInternalServerError, CodeInternal, err.Error(), "", reqID)
	}
}

// writeStale is the 409 of a lost compare-and-swap. The body carries the
// generation the registry is actually at — a client that only ever guessed
// would loop forever on a blind retry.
func writeStale(w http.ResponseWriter, current uint64, reqID string) {
	writeErrGen(w, http.StatusConflict, CodeStalePrecondition,
		fmt.Sprintf("the configuration changed since it was read (it is now at generation %d)", current),
		"re-read the configuration and retry against the current generation", reqID, current)
}

// adminAudit is one configuration write as the audit stream records it.
//
// docs/modules/controlplane.md: every control-plane WRITE is audited, failures
// included —
// an attempt that was refused is exactly the line an operator looks for.
type adminAudit struct {
	// action is the frozen verb, e.g. "servers/add:github". It lands in the
	// record's Tool field, which is where the control plane has always put
	// its action names ("sessions/scope", "grants/decide:<id>").
	action string
	server string
	client string
	// body binds the record to the exact request via ArgsHash. Argument
	// BYTES never enter the stream — the record type has no field for them.
	body []byte
	err  error
	dur  time.Duration
}

func (s *Server) auditAdmin(r *http.Request, a adminAudit) {
	if s.opts.Audit == nil {
		return
	}
	decision := audit.DecisionAllowed
	if a.err != nil {
		decision = audit.DecisionDenied
	}
	hash := ""
	if len(a.body) > 0 {
		h, err := audit.ArgsHash(a.body)
		if err != nil {
			// Recorded rather than dropped: a line without a hash is still
			// evidence, a missing line is not.
			h = "unhashable"
		}
		hash = h
	}
	s.opts.Audit.Append(audit.Record{
		Actor:     actorFrom(r.Context()),
		Client:    a.client,
		Server:    a.server,
		Tool:      a.action,
		ArgsHash:  hash,
		Decision:  decision,
		DurMs:     a.dur.Milliseconds(),
		RequestID: requestIDFrom(r.Context()),
	})
}

// auditValue renders one governance value for an audit line: single-line and
// bounded, so a pasted blob cannot smear one record across the file.
func auditValue(v string) string {
	if v == "" {
		return "<unset>"
	}
	v = strings.ReplaceAll(strings.ReplaceAll(v, "\n", " "), "\r", " ")
	if len(v) > auditValueLimit {
		v = v[:auditValueLimit] + "…"
	}
	return v
}

// publishRegistryChange announces a write this process just made.
//
// It exists because registry self-write suppression works: the daemon's
// watcher shares this Store, so it deliberately does NOT re-emit our own
// write, and without this call a GUI edit would be the one change nobody is
// told about. The frames are byte-identical to the watcher's, so "someone
// else changed it" and "I changed it" look the same downstream — which is
// exactly what docs/modules/controlplane.md asks for.
func (s *Server) publishRegistryChange(kind registry.DocKind, rev uint64) {
	if s.opts.Bus == nil {
		return
	}
	s.opts.Bus.Publish(event.Event{
		Topic:   TopicRegistry,
		Key:     string(kind),
		Payload: registry.Change{Kind: kind, Rev: rev},
	})
	if kind == registry.DocServers {
		// Prefix "server." maps onto the coalesced `servers` SSE topic; the
		// payload is rebuilt server-side at fire time.
		s.opts.Bus.Publish(event.Event{Topic: "server.registry", Key: string(kind)})
	}
}

// stateOptions locates the state directory for the operations whose subject
// is not the registry. ok=false means no directory was injected, and the
// caller answers the uniform 404.
func (s *Server) stateOptions() (confops.StateOptions, bool) {
	if s.opts.StateDir == "" {
		return confops.StateOptions{}, false
	}
	return confops.StateOptions{Dir: s.opts.StateDir, LockTimeout: s.opts.StateLockTimeout}, true
}

// generation is the registry generation as of this Server's snapshot; every
// read response carries it so the write that follows can be guarded without
// a second round trip.
func (s *Server) generation() uint64 {
	return s.opts.Registry.Snapshot().Generation
}

// checkSnapshotPrecondition is the WEAK form of the guard, for writes whose
// subject is not the registry (tool governance, quarantine). Those files
// have their own cross-process locks, so the registry generation can move
// between this comparison and the write.
//
// It therefore catches "the operator's view is stale", not "nothing changed
// under me" — the same distinction confops draws between check and
// checkSnapshot. Registry writes get the strong form, inside the lock.
func (s *Server) checkSnapshotPrecondition(pre confops.Precondition) error {
	current := s.generation()
	if pre.Generation == 0 || pre.Generation == current {
		return nil
	}
	return &confops.StaleError{Want: pre.Generation, Got: current}
}

// readAdminBody reads and bounds one write body. An empty body is legal
// (DELETE), and every handler that needs fields validates them itself.
func readAdminBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", requestIDFrom(r.Context()))
		return nil, false
	}
	return body, true
}

// decodeAdminBody decodes a required JSON body into v.
func decodeAdminBody(w http.ResponseWriter, r *http.Request, body []byte, v any) bool {
	if len(body) == 0 {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "a JSON body is required", "", requestIDFrom(r.Context()))
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "decoding body: "+err.Error(), "",
			requestIDFrom(r.Context()))
		return false
	}
	return true
}

// adminPrecondition resolves the guard or answers 400.
func adminPrecondition(w http.ResponseWriter, r *http.Request, body []byte) (confops.Precondition, bool) {
	pre, err := preconditionFrom(r, body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", requestIDFrom(r.Context()))
		return confops.Precondition{}, false
	}
	return pre, true
}

// toolSelectionWire is the three-state tool selector on the wire. Mode is
// mandatory: an absent mode is REFUSED by confops rather than defaulted,
// because every default here is a guess about how much a client may see.
type toolSelectionWire struct {
	// Mode is "all" (drop the rule), "only" (narrow to Tools) or "none"
	// (block every tool of the server).
	Mode string `json:"mode"`
	// Tools are the RAW downstream tool names, required by "only".
	Tools []string `json:"tools,omitempty"`
}

func (t toolSelectionWire) selection() confops.ToolSelection {
	return confops.ToolSelection{Mode: toolSelectMode(t.Mode), Tools: t.Tools}
}

// toolSelectMode maps the wire spelling onto the confops mode. An unknown
// spelling maps to the UNSET mode, which confops refuses — the unknown case
// must never land on the loose one.
func toolSelectMode(mode string) confops.ToolSelectMode {
	switch mode {
	case "all":
		return confops.ToolSelectAll
	case "only":
		return confops.ToolSelectOnly
	case "none":
		return confops.ToolSelectNone
	default:
		return confops.ToolSelectUnset
	}
}
