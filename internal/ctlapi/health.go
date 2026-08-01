package ctlapi

import (
	"fmt"
	"strings"

	"github.com/dinstein/agent-hub/api"
)

// ConnState is the downstream connection state as reported by the runtime
// state source. The empty string means NOT OBSERVED: no gateway currently
// holds a connection to this server (no client is using it, or the one that
// was has not sent its first report yet). It is a statement about the
// observer, never about the server.
type ConnState string

const (
	ConnUnknown    ConnState = ""
	ConnConnected  ConnState = "connected"
	ConnConnecting ConnState = "connecting"
	// ConnDisconnected is understood but never reported: no state source in
	// the tree emits it (internal/downstream reports connecting/connected/
	// error, and a gateway that drops a connection stops reporting rather
	// than reporting a state). The handling stays because the fold and
	// ComputeHealth would otherwise treat it as unrecognized the day
	// something does.
	ConnDisconnected ConnState = "disconnected"
	ConnError        ConnState = "error"
)

// TokenState is the OAuth token lifecycle state of a server that uses one.
// The empty string means "no token involved or token fine".
type TokenState string

const (
	TokenOK       TokenState = ""
	TokenExpiring TokenState = "expiring"
	TokenExpired  TokenState = "expired"
)

// HealthInput is everything ComputeHealth consumes: one flat snapshot of a
// server's administrative and runtime condition. It is assembled by the
// /v1/servers handler from the registry entry plus the injected
// ServerStateSource; keeping it a plain struct keeps ComputeHealth a pure
// function (docs/modules/controlplane.md: computed once server-side, frontends only render).
type HealthInput struct {
	// AdminState is one of the api.AdminState* constants. "" is treated as
	// enabled (a registry entry with no admin intervention).
	AdminState string
	// MissingSecrets lists unresolved secret names required by the server.
	MissingSecrets []string
	// OAuthConfigError is a non-empty description when the OAuth
	// configuration itself is broken (bad issuer, failed discovery, ...).
	OAuthConfigError string
	// NeedsAuth reports a 401/403 that prevented the initial MCP handshake.
	// It is runtime evidence from a live connection attempt, never stored
	// configuration.
	NeedsAuth bool
	// Conn is the connection state; ConnDetail optionally elaborates
	// (last error text, retry countdown, ...).
	Conn       ConnState
	ConnDetail string
	// CallAuthFailed reports 401/403-class failures observed on tool calls
	// while the connection itself is up (call-time OAuth rung).
	CallAuthFailed bool
	// Token is the OAuth token lifecycle state.
	Token TokenState
}

// ComputeHealth is the pure function of docs/modules/controlplane.md: it maps one
// HealthInput to the display contract every frontend renders verbatim.
//
// Priority ladder, first match wins: admin state → missing secret → OAuth
// misconfiguration → connection state → call-time OAuth → token state →
// healthy.
//
//  1. AdminState disabled: Level=healthy ON PURPOSE — turned off
//     intentionally is not broken
//     (docs/modules/controlplane.md).
//  2. Missing secrets: unhealthy, action set_secret.
//  3. OAuth config error: unhealthy, action login.
//  4. Connection: handshake auth refusal → unhealthy (login);
//     error/disconnected → unhealthy (restart); connecting → degraded
//     (transient, no action); connected/unknown → next rung.
//  5. Call-time auth failures: degraded, action login (connection is up but
//     calls bounce off authentication).
//  6. Token: expired → unhealthy + login (calls will fail); expiring →
//     degraded + login (works now, act soon).
//  7. healthy — "ok" when someone is actually watching, "not observed" when
//     nobody is (see below).
//
// On the last rung, ConnUnknown and ConnConnected part ways. Now that the
// state source is the fleet of live gateways, "unknown" is a normal and
// COMMON condition: no client is connected, so no process holds this
// server, so nothing has looked at it. That is not a fault — Level stays
// healthy, because painting every idle server yellow would replace one
// misleading signal with a screenful of noise. But it is not "ok" either:
// claiming a verdict that was never made is precisely what this file's
// "fail toward visibility" rule forbids, so the summary says which it is.
func ComputeHealth(in HealthInput) api.Health {
	admin := in.AdminState
	if admin == "" {
		admin = api.AdminStateEnabled
	}

	// Rung 1: admin state.
	if admin == api.AdminStateDisabled {
		return api.Health{
			Level:      api.HealthLevelHealthy,
			AdminState: admin,
			Summary:    "disabled by operator",
			Action:     api.ActionEnable,
		}
	}

	// Rung 2: missing secrets.
	if len(in.MissingSecrets) > 0 {
		return api.Health{
			Level:      api.HealthLevelUnhealthy,
			AdminState: admin,
			Summary:    "missing secrets",
			Detail:     strings.Join(in.MissingSecrets, ", "),
			Action:     api.ActionSetSecret,
		}
	}

	// Rung 3: OAuth configuration error.
	if in.OAuthConfigError != "" {
		return api.Health{
			Level:      api.HealthLevelUnhealthy,
			AdminState: admin,
			Summary:    "OAuth configuration error",
			Detail:     in.OAuthConfigError,
			Action:     api.ActionLogin,
		}
	}

	// Rung 4: connection state. A typed 401/403 from the handshake is the
	// one connection failure a restart cannot repair, so surface login before
	// the generic ConnError branch consumes it.
	if in.NeedsAuth {
		return api.Health{
			Level:      api.HealthLevelUnhealthy,
			AdminState: admin,
			Summary:    "authentication required",
			Detail:     in.ConnDetail,
			Action:     api.ActionLogin,
		}
	}
	switch in.Conn {
	case ConnError:
		return api.Health{
			Level:      api.HealthLevelUnhealthy,
			AdminState: admin,
			Summary:    "connection error",
			Detail:     in.ConnDetail,
			Action:     api.ActionRestart,
		}
	case ConnDisconnected:
		return api.Health{
			Level:      api.HealthLevelUnhealthy,
			AdminState: admin,
			Summary:    "disconnected",
			Detail:     in.ConnDetail,
			Action:     api.ActionRestart,
		}
	case ConnConnecting:
		return api.Health{
			Level:      api.HealthLevelDegraded,
			AdminState: admin,
			Summary:    "connecting",
			Detail:     in.ConnDetail,
		}
	case ConnConnected, ConnUnknown:
		// Fall through to the remaining rungs. The two are still handled
		// together HERE on purpose: rungs 5 and 6 read secret/OAuth/token
		// facts that hold whether or not anyone is connected, so an
		// unobserved server with an expired token must still say so. They
		// separate at rung 7, where the verdict itself is at stake.
	default:
		// An unrecognized state is a bug in the state source; surface it
		// rather than guessing healthy (fail toward visibility, not green).
		return api.Health{
			Level:      api.HealthLevelUnhealthy,
			AdminState: admin,
			Summary:    fmt.Sprintf("unknown connection state %q", string(in.Conn)),
			Action:     api.ActionViewLogs,
		}
	}

	// Rung 5: call-time OAuth.
	if in.CallAuthFailed {
		return api.Health{
			Level:      api.HealthLevelDegraded,
			AdminState: admin,
			Summary:    "authentication failing on calls",
			Action:     api.ActionLogin,
		}
	}

	// Rung 6: token state.
	switch in.Token {
	case TokenExpired:
		return api.Health{
			Level:      api.HealthLevelUnhealthy,
			AdminState: admin,
			Summary:    "token expired",
			Action:     api.ActionLogin,
		}
	case TokenExpiring:
		return api.Health{
			Level:      api.HealthLevelDegraded,
			AdminState: admin,
			Summary:    "token expiring soon",
			Action:     api.ActionLogin,
		}
	}

	// Rung 7: healthy.
	if in.Conn == ConnUnknown {
		// Nothing is wrong AND nothing has been checked. Say the second part
		// out loud rather than issuing a health certificate nobody earned.
		return api.Health{
			Level:      api.HealthLevelHealthy,
			AdminState: admin,
			Summary:    "not observed",
			Detail:     "no connected client is using this server",
		}
	}
	return api.Health{
		Level:      api.HealthLevelHealthy,
		AdminState: admin,
		Summary:    "ok",
	}
}
