package confops

import (
	"context"
	"testing"
)

// The MCP data plane's opt-in became a stored setting when the desktop
// application became what starts a hub: an application types no flags, so an
// opt-in that only existed as argv could not be given at all. What must NOT
// come with it is a lower bar — the value is still validated here, and the
// bind guards still apply in the daemon.

func TestSetHTTPFace(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	// Unset reads as "no listener", which is what an installation that has
	// never configured one must keep reporting.
	if face := st.Snapshot().Governance.V.ResolvedHTTP(); face.Addr != "" {
		t.Fatalf("a fresh registry already has an http address: %+v", face)
	}

	if _, err := SetGovernance(ctx, st, "http.addr", "localhost:7777", Precondition{}); err != nil {
		t.Fatalf("set http.addr: %v", err)
	}
	if _, err := SetGovernance(ctx, st, "http_allow_remote", "true", Precondition{}); err != nil {
		t.Fatalf("set through the snake_case alias: %v", err)
	}
	face := st.Snapshot().Governance.V.ResolvedHTTP()
	if face.Addr != "localhost:7777" || !face.AllowRemote || face.InsecureLoopback {
		t.Fatalf("stored face = %+v", face)
	}

	entry, err := GetGovernance(st.Snapshot().Governance.V, "http.addr")
	if err != nil || entry.Value != "localhost:7777" {
		t.Fatalf("get = %+v, %v", entry, err)
	}

	// Clearing it turns the listener off again, which has to be reachable
	// from the same surface that turned it on.
	if _, err := SetGovernance(ctx, st, "http.addr", "-", Precondition{}); err != nil {
		t.Fatalf("clear http.addr: %v", err)
	}
	if got := st.Snapshot().Governance.V.ResolvedHTTP().Addr; got != "" {
		t.Fatalf("http.addr after clearing = %q", got)
	}
}

func TestSetHTTPAddrValidation(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	// Failure direction: a typo is refused here, where the person who wrote
	// it is watching. Accepting it would defer the discovery to the next
	// start of the hub, which happens when nobody is looking at a terminal.
	for _, bad := range []string{"7777", "localhost", "localhost:0", "localhost:99999", "localhost:http"} {
		if _, err := SetGovernance(ctx, st, "http.addr", bad, Precondition{}); err == nil {
			t.Errorf("http.addr = %q was accepted", bad)
		}
	}
	if got := st.Snapshot().Governance.V.ResolvedHTTP().Addr; got != "" {
		t.Fatalf("a rejected address was applied: %q", got)
	}

	// An empty host means every interface. It is accepted here and decided by
	// the bind guard, which is the component that knows whether the
	// confirmation for a non-loopback address was given.
	if _, err := SetGovernance(ctx, st, "http.addr", ":7777", Precondition{}); err != nil {
		t.Fatalf(":7777 was refused here rather than at the bind: %v", err)
	}
}

func TestHTTPKeysAreListedWithTheRest(t *testing.T) {
	// get/set/ls read one table; a key that can be set but not listed is a
	// setting nobody can find.
	want := map[string]bool{"http.addr": false, "http.allowRemote": false, "http.insecureLoopback": false}
	for _, k := range GovernanceKeys() {
		if _, ok := want[k.Name]; ok {
			want[k.Name] = true
			if k.Doc == "" {
				t.Errorf("%s is listed with no explanation", k.Name)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("%s is settable but not listed", name)
		}
	}
}
