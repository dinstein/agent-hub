package httpbridge

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/guard/netguard"
)

// Ingress hard bounds (docs/subsystems/docs/subsystems/controlplane.md; the numbers are inherited
// verbatim from toolport's ingress limits, per docs/conventions.md#reference-code-policy). They are hard because they bound work an UNAUTHENTICATED
// caller can cause: header and body reads happen before, or alongside,
// credential checking.
const (
	// MaxHeaderBytes bounds the request head.
	MaxHeaderBytes = 64 << 10 // 64 KiB
	// HeaderReadTimeout bounds how long a client may take to send the head.
	// It is the slowloris bound: a connection that sends one byte a minute
	// costs a goroutine and nothing else, but it costs it forever.
	HeaderReadTimeout = 10 * time.Second
	// MaxBodyBytes bounds one JSON-RPC message.
	MaxBodyBytes = 4 << 20 // 4 MiB
	// BodyReadTimeout bounds the body read once the head is in.
	BodyReadTimeout = 30 * time.Second
	// MaxInFlight bounds concurrently executing requests. Past it the
	// server sheds load with 503 rather than queueing: a queue behind a
	// saturated downstream pool converts a slow server into an unbounded
	// memory sink.
	MaxInFlight = 256
	// MaxStreams bounds concurrently OPEN notification streams, and is a
	// second quota rather than a slice of the first because the two limit
	// different things. MaxInFlight bounds WORK an unauthenticated caller
	// can induce, and every request it covers is short by construction. A
	// notification stream is the opposite: it does no work at all after it
	// opens, and it stays open for hours. Counted against MaxInFlight, 256
	// idle streams would shed every real call on the server while consuming
	// nothing — the cap would be enforcing the wrong thing, loudly.
	//
	// A stream is only reachable AFTER authentication, so this quota bounds
	// what a credential holder can hold open, not what an anonymous caller
	// can cause. It is below MaxSessions on purpose: an open response costs
	// more than a table entry, and a client needs at most one stream per
	// session, never the reverse.
	MaxStreams = 64
)

// httpError is a rejection with its status and stable code. Codes are ABI
// for anything parsing the error body.
type httpError struct {
	Status  int
	Code    string
	Message string
}

func (e *httpError) Error() string { return e.Code + ": " + e.Message }

// Stable ingress rejection codes.
const (
	CodeUnauthorized   = "unauthorized"
	CodeForbidden      = "forbidden"
	CodePayloadTooBig  = "payload_too_large"
	CodeBadRequest     = "bad_request"
	CodeNotFound       = "not_found"
	CodeOverloaded     = "overloaded"
	CodeMethodNotAllow = "method_not_allowed"
	// CodeInternal is this server failing, not the request being wrong. It
	// was already the spelling asHTTPError falls back to; naming it made it
	// usable by the one path that has a specific internal failure to report.
	CodeInternal = "internal"
)

// Frozen rejection bodies. "not found" in particular is ONE sentence for
// every miss — unknown session, expired session, foreign session, unknown
// path — so the endpoint cannot be probed for which sessions exist
// (docs/subsystems/docs/subsystems/controlplane.md, "The 404 text is unified and frozen byte for
// byte"; the same anti-enumeration rule docs/decisions/0004-toon-and-signature-grammars.md freezes for
// describe_tool and fetch_result).
var (
	errUnauthorized = &httpError{http.StatusUnauthorized, CodeUnauthorized, "missing or invalid credentials"}
	errNotFound     = &httpError{http.StatusNotFound, CodeNotFound, "not found"}
	errOverloaded   = &httpError{http.StatusServiceUnavailable, CodeOverloaded, "too many requests in flight"}
	errCrossSite    = &httpError{http.StatusForbidden, CodeForbidden, "cross-site requests are not accepted"}
	errBodyTooLarge = &httpError{http.StatusRequestEntityTooLarge, CodePayloadTooBig, "request body exceeds the ingress limit"}
	// errNoStream answers a GET that did not ask for the stream. The stream
	// itself exists (stream.go); what this rejects is a GET with an Accept
	// header that rules out text/event-stream, which the specification
	// answers with exactly this status.
	errNoStream  = &httpError{http.StatusMethodNotAllowed, CodeMethodNotAllow, "this endpoint offers only a text/event-stream response to GET"}
	errNoStreams = &httpError{http.StatusServiceUnavailable, CodeOverloaded, "too many notification streams are open"}
	// Distinct from errNoStreams, and it was not at first. A quota shed and a
	// gateway that would not assemble are different operator problems — wait
	// and retry, versus go and read the logs — and the ordering invariant
	// this package documents requires every rejection to be distinguishable.
	// Reporting both as 503 `overloaded` made a broken assembly read as load,
	// which is the one reading that sends nobody to look at it.
	errStreamSetup = &httpError{http.StatusInternalServerError, CodeInternal,
		"the notification stream could not be opened"}
	errMethod = &httpError{http.StatusMethodNotAllowed, CodeMethodNotAllow, "method not allowed"}
	// Outside the frozen set above, and deliberately: that rule unifies
	// every answer about an id that was PRESENTED, so the endpoint cannot
	// be probed for which sessions exist. This one answers a request that
	// presented none, names no session, and reveals nothing. The
	// specification asks for 400 here (≤ 2025-11-25 transports, "Session
	// Management") because the client rule attached to 404 is "start a new
	// session" — which for a caller that simply omitted the header is a
	// loop, not a recovery.
	errSessionRequired = &httpError{http.StatusBadRequest, CodeBadRequest,
		"this request requires an Mcp-Session-Id header"}
)

// semaphore is the in-flight limiter.
type semaphore chan struct{}

func newSemaphore(n int) semaphore { return make(semaphore, n) }

// acquire takes a slot without blocking. false = shed.
func (s semaphore) acquire() bool {
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s semaphore) release() { <-s }

// slot is one held semaphore acquisition that can be handed back EARLY.
//
// It exists for the one request shape that stops being a request: a
// notification stream acquires an in-flight slot like everything else, then
// gives it back before parking for hours on its own quota. Release is
// idempotent so the handler's `defer` stays exactly where it is and stays
// correct either way — the alternative, a bool the deferred call consults,
// puts the decision in two places and is how a slot ends up released twice.
type slot struct {
	sem  semaphore
	once sync.Once
}

func (s *slot) release() {
	if s == nil {
		return
	}
	s.once.Do(func() { s.sem.release() })
}

// checkFetchMetadata rejects browser-originated cross-site requests.
//
// Sec-Fetch-Site is set by the browser and cannot be forged by page script,
// which is exactly what makes it useful here: a non-browser client omits it
// (and is unaffected), while a malicious page on another origin cannot hide
// that it is one. "none" (address bar) and "same-origin" pass; "cross-site"
// and "same-site" do not — a sibling subdomain is not this endpoint's
// business either.
func checkFetchMetadata(r *http.Request) error {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "", "none", "same-origin":
		return nil
	default:
		return errCrossSite
	}
}

// checkOrigin rejects a browser request that did not come from a page this
// endpoint could plausibly have served, and with it DNS rebinding.
//
// It used to accept any Origin equal to the request's Host, with the comment
// that this stopped rebinding because "an attacker page that resolves its own
// hostname to 127.0.0.1 still sends its own Origin". The premise is true and
// the conclusion does not follow: under rebinding the Host header carries the
// attacker's hostname too. A page served from http://evil.example:7777 whose
// DNS has been rebound to 127.0.0.1 sends Origin AND Host as
// evil.example:7777, they compare equal, and Sec-Fetch-Site reads
// "same-origin" — because from the browser's point of view it IS same-origin,
// which is also why no preflight is sent. Equality was the one relation
// rebinding preserves.
//
// So both authorities must be provably loopback, not merely equal to each
// other. That is what a local UI served from this endpoint sends, and it is
// what an attacker cannot produce without already being on this machine.
//
// Failure direction: reject. netguard.AddrIsLoopback is false for anything it cannot
// prove (a hostname such as 127.0.0.1.nip.io, an unparsable authority), and
// the check runs before authentication, so a false positive costs a
// browser-shaped client a 403 while a false negative costs tool execution.
//
// CORS INVARIANT: this server never reflects Origin and never emits
// Access-Control-Allow-*. There is no browser client to enable, so the only
// effect of a permissive CORS header would be to let a page read tool
// results.
func checkOrigin(r *http.Request) error {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return nil // non-browser client
	}
	host := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	if host != r.Host {
		return errCrossSite
	}
	if !netguard.AddrIsLoopback(host) {
		return errCrossSite
	}
	return nil
}

// readBody reads at most MaxBodyBytes with a read deadline, so neither a
// huge body nor a slow one can be used to hold resources.
//
// The deadline is set through ResponseController: it must apply to THIS
// request only. A server-wide ReadTimeout would also bound long-lived
// connections, and the head timeout (ReadHeaderTimeout) already covers the
// pre-auth phase.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	// A transport without deadline support answers ErrNotSupported; the
	// MaxBytesReader bound below still holds, so this degrades rather than
	// fails.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(BodyReadTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errBodyTooLarge
		}
		return nil, &httpError{http.StatusBadRequest, CodeBadRequest, "request body could not be read"}
	}
	return body, nil
}

// acceptsJSON reports whether the client will take an application/json
// answer. An absent Accept header is treated as "anything" — MCP clients
// send the pair "application/json, text/event-stream", plain HTTP tooling
// sends nothing, and rejecting the latter would make the endpoint
// undebuggable for no security gain.
func acceptsJSON(r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("Accept")) == "" {
		return true
	}
	return acceptsMedia(r, "*/*", "application/*", "application/json")
}

// acceptsMedia reports whether the request's Accept header names any of want.
//
// It shares only the SPLIT — comma-separated entries, everything after the
// first semicolon dropped — because that is the whole of what its two callers
// had in common, and carrying it twice made "how does this face read an Accept
// header" a question with two answers.
//
// What an ABSENT header means is deliberately NOT decided here: acceptsJSON
// reads silence as "anything" and acceptsSSE reads it as "not a stream", and
// each states its reason at its own site. Folding that into a parameter would
// put two different arguments behind one boolean.
func acceptsMedia(r *http.Request, want ...string) bool {
	for part := range strings.SplitSeq(r.Header.Get("Accept"), ",") {
		media := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if slices.Contains(want, media) {
			return true
		}
	}
	return false
}
