package httpbridge

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// ErrBindUnauthorized is returned when a listener would accept connections
// nobody is authorized to make (docs/architecture.md §2, toolport
// http_bind_is_authorized).
var ErrBindUnauthorized = errors.New("httpbridge: refusing to bind an unauthenticated MCP endpoint")

// BindDecision describes an authorized bind and why it was authorized. It is
// returned so the daemon can log the reason — "bound because a client is
// registered" and "bound because the operator disabled authentication" must
// not look the same in a log.
type BindDecision struct {
	Addr string
	// Reason is one of: "admin-token", "agent-token", "registered-client",
	// "insecure-loopback".
	Reason string
	// Loopback records whether Addr resolves to a loopback host.
	Loopback bool
}

// BindConfig is the input of AuthorizeBind.
type BindConfig struct {
	// Addr is the listen address ("host:port"; an empty host means every
	// interface, which is NOT loopback).
	Addr string
	// HasAdminToken reports whether an admin token is configured.
	HasAdminToken bool
	// ActiveAgentTokens counts non-revoked, non-expired agent tokens.
	ActiveAgentTokens int
	// RegisteredClients counts clients registered in clients.json.
	RegisteredClients int
	// InsecureLoopback is the escape hatch.
	InsecureLoopback bool
}

// AuthorizeBind decides whether the MCP endpoint may be bound at all.
//
// The rule (fail-closed, docs/architecture.md §2): a listener with no admin token, no
// active agent token and no registered client would accept every local
// process as an authorized agent, so it is REFUSED. --insecure-loopback is
// the single documented escape, and it is deliberately narrower than the
// flag's name suggests:
//
//   - a NON-loopback address always needs a token. Neither a registered
//     client nor the escape hatch authorizes exposing tool execution to the
//     network — "insecure" was meant to describe a developer's own machine,
//     not a LAN.
//   - the registered-client path only authorizes loopback binds, for the
//     same reason: a clients.json entry is configuration, not a credential.
func AuthorizeBind(cfg BindConfig) (BindDecision, error) {
	loopback := AddrIsLoopback(cfg.Addr)
	dec := BindDecision{Addr: cfg.Addr, Loopback: loopback}

	switch {
	case cfg.HasAdminToken:
		dec.Reason = "admin-token"
		return dec, nil
	case cfg.ActiveAgentTokens > 0:
		dec.Reason = "agent-token"
		return dec, nil
	}
	if !loopback {
		return BindDecision{}, fmt.Errorf(
			"%w: %s is not loopback and no token is configured; create one with "+
				"'agenthub token create' or set AGENTHUB_HTTP_TOKEN", ErrBindUnauthorized, cfg.Addr)
	}
	if cfg.RegisteredClients > 0 {
		dec.Reason = "registered-client"
		return dec, nil
	}
	if cfg.InsecureLoopback {
		dec.Reason = "insecure-loopback"
		return dec, nil
	}
	return BindDecision{}, fmt.Errorf(
		"%w: no token, no registered client. Create a token with 'agenthub token create', "+
			"or pass --insecure-loopback to accept unauthenticated local callers", ErrBindUnauthorized)
}

// AddrIsLoopback reports whether addr names a loopback host. It is exported
// because the assembling daemon needs the SAME predicate to decide whether a
// configured listen address requires the operator's explicit
// expose-to-the-network confirmation; two implementations of "is this
// loopback" would eventually disagree, and the direction they disagree in is
// the one that publishes tool execution to a LAN.
//
// Failure direction: everything it cannot prove is loopback is treated as
// NOT loopback (an empty host — bind to every interface — a hostname, an
// unparsable address). The predicate is used to grant a weaker
// authorization, so it must be the "fail-to-false" of the pair, mirroring
// the SSRF screen's HostIsDefinitelyPrivate (docs/flows.md).
func AddrIsLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false // ":8080" listens on every interface
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
