package api

import (
	"encoding/json"
	"testing"
	"time"
)

// TestDTOGoldenJSON freezes the wire shape of every DTO: field names,
// field ORDER (encoding/json emits struct order) and omitempty behavior.
// A diff here is an API break — determinism is the contract.
func TestDTOGoldenJSON(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{
			name: "hello",
			v:    Hello{Version: "0.1.0", Pid: 1234, Generation: 42, Owner: 99},
			want: `{"version":"0.1.0","pid":1234,"generation":42,"owner":99}`,
		},
		{
			// A headless hub reports owner 0 rather than omitting the field:
			// "belongs to nobody" and "too old to say so" must not look alike
			// to a caller deciding whether it may stop this daemon.
			name: "hello_headless",
			v:    Hello{Version: "0.1.0", Pid: 1234, Generation: 42},
			want: `{"version":"0.1.0","pid":1234,"generation":42,"owner":0}`,
		},
		{
			name: "health_full",
			v: Health{
				Level:      HealthLevelDegraded,
				AdminState: AdminStateEnabled,
				Summary:    "token expired",
				Detail:     "refresh failed: invalid_grant",
				Action:     ActionLogin,
			},
			want: `{"level":"degraded","admin_state":"enabled","summary":"token expired","detail":"refresh failed: invalid_grant","action":"login"}`,
		},
		{
			name: "health_no_action_no_detail_omitted",
			v: Health{
				Level:      HealthLevelHealthy,
				AdminState: AdminStateEnabled,
				Summary:    "ok",
				Action:     ActionNone,
			},
			want: `{"level":"healthy","admin_state":"enabled","summary":"ok"}`,
		},
		{
			name: "server",
			v: Server{
				ID:        "github",
				Transport: "stdio",
				Enabled:   true,
				State:     "connected",
				Tools:     26,
				Source:    "manual",
				Health: Health{
					Level:      HealthLevelHealthy,
					AdminState: AdminStateEnabled,
					Summary:    "ok",
				},
			},
			want: `{"id":"github","transport":"stdio","enabled":true,"state":"connected","tools":26,"source":"manual",` +
				`"health":{"level":"healthy","admin_state":"enabled","summary":"ok"}}`,
		},
		{
			name: "session_info",
			v: SessionInfo{
				ID:          "claude:1",
				ClientID:    "claude",
				Origin:      "stdio",
				Root:        "/work/repo",
				ProfileName: "default",
				LastSeen:    time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
			},
			want: `{"id":"claude:1","client_id":"claude","origin":"stdio","root":"/work/repo",` +
				`"profile_name":"default","last_seen":"2026-07-26T12:00:00Z"}`,
		},
		{
			name: "session_info_optionals_omitted",
			v: SessionInfo{
				ID:          "openwebui:3",
				ClientID:    "openwebui",
				Origin:      "http",
				ProfileName: "default",
				LastSeen:    time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
			},
			want: `{"id":"openwebui:3","client_id":"openwebui","origin":"http","profile_name":"default","last_seen":"2026-07-26T12:00:00Z"}`,
		},
		{
			name: "event",
			v: Event{
				Topic:   "servers",
				Kind:    "changed",
				Rev:     7,
				Payload: json.RawMessage(`{"id":"github"}`),
			},
			want: `{"topic":"servers","kind":"changed","rev":7,"payload":{"id":"github"}}`,
		},
		{
			name: "error_body",
			v: ErrorBody{
				Code:    "E_SERVER_NOT_FOUND",
				Message: "no server 'gh'",
				Hint:    "did you mean 'github'?",
			},
			want: `{"code":"E_SERVER_NOT_FOUND","message":"no server 'gh'","hint":"did you mean 'github'?"}`,
		},
		{
			name: "error_body_no_hint_omitted",
			v:    ErrorBody{Code: "E_DAEMON_DOWN", Message: "daemon offline"},
			want: `{"code":"E_DAEMON_DOWN","message":"daemon offline"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("frozen wire shape changed\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestFrozenConstants pins the Health string constants (docs/subsystems/controlplane.md).
// These are ABI: GUI TS constants are generated from them.
func TestFrozenConstants(t *testing.T) {
	frozen := map[string]string{
		HealthLevelHealthy:   "healthy",
		HealthLevelDegraded:  "degraded",
		HealthLevelUnhealthy: "unhealthy",
		AdminStateEnabled:    "enabled",
		AdminStateDisabled:   "disabled",
	}
	for got, want := range frozen {
		if got != want {
			t.Errorf("frozen constant changed: got %q want %q", got, want)
		}
	}
	actions := []struct{ got, want string }{
		{ActionLogin, "login"},
		{ActionRestart, "restart"},
		{ActionEnable, "enable"},
		{ActionSetSecret, "set_secret"},
		{ActionViewLogs, "view_logs"},
		{ActionNone, ""},
	}
	for _, a := range actions {
		if a.got != a.want {
			t.Errorf("frozen action constant changed: got %q want %q", a.got, a.want)
		}
	}
}

func TestErrorString(t *testing.T) {
	e := &Error{ErrorBody: ErrorBody{Code: "E_X", Message: "msg", Hint: "try Y"}}
	if got, want := e.Error(), "agenthub api: E_X: msg (hint: try Y)"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	e2 := &Error{ErrorBody: ErrorBody{Code: "E_X", Message: "msg"}}
	if got, want := e2.Error(), "agenthub api: E_X: msg"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
