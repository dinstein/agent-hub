package oauthlogin_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/eventlog"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/oauthlogin"
)

// recordedKinds runs one login against fake and returns the kinds it left in
// the stream, in order.
func recordedKinds(t *testing.T, fake oauthlogin.Flow) []eventlog.Kind {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	stream, err := eventlog.Open(path, eventlog.Options{PID: 1})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	m, err := oauthlogin.New(oauthlogin.Config{
		Flows:  func(bool) oauthlogin.Flow { return fake },
		Events: stream,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	s, err := m.Start(oauthlogin.Request{ServerID: "github", ResourceURL: "https://example.test/mcp"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := m.Get(s.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Phase != oauthlogin.PhasePending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("login never left the pending phase")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stream.Sync()
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	res, err := eventlog.Read(path, eventlog.Query{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	kinds := make([]eventlog.Kind, 0, len(res.Records))
	for _, r := range res.Records {
		if r.Server != "github" || r.Session == "" {
			t.Errorf("record is not attributable: %+v", r)
		}
		kinds = append(kinds, r.Kind)
	}
	return kinds
}

// flowFunc adapts a function to the Flow interface.
type flowFunc func(context.Context, oauthflow.LoginRequest) (*oauthflow.LoginResult, error)

func (f flowFunc) Login(ctx context.Context, req oauthflow.LoginRequest) (*oauthflow.LoginResult, error) {
	return f(ctx, req)
}

// The whole point: a login that blocks on a human leaves a mark WHILE it is
// blocked. Without the waiting record, "pending for ten minutes because
// nobody opened the tab" and "failed at discovery a second in" are the same
// silence.
func TestLoginRecordsStartedWaitingAndCompleted(t *testing.T) {
	fake := flowFunc(func(_ context.Context, req oauthflow.LoginRequest) (*oauthflow.LoginResult, error) {
		_ = req.Open("https://provider.test/authorize?x=1")
		return &oauthflow.LoginResult{Mode: oauthflow.ModeLoopback}, nil
	})

	got := recordedKinds(t, fake)

	want := []eventlog.Kind{
		eventlog.KindOAuthLoginStarted,
		eventlog.KindOAuthLoginWaiting,
		eventlog.KindOAuthLoginCompleted,
	}
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
}

func TestFailedLoginIsRecordedAsFailed(t *testing.T) {
	fake := flowFunc(func(context.Context, oauthflow.LoginRequest) (*oauthflow.LoginResult, error) {
		return nil, errors.New("discovery refused")
	})

	got := recordedKinds(t, fake)

	want := []eventlog.Kind{eventlog.KindOAuthLoginStarted, eventlog.KindOAuthLoginFailed}
	if len(got) != len(want) || got[len(got)-1] != eventlog.KindOAuthLoginFailed {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
}
