// Package services holds the Go side of the Wails3 GUI: one bound service
// that the frontend calls, plus the SSE-to-Wails event bridge.
//
// Hard constraint (canonical.md §2 rule 1, docs/modules/controlplane.md, enforced by
// depguard and proven by internal/depguardtest): this package may import
// only the public api package. It must never import internal/*, never read
// or write the data directory, and never speak MCP. Everything the GUI can
// do therefore has a control-plane endpoint, which means the CLI can do it
// too — "GUI is optional" is a compile-time property, not a promise.
//
// File split (ruling A.6 #3, see docs/canonical.md §7 item 3): the whole
// service body lives here WITHOUT a build tag so that it compiles, vets and
// is unit-tested on CI runners that have no GTK/WebKit development packages.
// Only the ~50 lines of Wails wiring sit behind `//go:build wails` in
// service_wails.go. If the Wails3 alpha ever fails to build, this file and
// the frontend survive untouched.
//
// The bound method set is split by what a write is guarded against, because
// that is the distinction a page has to get right:
//
//	hub.go       connection, the SSE bridge, and the runtime surfaces
//	             (servers list, sessions)
//	registry.go  registry-backed configuration — every write carries an
//	             expectedGeneration and can lose a compare-and-swap
//	nonreg.go    stores that are NOT the registry (secrets, skills, tokens,
//	             client files, OAuth) — no generation, no conflict
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dinstein/agent-hub/api"
)

// EventPrefix namespaces every event this service emits into the webview,
// so page code can never collide with Wails' own event names.
const EventPrefix = "agenthub:"

// Emitted event names. Topic events carry a TopicEvent payload; EventDaemon
// carries a Status.
const (
	// EventDaemon reports connection state changes to the daemon.
	EventDaemon = EventPrefix + "daemon"
	// EventServers/Sessions/Activity/Skills mirror the SSE topics.
	EventServers  = EventPrefix + api.TopicServers
	EventSessions = EventPrefix + api.TopicSessions
	EventActivity = EventPrefix + api.TopicActivity
	EventSkills   = EventPrefix + api.TopicSkills
)

// subscribedTopics is the closed set the bridge subscribes to. Each page
// filters for the one it cares about on the frontend side.
//
// It must stay a subset of what the daemon serves. An unlisted name is a
// 400 on the subscribe request, so one retired topic left here does not
// degrade to "that topic is quiet" — it takes the entire event stream
// down, and the UI falls back to whatever it loaded once. Every entry is
// therefore an api.Topic* constant: retiring one there breaks this list at
// compile time instead of at runtime.
var subscribedTopics = []string{
	api.TopicServers, api.TopicSessions,
	api.TopicActivity, api.TopicSkills,
}

// Emitter is the sink the SSE bridge pushes frontend events into. The Wails
// application implements it; tests substitute a recorder. Keeping the pump
// behind this interface is what lets the bridge be tested without a webview.
type Emitter interface {
	Emit(name string, data any)
}

// EmitterFunc adapts a function to Emitter.
type EmitterFunc func(name string, data any)

// Emit implements Emitter.
func (f EmitterFunc) Emit(name string, data any) { f(name, data) }

// TopicEvent is the payload delivered to the frontend for one daemon event.
//
// Events are notifications, not snapshots (canonical.md §5c): Payload may be
// absent or partial, and a page that needs authoritative state re-reads it
// through the corresponding list call. Rev is the registry generation for
// registry-backed topics and is monotonic — a page applies a re-read when
// the read generation is >= the applied one, never "equal to Rev".
type TopicEvent struct {
	Topic   string          `json:"topic"`
	Kind    string          `json:"kind"`
	Rev     uint64          `json:"rev"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Status is the daemon connection state shown by the GUI shell.
type Status struct {
	Connected bool   `json:"connected"`
	Socket    string `json:"socket"`
	Version   string `json:"version"`
	Pid       int    `json:"pid"`
	// Generation is the registry generation from the last successful ping.
	Generation uint64 `json:"generation"`
	// Error is the last connection failure message ("" when connected). It
	// is a message for humans; frontends must not parse it.
	Error string `json:"error,omitempty"`
}

// ErrOffline is returned by every data method while the daemon is not
// reachable. Fail direction: methods fail loudly rather than returning empty
// results — an empty server list and an unreachable daemon must never look
// the same in the UI.
var ErrOffline = errors.New("agenthub daemon is not reachable")

// dialer abstracts client construction so tests can inject a fake daemon.
// The two production implementations are api.DialOrStart (start allowed)
// and api.Default + Ping (dial only).
type dialer interface {
	// dial connects without starting anything.
	dial(ctx context.Context) (*api.Client, error)
	// dialOrStart connects, starting the daemon if necessary. The bool
	// reports whether THIS call started it — see api.DialOrStartSpawned.
	dialOrStart(ctx context.Context) (*api.Client, bool, error)
}

// Hub is the bound service body: every method the frontend can call, plus
// the SSE bridge. The Wails service type embeds it (service_wails.go), so
// the bound method set is exactly this type's exported methods.
//
// Safe for concurrent use: the webview calls methods from several goroutines
// and the pump runs on its own.
type Hub struct {
	dialer       dialer
	buildVersion string
	// emitter is fixed at construction; nil means "drop events" so that a
	// Hub built without a webview (tests, headless probes) still works.
	emitter Emitter

	// prefs holds the window-local preferences (window.go). Atomic rather
	// than mutex-guarded because the close hook reads it on the UI thread,
	// which must not queue behind a control-plane call holding mu. A nil
	// pointer means "the frontend has not pushed anything yet".
	prefs atomic.Pointer[WindowPrefs]

	// ready closes once the startup connect has finished, successfully or
	// not. It exists because the window renders — and its pages start
	// calling — while that connect is still in flight: without the gate the
	// first paint reliably produced "daemon is not reachable" for a daemon
	// that was seconds from being up. Created in start; nil for a Hub that
	// was never started, where use must not block at all.
	ready chan struct{}

	mu     sync.Mutex
	client *api.Client
	status Status
	// spawned records that OUR startup connect launched the daemon, which is
	// what licenses stop to shut it down again. A daemon we merely found
	// belongs to whoever started it.
	spawned bool
	// pumpCancel stops the running SSE bridge, if any.
	pumpCancel context.CancelFunc
	pumpDone   chan struct{}
}

// realDialer is the production dialer.
type realDialer struct{}

func (realDialer) dial(ctx context.Context) (*api.Client, error) {
	c, err := api.Default()
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := c.Ping(pctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (realDialer) dialOrStart(ctx context.Context) (*api.Client, bool, error) {
	return api.DialOrStartSpawned(ctx, api.StartOptions{})
}

// NewHub returns a Hub that talks to the platform-default control socket.
// No I/O happens until start or the first method call.
func NewHub(e Emitter) *Hub { return &Hub{dialer: realDialer{}, emitter: e} }

// ApplicationVersion returns the immutable GUI build identity supplied by
// main. It is deliberately separate from Status.Version, which describes the
// daemon the GUI happens to be connected to and may name a different build.
func (h *Hub) ApplicationVersion() string { return h.buildVersion }

// start connects to the daemon (starting it if needed) and brings up the
// SSE bridge. It never blocks the caller: the GUI window must open even when
// the daemon is down, otherwise a broken daemon leaves the user with no
// surface to diagnose it from. Connection outcome arrives as an EventDaemon
// event and via Status.
func (h *Hub) start(ctx context.Context) {
	h.mu.Lock()
	if h.ready == nil {
		h.ready = make(chan struct{})
	}
	ready := h.ready
	h.mu.Unlock()
	go func() {
		// Closed on BOTH outcomes: a failed startup must release the waiters
		// too, or every page would block for its whole context instead of
		// showing the offline state the window exists to diagnose.
		defer close(ready)
		if _, err := h.connect(ctx, true); err != nil {
			return // status/event already published by connect
		}
	}()
}

// stop tears down the bridge, releases the client, and shuts the daemon down
// again if this process is the one that started it. It is idempotent.
//
// The ownership test is what keeps this safe. A daemon we spawned exists only
// because the GUI wanted one, so leaving it behind on exit strands a process
// the user never asked for. A daemon that was already running belongs to
// whoever started it — a terminal, a login item, another client mid-session —
// and taking it down would end their work to tidy up after ours.
func (h *Hub) stop() {
	h.mu.Lock()
	cancel, done := h.pumpCancel, h.pumpDone
	h.pumpCancel, h.pumpDone = nil, nil
	c := h.client
	h.client = nil
	pid := h.status.Pid
	ours := h.spawned
	h.spawned = false
	h.status = Status{Socket: h.status.Socket}
	h.mu.Unlock()

	defer func() {
		if ours {
			stopDaemon(pid)
		}
	}()

	if cancel != nil {
		cancel()
		<-done
	}
	if c != nil {
		c.Close()
	}
}

// Connect forces a connection attempt, starting the daemon if necessary. It
// backs the "start daemon / retry" button; every other method only dials.
func (h *Hub) Connect(ctx context.Context) (Status, error) {
	if _, err := h.connect(ctx, true); err != nil {
		return h.Status(), err
	}
	return h.Status(), nil
}

// Status reports the last known daemon connection state without doing I/O.
func (h *Hub) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

// connect establishes the client and (re)starts the pump. allowStart selects
// DialOrStart over a plain dial.
func (h *Hub) connect(ctx context.Context, allowStart bool) (*api.Client, error) {
	h.mu.Lock()
	if h.client != nil {
		c := h.client
		h.mu.Unlock()
		return c, nil
	}
	h.mu.Unlock()

	var (
		c   *api.Client
		err error
	)
	// Every connect WRITES the ownership claim, it never merely raises it.
	// dialOrStart answers "did this call start the daemon" both ways, and
	// only the true half used to be recorded — so a false, which is
	// dialOrStart saying "I found one already running", was discarded and
	// whatever the claim happened to hold survived. Combined with the
	// clearing in dropClient below, h.spawned now describes the daemon this
	// Hub is talking to RIGHT NOW rather than one it once started.
	if allowStart {
		var spawned bool
		c, spawned, err = h.dialer.dialOrStart(ctx)
		if err == nil {
			h.mu.Lock()
			h.spawned = spawned
			h.mu.Unlock()
		}
	} else {
		c, err = h.dialer.dial(ctx)
		if err == nil {
			// dial only ever FINDS a daemon. Reaching here means the
			// previous client was dropped, so anything answering now
			// started without us.
			h.mu.Lock()
			h.spawned = false
			h.mu.Unlock()
		}
	}
	if err != nil {
		h.setStatus(Status{Error: err.Error()})
		return nil, err
	}

	// Ping once so Status carries the daemon's identity from the start.
	st := Status{Connected: true, Socket: c.SocketPath()}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	hello, perr := c.Ping(pctx)
	cancel()
	if perr == nil {
		st.Version, st.Pid, st.Generation = hello.Version, hello.Pid, hello.Generation
	}

	h.mu.Lock()
	if h.client != nil { // lost a race with another connect
		other := h.client
		h.mu.Unlock()
		c.Close()
		return other, nil
	}
	h.client = c
	h.status = st
	h.mu.Unlock()

	h.emit(EventDaemon, st)
	h.startPump()
	return c, nil
}

// use returns a live client or ErrOffline. It only dials — it never starts a
// daemon, so a daemon that keeps dying cannot be re-spawned once per click.
func (h *Hub) use(ctx context.Context) (*api.Client, error) {
	h.mu.Lock()
	c := h.client
	ready := h.ready
	h.mu.Unlock()
	if c != nil {
		return c, nil
	}
	// Wait out a startup connect that is still running. The window paints
	// before the daemon finishes launching, so without this the first calls
	// of every page raced the spawn and reported the daemon unreachable
	// moments before it answered. Bounded by ctx alone: the connect closes
	// ready on failure too, so the only way to sit here is a launch that is
	// genuinely still in progress.
	if ready != nil {
		select {
		case <-ready:
			h.mu.Lock()
			c = h.client
			h.mu.Unlock()
			if c != nil {
				return c, nil
			}
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %v", ErrOffline, ctx.Err())
		}
	}
	c, err := h.connect(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	return c, nil
}

// dropClient forgets a client that failed at transport level so the next
// call re-dials. Control-plane errors (a well-formed error envelope) leave
// the connection alone: the daemon answered, it just said no.
func (h *Hub) dropClient(err error) {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return
	}
	h.mu.Lock()
	c := h.client
	h.client = nil
	// The ownership claim dies with the client that carried it. It named a
	// daemon reached over THIS connection, and a transport-level failure
	// means that daemon is gone or unreachable; leaving the claim standing
	// let the next connect — a plain dial, which cannot spawn — inherit it
	// and point it at somebody else's daemon, which the GUI then SIGTERMed
	// on window close.
	h.spawned = false
	h.status = Status{Socket: h.status.Socket, Error: err.Error()}
	cancel, done := h.pumpCancel, h.pumpDone
	h.pumpCancel, h.pumpDone = nil, nil
	st := h.status
	h.mu.Unlock()

	if cancel != nil {
		cancel()
		<-done
	}
	if c != nil {
		c.Close()
	}
	h.emit(EventDaemon, st)
}

func (h *Hub) setStatus(st Status) {
	h.mu.Lock()
	if st.Socket == "" {
		st.Socket = h.status.Socket
	}
	h.status = st
	h.mu.Unlock()
	h.emit(EventDaemon, st)
}

func (h *Hub) emit(name string, data any) {
	if h.emitter != nil {
		h.emitter.Emit(name, data)
	}
}

// startPump brings up the SSE bridge for the current client.
func (h *Hub) startPump() {
	h.mu.Lock()
	if h.pumpCancel != nil || h.client == nil {
		h.mu.Unlock()
		return
	}
	c := h.client
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	h.pumpCancel, h.pumpDone = cancel, done
	h.mu.Unlock()

	go func() {
		defer close(done)
		h.pump(ctx, c)
	}()
}

// pump subscribes to the daemon event stream and republishes every event as
// a Wails event. The api client reconnects internally (with Last-Event-ID
// resumption), so this loop only has to retry the initial subscribe.
//
// The topics are a CLOSED set at the daemon, so subscribedTopics has to be a
// subset of what it serves: an unlisted name is a 400 on the subscribe, and
// this loop would then retry forever against an error that cannot clear.
func (h *Hub) pump(ctx context.Context, c *api.Client) {
	const (
		retryMin = 250 * time.Millisecond
		retryMax = 5 * time.Second
	)
	backoff := retryMin
	for {
		ch, err := c.Events.Subscribe(ctx, subscribedTopics...)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, retryMax)
			continue
		}
		backoff = retryMin
		for ev := range ch {
			h.emit(EventPrefix+ev.Topic, TopicEvent{
				Topic: ev.Topic, Kind: ev.Kind, Rev: ev.Rev, Payload: ev.Payload,
			})
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Bound methods. Every one of these maps to exactly one control-plane call.
//
// They are deliberately thin. No bound method validates its arguments: the
// authoritative check lives in internal/confops, which is the ONE
// implementation the CLI and the control plane share
// (docs/modules/controlplane.md). A pre-check here would be a second
// validator that can — and eventually would — disagree with it, and the GUI
// must not be able to accept or refuse anything the CLI cannot.
// ---------------------------------------------------------------------------

// call runs one control-plane call on a live client.
//
// It concentrates the three things every bound method must do identically:
// fail with ErrOffline rather than an empty result when the daemon is
// unreachable, hand transport failures to dropClient so the next call
// re-dials, and leave the connection alone when the daemon merely said no.
func call[T any](ctx context.Context, h *Hub, fn func(*api.Client) (T, error)) (T, error) {
	var zero T
	c, err := h.use(ctx)
	if err != nil {
		return zero, err
	}
	out, err := fn(c)
	if err != nil {
		h.dropClient(err)
		return zero, err
	}
	return out, nil
}

// ListServers returns the configured servers with the server-computed Health
// display contract (docs/modules/controlplane.md). The frontend renders Health verbatim and
// never re-derives status from other fields.
//
// This is the RUNTIME view. The stored definition an edit form needs is
// GetServer, which answers with the generation the following write must
// carry — see registry.go.
func (h *Hub) ListServers(ctx context.Context) ([]api.Server, error) {
	return call(ctx, h, func(c *api.Client) ([]api.Server, error) {
		return c.Servers.List(ctx)
	})
}

// ServerHealth returns one server's Health.
//
// It filters ListServers rather than calling a per-server endpoint: the list
// payload and the `servers` SSE payload are the same bytes, so there is only
// one place where Health can come from and no second endpoint to drift.
func (h *Hub) ServerHealth(ctx context.Context, id string) (api.Health, error) {
	servers, err := h.ListServers(ctx)
	if err != nil {
		return api.Health{}, err
	}
	for _, s := range servers {
		if s.ID == id {
			return s.Health, nil
		}
	}
	return api.Health{}, fmt.Errorf("server %q not found", id)
}

// ListSessions returns the live sessions. Sessions are runtime objects: an
// empty list from a reachable daemon means "nobody is connected", which is
// why ErrOffline exists as a separate outcome.
func (h *Hub) ListSessions(ctx context.Context) ([]api.SessionInfo, error) {
	return call(ctx, h, func(c *api.Client) ([]api.SessionInfo, error) {
		return c.Sessions.List(ctx)
	})
}

// ListSkills returns the skills library with its install matrix. A daemon
// that does not serve the endpoint answers api.ErrCodeNotFound and the page
// renders "unavailable" — never an empty library.
//
// The rest of the skills surface is in nonreg.go.
func (h *Hub) ListSkills(ctx context.Context) ([]api.Skill, error) {
	return call(ctx, h, func(c *api.Client) ([]api.Skill, error) {
		return c.Skills.List(ctx)
	})
}

// ErrorKindConflict is the `kind` MarshalError stamps on a lost
// optimistic-concurrency check, and the ONLY error a page answers by
// re-reading and re-applying the user's intent rather than by reporting a
// failure ("this was changed somewhere else, reloaded").
//
// It is a kind and not a code because the code is already spoken for:
// E_STALE_PRECONDITION is one of SEVERAL 409s (a duplicate name, a skills
// target that drifted, a login already in flight), and none of the others
// gets better by re-reading. A page that branched on the status or on "some
// 409" would send the user into a retry loop that cannot succeed.
const ErrorKindConflict = "conflict"

// MarshalError converts an error into the JSON value the frontend receives
// as the `cause` of a rejected binding call. Control-plane errors keep their
// machine-readable code so pages can branch (offline vs. not-implemented vs.
// already-decided); anything else degrades to a message.
//
// A stale-precondition refusal additionally carries kind:"conflict" and, when
// the daemon reported it, currentGeneration. The field is ABSENT when the
// daemon did not report one: 0 is the wire spelling of "do not check", so
// feeding a defaulted 0 back as the next expectedGeneration would turn the
// retry into the blind overwrite the precondition exists to prevent. A page
// that finds no currentGeneration re-reads to obtain one.
//
// It must never fail: a nil return falls back to Wails' default handling.
func MarshalError(err error) []byte {
	if err == nil {
		return nil
	}
	type wireError struct {
		Code           string   `json:"code"`
		Message        string   `json:"message"`
		Hint           string   `json:"hint,omitempty"`
		MissingSecrets []string `json:"missingSecrets,omitempty"`
		Status         int      `json:"status,omitempty"`
		Offline        bool     `json:"offline,omitempty"`
		// Kind classifies the failure by the RESPONSE it calls for, where a
		// code alone is ambiguous. Empty for everything but a conflict.
		Kind string `json:"kind,omitempty"`
		// CurrentGeneration is where the registry actually stands after a
		// conflict — what the retry sends as its expectedGeneration.
		CurrentGeneration uint64 `json:"currentGeneration,omitempty"`
	}
	we := wireError{Code: "E_GUI", Message: err.Error()}
	var apiErr *api.Error
	// The conflict test comes first: *ConflictError unwraps to *api.Error,
	// so the generic branch would match it and drop the generation.
	if conflict, ok := api.AsConflict(err); ok {
		hint := conflict.Err.Hint
		if hint == "" {
			hint = "the configuration changed elsewhere; reload and re-apply"
		}
		we = wireError{
			Code: conflict.Err.Code, Message: conflict.Err.Message, Hint: hint,
			Status: conflict.Err.Status,
			Kind:   ErrorKindConflict, CurrentGeneration: conflict.CurrentGeneration,
		}
	} else {
		switch {
		case errors.As(err, &apiErr):
			we = wireError{
				Code: apiErr.Code, Message: apiErr.Message, Hint: apiErr.Hint,
				MissingSecrets: apiErr.MissingSecrets, Status: apiErr.Status,
			}
		case errors.Is(err, ErrOffline):
			we = wireError{Code: "E_OFFLINE", Message: err.Error(), Offline: true,
				Hint: "start the daemon with `agenthub daemon start`"}
		}
	}
	b, merr := json.Marshal(we)
	if merr != nil {
		return nil
	}
	return b
}

// stopDaemon asks the daemon we started to shut down, by SIGTERM to the pid
// its own ping reported. That signal is the daemon's graceful path — stop
// accepting, drain in-flight work, remove daemon.json — the same one
// `agenthub daemon stop` uses.
//
// Every failure is silent and non-fatal. This runs while the application is
// already tearing down, so there is no surface left to report on, and the
// worst case is the pre-existing behaviour: a daemon outliving the GUI. It
// is deliberately not escalated to SIGKILL — a daemon that needs longer to
// drain is finishing work (a tool call in flight, a token write), and killing
// it to speed up our own exit would be the wrong trade.
func stopDaemon(pid int) {
	if pid <= 0 {
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Signal(syscall.SIGTERM)
}
