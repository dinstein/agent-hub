package ctlapi

import (
	"strings"
	"testing"
)

// TestServerRuntimeAttributionIsNotDecidedByMapOrder pins the (client, sess)
// ordering of the reporter fold.
//
// The fold is not order-independent, which is what makes the sort load
// bearing rather than cosmetic. Two of its steps take first-wins semantics:
//
//   - winnerOf returns the FIRST reporter at the worst severity, and that
//     client's id is what attributeDetail stamps into ConnDetail. When two
//     clients report the same severity, the sort alone decides which one an
//     operator sees blamed.
//   - OAuthConfigError keeps the first non-empty value, so with two clients
//     reporting different configuration errors the sort picks which is shown.
//
// The reports are held in a map keyed by session id, so absent the sort both
// answers would follow Go's randomized iteration and flip between calls.
//
// The existing fold test does not reach this: it uses reporters at DIFFERENT
// severities, where the max is unique and order cannot matter. Mutation
// confirms the gap — dropping either key individually left the whole ctlapi
// package green, and only removing the sort entirely failed anything.
func TestServerRuntimeAttributionIsNotDecidedByMapOrder(t *testing.T) {
	// Two clients, same server, SAME severity, each with its own detail and
	// its own OAuth error. Sessions are named so that sorting by session
	// would disagree with sorting by client, which is what makes the primary
	// key observable.
	newStates := func() *GatewayStates {
		g := NewGatewayStates()
		g.ReportServers("s9", "alpha-client", []GatewayServerState{{
			ID: "elk", Conn: string(ConnConnecting), Detail: "alpha detail",
			OAuthConfigError: "alpha issuer",
		}})
		g.ReportServers("s1", "zeta-client", []GatewayServerState{{
			ID: "elk", Conn: string(ConnConnecting), Detail: "zeta detail",
			OAuthConfigError: "zeta issuer",
		}})
		return g
	}

	rt, ok := newStates().ServerRuntime("elk")
	if !ok {
		t.Fatal("no runtime for a reported server")
	}
	if !strings.Contains(rt.ConnDetail, "alpha-client") {
		t.Errorf("detail %q does not attribute the tie to alpha-client; "+
			"the lowest client id must win, not whichever session ranged first", rt.ConnDetail)
	}
	if rt.OAuthConfigError != "alpha issuer" {
		t.Errorf("oauth error = %q, want alpha issuer (first by client id)", rt.OAuthConfigError)
	}

	// Rebuilding the aggregator re-inserts into a fresh map, so this is the
	// assertion that the answer is a function of the reports and not of one
	// map's iteration order.
	for i := range 20 {
		again, ok := newStates().ServerRuntime("elk")
		if !ok {
			t.Fatalf("run %d: no runtime", i+2)
		}
		if again.ConnDetail != rt.ConnDetail || again.OAuthConfigError != rt.OAuthConfigError {
			t.Fatalf("run %d attributed differently: detail %q / oauth %q, first run %q / %q",
				i+2, again.ConnDetail, again.OAuthConfigError, rt.ConnDetail, rt.OAuthConfigError)
		}
	}

	// The session id is the tie-break when ONE client reports twice: with the
	// client equal on both sides, the session is the only order left.
	//
	// This case has to be sampled rather than asked once. Two reporters that
	// compare equal are left in the order the map ranged them, SortFunc is not
	// stable, and a two-entry map often happens to range the same way — so a
	// single call passes with a dropped tie-break about half the time. Twenty
	// fresh aggregators make that a coin flipped twenty times.
	newSolo := func() *GatewayStates {
		g := NewGatewayStates()
		g.ReportServers("s-late", "solo", []GatewayServerState{{
			ID: "elk", Conn: string(ConnConnecting), OAuthConfigError: "late issuer",
		}})
		g.ReportServers("s-early", "solo", []GatewayServerState{{
			ID: "elk", Conn: string(ConnConnecting), OAuthConfigError: "early issuer",
		}})
		return g
	}
	for i := range 20 {
		solo, ok := newSolo().ServerRuntime("elk")
		if !ok {
			t.Fatalf("run %d: no runtime for the single-client case", i+1)
		}
		if solo.OAuthConfigError != "early issuer" {
			t.Fatalf("run %d: oauth error = %q, want early issuer: with one client the "+
				"session id is the only remaining order", i+1, solo.OAuthConfigError)
		}
	}
}
