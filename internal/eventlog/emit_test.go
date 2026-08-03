package eventlog

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// Every kind must be given a level ON PURPOSE. Level's default arm returns
// Info, so a kind added without a thought about severity gets one silently —
// and a failure reported at Info is a failure nobody's filter catches. This
// walks the closed vocabulary so the omission cannot hide.
func TestEveryKindHasADeliberateLevel(t *testing.T) {
	// The failure kinds, listed independently of Level's own switch: a test
	// that read the same expression it checks would agree with any answer.
	warn := map[Kind]bool{
		KindConnectFailed: true, KindRespawnFailed: true, KindCircuitOpen: true,
		KindHealthDown: true, KindOAuthRefreshFailed: true, KindSecretsMissing: true,
		KindRegistryReloadFailed: true, KindOAuthLoginFailed: true,
	}
	for scope, kinds := range allKinds {
		for _, k := range kinds {
			want := slog.LevelInfo
			if warn[k] {
				want = slog.LevelWarn
			}
			if got := Level(k); got != want {
				t.Errorf("Level(%s/%s) = %v, want %v", scope, k, got, want)
			}
		}
	}
}

// One call, two streams. The point of Emit is that neither half can be
// written without the other.
func TestEmitWritesBothHalves(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir+"/events.jsonl", Options{PID: 42})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s.Emit(log, Record{Scope: ScopeServer, Server: "github", Kind: KindConnected, Count: 13},
		"downstream connected")
	s.Sync()

	res, err := Read(dir+"/events.jsonl", Query{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].Kind != KindConnected {
		t.Fatalf("record half missing: %+v", res.Records)
	}

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("prose half is not one JSON object: %v\n%s", err, buf.String())
	}
	if line["msg"] != "downstream connected" {
		t.Fatalf("prose half missing: %v", line)
	}
	// The number is labelled by the noun its kind gives it, so the two
	// streams cannot disagree about what it counts.
	if line["tools"] != float64(13) {
		t.Fatalf("count not rendered under CountNoun: %v", line)
	}
}

// The logger a call site holds is already bound to its identity. slog's JSON
// handler does not deduplicate, so stamping the same keys here would emit
// each field twice and a reader taking the last would read the second.
func TestEmitDoesNotRestampIdentityTheLoggerAlreadyCarries(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil)).With("server", "github", "inst", "work")

	var s *Stream // nil stream: the prose half must still be written
	s.Emit(log, Record{Scope: ScopeServer, Server: "github", Inst: "work",
		Kind: KindDisconnected, Count: 2}, "downstream connection closed")

	out := buf.String()
	if strings.Count(out, `"server"`) != 1 || strings.Count(out, `"inst"`) != 1 {
		t.Fatalf("identity emitted twice on one line: %s", out)
	}
	if !strings.Contains(out, `"reconnects":2`) {
		t.Fatalf("count missing under its noun: %s", out)
	}
}

// A failure kind must reach a --level warn filter. This is the whole reason
// severity is a property of the kind rather than of the sentence.
func TestFailureKindsAreLoggedAtWarn(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	var s *Stream
	s.Emit(log, Record{Scope: ScopeServer, Kind: KindRespawnFailed, Detail: "exec: not found"},
		"respawn failed", "error", "exec: not found")

	if !strings.Contains(buf.String(), `"level":"WARN"`) {
		t.Fatalf("failure kind did not reach a warn-level sink: %q", buf.String())
	}
	// Detail reaches the prose half under the name its kind gives it, passed
	// by the call site — never as a generic `detail`, which would say the
	// same vague word for an error, a cause, an address and a version.
	if strings.Contains(buf.String(), `"detail"`) {
		t.Fatalf("the record's polymorphic field leaked into the prose as `detail`: %q", buf.String())
	}
	if !strings.Contains(buf.String(), `"error":"exec: not found"`) {
		t.Fatalf("the call site's own name for the detail is missing: %q", buf.String())
	}
}

// Every kind is classified on purpose. ClassOf defaults to routine, so a new
// fault added without a thought lands in the class an operator scans past —
// this walks the vocabulary so the omission cannot hide.
func TestEveryKindIsClassifiedDeliberately(t *testing.T) {
	// Listed independently of the package's own list: a test reading the
	// expression it checks would agree with any answer.
	// Keyed by the PAIR, because two scopes share the spelling `started` and
	// a map keyed by kind alone cannot hold both.
	want := map[string]Class{
		"server/connected": ClassRoutine, "server/tools_changed": ClassRoutine,
		"server/oauth_login_started": ClassRoutine, "server/oauth_login_waiting": ClassRoutine,
		"server/oauth_login_completed": ClassRoutine,
		"server/connect_failed":        ClassDisruption, "server/disconnected": ClassDisruption,
		"server/respawned": ClassDisruption, "server/respawn_failed": ClassDisruption,
		"server/circuit_open": ClassDisruption, "server/circuit_half_open": ClassDisruption,
		"server/circuit_closed": ClassDisruption, "server/health_down": ClassDisruption,
		"server/health_up": ClassDisruption, "server/oauth_refresh_failed": ClassDisruption,
		"server/secrets_missing": ClassDisruption, "server/oauth_login_failed": ClassDisruption,
		"gateway/started": ClassRoutine, "gateway/stopped": ClassRoutine,
		"gateway/client_attached": ClassRoutine, "gateway/registry_reload_failed": ClassDisruption,
		"gateway/session_opened": ClassRoutine, "gateway/session_closed": ClassRoutine,
		"daemon/started": ClassRoutine, "daemon/stopping": ClassRoutine,
		"daemon/listener_bound": ClassRoutine, "daemon/config_reloaded": ClassRoutine,
	}
	for scope, kinds := range allKinds {
		for _, k := range kinds {
			expected, listed := want[string(scope)+"/"+string(k)]
			if !listed {
				t.Errorf("%s/%s is not classified in this test; classify it in class.go too", scope, k)
				continue
			}
			if got := ClassOf(k); got != expected {
				t.Errorf("ClassOf(%s/%s) = %s, want %s", scope, k, got, expected)
			}
		}
	}
}

// The recovery half of an outage must survive a disruption filter. Dropping
// it shows every outage starting and none of them ending, which reads as an
// outage still in progress.
func TestRecoveryKindsStayWithTheDisruptionTheyEnd(t *testing.T) {
	for _, k := range []Kind{KindCircuitClosed, KindHealthUp} {
		if ClassOf(k) != ClassDisruption {
			t.Errorf("%s is not in the class of the episode it ends", k)
		}
	}
}
