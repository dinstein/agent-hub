package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// A token can be renewed by this daemon or by a stdio gateway that took a
// 401, and an operator reading a log usually does not know which process was
// running. So the two sides emit the SAME four messages and distinguish
// themselves with `trigger`. These tests pin the daemon half; the gateway
// half is pinned by internal/gateway/authlog_test.go, and the two lists must
// be read together — a message renamed on one side alone is exactly the
// regression they exist to catch.

// logRecorder collects records for assertions.
type logRecorder struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *logRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}

func (h *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logRecorder) WithGroup(string) slog.Handler      { return h }

func (h *logRecorder) find(msg string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

func (h *logRecorder) messages() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.recs))
	for _, r := range h.recs {
		out = append(out, r.Level.String()+" "+r.Message)
	}
	return strings.Join(out, "; ")
}

func recordAttr(r slog.Record, key string) (slog.Value, bool) {
	var v slog.Value
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v, found = a.Value, true
			return false
		}
		return true
	})
	return v, found
}

// requireTrigger asserts the field that separates the two refreshing
// processes. Without it the shared wording would make the daemon's work and a
// gateway's indistinguishable in a merged log.
func requireTrigger(t *testing.T, r slog.Record, msg string) {
	t.Helper()
	v, ok := recordAttr(r, "trigger")
	if !ok || v.String() != oauthflow.TriggerExpiry {
		t.Errorf("%q carries trigger=%v, want %q", msg, v, oauthflow.TriggerExpiry)
	}
}

func TestRefresherLogsASuccessSymmetricallyWithTheGateway(t *testing.T) {
	as := newFakeAS(t)
	vault := newMemStore()
	seedState(t, vault, "remote", as.srv.URL+"/token", time.Now().Add(-10*time.Second).Unix())
	rec := &logRecorder{}

	r := testRefresher(t, refresherConfig{
		Store:   testRegistry(t, map[string]registry.ServerEntry{"remote": {Transport: "http", URL: "https://x/mcp", Enabled: true}}),
		Secrets: vault,
		Log:     slog.New(rec),
	})
	r.cycle(context.Background())

	attempt, ok := rec.find("refreshing a downstream access token")
	if !ok {
		t.Fatalf("no attempt line logged; got: %s", rec.messages())
	}
	if attempt.Level != slog.LevelDebug {
		t.Errorf("attempt logged at %s, want DEBUG (same as the gateway's)", attempt.Level)
	}
	requireTrigger(t, attempt, "the attempt line")

	done, ok := rec.find("access token refreshed")
	if !ok {
		t.Fatalf("no success line logged; got: %s", rec.messages())
	}
	if done.Level != slog.LevelInfo {
		t.Errorf("success logged at %s, want INFO", done.Level)
	}
	requireTrigger(t, done, "the success line")
	if v, ok := recordAttr(done, logx.FieldServer); !ok || v.String() != "remote" {
		t.Errorf("success line carries %s=%v, want the mandatory server attr", logx.FieldServer, v)
	}
	// superseded is the gateway's field, now on both sides: it answers "did
	// the cross-process file lock do its job", which shared (in-process
	// singleflight) does not.
	if v, ok := recordAttr(done, "superseded"); !ok || v.Bool() {
		t.Errorf("superseded=%v on a refresh this daemon performed itself, want false", v)
	}
	if _, ok := recordAttr(done, "shared"); !ok {
		t.Error("success line dropped shared; it is what reports the in-process singleflight")
	}
}

// The failure message is shared with the gateway and the schedule is not:
// only the daemon has a ladder of its own to report.
func TestRefresherLogsAFailureSymmetricallyWithTheGateway(t *testing.T) {
	as := newFakeAS(t)
	as.fail.Store(true)
	vault := newMemStore()
	seedState(t, vault, "remote", as.srv.URL+"/token", time.Now().Add(-10*time.Second).Unix())
	rec := &logRecorder{}

	r := testRefresher(t, refresherConfig{
		Store:   testRegistry(t, map[string]registry.ServerEntry{"remote": {Transport: "http", URL: "https://x/mcp", Enabled: true}}),
		Secrets: vault,
		Log:     slog.New(rec),
	})
	r.cycle(context.Background())

	failed, ok := rec.find("access token refresh failed")
	if !ok {
		t.Fatalf("no failure line logged; got: %s", rec.messages())
	}
	if failed.Level != slog.LevelWarn {
		t.Errorf("failure logged at %s, want WARN", failed.Level)
	}
	requireTrigger(t, failed, "the failure line")
	if v, ok := recordAttr(failed, "retry_in"); !ok || v.Duration() != oauthflow.RefreshRetryBackoff {
		t.Errorf("retry_in=%v, want the %s first-failure backoff", v, oauthflow.RefreshRetryBackoff)
	}
	if v, ok := recordAttr(failed, "attempt"); !ok || v.Int64() != 1 {
		t.Errorf("attempt=%v on the first failure, want 1", v)
	}
}

// The dead end: no refresh token, so no retry helps and the message is the
// same one the gateway logs for the same state.
func TestRefresherLogsTheLoginRequiredDeadEnd(t *testing.T) {
	as := newFakeAS(t)
	vault := newMemStore()
	st := oauthflow.State{
		TokenEndpoint: as.srv.URL + "/token",
		ClientID:      "c",
		IssuedAt:      time.Now().Add(-time.Hour).Unix(),
		ExpiresAt:     time.Now().Add(-time.Minute).Unix(),
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Set(context.Background(), secrets.OAuthStateRef("remote"), string(raw)); err != nil {
		t.Fatal(err)
	}
	rec := &logRecorder{}

	r := testRefresher(t, refresherConfig{
		Store:   testRegistry(t, map[string]registry.ServerEntry{"remote": {Transport: "http", URL: "https://x/mcp", Enabled: true}}),
		Secrets: vault,
		Log:     slog.New(rec),
	})
	r.cycle(context.Background())

	got, ok := rec.find("token cannot be refreshed without a new login")
	if !ok {
		t.Fatalf("no login-required line logged; got: %s", rec.messages())
	}
	if got.Level != slog.LevelWarn {
		t.Errorf("login-required logged at %s, want WARN", got.Level)
	}
	requireTrigger(t, got, "the login-required line")
}
