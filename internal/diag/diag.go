// Package diag serves the Go runtime's own profiles — heap, goroutines,
// allocations, mutex and CPU — over HTTP, so a live agenthub process can be
// asked where its memory and its goroutines went. It is the answer to
// questions no log line can carry: a stdio gateway holding 21 downstreams
// sits at roughly 28MB of physical footprint against 8MB of live heap, and
// only an allocation profile says which call site owns the difference.
//
// The endpoint does not exist unless AGENTHUB_PPROF_ADDR names an address,
// and it is never reachable off the machine: a non-loopback address is
// REFUSED, not quietly downgraded to loopback. That is the same direction
// internal/daemon takes for its MCP listener, and for a sharper reason here.
// These profiles are the process memory itself — a heap dump of a gateway
// holds downstream credentials, tool arguments and tool results in whatever
// form they had when the snapshot was taken, and a goroutine dump names
// every downstream it is talking to. A listener reachable from the network
// is therefore a credential disclosure with a stable URL, not a debugging
// convenience, so this package has no equivalent of the daemon's
// allow-remote escape hatch: there is no address that opens it up.
//
// Binding loopback-only stops the network, but not a local browser under DNS
// rebinding: a page served from evil.example:PORT whose name is rebound to
// 127.0.0.1 is same-origin from the browser's point of view and can fetch
// /debug/pprof/heap and read the response — see internal/httpbridge's
// checkOrigin for the mechanism, which this package's request-level guard
// reuses rather than re-deriving. Every request is checked before it reaches
// the pprof handlers, fail-closed: any Origin header refuses the request
// outright (there is no browser client to serve here, so none is ever
// legitimate — stricter than the bridge, which has one), and the Host header
// must independently prove loopback via netguard.AddrIsLoopback, since under
// rebinding Host carries the attacker's chosen name rather than this
// process's own address.
//
// A failed bind FAILS THE PROCESS START rather than continuing without an
// endpoint. An operator who asked for profiles and silently did not get them
// would attach to a port that never answers and conclude the process was
// wedged, which is the diagnosis this package exists to prevent.
//
// That firmness has a consequence worth planning for: one gateway process
// runs per client, so a single fixed port set in a shell profile lets the
// first gateway start and breaks every one after it with "address already in
// use". Port 0 is the intended spelling for that case — every process binds
// its own ephemeral port and reports it — which is why Server.Addr reads the
// listener rather than echoing the request.
package diag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/guard/netguard"
)

// EnvAddr is the environment variable that turns the endpoint on by naming
// the address to serve it at. An environment variable rather than a flag
// because the process that most needs profiling is 'agenthub connect', whose
// argv belongs to the AI client's configuration file and is not the
// operator's to edit.
const EnvAddr = "AGENTHUB_PPROF_ADDR"

// ErrNotLoopback is returned by Serve for an address that is not provably
// loopback. It is a distinct error because the caller reports it to a human
// who has to be told the address was refused rather than adjusted.
var ErrNotLoopback = errors.New("diag: refusing to serve profiles on a non-loopback address")

// Addr returns the address the profiling endpoint was asked to serve at, or
// "" when AGENTHUB_PPROF_ADDR is unset or blank — the ordinary case, in
// which no listener is created and nothing is logged.
func Addr() string { return strings.TrimSpace(os.Getenv(EnvAddr)) }

// Server is a running profiling endpoint. Close stops it.
type Server struct {
	ln  net.Listener
	srv *http.Server
}

// Serve starts the profiling endpoint on addr, which must be loopback. The
// returned Server reports the address actually bound, which differs from the
// request whenever the port was 0.
//
// Failure direction: fail closed on every count. A non-loopback address is
// refused, and so is one whose loopback-ness cannot be proven — the
// predicate is netguard.AddrIsLoopback, shared with the daemon's listener
// so that "is this loopback" has one answer repo-wide, and it already
// resolves everything it cannot prove to false.
func Serve(addr string) (*Server, error) {
	if !netguard.AddrIsLoopback(addr) {
		return nil, fmt.Errorf("%w: %q. Profiles are the process memory — they carry "+
			"downstream credentials and call payloads — so there is no allow-remote for "+
			"them. Use a loopback address such as 127.0.0.1:0", ErrNotLoopback, addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("diag: cannot serve profiles on %s: %w", addr, err)
	}

	// A private mux, never http.DefaultServeMux. Importing net/http/pprof
	// registers its handlers on the default mux as a side effect, and a
	// package that decides its own exposure must not depend on which mux
	// some other server in this process happens to pass to http.Server.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// No WriteTimeout: a CPU profile or an execution trace is a request that
	// stays open for its whole duration (30s by default, and '?seconds=' can
	// ask for more), and a write deadline would truncate exactly the long
	// profile that was worth taking. ReadHeaderTimeout still bounds a client
	// that connects and says nothing.
	s := &Server{ln: ln, srv: &http.Server{
		Handler:           requestGuard(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// requestGuard wraps the pprof mux with the one check loopback binding alone
// cannot provide. The bind address (checked once, above, in Serve) proves
// where the socket sits; it says nothing about who a browser thinks it is
// talking to once DNS has been rebound to point at it — see the package doc
// and internal/httpbridge/ingress.go's checkOrigin, which spells out the
// rebinding mechanism this reuses rather than repeats.
//
// It is STRICTER than checkOrigin: the bridge serves a local UI, so a
// same-origin request from that UI is legitimate traffic it must let
// through. This package serves no browser client at all, so there is no
// Origin value — same-origin or otherwise — that is ever legitimate: ANY
// Origin header refuses the request, without comparing it to Host.
//
// Host still has to be checked independently, because an absent Origin is
// also what a rebound page's simple GET for an image or script produces, and
// because a non-browser client's Host is the only authority available at
// all: under rebinding the attacker's DNS answer is what the browser puts in
// Host, so equality with anything derived from the request proves nothing
// and Host must instead be checked against netguard.AddrIsLoopback, which
// resolves everything it cannot prove to false.
//
// Failure direction: fail closed, like Serve's own address check — anything
// this cannot prove safe (a present Origin, a Host that is not provably
// loopback) is refused with a bare 403, no body detail, before reaching any
// pprof handler.
func requestGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" || !netguard.AddrIsLoopback(r.Host) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ServeFromEnv starts the endpoint described by AGENTHUB_PPROF_ADDR and
// announces where it landed. It returns (nil, nil) when the variable is
// unset — the ordinary case — so a caller can assemble it unconditionally:
//
//	prof, err := diag.ServeFromEnv(log)
//	if err != nil {
//		return fmt.Errorf("gateway: %w", err)
//	}
//	defer func() { _ = prof.Close() }()
//
// The announcement goes through the process logger rather than to stderr
// because port 0 — the spelling that works when several gateways run at
// once — makes the log the only place the bound port is written down, and
// 'agenthub logs' is where an operator will look for it.
func ServeFromEnv(log *slog.Logger) (*Server, error) {
	addr := Addr()
	if addr == "" {
		return nil, nil
	}
	s, err := Serve(addr)
	if err != nil {
		return nil, err
	}
	if log != nil {
		log.Info("profiling endpoint serving", "addr", s.Addr(),
			"heap", "http://"+s.Addr()+"/debug/pprof/heap")
	}
	return s, nil
}

// Addr reports the address the endpoint is actually listening on, which is
// what the operator needs when the requested port was 0.
func (s *Server) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Close stops the endpoint. It is safe on a nil Server so a caller that
// never started one can defer it unconditionally.
func (s *Server) Close() error {
	if s == nil || s.srv == nil {
		return nil
	}
	// Shutdown rather than Close: an in-flight CPU profile is the one
	// request whose result is lost by hanging up on it, and the caller is
	// already on its way out, so a bounded wait costs nothing else.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}
