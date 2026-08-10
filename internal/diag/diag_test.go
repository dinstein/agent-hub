package diag_test

import (
	"errors"
	"io"
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
