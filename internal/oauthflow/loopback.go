package oauthflow

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LoopbackTimeout is the total wall-clock budget for one loopback
// authorization: bind → browser → consent → callback. 180s is the value
// docs/modules/oauth.md fixes; past it the listener is torn down so a stale one can
// never intercept a later flow.
const LoopbackTimeout = 180 * time.Second

// DefaultCallbackPath is the path the redirect URI points at. Any path is
// accepted on the listener (see the handler) — this is just what gets
// registered.
const DefaultCallbackPath = "/callback"

// LoopbackListener is a bound, not-yet-serving loopback callback endpoint.
//
// # Why the listener is bound before the browser opens
//
// Two independent reasons, both learned the hard way:
//
//  1. Racing the redirect. Some authorization servers (and some browser
//     password managers with auto-submit) complete the round trip in
//     milliseconds. Opening the browser first leaves a window in which the
//     redirect arrives at a port nobody is listening on and the user sees a
//     connection-refused page with the code stranded in the URL bar.
//
//  2. The port must be known to build the URL. redirect_uri has to be in
//     the authorization request AND registered with the AS, so the port has
//     to exist before either happens.
//
// # Why a fresh random port every time
//
// The listener binds 127.0.0.1:0 and lets the OS pick. A FIXED callback
// port is the classic bug: an earlier authorization the user abandoned
// leaves its listener alive, and it — not the new flow — accepts the new
// callback. The new flow then times out while the stale one reports a state
// mismatch, and the state check (which is doing its job) gets blamed and
// disabled. Random ports make cross-flow interception structurally
// impossible instead of merely detected.
//
// The cost is that providers requiring an exact pre-registered redirect_uri
// cannot use a random port. For those, State.CallbackPort persists the port
// that was registered and ListenOnPort re-binds it; if it is occupied, the
// caller discards the DCR credentials and re-registers rather than silently
// binding a different port (docs/modules/oauth.md).
//
// (The BSD/macOS "clear O_NONBLOCK on the accepted socket" caveat from the
// Rust implementation has no analogue here: Go's netpoller owns the
// accepted connection's flags and net/http never sees a raw fd.)
type LoopbackListener struct {
	ln   net.Listener
	port int
	path string
	// host overrides the hostname RedirectURI advertises. Empty means the
	// 127.0.0.1 literal (the correct default). The socket always binds
	// 127.0.0.1 either way.
	host string

	mu       sync.Mutex
	state    string
	srv      *http.Server
	results  chan callbackResult
	serveErr chan error
	once     sync.Once
}

// Listen binds 127.0.0.1 on an OS-assigned port.
func Listen() (*LoopbackListener, error) { return ListenOnPort(0) }

// ListenOnPort binds 127.0.0.1 on a specific port; port 0 means
// OS-assigned. A busy fixed port returns the bind error unchanged so the
// caller can distinguish it and re-register (see the type doc).
func ListenOnPort(port int) (*LoopbackListener, error) {
	return listenLoopback(port, "", DefaultCallbackPath)
}

// listenLoopback binds the callback endpoint. host and path override what
// RedirectURI advertises; both empty means the defaults (127.0.0.1 literal,
// /callback). The socket ALWAYS binds 127.0.0.1 regardless of host — see
// (*LoopbackListener).RedirectURI for why the advertised name may differ.
func listenLoopback(port int, host, path string) (*LoopbackListener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("oauthflow: bind loopback callback: %w", err))
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("oauthflow: loopback listener has no TCP address"))
	}
	if path == "" {
		path = DefaultCallbackPath
	}
	return &LoopbackListener{ln: ln, port: addr.Port, path: path, host: host}, nil
}

// Port is the bound port.
func (l *LoopbackListener) Port() int { return l.port }

// RedirectURI is the value to register and to send as redirect_uri.
//
// It uses the 127.0.0.1 literal, not "localhost": RFC 8252 §8.3 requires
// the literal, because "localhost" can resolve to ::1 (or, on a poisoned
// resolver, elsewhere) and then the registered URI and the browser's actual
// target disagree.
//
// An explicit host (from --redirect-uri) overrides that. The rule above is
// what agenthub should ASK for; it is not something agenthub can impose on
// a provider whose allowlist already contains a literal "localhost" entry.
// Sending the 127.0.0.1 form to such a provider is rejected outright —
// Google compares redirect URIs byte for byte — so refusing to spell it
// their way would not uphold RFC 8252, it would just make those servers
// unusable. The socket still binds 127.0.0.1: only the advertised name
// differs, and a "localhost" that resolves to ::1 fails loudly at redirect
// time rather than silently landing somewhere else.
func (l *LoopbackListener) RedirectURI() string {
	host := l.host
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(l.port)) + l.path
}

// Close releases the listener. Safe to call twice; Wait closes it too.
func (l *LoopbackListener) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ln == nil {
		return nil
	}
	err := l.ln.Close()
	l.ln = nil
	return err
}

// callbackResult is one accepted callback.
type callbackResult struct {
	code string
	iss  string // RFC 9207 iss parameter, "" when absent
	err  error
}

// Serve starts answering callbacks for the given state. It returns as soon
// as the server goroutine is running.
//
// Serve exists separately from Wait because binding is not enough: a socket
// in the accept backlog with nobody serving it holds the browser's redirect
// open until somebody does. The full ordering the loopback mode needs is
//
//	bind → SERVE → open browser → wait
//
// so that a redirect arriving in the same millisecond as the browser launch
// gets an actual response instead of stalling. Calling Serve twice, or with
// a different state, is a programming error and returns one.
func (l *LoopbackListener) Serve(state string) error {
	if strings.TrimSpace(state) == "" {
		return newFlowError(ErrorTypeAuthorization, fmt.Errorf("oauthflow: loopback serve needs a state"))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.srv != nil {
		if l.state != state {
			return newFlowError(ErrorTypeAuthorization,
				fmt.Errorf("oauthflow: loopback listener already serving a different authorization"))
		}
		return nil
	}
	if l.ln == nil {
		return newFlowError(ErrorTypeAuthorization, fmt.Errorf("oauthflow: loopback listener is closed"))
	}
	l.state = state
	l.results = make(chan callbackResult, 1)
	l.serveErr = make(chan error, 1)
	deliver := func(r callbackResult) {
		l.once.Do(func() { l.results <- r })
	}
	l.srv = &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { l.handle(w, r, state, deliver) }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	srv, ln, errCh := l.srv, l.ln, l.serveErr
	go func() { errCh <- srv.Serve(ln) }()
	return nil
}

// Wait serves the callback endpoint (if Serve has not already been called)
// until it sees an authorization result or the deadline expires. It always
// shuts the server and releases the port before returning.
//
// Acceptance rules, in order:
//
//   - A request carrying neither `code` nor `error` — a favicon fetch, a
//     browser probe, a bare GET / — is ignored: answered 204 and not
//     treated as the callback.
//   - Any other request with a MISSING or WRONG `state` fails the flow with
//     ErrStateMismatch, whether it carries a code or an error. This is
//     deliberately loud rather than ignored: with a random port per flow
//     there is no benign explanation for it, so it means either a
//     cross-flow mix-up or someone on the machine feeding us a callback,
//     and silently continuing to wait would hide both. It applies to error
//     responses because RFC 6749 §4.1.2.1 obliges the AS to echo state
//     there as well — a stranger's error is not this flow's outcome.
//   - A request carrying `error` fails the flow with the AS's own error
//     code (the user pressed Deny, or the AS refused the request), and
//     returns the `iss` alongside it so the caller can apply RFC 9207
//     before acting on the error.
//   - A request carrying `code` succeeds.
func (l *LoopbackListener) Wait(ctx context.Context, state string, timeout time.Duration) (code, iss string, err error) {
	if timeout <= 0 {
		timeout = LoopbackTimeout
	}
	if err := l.Serve(state); err != nil {
		_ = l.Close()
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	l.mu.Lock()
	srv, results, serveErr := l.srv, l.results, l.serveErr
	l.mu.Unlock()

	serveErrConsumed := false
	defer func() {
		// Shutdown drains the in-flight response (the browser must get its
		// "you can close this tab" page) and closes the listener. The port
		// MUST be released here: a listener outliving its flow is exactly
		// the stale-interceptor bug random ports exist to prevent.
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
		if !serveErrConsumed {
			<-serveErr
		}
		l.mu.Lock()
		l.ln, l.srv = nil, nil
		l.mu.Unlock()
	}()

	select {
	case r := <-results:
		return r.code, r.iss, r.err
	case err := <-serveErr:
		serveErrConsumed = true
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return "", "", newFlowError(ErrorTypeAuthorization, fmt.Errorf("oauthflow: callback server: %w", err))
		}
		return "", "", newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("%w: callback listener closed before a callback arrived", ErrTimeout))
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			e := newFlowError(ErrorTypeAuthorization,
				fmt.Errorf("%w: no callback within %s", ErrTimeout, timeout))
			e.Suggestion = "complete the consent screen in the browser, or use `--manual` if this host cannot open one"
			return "", "", e
		}
		return "", "", newFlowError(ErrorTypeAuthorization, ctx.Err())
	}
}

func (l *LoopbackListener) handle(w http.ResponseWriter, r *http.Request, wantState string, deliver func(callbackResult)) {
	q := r.URL.Query()
	code := q.Get("code")
	asErr := q.Get("error")

	switch {
	case code == "" && asErr == "":
		// Stray request (favicon, probe, manual GET). Ignore it: it is not
		// the callback and must not end the flow.
		w.WriteHeader(http.StatusNoContent)
	case q.Get("state") != wantState:
		// Checked BEFORE the error branch, and for the error branch too.
		// RFC 6749 §4.1.2.1 obliges an AS to echo state on an error response
		// exactly as on a success, so a mismatch here is never the genuine
		// AS. Without this, anything that reaches the loopback port during
		// the flow could end it — no state to guess — and put its own
		// error_description into what the operator reads.
		writeCallbackPage(w, http.StatusBadRequest, "Unexpected callback",
			"This callback did not match the pending authorization request. Nothing was saved.")
		e := newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("%w: callback state does not match this authorization request", ErrStateMismatch))
		e.Suggestion = "re-run the login; do not paste a callback URL from a different session"
		deliver(callbackResult{err: e})
	case asErr != "":
		te := &TokenError{Code: asErr, Description: q.Get("error_description"), URI: q.Get("error_uri"), HTTPStatus: 0}
		writeCallbackPage(w, http.StatusOK, "Authorization failed", "You can close this tab and return to the terminal.")
		fe := newFlowError(ErrorTypeAuthorization, te)
		if te.Code == errAccessDenied {
			fe.Err = fmt.Errorf("%w: %w", ErrAuthorizationDenied, te)
		}
		// iss travels with the failure: RFC 9207 validation applies to an
		// error response too, and on mismatch the caller MUST NOT act on or
		// display the AS's error members. The caller cannot check what it
		// was never handed.
		deliver(callbackResult{iss: q.Get("iss"), err: fe})
	default:
		writeCallbackPage(w, http.StatusOK, "Authorized",
			"agenthub received the authorization code. You can close this tab and return to the terminal.")
		deliver(callbackResult{code: code, iss: q.Get("iss")})
	}
}

// writeCallbackPage renders the minimal browser-facing page. title and msg
// are package constants at every call site — nothing from the request
// reaches this function — so the page cannot be turned into a
// reflected-XSS or token-display surface.
//
// The page is static and scriptless, success included. An earlier version
// ran a countdown and then called window.close(), but browsers refuse that
// for a tab the script did not open: the OAuth tab is opened by the OS
// handler and arrives with several history entries, failing both halves of
// the closeable test. The close therefore never fired in practice, and the
// countdown only promised something the browser had already vetoed before
// swapping in a "you can close it now" line anyway. Telling the user that
// directly is the same outcome, minus the wait and the false promise.
func writeCallbackPage(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>%s</title>"+
		"<body style=\"font-family:system-ui;padding:3rem\"><h1>%s</h1><p>%s</p></body>",
		title, title, msg)
}

// BrowserOpener opens a URL in the user's browser. It returns an error when
// this host has none, which is the trigger to fall back to manual mode.
type BrowserOpener func(url string) error

// NoBrowser is a BrowserOpener that always fails. Wire it for --no-browser
// so mode selection takes the manual path through the same code.
func NoBrowser(string) error {
	return errors.New("oauthflow: browser opening disabled")
}

// LoopbackFlow runs the full loopback authorization: bind, build URL, open
// browser, wait.
type LoopbackFlow struct {
	// Open opens the authorization URL. Required.
	Open BrowserOpener
	// Timeout overrides LoopbackTimeout.
	Timeout time.Duration
	// FixedPort re-binds a previously registered callback port instead of
	// asking the OS for a fresh one. Non-zero only for providers demanding
	// an exact redirect_uri match.
	FixedPort int
	// FixedHost and FixedPath override what the redirect URI advertises,
	// for providers whose allowlist holds a spelling the defaults cannot
	// produce (e.g. "localhost" rather than 127.0.0.1, or a callback path
	// other than /callback). Empty means the default for each.
	FixedHost string
	FixedPath string
}

// LoopbackResult is what Run produces.
type LoopbackResult struct {
	Code        string
	RedirectURI string
	Port        int
	// Iss is the RFC 9207 iss authorization-response parameter, "" when the
	// AS sent none. The login flow validates it before redeeming Code — and
	// before acting on a failure, which is why Run returns a result
	// carrying only this field when the callback was an error response.
	Iss string
}

// Run executes the loopback mode. The caller supplies build, which turns
// the bound redirect URI into the authorization URL — that indirection is
// what lets the caller register the redirect URI with the AS (DCR) between
// the bind and the browser opening, without Run needing to know about
// registration.
//
// Sequence invariant: bind → build (register) → SERVE → open browser →
// wait. Run enforces it; do not reorder. Serving before the browser opens
// is what makes an instantaneous redirect safe — a bound-but-unserved
// socket would leave the browser hanging in the accept backlog instead of
// refusing, which is better but still not right.
func (f *LoopbackFlow) Run(ctx context.Context, state string, build func(redirectURI string) (string, error)) (*LoopbackResult, error) {
	ln, err := listenLoopback(f.FixedPort, f.FixedHost, f.FixedPath)
	if err != nil {
		return nil, err
	}
	// From here on every path must release the listener. Wait closes it;
	// the early-error paths below close it explicitly.
	redirect := ln.RedirectURI()
	authURL, err := build(redirect)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	if f.Open == nil {
		_ = ln.Close()
		return nil, newFlowError(ErrorTypeAuthorization, fmt.Errorf("oauthflow: no browser opener configured"))
	}
	if err := ln.Serve(state); err != nil {
		_ = ln.Close()
		return nil, err
	}
	if err := f.Open(authURL); err != nil {
		_ = ln.Close()
		e := newFlowError(ErrorTypeAuthorization, fmt.Errorf("oauthflow: open browser: %w", err))
		e.Suggestion = "no browser on this host; re-run with `--manual` or `--device`"
		return nil, e
	}
	code, iss, err := ln.Wait(ctx, state, f.Timeout)
	if err != nil {
		// A non-nil result WITH a non-nil error, and only in this branch:
		// RFC 9207 applies to an error response too, and the caller cannot
		// validate an iss it was never handed. Code is empty, so nothing
		// here can be mistaken for success — read it through issOf.
		return &LoopbackResult{RedirectURI: redirect, Port: ln.port, Iss: iss}, err
	}
	return &LoopbackResult{Code: code, RedirectURI: redirect, Port: ln.port, Iss: iss}, nil
}

// issOf reads the iss out of a LoopbackResult that may be nil, which it is
// for every Run failure before the callback arrived.
func issOf(res *LoopbackResult) string {
	if res == nil {
		return ""
	}
	return res.Iss
}

// ParseLoopbackRedirectURI splits a pinned redirect URI into the host, port
// and path a LoopbackFlow needs.
//
// It is deliberately strict, because this value decides where a browser
// will deliver an authorization code:
//
//   - the scheme must be http — https on loopback needs a certificate no
//     local listener has, and accepting it would produce a URI that can
//     never receive the redirect;
//   - the host must be a LOOPBACK name: the 127.0.0.0/8 or ::1 literals, or
//     "localhost". Anything else would hand the code to another machine,
//     which is the whole attack this check exists to prevent — and agenthub
//     cannot serve it either way, since the listener binds 127.0.0.1;
//   - the port must be present and non-zero. A pinned URI exists to match
//     an allowlist entry exactly, and an OS-assigned port cannot do that;
//     silently substituting one would reintroduce the mismatch the flag was
//     added to fix.
//
// Query and fragment are rejected rather than dropped: a caller who pasted
// one is working from a different URI than the one that will be sent, and
// quietly trimming it would hide that.
func ParseLoopbackRedirectURI(raw string) (host string, port int, path string, err error) {
	u, perr := url.Parse(strings.TrimSpace(raw))
	if perr != nil {
		return "", 0, "", fmt.Errorf("oauthflow: parse redirect uri: %w", perr)
	}
	if u.Scheme != "http" {
		return "", 0, "", fmt.Errorf(
			"oauthflow: redirect uri %q must use http (a loopback listener cannot serve https)", raw)
	}
	host = u.Hostname()
	if !isLoopbackHost(host) {
		return "", 0, "", fmt.Errorf(
			"oauthflow: redirect uri %q must point at loopback (127.0.0.1, ::1 or localhost), not %q", raw, host)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", 0, "", fmt.Errorf(
			"oauthflow: redirect uri %q must not carry a query or fragment", raw)
	}
	p, aerr := strconv.Atoi(u.Port())
	if aerr != nil || p <= 0 {
		return "", 0, "", fmt.Errorf(
			"oauthflow: redirect uri %q needs an explicit port (it must match the provider's allowlist exactly)", raw)
	}
	path = u.EscapedPath()
	if path == "" {
		path = DefaultCallbackPath
	}
	return host, p, path, nil
}

// isLoopbackHost reports whether host names this machine's loopback
// interface. "localhost" is accepted by name because that is how some
// providers spell their allowlist entry; every other value must be a
// loopback IP literal.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CallbackPortOf extracts the port from a redirect URI, for persisting
// State.CallbackPort — the port a provider requiring an exact redirect_uri
// match must see again after a restart. Returns 0 when there is none.
func CallbackPortOf(redirectURI string) int {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		return 0
	}
	return p
}
