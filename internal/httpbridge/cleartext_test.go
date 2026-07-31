package httpbridge

import (
	"strings"
	"testing"
)

// TestNonLoopbackBindIsToldItIsUnencrypted is the record of a decision, not
// of a bug fix.
//
// The 2026-07-31 sweep found that a network-reachable listener serves plain
// HTTP: AuthorizeBind refuses to bind one without a credential, and that
// credential then crosses the network in the clear along with every tool
// call and result. Terminating TLS is out of scope — it needs certificate
// material, rotation and trust configuration, which is a feature with its
// own argument. Saying nothing is not: "delivered or refused" applies to
// the channel too, and the honest form is to bind and state plainly what is
// unprotected.
//
// If TLS is ever implemented, this test is the place the decision was
// written down, and it should change with it.
func TestNonLoopbackBindIsToldItIsUnencrypted(t *testing.T) {
	remote := []string{"0.0.0.0:7777", "192.168.1.10:7777", ":7777", "[::]:7777"}
	for _, addr := range remote {
		t.Run(addr, func(t *testing.T) {
			dec, err := AuthorizeBind(BindConfig{Addr: addr, HasAdminToken: true})
			if err != nil {
				t.Fatalf("bind with an admin token was refused: %v", err)
			}
			if dec.Cleartext == "" {
				t.Fatal("a network-reachable bind was authorized with no word about the channel being unencrypted")
			}
			if !strings.Contains(dec.Cleartext, "unencrypted") {
				t.Errorf("the warning does not say what is wrong: %q", dec.Cleartext)
			}
		})
	}
}

// TestLoopbackBindIsNotWarned keeps the warning meaningful. A loopback
// listener never puts anything on a network, and a warning printed on every
// ordinary start is a warning nobody reads.
func TestLoopbackBindIsNotWarned(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7777", "localhost:7777", "[::1]:7777"} {
		t.Run(addr, func(t *testing.T) {
			dec, err := AuthorizeBind(BindConfig{Addr: addr, HasAdminToken: true})
			if err != nil {
				t.Fatalf("loopback bind was refused: %v", err)
			}
			if dec.Cleartext != "" {
				t.Errorf("a loopback bind was warned about the network: %q", dec.Cleartext)
			}
		})
	}
}

// TestARefusedBindCarriesNoDecision pins that the warning cannot be read off
// a bind that never happened — a refusal returns the zero decision.
func TestARefusedBindCarriesNoDecision(t *testing.T) {
	dec, err := AuthorizeBind(BindConfig{Addr: "0.0.0.0:7777"})
	if err == nil {
		t.Fatal("an unauthenticated non-loopback bind was allowed")
	}
	if dec.Cleartext != "" || dec.Reason != "" {
		t.Errorf("a refused bind returned a populated decision: %+v", dec)
	}
}
