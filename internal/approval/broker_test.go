package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

const testWait = 5 * time.Second

func testRequest() Request {
	return Request{
		Server:      "github",
		Tool:        "delete_repo",
		ArgsJSON:    json.RawMessage(`{"repo":"a/b"}`),
		ArgsHash:    "hash-1",
		Fingerprint: "v1:aaaa",
		GateReason:  ReasonDestructive,
		Client:      "claude-code",
		SessionID:   "sid-1",
	}
}

// askAsync starts Ask in a goroutine and returns the decision channel.
func askAsync(b *MemBroker, ctx context.Context, req Request) <-chan Decision {
	out := make(chan Decision, 1)
	go func() { out <- b.Ask(ctx, req) }()
	return out
}

func recvReq(t *testing.T, ch <-chan Request) Request {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(testWait):
		t.Fatal("timed out waiting for fanned-out request")
		return Request{}
	}
}

func recvDecision(t *testing.T, ch <-chan Decision) Decision {
	t.Helper()
	select {
	case d := <-ch:
		return d
	case <-time.After(testWait):
		t.Fatal("timed out waiting for Ask to return")
		return Denied
	}
}

func TestAskNoFrontendUnreachable(t *testing.T) {
	b := NewMemBroker(Options{})
	if d := b.Ask(context.Background(), testRequest()); d != Unreachable {
		t.Fatalf("Ask with no frontend = %v, want Unreachable", d)
	}
	// A cancelled subscription must not count as a frontend.
	_, cancel := b.Subscribe("gui")
	cancel()
	if d := b.Ask(context.Background(), testRequest()); d != Unreachable {
		t.Fatalf("Ask after last frontend left = %v, want Unreachable", d)
	}
	if n := b.FrontendCount(); n != 0 {
		t.Fatalf("FrontendCount = %d, want 0", n)
	}
}

func TestApproveFlow(t *testing.T) {
	b := NewMemBroker(Options{})
	ch, cancel := b.Subscribe("cli")
	defer cancel()

	before := time.Now()
	done := askAsync(b, context.Background(), testRequest())
	got := recvReq(t, ch)
	if got.Token == "" {
		t.Fatal("broker did not stamp a token")
	}
	if got.Deadline.Before(before.Add(defaultTTL - time.Minute)) {
		t.Fatalf("broker did not stamp deadline near now+%v: %v", defaultTTL, got.Deadline)
	}
	if string(got.ArgsJSON) != `{"repo":"a/b"}` {
		t.Fatalf("ArgsJSON did not reach frontend: %q", got.ArgsJSON)
	}
	if err := b.Answer(got.Token, true, RememberNone); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if d := recvDecision(t, done); d != Approved {
		t.Fatalf("Ask = %v, want Approved", d)
	}
}

func TestDenyFlowAndAlreadyDecided(t *testing.T) {
	b := NewMemBroker(Options{})
	ch, cancel := b.Subscribe("cli")
	defer cancel()

	done := askAsync(b, context.Background(), testRequest())
	got := recvReq(t, ch)
	if err := b.Answer(got.Token, false, RememberForever); err != nil {
		// remember is ignored on deny: denials are never remembered.
		t.Fatalf("Answer(deny): %v", err)
	}
	if d := recvDecision(t, done); d != Denied {
		t.Fatalf("Ask = %v, want Denied", d)
	}
	if err := b.Answer(got.Token, true, RememberNone); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("second Answer err = %v, want ErrAlreadyDecided", err)
	}
}

func TestTimeoutAutoDeny(t *testing.T) {
	b := NewMemBroker(Options{})
	ch, cancel := b.Subscribe("cli")
	defer cancel()

	req := testRequest()
	req.Deadline = time.Now().Add(80 * time.Millisecond)
	done := askAsync(b, context.Background(), req)
	got := recvReq(t, ch)
	if d := recvDecision(t, done); d != Timedout {
		t.Fatalf("Ask = %v, want Timedout", d)
	}
	if err := b.Answer(got.Token, true, RememberNone); !errors.Is(err, ErrExpired) {
		t.Fatalf("late Answer err = %v, want ErrExpired", err)
	}
}

func TestAnswerAfterDeadlineBeforeTimerExpired(t *testing.T) {
	// Fake clock jumps past the deadline while Ask's real timer has not
	// fired: Answer itself must refuse (late approvals never execute).
	now := time.Now()
	var mu sync.Mutex
	cur := now
	b := NewMemBroker(Options{Now: func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}})
	ch, cancel := b.Subscribe("cli")
	defer cancel()

	req := testRequest()
	req.Deadline = now.Add(time.Hour)
	done := askAsync(b, context.Background(), req)
	got := recvReq(t, ch)

	mu.Lock()
	cur = now.Add(2 * time.Hour)
	mu.Unlock()
	if err := b.Answer(got.Token, true, RememberNone); !errors.Is(err, ErrExpired) {
		t.Fatalf("Answer past deadline err = %v, want ErrExpired", err)
	}
	if d := recvDecision(t, done); d != Timedout {
		t.Fatalf("Ask = %v, want Timedout", d)
	}
}

func TestCtxCancelUnreachable(t *testing.T) {
	b := NewMemBroker(Options{})
	ch, cancel := b.Subscribe("cli")
	defer cancel()

	ctx, cancelCtx := context.WithCancel(context.Background())
	done := askAsync(b, ctx, testRequest())
	got := recvReq(t, ch)
	cancelCtx()
	if d := recvDecision(t, done); d != Unreachable {
		t.Fatalf("Ask after ctx cancel = %v, want Unreachable", d)
	}
	if err := b.Answer(got.Token, true, RememberNone); !errors.Is(err, ErrExpired) {
		t.Fatalf("Answer after asker vanished err = %v, want ErrExpired", err)
	}
}

func TestStaleOnAnswer(t *testing.T) {
	dir := t.TempDir()
	al, err := OpenAllowlist(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := NewMemBroker(Options{
		Allowlist: al,
		Check:     func(server, tool, fp string) bool { return false }, // definition drifted
	})
	ch, cancel := b.Subscribe("cli")
	defer cancel()

	done := askAsync(b, context.Background(), testRequest())
	got := recvReq(t, ch)
	if err := b.Answer(got.Token, true, RememberForever); !errors.Is(err, ErrStale) {
		t.Fatalf("Answer on drifted tool err = %v, want ErrStale", err)
	}
	if d := recvDecision(t, done); d != Stale {
		t.Fatalf("Ask = %v, want Stale", d)
	}
	// A stale approval must not have written a remember grant.
	if al.Match(testRequest()) {
		t.Fatal("stale approval leaked into the allowlist")
	}
	if len(al.Entries()) != 0 {
		t.Fatalf("allowlist entries = %d, want 0", len(al.Entries()))
	}
}

func TestMultiFrontendFirstWins(t *testing.T) {
	b := NewMemBroker(Options{})
	chA, cancelA := b.Subscribe("gui")
	defer cancelA()
	chB, cancelB := b.Subscribe("cli")
	defer cancelB()
	if n := b.FrontendCount(); n != 2 {
		t.Fatalf("FrontendCount = %d, want 2", n)
	}

	done := askAsync(b, context.Background(), testRequest())
	gotA := recvReq(t, chA)
	gotB := recvReq(t, chB)
	if gotA.Token != gotB.Token {
		t.Fatalf("frontends saw different tokens: %q vs %q", gotA.Token, gotB.Token)
	}
	if err := b.Answer(gotA.Token, true, RememberNone); err != nil {
		t.Fatalf("first Answer: %v", err)
	}
	if err := b.Answer(gotB.Token, false, RememberNone); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("second Answer err = %v, want ErrAlreadyDecided (first wins)", err)
	}
	if d := recvDecision(t, done); d != Approved {
		t.Fatalf("Ask = %v, want Approved (first answer)", d)
	}
}

func TestAnswerUnknownToken(t *testing.T) {
	b := NewMemBroker(Options{})
	if err := b.Answer("no-such-token", true, RememberNone); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("err = %v, want ErrUnknownToken", err)
	}
}

func TestSubscribeCancelIdempotentAndCloses(t *testing.T) {
	b := NewMemBroker(Options{})
	ch, cancel := b.Subscribe("gui")
	cancel()
	cancel() // must not panic
	if _, open := <-ch; open {
		t.Fatal("channel still open after cancel")
	}
}

func TestSubscribeReplaysPending(t *testing.T) {
	b := NewMemBroker(Options{})
	ch1, cancel1 := b.Subscribe("gui")
	defer cancel1()

	done := askAsync(b, context.Background(), testRequest())
	got := recvReq(t, ch1)

	// A frontend attaching after the Ask must still see the queue.
	ch2, cancel2 := b.Subscribe("cli")
	defer cancel2()
	replayed := recvReq(t, ch2)
	if replayed.Token != got.Token {
		t.Fatalf("replayed token %q != original %q", replayed.Token, got.Token)
	}
	if err := b.Answer(replayed.Token, false, RememberNone); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if d := recvDecision(t, done); d != Denied {
		t.Fatalf("Ask = %v, want Denied", d)
	}
}

func TestRememberSession(t *testing.T) {
	b := NewMemBroker(Options{})
	ch, cancel := b.Subscribe("cli")

	done := askAsync(b, context.Background(), testRequest())
	got := recvReq(t, ch)
	if err := b.Answer(got.Token, true, RememberSession); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if d := recvDecision(t, done); d != Approved {
		t.Fatalf("Ask = %v, want Approved", d)
	}
	cancel() // no frontends left: only a remembered grant can approve now

	// Same session + fingerprint: auto-approved without any frontend.
	if d := b.Ask(context.Background(), testRequest()); d != Approved {
		t.Fatalf("remembered Ask = %v, want Approved", d)
	}
	// Different session: no grant, no frontend -> Unreachable.
	other := testRequest()
	other.SessionID = "sid-2"
	if d := b.Ask(context.Background(), other); d != Unreachable {
		t.Fatalf("other-session Ask = %v, want Unreachable", d)
	}
	// Drifted fingerprint: grant must not match.
	drifted := testRequest()
	drifted.Fingerprint = "v1:bbbb"
	if d := b.Ask(context.Background(), drifted); d != Unreachable {
		t.Fatalf("drifted-fingerprint Ask = %v, want Unreachable", d)
	}
	// Session end drops the grant.
	b.ForgetSession("sid-1")
	if d := b.Ask(context.Background(), testRequest()); d != Unreachable {
		t.Fatalf("Ask after ForgetSession = %v, want Unreachable", d)
	}
}

func TestRememberSessionWithoutSessionID(t *testing.T) {
	b := NewMemBroker(Options{})
	ch, cancel := b.Subscribe("cli")
	defer cancel()

	req := testRequest()
	req.SessionID = ""
	done := askAsync(b, context.Background(), req)
	got := recvReq(t, ch)
	err := b.Answer(got.Token, true, RememberSession)
	if !errors.Is(err, ErrRememberFailed) {
		t.Fatalf("Answer err = %v, want ErrRememberFailed", err)
	}
	// The single approval still stands.
	if d := recvDecision(t, done); d != Approved {
		t.Fatalf("Ask = %v, want Approved", d)
	}
}

func TestRememberForeverRoundTrip(t *testing.T) {
	dir := t.TempDir()
	al, err := OpenAllowlist(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := NewMemBroker(Options{Allowlist: al})
	ch, cancel := b.Subscribe("cli")

	done := askAsync(b, context.Background(), testRequest())
	got := recvReq(t, ch)
	if err := b.Answer(got.Token, true, RememberForever); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if d := recvDecision(t, done); d != Approved {
		t.Fatalf("Ask = %v, want Approved", d)
	}
	cancel()

	// Fresh broker + reopened allowlist (daemon restart): grant survives.
	al2, err := OpenAllowlist(dir)
	if err != nil {
		t.Fatal(err)
	}
	b2 := NewMemBroker(Options{Allowlist: al2})
	if d := b2.Ask(context.Background(), testRequest()); d != Approved {
		t.Fatalf("allowlisted Ask after restart = %v, want Approved", d)
	}
	// Different args still hit: remember-forever binds the tool, not args.
	otherArgs := testRequest()
	otherArgs.ArgsHash = "hash-2"
	if d := b2.Ask(context.Background(), otherArgs); d != Approved {
		t.Fatalf("allowlisted Ask with new args = %v, want Approved", d)
	}
	// Drifted fingerprint misses: no frontend -> Unreachable, must re-approve.
	drifted := testRequest()
	drifted.Fingerprint = "v1:cccc"
	if d := b2.Ask(context.Background(), drifted); d != Unreachable {
		t.Fatalf("drifted Ask = %v, want Unreachable (stale grants never hit)", d)
	}
}

func TestRememberForeverWithoutAllowlist(t *testing.T) {
	b := NewMemBroker(Options{})
	ch, cancel := b.Subscribe("cli")
	defer cancel()

	done := askAsync(b, context.Background(), testRequest())
	got := recvReq(t, ch)
	if err := b.Answer(got.Token, true, RememberForever); !errors.Is(err, ErrRememberFailed) {
		t.Fatalf("Answer err = %v, want ErrRememberFailed", err)
	}
	if d := recvDecision(t, done); d != Approved {
		t.Fatalf("Ask = %v, want Approved (approval stands, grant failed)", d)
	}
}

func TestConcurrentAskApprove(t *testing.T) {
	// Buffer sized above the burst: a full subscriber channel drops the
	// fan-out (degrading to Timedout), which is not what this test probes.
	b := NewMemBroker(Options{SubscriberBuffer: 64, DefaultTTL: testWait})
	ch, cancel := b.Subscribe("cli")
	defer cancel()

	// Frontend goroutine approves everything it sees.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case r := <-ch:
				_ = b.Answer(r.Token, true, RememberNone)
			case <-stop:
				return
			}
		}
	}()

	const n = 32
	var wg sync.WaitGroup
	results := make([]Decision, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := testRequest()
			req.SessionID = fmt.Sprintf("sid-%d", i)
			req.Fingerprint = fmt.Sprintf("v1:%04d", i)
			results[i] = b.Ask(context.Background(), req)
		}(i)
	}
	wg.Wait()
	for i, d := range results {
		if d != Approved {
			t.Fatalf("Ask %d = %v, want Approved", i, d)
		}
	}
}

func TestConcurrentDuplicateAnswers(t *testing.T) {
	b := NewMemBroker(Options{})
	ch, cancel := b.Subscribe("cli")
	defer cancel()

	done := askAsync(b, context.Background(), testRequest())
	got := recvReq(t, ch)

	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = b.Answer(got.Token, i%2 == 0, RememberNone)
		}(i)
	}
	wg.Wait()

	var wins int
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrAlreadyDecided):
		default:
			t.Fatalf("unexpected Answer error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("nil-error answers = %d, want exactly 1 (first wins)", wins)
	}
	if d := recvDecision(t, done); d != Approved && d != Denied {
		t.Fatalf("Ask = %v, want the winner's Approved or Denied", d)
	}
}
