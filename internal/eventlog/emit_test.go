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
		KindRegistryReloadFailed: true,
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
		"respawn failed")

	if !strings.Contains(buf.String(), `"level":"WARN"`) {
		t.Fatalf("failure kind did not reach a warn-level sink: %q", buf.String())
	}
	if !strings.Contains(buf.String(), `"detail":"exec: not found"`) {
		t.Fatalf("detail is spelled the same in both streams, or should be: %q", buf.String())
	}
}
