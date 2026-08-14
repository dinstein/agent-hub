package api

import (
	"encoding/json"
	"time"
)

// Health level constants (docs/subsystems/docs/subsystems/controlplane.md). Frozen wire values: frontends
// render them verbatim and the generated TS constants mirror this list.
const (
	// HealthLevelHealthy means the server is fully operational.
	HealthLevelHealthy = "healthy"
	// HealthLevelDegraded means the server works but needs attention
	// (e.g. an expiring token or partial tool availability).
	HealthLevelDegraded = "degraded"
	// HealthLevelUnhealthy means the server is not usable right now.
	HealthLevelUnhealthy = "unhealthy"
)

// AdminState constants (docs/subsystems/docs/subsystems/controlplane.md). A disabled server
// reports Level=healthy on purpose: intentionally off is not broken.
//
// There are two, because a server is either in service or switched off.
// A third value once meant "isolated by the integrity subsystem", and it
// outranked both — a state nothing can enter is worse than a missing one,
// because the rung reading it looks like a live branch.
const (
	// AdminStateEnabled: the server participates in routing.
	AdminStateEnabled = "enabled"
	// AdminStateDisabled: turned off by the operator.
	AdminStateDisabled = "disabled"
)

// Action constants (docs/subsystems/docs/subsystems/controlplane.md): the machine-readable suggested action
// that drives UI buttons, `server ls` hint columns and doctor output.
const (
	// ActionLogin suggests running OAuth login for the server: a browser and
	// a human, because nothing stored can renew the credential unattended.
	ActionLogin = "login"
	// ActionRefresh suggests renewing the token from the stored refresh
	// token — the repair that needs no browser. It exists because an expired
	// access token with a refresh token behind it was previously reported as
	// `login`, and the two differ by whether a human has to be present, which
	// is the whole question the operator (or the frontend choosing a button)
	// is asking. Emitted only where a refresh token is KNOWN to exist; where
	// that is unknown, `login` remains the honest answer, since it repairs
	// both cases and refresh repairs only one.
	ActionRefresh = "refresh"
	// ActionRestart suggests restarting the server connection.
	ActionRestart = "restart"
	// ActionEnable suggests re-enabling a disabled server.
	ActionEnable = "enable"
	// ActionSetSecret suggests providing a missing secret.
	ActionSetSecret = "set_secret"
	// ActionViewLogs suggests inspecting the per-server log.
	ActionViewLogs = "view_logs"
	// ActionNone means no action is suggested (omitted on the wire).
	ActionNone = ""
)

// Hello is the daemon's answer to Ping (docs/subsystems/docs/subsystems/controlplane.md): version for
// negotiation, pid for liveness checks, and the registry generation counter
// that replaces mtime polling for change detection.
type Hello struct {
	Version    string `json:"version"`
	Pid        int    `json:"pid"`
	Generation uint64 `json:"generation"`
	// Owner is the pid of the application this hub belongs to, or 0 for a
	// headless one. It is the authoritative answer to "is this hub mine to
	// stop", and it is answered HERE rather than remembered by whoever
	// started the daemon: a caller's memory of having spawned one outlives
	// the daemon it described, and a reconnect would then aim a shutdown at
	// somebody else's hub. Always emitted, including the 0, so "headless" and
	// "a daemon too old to say" stay distinguishable.
	Owner int `json:"owner"`
}

// Health is the display contract computed server-side by a single pure
// function (docs/subsystems/docs/subsystems/controlplane.md). A frontend presenting this DTO
// renders it verbatim and never re-derives it from connection flags. A
// purpose-specific live self-test may replace the whole observation with its
// typed outcome; it must not remix individual Health fields. The SSE
// `servers` event payload embeds the same struct, so push and pull are
// byte-identical.
type Health struct {
	// Level is one of the HealthLevel* constants.
	Level string `json:"level"`
	// AdminState is one of the AdminState* constants. When it is
	// disabled, Level stays "healthy" (intentional != broken).
	AdminState string `json:"admin_state"`
	// Summary is a one-line human-readable status.
	Summary string `json:"summary"`
	// Detail is an optional expanded explanation.
	Detail string `json:"detail,omitempty"`
	// Action is one of the Action* constants ("" = nothing to do).
	Action string `json:"action,omitempty"`
}

// Server is one configured downstream server as reported by Servers.List
// and the `servers` SSE topic (field set matches `server ls --json`).
type Server struct {
	ID        string `json:"id"`
	Transport string `json:"transport"`
	Enabled   bool   `json:"enabled"`
	State     string `json:"state"`
	Tools     int    `json:"tools"`
	Source    string `json:"source"`
	Health    Health `json:"health"`
}

// SessionInfo describes one live session registered with the daemon.
// Sessions are runtime objects: they are never persisted to the registry,
// so listing them requires a running daemon.
type SessionInfo struct {
	// ID is the short human-facing session id ("client:seq", ruling #7).
	ID       string `json:"id"`
	ClientID string `json:"client_id"`
	// Origin is how the session is attached (e.g. "stdio", "http").
	Origin string `json:"origin"`
	// Root is the project root bound to the session, when known.
	Root        string    `json:"root,omitempty"`
	ProfileName string    `json:"profile_name"`
	LastSeen    time.Time `json:"last_seen"`
}

// Event is one daemon event as delivered over the SSE stream
// (GET /v1/events). Topic selects the stream ("servers", "sessions",
// "skills"), Kind the event type within it, and
// Rev the registry generation for registry-backed topics. Change events
// are notifications only — they carry no snapshot; consumers re-read state
// and apply it when the read generation is >= the applied one
// (canonical.md §5c).
type Event struct {
	Topic   string          `json:"topic"`
	Kind    string          `json:"kind"`
	Rev     uint64          `json:"rev"`
	Payload json.RawMessage `json:"payload"`
}
