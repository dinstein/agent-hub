package httpbridge_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/tier"
)

// TestInsecureLoopbackRequiresALoopbackPeer is the second half of the fix for
// a bind-time narrowing that a configured token skipped past.
//
// AuthorizeBind now refuses the flag on a non-loopback address, so in a
// correctly assembled daemon this Authenticator should never see
// InsecureLoopback on a public listener at all. The check is made here anyway
// because InsecureLoopback arrives as a bare bool carrying no evidence of the
// address the listener actually got: if the two ever disagree again, the one
// that answers the request has to be the one that can see the peer.
func TestInsecureLoopbackRequiresALoopbackPeer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		remoteAddr string
		wantOK     bool
	}{
		{name: "IPv4 loopback", remoteAddr: "127.0.0.1:51234", wantOK: true},
		{name: "IPv4 loopback range", remoteAddr: "127.0.0.53:51234", wantOK: true},
		{name: "IPv6 loopback", remoteAddr: "[::1]:51234", wantOK: true},
		{name: "LAN peer", remoteAddr: "192.168.1.20:51234", wantOK: false},
		{name: "public peer", remoteAddr: "203.0.113.7:51234", wantOK: false},
		{name: "IPv4-mapped LAN peer", remoteAddr: "[::ffff:192.168.1.20]:51234", wantOK: false},
		{name: "no port", remoteAddr: "192.168.1.20", wantOK: false},
		{name: "unparsable", remoteAddr: "not-an-address", wantOK: false},
		{name: "empty", remoteAddr: "", wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &httpbridge.Authenticator{InsecureLoopback: true}
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.RemoteAddr = tc.remoteAddr

			caller, err := a.Authenticate(r)
			if !tc.wantOK {
				if !errors.Is(err, httpbridge.ErrUnauthorized) {
					t.Fatalf("peer %q: err = %v, want ErrUnauthorized — an unauthenticated "+
						"non-loopback caller was answered", tc.remoteAddr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("peer %q: err = %v, want the escape hatch to apply", tc.remoteAddr, err)
			}
			if caller.Kind != httpbridge.CallerLoopback || caller.Tier != tier.Destructive {
				t.Fatalf("peer %q: caller = %+v, want the loopback caller at the destructive tier",
					tc.remoteAddr, caller)
			}
		})
	}
}

// Without the hatch the peer address decides nothing: a loopback peer with no
// credential is still unauthenticated. This pins that the check added above is
// an extra condition on the hatch rather than a second way to pass it.
func TestALoopbackPeerIsNotItselfACredential(t *testing.T) {
	t.Parallel()
	a := &httpbridge.Authenticator{}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "127.0.0.1:51234"
	if _, err := a.Authenticate(r); !errors.Is(err, httpbridge.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}
