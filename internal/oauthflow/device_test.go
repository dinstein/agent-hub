package oauthflow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// recordingSleeper stands in for the poll wait so tests observe the
// interval ladder without spending wall-clock time on it.
type recordingSleeper struct {
	mu     sync.Mutex
	slept  []time.Duration
	clock  time.Time
	frozen bool
}

func newSleeper() *recordingSleeper {
	return &recordingSleeper{clock: time.Unix(1_700_000_000, 0)}
}

func (s *recordingSleeper) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slept = append(s.slept, d)
	if !s.frozen {
		s.clock = s.clock.Add(d)
	}
	return nil
}

func (s *recordingSleeper) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clock
}

func (s *recordingSleeper) intervals() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.slept...)
}

func TestDeviceAuthorizationStart(t *testing.T) {
	as := newFakeAS(t)
	as.deviceEndpoint = true
	as.deviceInterval = 7
	c := as.client()
	da, err := c.StartDevice(context.Background(), DeviceRequest{
		Metadata: as.metadata(),
		ClientID: "client-abc",
		Scopes:   []string{"read"},
		Resource: "https://mcp.example/mcp",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if da.UserCode != "WDJB-MJHT" || da.DeviceCode != "device-code-1" {
		t.Fatalf("device auth = %+v", da)
	}
	if da.PollInterval() != 7*time.Second {
		t.Fatalf("interval = %s", da.PollInterval())
	}
	if da.ExpiresIn != 1800 {
		t.Fatalf("expires_in = %d", da.ExpiresIn)
	}
}

func TestDeviceIntervalDefaults(t *testing.T) {
	da := &DeviceAuthorization{}
	if da.PollInterval() != DefaultDeviceInterval {
		t.Fatalf("interval = %s, want the RFC 8628 default", da.PollInterval())
	}
	now := time.Unix(1000, 0)
	if got := da.Expiry(now); got != now.Add(DefaultDeviceExpiry) {
		t.Fatalf("expiry = %s", got)
	}
}

func TestStartDeviceWithoutEndpoint(t *testing.T) {
	as := newFakeAS(t) // deviceEndpoint stays false
	_, err := as.client().StartDevice(context.Background(), DeviceRequest{Metadata: as.metadata()})
	if err == nil {
		t.Fatal("device start without an endpoint must fail, not fall back silently")
	}
	var fe *FlowError
	if !errors.As(err, &fe) || fe.Suggestion == "" {
		t.Fatalf("missing suggestion: %v", err)
	}
}

// TestDevicePollHonoursSlowDown is the RFC 8628 §3.5 rule: slow_down raises
// the interval by at least 5s and the raise is PERMANENT for the rest of
// the loop.
func TestDevicePollHonoursSlowDown(t *testing.T) {
	as := newFakeAS(t)
	as.deviceEndpoint = true
	as.deviceInterval = 2
	as.deviceSlowDown = 2
	as.devicePending = 2

	sl := newSleeper()
	c := as.client()
	da, err := c.StartDevice(context.Background(), DeviceRequest{Metadata: as.metadata(), ClientID: "client-abc"})
	if err != nil {
		t.Fatal(err)
	}
	p := &DevicePoller{Client: c, Now: sl.now, Sleep: sl.sleep}
	tok, err := p.PollDevice(context.Background(), as.metadata(), "client-abc", da, "")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if tok.AccessToken != "access-device" {
		t.Fatalf("token = %q", tok.AccessToken)
	}
	// 2 slow_down answers then 2 pending answers: 2s → 7s → 12s → 12s →
	// 12s. The interval never drops back down.
	want := []time.Duration{7 * time.Second, 12 * time.Second, 12 * time.Second, 12 * time.Second}
	got := sl.intervals()
	if len(got) != len(want) {
		t.Fatalf("intervals = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("interval[%d] = %s want %s (full ladder %v)", i, got[i], want[i], got)
		}
	}
}

func TestDevicePollStopsAtDeviceCodeExpiry(t *testing.T) {
	as := newFakeAS(t)
	as.deviceEndpoint = true
	as.devicePending = 1_000_000 // never authorizes

	sl := newSleeper()
	c := as.client()
	da := &DeviceAuthorization{DeviceCode: "d", UserCode: "u", ExpiresIn: 20, Interval: 5}
	p := &DevicePoller{Client: c, Now: sl.now, Sleep: sl.sleep}
	_, err := p.PollDevice(context.Background(), as.metadata(), "client-abc", da, "")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	// 20s lifetime at 5s intervals: the loop must not run forever.
	if n := len(sl.intervals()); n > 6 {
		t.Fatalf("polled %d times inside a 20s device code lifetime", n)
	}
}

// TestDevicePollSurfacesDenial: a denial ends the loop immediately. It must
// not be mistaken for authorization_pending, or the CLI polls a dead device
// code until it expires.
func TestDevicePollSurfacesDenial(t *testing.T) {
	as := newFakeAS(t)
	as.deviceEndpoint = true
	as.deviceDenied = true

	sl := newSleeper()
	c := as.client()
	da := &DeviceAuthorization{DeviceCode: "d", UserCode: "u", ExpiresIn: 600, Interval: 1}
	p := &DevicePoller{Client: c, Now: sl.now, Sleep: sl.sleep}
	_, err := p.PollDevice(context.Background(), as.metadata(), "client-abc", da, "")
	if !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("err = %v, want ErrAuthorizationDenied", err)
	}
	if n := len(sl.intervals()); n != 0 {
		t.Fatalf("a denial must stop the loop at once, but it slept %d times", n)
	}
	if !errors.Is(&TokenError{Code: errExpiredToken}, ErrTimeout) {
		t.Fatal("expired_token must classify as ErrTimeout")
	}
}

func TestDevicePollRefusesEmptyAuthorization(t *testing.T) {
	p := &DevicePoller{Client: NewClient(Config{})}
	if _, err := p.PollDevice(context.Background(), &AuthServerMetadata{}, "c", nil, ""); err == nil {
		t.Fatal("polling with no device authorization must fail")
	}
}

func TestSlowDownIncrementIsAtLeastFiveSeconds(t *testing.T) {
	if SlowDownIncrement < 5*time.Second {
		t.Fatalf("RFC 8628 §3.5 requires at least 5s, got %s", SlowDownIncrement)
	}
}

func TestDeviceIntervalIsCapped(t *testing.T) {
	as := newFakeAS(t)
	as.deviceEndpoint = true
	as.deviceSlowDown = 50 // a misbehaving AS
	as.devicePending = 0

	sl := newSleeper()
	sl.frozen = true // never advance the clock: only the cap can stop growth
	c := as.client()
	da := &DeviceAuthorization{DeviceCode: "d", UserCode: "u", ExpiresIn: 3600, Interval: 5}
	p := &DevicePoller{Client: c, Now: sl.now, Sleep: sl.sleep}
	if _, err := p.PollDevice(context.Background(), as.metadata(), "client-abc", da, ""); err != nil {
		t.Fatalf("poll: %v", err)
	}
	for _, d := range sl.intervals() {
		if d > maxDeviceInterval {
			t.Fatalf("interval %s exceeded the cap %s", d, maxDeviceInterval)
		}
	}
}
