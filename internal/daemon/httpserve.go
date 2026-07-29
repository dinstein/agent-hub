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
	"github.com/dinstein/agent-hub/internal/secrets"
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

// startHTTPPlane binds and serves the MCP data plane described by cfg.
//
// Returns (nil, nil) when no address is configured — the default, and the
// only path on which nothing is listening. Every other outcome is either a
// live endpoint or an error: a configured endpoint that cannot be brought up
// fails the daemon rather than degrading it into a daemon without the data
// plane its operator asked for.
func startHTTPPlane(ctx context.Context, cfg Config, deps httpPlaneDeps, tokens *httpbridge.Store, snap *registry.Snapshot, log *slog.Logger) (*httpEndpoint, error) {
	addr := strings.TrimSpace(cfg.HTTPAddr)
	if addr == "" {
		return nil, nil
	}
	if !httpbridge.AddrIsLoopback(addr) && !cfg.HTTPAllowRemote {
		return nil, fmt.Errorf(
			"daemon: refusing to serve MCP on %s: it is not a loopback address. "+
				"Exposing tool execution beyond this machine needs an explicit "+
				"--http-allow-remote (and a token: --insecure-loopback never covers it)", addr)
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
		InsecureLoopback:  cfg.HTTPInsecureLoopback,
	})
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}

	plane := newHTTPPlane(deps)
	srv, err := httpbridge.New(httpbridge.Options{
		Dispatcher: plane,
		Auth: &httpbridge.Authenticator{
			AdminToken:       cfg.HTTPAdminToken,
			Tokens:           tokens,
			InsecureLoopback: cfg.HTTPInsecureLoopback,
		},
		Logger: log,
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

	log.Info("MCP data plane serving",
		"addr", addr, "bound", ep.addrs, "reason", decision.Reason, "loopback", decision.Loopback)
	return ep, nil
}

// dataPlaneSecrets narrows the daemon's vault to the resolve-one-ref face the
// data plane's downstream dialer needs. A nil vault stays nil, which is NOT
// "resolves nothing": the gateway then builds the production chain itself,
// and a resolver that is absent (rather than empty) makes every unresolved
// ${SECRET_x} a dial error instead of a silently blank credential.
func dataPlaneSecrets(vault secrets.Store) secrets.Resolver {
	if vault == nil {
		return nil
	}
	return vault.Get
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
