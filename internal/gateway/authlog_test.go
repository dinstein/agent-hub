package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/oauthflow"
)

// A stdio gateway's refresh is invisible unless this file's lines are
// emitted: the 401/403 round tripper below it swallows a refresh failure by
// design (it hands the downstream's own 401 back, WWW-Authenticate and all),
// and the daemon — which is the only other thing that logs a refresh — is not
// running in the case these tests describe.

// recordHandler collects records so a test can assert on message and attrs
// without parsing formatted output.
type recordHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}

func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler      { return h }

// find returns the first record whose message matches, and whether there was
// one at all.
func (h *recordHandler) find(msg string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

// messages lists what was logged, for failure output.
func (h *recordHandler) messages() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.recs))
	for _, r := range h.recs {
		out = append(out, r.Level.String()+" "+r.Message)
	}
	return strings.Join(out, "; ")
}

// attr reads one attribute off a record.
func attr(r slog.Record, key string) (slog.Value, bool) {
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

// fakeRefresher stands in for the coordinator. The real one cannot reach a
// test authorization server: the gateway builds its OAuth client without
// AllowLoopback, so an httptest token endpoint is screened out before any
// success is possible.
type fakeRefresher struct {
	tok string
	err error
}

func (f fakeRefresher) Refresh(context.Context, string) (*oauthflow.State, string, error) {
	return nil, f.tok, f.err
}

func TestRenewLogsASuccessfulRefresh(t *testing.T) {
	t.Parallel()
	h := &recordHandler{}
	renew := loggedRenew(fakeRefresher{tok: "fresh"}, "alpha", "_global", slog.New(h))

	tok, err := renew(context.Background())
	if err != nil || tok != "fresh" {
		t.Fatalf("renew() = %q, %v; want the fresh token and no error", tok, err)
	}

	rec, ok := h.find("access token refreshed")
	if !ok {
		t.Fatalf("no success line logged; got: %s", h.messages())
	}
	if rec.Level != slog.LevelInfo {
		t.Errorf("success logged at %s, want INFO", rec.Level)
	}
	if v, ok := attr(rec, logx.FieldServer); !ok || v.String() != "alpha" {
		t.Errorf("success line carries %s=%v, want the mandatory server attr", logx.FieldServer, v)
	}
	if v, ok := attr(rec, "superseded"); !ok || v.Bool() {
		t.Errorf("superseded=%v on a refresh this call performed itself, want false", v)
	}
}

// A superseded refresh is a SUCCESS — another process stored a usable
// credential first — and the field is what tells the two apart in the log,
// so a reader can see the file lock doing its job instead of reading a
// doubled refresh into two "refreshed" lines.
func TestRenewLogsASupersededRefreshAsSuccess(t *testing.T) {
	t.Parallel()
	h := &recordHandler{}
	renew := loggedRenew(
		fakeRefresher{tok: "someone-elses", err: oauthflow.ErrRefreshSuperseded},
		"alpha", "_global", slog.New(h))

	tok, err := renew(context.Background())
	if err != nil || tok != "someone-elses" {
		t.Fatalf("renew() = %q, %v; want the other writer's token and no error", tok, err)
	}

	rec, ok := h.find("access token refreshed")
	if !ok {
		t.Fatalf("no success line logged; got: %s", h.messages())
	}
	if v, ok := attr(rec, "superseded"); !ok || !v.Bool() {
		t.Errorf("superseded=%v on ErrRefreshSuperseded, want true", v)
	}
}

// The dead end that needs a human: distinguishing it from a transient
// failure is the difference between "wait" and "run auth login", and the
// message matches the daemon's so both read as one event.
func TestRenewLogsAMissingRefreshTokenAsNeedingALogin(t *testing.T) {
	t.Parallel()
	h := &recordHandler{}
	renew := loggedRenew(fakeRefresher{err: oauthflow.ErrNoRefreshToken}, "alpha", "_global", slog.New(h))

	if _, err := renew(context.Background()); !errors.Is(err, oauthflow.ErrNoRefreshToken) {
		t.Fatalf("renew() error = %v, want it to carry ErrNoRefreshToken through", err)
	}

	rec, ok := h.find("token cannot be refreshed without a new login")
	if !ok {
		t.Fatalf("no login-required line logged; got: %s", h.messages())
	}
	if rec.Level != slog.LevelWarn {
		t.Errorf("login-required logged at %s, want WARN", rec.Level)
	}
}

// The case the whole change exists for: the layer below returns the
// downstream's 401 and drops this error on the floor, so if it is not logged
// here it is not recorded anywhere.
func TestRenewLogsAFailedRefreshWithItsError(t *testing.T) {
	t.Parallel()
	h := &recordHandler{}
	boom := errors.New("token endpoint said 500")
	renew := loggedRenew(fakeRefresher{err: boom}, "alpha", "_global", slog.New(h))

	if _, err := renew(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("renew() error = %v, want the coordinator's error unwrapped", err)
	}

	rec, ok := h.find("access token refresh failed; the downstream rejection stands")
	if !ok {
		t.Fatalf("no failure line logged; got: %s", h.messages())
	}
	if rec.Level != slog.LevelWarn {
		t.Errorf("failure logged at %s, want WARN", rec.Level)
	}
	v, ok := attr(rec, "error")
	if !ok || !strings.Contains(v.String(), "token endpoint said 500") {
		t.Errorf("failure line carries error=%v, want the underlying cause", v)
	}
}

// The attempt line is what separates "hung 30s on the sibling refresh lock"
// from "never tried": both produce the same outcome line, and only one of
// them is a problem.
func TestRenewLogsTheAttemptBeforeTheOutcome(t *testing.T) {
	t.Parallel()
	h := &recordHandler{}
	renew := loggedRenew(fakeRefresher{tok: "fresh"}, "alpha", "derived-key", slog.New(h))

	if _, err := renew(context.Background()); err != nil {
		t.Fatalf("renew() error = %v", err)
	}

	rec, ok := h.find("refreshing a downstream access token")
	if !ok {
		t.Fatalf("no attempt line logged; got: %s", h.messages())
	}
	if rec.Level != slog.LevelDebug {
		t.Errorf("attempt logged at %s, want DEBUG: it fires per 401 on every derivation", rec.Level)
	}
	// The scope is on the attempt and not the outcome on purpose: refresh is
	// keyed on the SERVER (one refresh token, one lock), so an outcome line
	// naming a scope would claim a per-scope renewal that never happens.
	if v, ok := attr(rec, "scope"); !ok || v.String() != "derived-key" {
		t.Errorf("attempt line carries scope=%v, want the derive key that asked", v)
	}
}
