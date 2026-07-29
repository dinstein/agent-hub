package httpbridge_test

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/tier"
)

// "localhost" must be reachable over BOTH loopback families, or the endpoint
// works on one machine and refuses connections on the next depending on how
// the client's resolver ordered the answers.
func TestListenBindsBothLoopbackFamilies(t *testing.T) {
	t.Parallel()
	listeners, warn, err := httpbridge.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	if len(listeners) == 0 {
		t.Fatal("Listen returned no listeners")
	}
	if len(listeners) == 1 {
		// Single-stack host: degrading is correct, but it must SAY so.
		if warn == nil {
			t.Fatal("only one family bound and no warning was reported")
		}
		t.Logf("single-stack host: %v", warn)
		return
	}
	// Both families must land on the SAME port: port 0 resolution has to be
	// shared, or half the endpoint is somewhere nobody was told about.
	ports := map[int]bool{}
	families := map[bool]bool{}
	for _, l := range listeners {
		ta, ok := l.Addr().(*net.TCPAddr)
		if !ok {
			t.Fatalf("listener address is %T, want *net.TCPAddr", l.Addr())
		}
		ports[ta.Port] = true
		families[ta.IP.To4() != nil] = true
		if !ta.IP.IsLoopback() {
			t.Errorf("bound a non-loopback address %s for localhost", ta)
		}
	}
	if len(ports) != 1 {
		t.Errorf("the two families bound different ports: %v", ports)
	}
	if len(families) != 2 {
		t.Errorf("both listeners are the same family")
	}
}

func TestListenRejectsAMalformedAddress(t *testing.T) {
	t.Parallel()
	if _, _, err := httpbridge.Listen("no-port-here"); err == nil {
		t.Fatal("Listen accepted an address without a port")
	}
}

// Serve must answer on every listener it was given and stop when the context
// is cancelled, closing them on the way out.
func TestServeAnswersOnEveryListenerAndStops(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	_, value := mustCreate(t, store, httpbridge.CreateSpec{Name: "a", Tier: tier.Write})
	bridge, err := httpbridge.New(httpbridge.Options{
		Dispatcher: &recordingDispatcher{},
		Auth:       &httpbridge.Authenticator{Tokens: store},
	})
	if err != nil {
		t.Fatal(err)
	}
	listeners, _, err := httpbridge.Listen("localhost:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bridge.Serve(ctx, listeners...) }()

	client := &http.Client{Timeout: 5 * time.Second}
	for _, l := range listeners {
		url := "http://" + l.Addr().String() + httpbridge.DefaultPath
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(initFrame))
		req.Header.Set("Authorization", "Bearer "+value)
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", url, res.StatusCode)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil after cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
	// The listeners are closed: a fresh connection must be refused.
	addr := listeners[0].Addr().String()
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = c.Close()
		t.Errorf("%s still accepts connections after shutdown", addr)
	}
}

func TestServeNeedsAListener(t *testing.T) {
	t.Parallel()
	bridge, err := httpbridge.New(httpbridge.Options{
		Dispatcher: &recordingDispatcher{}, Auth: &httpbridge.Authenticator{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.Serve(context.Background()); err == nil {
		t.Fatal("Serve accepted an empty listener set")
	}
}
