package httpbridge

import (
	"errors"
	"fmt"

	"github.com/dinstein/agent-hub/internal/guard/netguard"
)

// ErrBindUnauthorized is returned when a listener would accept connections
// nobody is authorized to make (docs/architecture.md#the-processes, toolport
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
	// Cleartext warns that the bind carries credentials over an unencrypted
	// channel. Set for every non-loopback bind, because this package
	// terminates no TLS: see CleartextWarning.
	Cleartext string
}

// CleartextWarning is what a non-loopback bind is told about its channel.
//
// This package serves plain HTTP and has no TLS configuration, certificate
// or ServeTLS anywhere in it. Everything above insists on a credential for a
// network-reachable listener — AuthorizeBind refuses without one — and then
// that credential crosses the network in the clear, along with every tool
// call and result. An on-path observer can read the bearer and replay it at
// its tier.
//
// Terminating TLS is deliberately out of scope: it needs certificate
// material, rotation and trust configuration, which is a feature with its
// own argument and not something a bind check should invent. What is NOT
// acceptable is silence. The "isolation a config claims must be delivered or
// refused" rule applies to the channel too, and the honest form here is to
// deliver the bind and say plainly what it does not protect, so the operator
// can put it behind a TLS-terminating proxy or a private network.
const CleartextWarning = "this endpoint serves plain HTTP: bearer tokens and all MCP traffic cross the network unencrypted. " +
	"Put it behind a TLS-terminating proxy, or restrict it to a trusted network"

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
// The rule (fail-closed, docs/architecture.md#the-processes): a listener with no admin token, no
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
	loopback := netguard.AddrIsLoopback(cfg.Addr)
	dec := BindDecision{Addr: cfg.Addr, Loopback: loopback}

	// The escape hatch is judged BEFORE anything can authorize the bind, and
	// it is judged on the address alone.
	//
	// It used to be judged only as a last-resort bind reason, which meant a
	// token short-circuited the switch below and the hatch was never looked
	// at: `--http-addr 0.0.0.0:7777 --http-allow-remote --insecure-loopback`
	// with any token configured bound successfully AND handed
	// InsecureLoopback to the Authenticator, so every unauthenticated LAN
	// request was answered at the destructive tier. The narrowing was real
	// but it lived in a branch the common configuration never reached.
	//
	// Refusing rather than ignoring the flag is the "delivered or refused"
	// rule: an operator who asked for unauthenticated access on a public
	// address has asked for something this build will not do, and silently
	// dropping the flag would leave them believing it took effect.
	if cfg.InsecureLoopback && !loopback {
		return BindDecision{}, fmt.Errorf(
			"%w: --insecure-loopback names %s, which is not a loopback address. "+
				"The hatch covers a developer's own machine, never a network-reachable "+
				"listener — drop the flag and authenticate with a token, or bind loopback",
			ErrBindUnauthorized, cfg.Addr)
	}

	if !loopback {
		dec.Cleartext = CleartextWarning
	}

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

// The loopback predicate this package binds and screens Origin by is
// netguard.AddrIsLoopback. It used to live here and moved when internal/diag
// came to need the same answer: that package sits below this one, so keeping
// it here would have meant either an upward import or a second
// implementation, and a second answer to "is this loopback" disagrees,
// eventually, in the direction that publishes something to a LAN.
