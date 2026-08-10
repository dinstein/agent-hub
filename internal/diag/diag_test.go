package diag_test

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/diag"
)

// TestAddrReadsEnv covers the ordinary case (unset means no endpoint) and
// the blank-but-set one, which must read as "off" rather than as an empty
// address that net.Listen would happily bind to every interface.
func TestAddrReadsEnv(t *testing.T) {
	for _, tc := range []struct{ set, want string }{
		{"", ""},
		{"   ", ""},
		{"127.0.0.1:0", "127.0.0.1:0"},
		{"  127.0.0.1:0\n", "127.0.0.1:0"},
	} {
		t.Setenv(diag.EnvAddr, tc.set)
		if got := diag.Addr(); got != tc.want {
			t.Errorf("Addr() with %q = %q, want %q", tc.set, got, tc.want)
		}
	}
}

// TestServeRefusesNonLoopback is the fail-closed proof. Every address here
// is one whose loopback-ness cannot be proven, and each must be refused
// rather than bound — a bound listener is what publishes heap dumps holding
// downstream credentials.
func TestServeRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:0",     // every interface
		":0",            // every interface, spelled shorter
		"",              // ditto, and what a blank env var would produce
		"192.168.1.5:0", // private, but not this machine
		"8.8.8.8:0",     // public
		"[::]:0",        // every interface, v6
		"example.com:0", // a name, which the predicate cannot resolve
		"not an addr",   // unparsable
	} {
		s, err := diag.Serve(addr)
		if err == nil {
			t.Errorf("Serve(%q) bound a listener at %s; want refusal", addr, s.Addr())
			_ = s.Close()
			continue
		}
		if !errors.Is(err, diag.ErrNotLoopback) {
			t.Errorf("Serve(%q) error = %v, want ErrNotLoopback", addr, err)
		}
	}
}

// TestServeBindsLoopbackAndAnswers proves the endpoint actually serves
// profiles, not just that it binds. It also pins Addr() reporting the bound
// port rather than the requested one: with one gateway process per client, a
// fixed port cannot be shared, so port 0 is the usable spelling and it is
// only usable if the process reports where it landed.
func TestServeBindsLoopbackAndAnswers(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "localhost:0"} {
		s, err := diag.Serve(addr)
		if err != nil {
			t.Fatalf("Serve(%q): %v", addr, err)
		}
		defer func() {
			if err := s.Close(); err != nil {
				t.Errorf("Close(): %v", err)
			}
		}()

		if strings.HasSuffix(s.Addr(), ":0") || s.Addr() == addr {
			t.Errorf("Addr() = %q, want the bound port, not the requested one", s.Addr())
		}

		// The heap profile is the one this package exists for, so it is the
		// one the test fetches.
		c := &http.Client{Timeout: 10 * time.Second}
		resp, err := c.Get("http://" + s.Addr() + "/debug/pprof/heap?debug=1")
		if err != nil {
			t.Fatalf("GET heap profile: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("reading heap profile: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("heap profile status = %d, want 200", resp.StatusCode)
		}
		// debug=1 renders the profile as text with this header line.
		if !strings.Contains(string(body), "heap profile:") {
			t.Errorf("heap profile body does not look like a profile: %.120q", body)
		}
	}
}

// TestServeFromEnv covers the three outcomes the two call sites assemble
// against: unset is not an error and starts nothing, a loopback address
// starts an endpoint, and a refused address is an error rather than a
// silently absent endpoint.
func TestServeFromEnv(t *testing.T) {
	t.Run("unset starts nothing", func(t *testing.T) {
		t.Setenv(diag.EnvAddr, "")
		s, err := diag.ServeFromEnv(nil)
		if err != nil {
			t.Fatalf("ServeFromEnv() = %v, want nil", err)
		}
		if s != nil {
			t.Errorf("ServeFromEnv() started %s, want no endpoint", s.Addr())
			_ = s.Close()
		}
	})

	t.Run("loopback serves", func(t *testing.T) {
		t.Setenv(diag.EnvAddr, "127.0.0.1:0")
		s, err := diag.ServeFromEnv(nil)
		if err != nil {
			t.Fatalf("ServeFromEnv(): %v", err)
		}
		defer func() { _ = s.Close() }()
		if s == nil || s.Addr() == "" {
			t.Fatal("ServeFromEnv() returned no listening endpoint")
		}
	})

	t.Run("non-loopback is an error", func(t *testing.T) {
		t.Setenv(diag.EnvAddr, "0.0.0.0:0")
		s, err := diag.ServeFromEnv(nil)
		if !errors.Is(err, diag.ErrNotLoopback) {
			t.Fatalf("ServeFromEnv() error = %v, want ErrNotLoopback", err)
		}
		if s != nil {
			t.Errorf("ServeFromEnv() started %s despite refusing", s.Addr())
			_ = s.Close()
		}
	})
}

// TestRequestGuardDefendsAgainstRebinding is the request-level proof to go
// with TestServeRefusesNonLoopback's address-level one. Binding to
// 127.0.0.1 stops the network, but a browser that has been DNS-rebound onto
// this loopback address is a same-origin client the bind address cannot
// distinguish from a legitimate one — see the guard's doc comment and
// internal/httpbridge/ingress.go's checkOrigin for the mechanism.
func TestRequestGuardDefendsAgainstRebinding(t *testing.T) {
	s, err := diag.Serve("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer func() { _ = s.Close() }()

	get := func(t *testing.T, host, origin string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/debug/pprof/", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if host != "" {
			req.Host = host
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("any Origin is refused, even one that equals Host", func(t *testing.T) {
		// This is the exact shape a rebound page produces: Origin and Host
		// read identical because, from the browser's point of view, the
		// request IS same-origin. There is no browser client here at all, so
		// unlike the bridge's checkOrigin this refuses it anyway.
		if got := get(t, s.Addr(), "http://"+s.Addr()); got != http.StatusForbidden {
			t.Errorf("status = %d, want %d for an Origin matching Host", got, http.StatusForbidden)
		}
	})

	t.Run("a Host that cannot be proven loopback is refused", func(t *testing.T) {
		if got := get(t, "evil.example:6060", ""); got != http.StatusForbidden {
			t.Errorf("status = %d, want %d for a non-loopback Host", got, http.StatusForbidden)
		}
	})

	t.Run("a curl-shaped request with port succeeds", func(t *testing.T) {
		if got := get(t, s.Addr(), ""); got != http.StatusOK {
			t.Errorf("status = %d, want %d for Host=%s, no Origin", got, http.StatusOK, s.Addr())
		}
	})

	t.Run("a curl-shaped request without a port succeeds", func(t *testing.T) {
		host, _, err := net.SplitHostPort(s.Addr())
		if err != nil {
			t.Fatalf("SplitHostPort(%q): %v", s.Addr(), err)
		}
		if got := get(t, host, ""); got != http.StatusOK {
			t.Errorf("status = %d, want %d for bare Host=%s, no Origin", got, http.StatusOK, host)
		}
	})
}

// TestCloseIsSafeWhenNothingStarted pins the nil-receiver contract, which is
// what lets a caller defer Close unconditionally next to a conditional
// Serve — the shape both the gateway and the daemon use.
func TestCloseIsSafeWhenNothingStarted(t *testing.T) {
	var s *diag.Server
	if err := s.Close(); err != nil {
		t.Errorf("Close() on a nil Server = %v, want nil", err)
	}
	if got := s.Addr(); got != "" {
		t.Errorf("Addr() on a nil Server = %q, want \"\"", got)
	}
}
