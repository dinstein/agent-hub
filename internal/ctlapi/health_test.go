package ctlapi

import (
	"testing"

	"github.com/dinstein/agent-hub/api"
)

// fullStack returns an input with EVERY rung triggered at and below `from`
// (1-based ladder position). Used to prove first-match-wins: the expected
// output must ignore everything below the highest triggered rung.
func fullStack(from int) HealthInput {
	in := HealthInput{}
	if from <= 1 {
		in.AdminState = api.AdminStateQuarantined
	}
	if from <= 2 {
		in.MissingSecrets = []string{"API_KEY"}
	}
	if from <= 3 {
		in.OAuthConfigError = "bad issuer"
	}
	if from <= 4 {
		in.Conn = ConnError
		in.ConnDetail = "dial tcp: refused"
	}
	if from <= 5 {
		in.CallAuthFailed = true
	}
	if from <= 6 {
		in.Token = TokenExpired
	}
	return in
}

// TestComputeHealthMatrix is the full-matrix golden table for the 7.4
// priority ladder. Each case pins the exact api.Health value.
func TestComputeHealthMatrix(t *testing.T) {
	cases := []struct {
		name string
		in   HealthInput
		want api.Health
	}{
		// Rung 7: nothing wrong. The zero value has Conn == ConnUnknown, so
		// it lands on the "not observed" side of the last rung — nothing is
		// broken, but nothing was checked either.
		{
			name: "zero value is healthy but unobserved",
			in:   HealthInput{},
			want: api.Health{
				Level: api.HealthLevelHealthy, AdminState: api.AdminStateEnabled,
				Summary: "not observed", Detail: "no connected client is using this server",
			},
		},
		{
			name: "explicit enabled connected healthy",
			in:   HealthInput{AdminState: api.AdminStateEnabled, Conn: ConnConnected},
			want: api.Health{Level: api.HealthLevelHealthy, AdminState: api.AdminStateEnabled, Summary: "ok"},
		},
		// Rung 1: admin state. disabled/quarantined report healthy ON
		// PURPOSE and swallow every lower rung.
		{
			name: "disabled is healthy with enable action",
			in:   HealthInput{AdminState: api.AdminStateDisabled},
			want: api.Health{Level: api.HealthLevelHealthy, AdminState: api.AdminStateDisabled, Summary: "disabled by operator", Action: api.ActionEnable},
		},
		{
			name: "disabled outranks connection error",
			in: HealthInput{
				AdminState: api.AdminStateDisabled,
				Conn:       ConnError, ConnDetail: "boom",
				MissingSecrets: []string{"X"},
			},
			want: api.Health{Level: api.HealthLevelHealthy, AdminState: api.AdminStateDisabled, Summary: "disabled by operator", Action: api.ActionEnable},
		},
		{
			name: "quarantined outranks everything",
			in:   fullStack(1),
			want: api.Health{Level: api.HealthLevelHealthy, AdminState: api.AdminStateQuarantined, Summary: "quarantined pending approval", Action: api.ActionApprove},
		},
		// Rung 2: missing secrets outrank OAuth config, connection, token.
		{
			name: "missing secret outranks lower rungs",
			in:   fullStack(2),
			want: api.Health{Level: api.HealthLevelUnhealthy, AdminState: api.AdminStateEnabled, Summary: "missing secrets", Detail: "API_KEY", Action: api.ActionSetSecret},
		},
		{
			name: "multiple missing secrets joined in detail",
			in:   HealthInput{MissingSecrets: []string{"A", "B"}},
			want: api.Health{Level: api.HealthLevelUnhealthy, AdminState: api.AdminStateEnabled, Summary: "missing secrets", Detail: "A, B", Action: api.ActionSetSecret},
		},
		// Rung 3: OAuth config error.
		{
			name: "oauth config error outranks connection and token",
			in:   fullStack(3),
			want: api.Health{Level: api.HealthLevelUnhealthy, AdminState: api.AdminStateEnabled, Summary: "OAuth configuration error", Detail: "bad issuer", Action: api.ActionLogin},
		},
		// Rung 4: connection states.
		{
			name: "connection error outranks call auth and token",
			in:   fullStack(4),
			want: api.Health{Level: api.HealthLevelUnhealthy, AdminState: api.AdminStateEnabled, Summary: "connection error", Detail: "dial tcp: refused", Action: api.ActionRestart},
		},
		{
			name: "disconnected is unhealthy restart",
			in:   HealthInput{Conn: ConnDisconnected},
			want: api.Health{Level: api.HealthLevelUnhealthy, AdminState: api.AdminStateEnabled, Summary: "disconnected", Action: api.ActionRestart},
		},
		{
			name: "connecting is degraded no action",
			in:   HealthInput{Conn: ConnConnecting},
			want: api.Health{Level: api.HealthLevelDegraded, AdminState: api.AdminStateEnabled, Summary: "connecting"},
		},
		{
			name: "unrecognized conn state fails visible",
			in:   HealthInput{Conn: ConnState("weird")},
			want: api.Health{Level: api.HealthLevelUnhealthy, AdminState: api.AdminStateEnabled, Summary: `unknown connection state "weird"`, Action: api.ActionViewLogs},
		},
		// Rung 5: call-time OAuth.
		{
			name: "call auth failure outranks token state",
			in:   fullStack(5),
			want: api.Health{Level: api.HealthLevelDegraded, AdminState: api.AdminStateEnabled, Summary: "authentication failing on calls", Action: api.ActionLogin},
		},
		{
			name: "call auth failure while connected",
			in:   HealthInput{Conn: ConnConnected, CallAuthFailed: true},
			want: api.Health{Level: api.HealthLevelDegraded, AdminState: api.AdminStateEnabled, Summary: "authentication failing on calls", Action: api.ActionLogin},
		},
		// Rung 6: token state.
		{
			name: "token expired is unhealthy login",
			in:   fullStack(6),
			want: api.Health{Level: api.HealthLevelUnhealthy, AdminState: api.AdminStateEnabled, Summary: "token expired", Action: api.ActionLogin},
		},
		{
			name: "token expiring is degraded login",
			in:   HealthInput{Conn: ConnConnected, Token: TokenExpiring},
			want: api.Health{Level: api.HealthLevelDegraded, AdminState: api.AdminStateEnabled, Summary: "token expiring soon", Action: api.ActionLogin},
		},
		// ConnUnknown and ConnConnected share rung 4 (both fall through) and
		// share rungs 5 and 6 — secret/OAuth/token facts hold whether or not
		// anyone is connected. They separate only at rung 7, where the
		// verdict itself is at stake: "ok" is a claim, and nobody made it.
		{
			name: "unknown conn is healthy but not observed",
			in:   HealthInput{Conn: ConnUnknown},
			want: api.Health{
				Level: api.HealthLevelHealthy, AdminState: api.AdminStateEnabled,
				Summary: "not observed", Detail: "no connected client is using this server",
			},
		},
		{
			name: "unknown conn still surfaces an expired token",
			in:   HealthInput{Conn: ConnUnknown, Token: TokenExpired},
			want: api.Health{Level: api.HealthLevelUnhealthy, AdminState: api.AdminStateEnabled, Summary: "token expired", Action: api.ActionLogin},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeHealth(tc.in)
			if got != tc.want {
				t.Errorf("ComputeHealth(%+v)\n got  %+v\n want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// TestComputeHealthPure pins purity: same input, same output, and the input
// struct is not mutated.
func TestComputeHealthPure(t *testing.T) {
	in := fullStack(2)
	before := in
	a := ComputeHealth(in)
	b := ComputeHealth(in)
	if a != b {
		t.Errorf("not deterministic: %+v vs %+v", a, b)
	}
	if in.AdminState != before.AdminState || in.OAuthConfigError != before.OAuthConfigError {
		t.Error("input mutated")
	}
}
