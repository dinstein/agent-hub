package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"

	"github.com/dinstein/agent-hub/internal/guard"
)

// stubResolver replaces the package DNS hook for the duration of the test.
// Tests that stub MUST NOT be parallel with each other (package-level var).
func stubResolver(t *testing.T, answers map[string][]netip.Addr) {
	t.Helper()
	old := lookupNetIP
	lookupNetIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		if addrs, ok := answers[host]; ok {
			return addrs, nil
		}
		return nil, fmt.Errorf("stub: no such host %q", host)
	}
	t.Cleanup(func() { lookupNetIP = old })
}

func addrs(ips ...string) []netip.Addr {
	out := make([]netip.Addr, len(ips))
	for i, s := range ips {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

func TestAddrIsPrivate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ip   string
		want bool
	}{
		// RFC1918
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		// just outside RFC1918
		{"172.32.0.1", false},
		{"11.0.0.1", false},
		{"192.169.0.1", false},
		// loopback / unspecified
		{"127.0.0.1", true},
		{"127.8.9.10", true},
		{"::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"0.1.2.3", true}, // 0.0.0.0/8 "this network"
		// link-local
		{"169.254.1.1", true},
		{"fe80::1", true},
		// ULA
		{"fc00::1", true},
		{"fd12:3456::1", true},
		// Deprecated site-local: the ULA range above replaced it, and
		// netip.Addr's IsPrivate follows the replacement only. A host that
		// still holds a route to fec0::/10 is precisely the host where
		// reaching it is worth something.
		{"fec0::1", true},
		{"feff:ffff:ffff:ffff::1", true},
		{"fe00::1", false}, // just below the range: still public
		{"ff00::1", true},  // just above: multicast, already covered
		// CGNAT
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"100.128.0.1", false},
		// v4-mapped v6 unwraps before classification
		{"::ffff:10.0.0.1", true},
		{"::ffff:192.168.0.5", true},
		{"::ffff:8.8.8.8", false},
		// documentation / benchmarking / reserved
		{"192.0.2.1", true},
		{"198.51.100.7", true},
		{"203.0.113.9", true},
		{"198.18.0.1", true},
		{"198.19.255.255", true},
		{"240.0.0.1", true},
		{"255.255.255.255", true},
		{"2001:db8::1", true},
		{"100::1", true},
		{"64:ff9b::a00:1", true}, // NAT64 embedding 10.0.0.1
		// multicast
		{"224.0.0.1", true},
		{"ff02::1", true},
		// public
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false},
		{"2607:f8b0::1", false},
		{"2600::1", false},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			t.Parallel()
			if got := AddrIsPrivate(netip.MustParseAddr(tc.ip)); got != tc.want {
				t.Fatalf("AddrIsPrivate(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
	t.Run("invalid addr fail-closed", func(t *testing.T) {
		t.Parallel()
		if !AddrIsPrivate(netip.Addr{}) {
			t.Fatal("AddrIsPrivate(zero) = false, want true (fail-closed)")
		}
	})
}

func TestHostIsPrivateLiterals(t *testing.T) {
	t.Parallel()
	// Literal-only inputs never touch DNS, so this test can be parallel.
	cases := []struct {
		host string
		want bool
	}{
		{"", true}, // fail-closed
		{"10.0.0.1", true},
		{"10.0.0.1:8080", true},
		{"8.8.8.8", false},
		{"8.8.8.8:443", false},
		{"[::1]:8080", true},
		{"::1", true},
		{"[2607:f8b0::1]:443", false},
		{"localhost", true},
		{"LOCALHOST", true},
		{"localhost:3000", true},
		{"api.localhost", true},
		{"app.localhost.", true},
		{"fe80::1%en0", true},
		{"::ffff:169.254.1.1", true},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			t.Parallel()
			if got := HostIsPrivate(tc.host); got != tc.want {
				t.Fatalf("HostIsPrivate(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestHostIsPrivateResolution(t *testing.T) {
	// Not parallel: stubs the package resolver.
	stubResolver(t, map[string][]netip.Addr{
		"public.example":   addrs("93.184.216.34"),
		"internal.corp":    addrs("10.0.0.5"),
		"mixed.example":    addrs("93.184.216.34", "192.168.0.7"),
		"v6.example":       addrs("2607:f8b0::1"),
		"mapped.example":   addrs("::ffff:10.1.2.3"),
		"noanswer.example": {},
	})
	cases := []struct {
		host string
		want bool
	}{
		{"public.example", false},
		{"internal.corp", true},
		// one attacker-controlled private record must trip the refusal
		{"mixed.example", true},
		{"v6.example", false},
		{"mapped.example", true},
		{"noanswer.example", true},     // fail-closed: empty answer
		{"unresolvable.test", true},    // fail-closed: resolver error
		{"public.example:8443", false}, // port stripped before resolution
	}
	for _, tc := range cases {
		if got := HostIsPrivate(tc.host); got != tc.want {
			t.Errorf("HostIsPrivate(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestHostIsDefinitelyPrivate(t *testing.T) {
	// Not parallel: proves DNS is never consulted by making any lookup fail
	// the test.
	old := lookupNetIP
	lookupNetIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		t.Errorf("HostIsDefinitelyPrivate resolved %q — DNS must never confer trust", host)
		return nil, errors.New("unexpected lookup")
	}
	t.Cleanup(func() { lookupNetIP = old })

	cases := []struct {
		host string
		want bool
	}{
		{"", false}, // fail-to-false
		{"10.0.0.1", true},
		{"192.168.1.1:9000", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"[::1]:8080", true},
		{"fe80::1", true},
		{"fd00::1", true},
		{"0.0.0.0", true},
		{"localhost", true},
		{"db.localhost", true},
		// public literals
		{"8.8.8.8", false},
		{"2607:f8b0::1", false},
		// non-routable but not locally private: refused by HostIsPrivate,
		// yet they must not unlock local-only trust either
		{"192.0.2.1", false},
		{"100.64.0.1", false},
		{"198.18.0.1", false},
		// hostnames are never definitely private — even ones that would
		// resolve private (rebinding can change the answer at any time)
		{"internal.corp", false},
		{"example.com", false},
		{"not a hostname", false},
	}
	for _, tc := range cases {
		if got := HostIsDefinitelyPrivate(tc.host); got != tc.want {
			t.Errorf("HostIsDefinitelyPrivate(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestDialControl(t *testing.T) {
	t.Parallel()
	blockCases := []string{
		"10.0.0.5:443",
		"127.0.0.1:80",
		"[::1]:8080",
		"[fe80::1%en0]:22",
		"169.254.169.254:80", // cloud metadata endpoint
		"[::ffff:192.168.0.9]:443",
		"0.0.0.0:9",
		"not-an-ip:80",     // fail-closed: Control must only see IP literals
		"garbage",          // fail-closed: unparsable
		"evil.example:443", // fail-closed: unresolved hostname reached Control
	}
	for _, addr := range blockCases {
		t.Run("block "+addr, func(t *testing.T) {
			t.Parallel()
			err := DialControl("tcp", addr, nil)
			if err == nil {
				t.Fatalf("DialControl(%q) = nil, want block", addr)
			}
			if !errors.Is(err, guard.ErrBlocked) {
				t.Fatalf("error %v does not unwrap to guard.ErrBlocked", err)
			}
			var b *BlockedError
			if !errors.As(err, &b) || b.Addr != addr {
				t.Fatalf("unexpected typed error %#v", err)
			}
		})
	}
	allowCases := []string{
		"8.8.8.8:53",
		"93.184.216.34:443",
		"[2607:f8b0::1]:443",
	}
	for _, addr := range allowCases {
		t.Run("allow "+addr, func(t *testing.T) {
			t.Parallel()
			if err := DialControl("tcp", addr, nil); err != nil {
				t.Fatalf("DialControl(%q) = %v, want nil", addr, err)
			}
		})
	}
}

// TestDNSRebindScenario walks the TOCTOU sequence the two-layer design
// closes: the hostname passes the precheck while its DNS answer is public,
// then the attacker rebinds the name to a private address — and the dial-time
// hook still blocks, because it sees the resolved IP, not the name.
func TestDNSRebindScenario(t *testing.T) {
	// Not parallel: stubs the package resolver.
	stubResolver(t, map[string][]netip.Addr{
		"rebind.example": addrs("93.184.216.34"), // public at check time
	})
	if HostIsPrivate("rebind.example") {
		t.Fatal("precheck: rebind.example should look public at check time")
	}
	// ...attacker flips the record to 10.0.0.5; the dialer resolves the new
	// answer and hands the resolved address to Control:
	err := DialControl("tcp", "10.0.0.5:443", nil)
	if err == nil {
		t.Fatal("DialControl let the rebound private address through")
	}
	if !errors.Is(err, guard.ErrBlocked) {
		t.Fatalf("rebind block %v does not unwrap to guard.ErrBlocked", err)
	}
	// And the trust-direction predicate never believed the name either way.
	if HostIsDefinitelyPrivate("rebind.example") {
		t.Fatal("HostIsDefinitelyPrivate trusted a hostname")
	}
}

// TestV4EmbeddingV6FormsAreNotPublic covers the three ways an IPv4 address can
// be written as an IPv6 one. Classifying the v6 form on its own merits answers
// the wrong question: ::127.0.0.1, 2002:7f00:1:: and 64:ff9b::7f00:1 all name
// 127.0.0.1, and none of them looks like loopback to netip.
//
// Only the NAT64 prefix was covered. The other two reached DialControl — the
// re-screen that exists precisely to be the last word on where a socket may
// connect — and were allowed there too, so neither the hostname precheck nor
// the dial-time check would have stopped them.
func TestV4EmbeddingV6FormsAreNotPublic(t *testing.T) {
	private := []string{
		"::ffff:127.0.0.1", // v4-mapped (already handled by Unmap)
		"::ffff:10.0.0.1",
		"64:ff9b::7f00:1", // NAT64 well-known
		"64:ff9b::a00:1",
		"::127.0.0.1", // IPv4-compatible, deprecated
		"::10.0.0.1",
		"::169.254.169.254", // the cloud metadata address, v4-compatible
		"2002:7f00:1::",     // 6to4 of 127.0.0.1
		"2002:a00:1::",      // 6to4 of 10.0.0.1
		"2002:a9fe:a9fe::",  // 6to4 of 169.254.169.254
	}
	for _, s := range private {
		t.Run(s, func(t *testing.T) {
			if !AddrIsPrivate(netip.MustParseAddr(s)) {
				t.Errorf("%s classified as publicly routable", s)
			}
			if err := DialControl("tcp", net.JoinHostPort(s, "80"), nil); err == nil {
				t.Errorf("DialControl allowed a connection to %s", s)
			}
		})
	}

	// The fix must not swallow ordinary public IPv6. These are real resolver
	// addresses; blocking them would take out every remote server on v6.
	for _, s := range []string{
		"2606:4700:4700::1111", "2001:4860:4860::8888", "2620:fe::fe",
		"8.8.8.8", "1.1.1.1",
	} {
		t.Run("public/"+s, func(t *testing.T) {
			if AddrIsPrivate(netip.MustParseAddr(s)) {
				t.Errorf("%s classified as private", s)
			}
		})
	}
}

// The trust-granting direction must stay unchanged: a v4-embedding form is
// refused above, but it must never CONFER local trust either — only a literal
// private address or a localhost name does that.
func TestV4EmbeddingFormsDoNotGrantLocalTrust(t *testing.T) {
	for _, s := range []string{"::127.0.0.1", "2002:7f00:1::", "64:ff9b::7f00:1"} {
		if HostIsDefinitelyPrivate(s) {
			t.Errorf("%s granted local trust; only literal private addresses may", s)
		}
	}
}
