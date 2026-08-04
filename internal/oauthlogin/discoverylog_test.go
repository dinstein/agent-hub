package oauthlogin

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/oauthflow"
)

// errNotDiscovery is a failure that never reached the discovery chain.
var errNotDiscovery = errors.New("no browser on this host")

func managerWithLog(t *testing.T, flow Flow) (*Manager, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	m, err := New(Config{
		Flows: func(bool) Flow { return flow },
		Log:   slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, &buf
}

// oauthflow collects the discovery trace and has no logger to write it with,
// by design. This is the other end of that arrangement — and until it existed
// the data was gathered and dropped, on the path where "this provider will not
// connect" is most often reported.
func TestAFailedLoginLogsTheDiscoveryChain(t *testing.T) {
	flow := &fakeFlow{err: &oauthflow.FlowError{
		Type:      oauthflow.ErrorTypeDiscovery,
		Discovery: oauthflow.DiscoveryFailed,
		Attempted: []oauthflow.Attempt{
			{URL: "https://as.example/.well-known/oauth-authorization-server", Outcome: oauthflow.AttemptNoDocument},
			{URL: "https://as.example/.well-known/openid-configuration", Outcome: oauthflow.AttemptUnusable},
		},
	}}
	m, buf := managerWithLog(t, flow)

	sess, err := m.Start(Request{ServerID: "linear", Issuer: "https://as.example"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the login to fail", func() bool {
		s, gerr := m.Get(sess.ID)
		return gerr == nil && s.Phase == PhaseFailed
	})

	out := buf.String()
	if !strings.Contains(out, `"msg":"oauth discovery finished"`) {
		t.Fatalf("the chain summary was not logged: %s", out)
	}
	if !strings.Contains(out, `"status":"`+string(oauthflow.DiscoveryFailed)+`"`) {
		t.Fatalf("the summary does not carry the status: %s", out)
	}
	// One line per candidate, each with its outcome: the summary alone still
	// cannot say WHICH URL was wrong, which is the whole question.
	for _, want := range []string{
		`"outcome":"` + oauthflow.AttemptNoDocument + `"`,
		`"outcome":"` + oauthflow.AttemptUnusable + `"`,
		`"url":"https://as.example/.well-known/openid-configuration"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("candidate trace missing %s: %s", want, out)
		}
	}
	if n := strings.Count(out, `"msg":"oauth discovery candidate"`); n != 2 {
		t.Fatalf("logged %d candidates, want one per attempt", n)
	}
}

// A login that SUCCEEDED through the synthesized-endpoints fallback is one
// candidate away from one that failed, and it is the case that goes wrong
// later: a 403 from a guessed /register means something different from a 403
// from an advertised registration_endpoint.
func TestASuccessfulLoginAlsoLogsHowDiscoveryWent(t *testing.T) {
	flow := &fakeFlow{result: &oauthflow.LoginResult{
		Mode:  oauthflow.ModeLoopback,
		State: &oauthflow.State{},
		Discovery: &oauthflow.DiscoveryResult{
			Status: oauthflow.DiscoveryDefaults,
			Attempted: []oauthflow.Attempt{
				{URL: "https://as.example/.well-known/oauth-authorization-server", Outcome: oauthflow.AttemptNoDocument},
			},
		},
	}}
	m, buf := managerWithLog(t, flow)

	sess, err := m.Start(Request{ServerID: "linear", Issuer: "https://as.example"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the login to complete", func() bool {
		s, gerr := m.Get(sess.ID)
		return gerr == nil && s.Phase == PhaseComplete
	})

	out := buf.String()
	if !strings.Contains(out, `"status":"`+string(oauthflow.DiscoveryDefaults)+`"`) {
		t.Fatalf("a successful login did not record that it used synthesized endpoints: %s", out)
	}
}

// A failure that never reached discovery has no chain to describe, and an
// empty one would read as "every candidate was fine".
func TestALoginThatNeverDiscoveredLogsNoChain(t *testing.T) {
	flow := &fakeFlow{err: errNotDiscovery}
	m, buf := managerWithLog(t, flow)

	sess, err := m.Start(Request{ServerID: "linear", Issuer: "https://as.example"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the login to fail", func() bool {
		s, gerr := m.Get(sess.ID)
		return gerr == nil && s.Phase == PhaseFailed
	})

	if strings.Contains(buf.String(), "oauth discovery finished") {
		t.Fatalf("a non-discovery failure invented a chain: %s", buf.String())
	}
}
