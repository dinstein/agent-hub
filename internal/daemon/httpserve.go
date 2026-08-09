package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/registry"
)

// This file turns the configured listen address into a running MCP endpoint.
//
// Two properties are load-bearing and both fail CLOSED:
//
//  1. NO ADDRESS, NO LISTENER. There is no default port. A security gateway
//     that binds a network socket because nobody said otherwise has already
//     made the operator's decision for them, and the failure is silent — the
//     endpoint exists whether or not anyone meant it to.
//
//  2. NO NON-LOOPBACK WITHOUT AN EXPLICIT SAY-SO. A configured address that
//     is not loopback needs HTTPAllowRemote as well; without it the daemon
//     REFUSES TO START rather than quietly binding loopback instead. Silently
//     narrowing what the operator asked for is the same class of bug as
//     silently widening it: the running system stops matching its
//     configuration, and nobody is told.
//
// The bind itself is a third authorization decision, and it belongs to
// httpbridge.AuthorizeBind: an endpoint nobody holds a credential for is
// refused (docs/architecture.md §2).

// httpEndpoint is a running MCP data plane.
type httpEndpoint struct {
	plane *httpPlane
	addrs []string

	stop    context.CancelFunc
	done    chan struct{}
	stopped sync.Once
}

// Addrs reports the bound addresses (readiness logging and tests).
func (e *httpEndpoint) Addrs() []string { return e.addrs }

// Close drains the endpoint and stops every gateway behind it.
func (e *httpEndpoint) Close() {
	if e == nil {
		return
	}
	e.stopped.Do(func() {
		e.stop()
		<-e.done
		e.plane.Close()
	})
}

// resolveHTTPFace answers where the data plane's opt-in comes from: the
// command line, or the stored configuration.
//
// ONE SOURCE AT A TIME, never a merge. An address and its two confirmations
// are a set: "expose this endpoint, to these callers, with this much
// authentication". Taking the address from one place and a confirmation from
// the other assembles a listener nobody asked for — the operator who types
// `--http-addr 0.0.0.0:9999` expecting to be refused would instead be let
// through by an allowRemote somebody stored months ago for a different
// address.
//
// The command line wins whenever it says anything at all, because it is the
// more specific statement: it applies to this run only, and the person typing
// it is present. Otherwise the registry answers, which is the ordinary case
// now that the desktop application starts the hub and passes no flags.
func resolveHTTPFace(cfg Config, snap *registry.Snapshot, log *slog.Logger) registry.HTTPFace {
	if strings.TrimSpace(cfg.HTTPAddr) != "" || cfg.HTTPAllowRemote || cfg.HTTPInsecureLoopback {
		return registry.HTTPFace{
			Addr:             cfg.HTTPAddr,
			AllowRemote:      cfg.HTTPAllowRemote,
			InsecureLoopback: cfg.HTTPInsecureLoopback,
		}
	}
	if snap == nil {
		return registry.HTTPFace{}
	}
	face := snap.Governance.V.ResolvedHTTP()
	if strings.TrimSpace(face.Addr) != "" {
		// Said out loud on every start. A listener that was configured once
		// and then serves for months is otherwise invisible in the log, and
		// "since when has this been open" is the first question asked about
		// it.
		log.Info("MCP data plane address comes from the stored configuration",
			"addr", face.Addr, "allowRemote", face.AllowRemote, "insecureLoopback", face.InsecureLoopback)
	}
	return face
}

// startHTTPPlane binds and serves the MCP data plane described by cfg.
//
// Returns (nil, nil) when no address is configured — the default, and the
// only path on which nothing is listening. Every other outcome is either a
// live endpoint or an error: a configured endpoint that cannot be brought up
// fails the daemon rather than degrading it into a daemon without the data
// plane its operator asked for.
//
// The logger comes from deps and only from there. It was a parameter as well,
// handed the same value by the one caller, so this function carried two names
// for one logger while newHTTPPlane below read the other one.
func startHTTPPlane(ctx context.Context, cfg Config, deps httpPlaneDeps, tokens *httpbridge.Store, snap *registry.Snapshot) (*httpEndpoint, error) {
	log := deps.Log
	if log == nil {
		// Same default newHTTPPlane applies to the copy it keeps, so the two
		// halves of this assembly cannot end up logging to different places.
		log = slog.New(slog.DiscardHandler)
	}
	face := resolveHTTPFace(cfg, snap, log)
	addr := strings.TrimSpace(face.Addr)
	if addr == "" {
		return nil, nil
	}
	if !httpbridge.AddrIsLoopback(addr) && !face.AllowRemote {
		return nil, fmt.Errorf(
			"daemon: refusing to serve MCP on %s: it is not a loopback address. "+
				"Exposing tool execution beyond this machine needs an explicit "+
				"confirmation — --http-allow-remote, or `agenthub config set http.allowRemote true` "+
				"— and a token (--insecure-loopback never covers it)", addr)
	}

	active := 0
	if tokens != nil {
		n, err := tokens.ActiveCount(time.Now())
		if err != nil {
			// A credential store we cannot read is not a store that
			// authorizes a bind: count nothing rather than assume the best.
			log.Warn("agent-token store unreadable; it authorizes no bind", "error", err)
		} else {
			active = n
		}
	}
	clients := 0
	if snap != nil {
		clients = len(snap.Clients.V.Clients)
	}
	decision, err := httpbridge.AuthorizeBind(httpbridge.BindConfig{
		Addr:              addr,
		HasAdminToken:     cfg.HTTPAdminToken != "",
		ActiveAgentTokens: active,
		RegisteredClients: clients,
		InsecureLoopback:  face.InsecureLoopback,
	})
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}

	plane := newHTTPPlane(deps)
	srv, err := httpbridge.New(httpbridge.Options{
		Dispatcher: plane,
		Auth: &httpbridge.Authenticator{
			AdminToken: cfg.HTTPAdminToken,
			Tokens:     tokens,
			// face, not cfg. AuthorizeBind above already reads the RESOLVED
			// answer, and these two are halves of one decision: the bind is
			// authorized because unauthenticated loopback callers are to be
			// accepted, and this is what accepts them. Reading cfg here made
			// the stored source authorize a credential-less bind and then
			// refuse every caller of it.
			InsecureLoopback: face.InsecureLoopback,
		},
		Logger: log,
		Events: deps.Events,
	})
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}

	listeners, warn, err := httpbridge.Listen(addr)
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	if warn != nil {
		log.Warn("MCP endpoint bound on one address family only", "addr", addr, "error", warn)
	}

	serveCtx, stop := context.WithCancel(context.Background())
	ep := &httpEndpoint{plane: plane, addrs: addrsOf(listeners), stop: stop, done: make(chan struct{})}
	go func() {
		defer close(ep.done)
		if err := srv.Serve(serveCtx, listeners...); err != nil {
			log.Error("MCP endpoint stopped", "error", err)
		}
	}()
	go plane.reap(serveCtx)
	// The caller's context ends the endpoint too, so a daemon that is torn
	// down without reaching its cleanup path still stops accepting.
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-serveCtx.Done():
		}
	}()

	// A network-reachable bind says plainly what it does not protect. The
	// layer above insists on a credential for exactly this case and then
	// puts it on the wire in the clear; an operator who is never told that
	// cannot decide to put a proxy in front of it. WARN, not Info: it is
	// the one line here anybody needs to act on.
	if decision.Cleartext != "" {
		log.Warn("MCP data plane is unencrypted", "addr", addr, "bound", ep.addrs, "warning", decision.Cleartext)
	}
	log.Info("MCP data plane serving",
		"addr", addr, "bound", ep.addrs, "reason", decision.Reason, "loopback", decision.Loopback)
	return ep, nil
}

// addrsOf renders the concrete addresses a set of listeners bound (port 0
// resolves here, which is what tests and readiness logs need).
func addrsOf(ls []net.Listener) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Addr().String())
	}
	return out
}
