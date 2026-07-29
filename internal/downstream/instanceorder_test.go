package downstream_test

import (
	"context"
	"slices"
	"testing"

	"github.com/dinstein/agent-hub/internal/downstream"
)

// TestInstancesOrderIsDeterministic pins the (server, key) ordering of
// Pool.Instances.
//
// The projection has no non-test caller (see InstanceInfo), which is exactly
// why the ordering needs a test rather than a comment. Nothing renders it
// today, so nothing would notice it decaying; the guarantee is being kept for
// whatever eventually does, and a guarantee nobody checks is not one.
//
// The instances are built out of a map of maps in the pool, so without the
// sort the result would follow Go's randomized map iteration. Both keys are
// exercised: two servers, and two derive keys under one of them, so ServerID
// decides across servers and Key decides within one.
func TestInstancesOrderIsDeterministic(t *testing.T) {
	f := newPoolFixture(t, 4)

	// Acquired in an order that is not the sorted one, so a pass cannot come
	// from insertion order happening to agree.
	acquire := func(id string, key downstream.DeriveKey) {
		t.Helper()
		spec := downstream.Spec{ID: id, Command: "srv", Derive: downstream.DeriveSession}
		l, err := f.pool.Acquire(context.Background(), baseServer(t, id),
			spec.Derived(key, downstream.DeriveContext{}), key)
		if err != nil {
			t.Fatalf("acquire %s/%v: %v", id, key, err)
		}
		t.Cleanup(l.Release)
	}
	zebra := downstream.SessionDeriveKey("zebra")
	alpha := downstream.SessionDeriveKey("alpha")
	acquire("fs", zebra)
	acquire("git", alpha)
	acquire("fs", alpha)

	got := f.pool.Instances()
	if len(got) != 3 {
		t.Fatalf("instances = %+v, want 3", got)
	}

	type pair struct {
		server string
		key    downstream.DeriveKey
	}
	want := []pair{{"fs", alpha}, {"fs", zebra}, {"git", alpha}}
	if a, z := string(alpha), string(zebra); a >= z {
		t.Fatalf("fixture assumption broken: derive keys %q and %q are not in ascending order", a, z)
	}
	for i, w := range want {
		if got[i].ServerID != w.server || got[i].Key != w.key {
			t.Fatalf("instance %d = (%s, %v), want (%s, %v)\nfull: %+v",
				i, got[i].ServerID, got[i].Key, w.server, w.key, got)
		}
	}

	// Repeated snapshots agree: the order is a function of the contents, not
	// of whichever way the maps ranged this time.
	for i := range 10 {
		again := f.pool.Instances()
		if !slices.EqualFunc(again, got, func(a, b downstream.InstanceInfo) bool {
			return a.ServerID == b.ServerID && a.Key == b.Key
		}) {
			t.Fatalf("snapshot %d disagrees with the first:\n%+v\nvs\n%+v", i+2, again, got)
		}
	}
}
