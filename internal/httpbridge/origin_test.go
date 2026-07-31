package httpbridge

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCheckOriginRefusesDNSRebinding pins the shape the previous check
// claimed to stop and did not.
//
// The old rule was "Origin equals Host". Under DNS rebinding the browser
// sends the attacker's hostname in BOTH — the page really was served from
// evil.example:7777, and that name now resolves to 127.0.0.1 — so equality
// held, Sec-Fetch-Site read "same-origin", and no preflight was sent, because
// from the browser's point of view it genuinely was same-origin. Equality is
// the one relation rebinding preserves, which is why the authority has to be
// proved loopback instead.
func TestCheckOriginRefusesDNSRebinding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		origin string
		host   string
		want   bool // true = allowed
	}{
		{name: "no origin, non-browser client", origin: "", host: "127.0.0.1:7777", want: true},
		{name: "local UI on IPv4 loopback", origin: "http://127.0.0.1:7777", host: "127.0.0.1:7777", want: true},
		{name: "local UI on localhost", origin: "http://localhost:7777", host: "localhost:7777", want: true},
		{name: "local UI on IPv6 loopback", origin: "http://[::1]:7777", host: "[::1]:7777", want: true},

		{name: "rebound attacker page", origin: "http://evil.example:7777", host: "evil.example:7777"},
		{name: "rebound attacker page over https", origin: "https://evil.example", host: "evil.example"},
		{name: "loopback-shaped hostname", origin: "http://127.0.0.1.nip.io:7777", host: "127.0.0.1.nip.io:7777"},
		{name: "loopback-prefixed hostname", origin: "http://localhost.evil.example:7777", host: "localhost.evil.example:7777"},
		{name: "ordinary cross-origin page", origin: "https://app.example.com", host: "127.0.0.1:7777"},
		{name: "LAN authority", origin: "http://192.168.1.9:7777", host: "192.168.1.9:7777"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			err := checkOrigin(r)
			if tc.want {
				if err != nil {
					t.Fatalf("Origin %q Host %q: err = %v, want allowed", tc.origin, tc.host, err)
				}
				return
			}
			if !errors.Is(err, errCrossSite) {
				t.Fatalf("Origin %q Host %q: err = %v, want errCrossSite", tc.origin, tc.host, err)
			}
		})
	}
}

// A mismatched pair was already refused and must stay refused: proving the
// authority is loopback is an ADDITIONAL condition, not a replacement for the
// equality it used to be.
func TestCheckOriginStillRefusesAMismatchedPair(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Host = "127.0.0.1:7777"
	r.Header.Set("Origin", "http://localhost:7777")
	if err := checkOrigin(r); !errors.Is(err, errCrossSite) {
		t.Fatalf("err = %v, want errCrossSite: two different loopback spellings are still two origins", err)
	}
}
