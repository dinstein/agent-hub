package httpbridge

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// Ingress hard bounds (docs/modules/controlplane.md; the numbers are inherited
// verbatim from toolport's ingress limits, per canonical.md §5). They are hard because they bound work an UNAUTHENTICATED
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
)

// Frozen rejection bodies. "not found" in particular is ONE sentence for
// every miss — unknown session, expired session, foreign session, unknown
// path — so the endpoint cannot be probed for which sessions exist
// (docs/modules/controlplane.md, "The 404 text is unified and frozen byte for
// byte"; the same anti-enumeration rule canonical.md §7 #4 freezes for
// describe_tool and fetch_result).
var (
	errUnauthorized = &httpError{http.StatusUnauthorized, CodeUnauthorized, "missing or invalid credentials"}
	errNotFound     = &httpError{http.StatusNotFound, CodeNotFound, "not found"}
	errOverloaded   = &httpError{http.StatusServiceUnavailable, CodeOverloaded, "too many requests in flight"}
	errCrossSite    = &httpError{http.StatusForbidden, CodeForbidden, "cross-site requests are not accepted"}
	errBodyTooLarge = &httpError{http.StatusRequestEntityTooLarge, CodePayloadTooBig, "request body exceeds the ingress limit"}
	errNoStream     = &httpError{http.StatusMethodNotAllowed, CodeMethodNotAllow, "this endpoint does not offer a server-initiated stream"}
	errMethod       = &httpError{http.StatusMethodNotAllowed, CodeMethodNotAllow, "method not allowed"}
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
// Failure direction: reject. AddrIsLoopback is false for anything it cannot
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
	if !AddrIsLoopback(host) {
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
	accept := r.Header.Get("Accept")
	if strings.TrimSpace(accept) == "" {
		return true
	}
	for _, part := range strings.Split(accept, ",") {
		media := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		switch media {
		case "*/*", "application/*", "application/json":
			return true
		}
	}
	return false
}
