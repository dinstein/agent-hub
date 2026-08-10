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
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/httpbridge"
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
// predicate is httpbridge.AddrIsLoopback, shared with the daemon's listener
// so that "is this loopback" has one answer repo-wide, and it already
// resolves everything it cannot prove to false.
func Serve(addr string) (*Server, error) {
	if !httpbridge.AddrIsLoopback(addr) {
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
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}}
	go func() { _ = s.srv.Serve(ln) }()
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
