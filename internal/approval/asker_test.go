package approval

import (
	"context"
	"errors"
	"testing"
)

func TestRemoteAskerUnwiredUnreachable(t *testing.T) {
	var nilAsker *RemoteAsker
	if d := nilAsker.Ask(context.Background(), testRequest()); d != Unreachable {
		t.Fatalf("nil asker = %v, want Unreachable", d)
	}
	empty := &RemoteAsker{}
	if d := empty.Ask(context.Background(), testRequest()); d != Unreachable {
		t.Fatalf("nil Send = %v, want Unreachable", d)
	}
}

func TestRemoteAskerTransportErrorUnreachable(t *testing.T) {
	a := &RemoteAsker{Send: func(_ context.Context, _ Request) (Decision, error) {
		// Whatever decision the transport claims, an error voids it.
		return Approved, errors.New("daemon down")
	}}
	if d := a.Ask(context.Background(), testRequest()); d != Unreachable {
		t.Fatalf("transport error = %v, want Unreachable (fail-closed)", d)
	}
}

func TestRemoteAskerPassthrough(t *testing.T) {
	for _, want := range []Decision{Denied, Approved, Timedout, Unreachable, Stale} {
		var seen Request
		a := &RemoteAsker{Send: func(_ context.Context, req Request) (Decision, error) {
			seen = req
			return want, nil
		}}
		req := testRequest()
		if d := a.Ask(context.Background(), req); d != want {
			t.Fatalf("passthrough = %v, want %v", d, want)
		}
		if seen.Server != req.Server || seen.Tool != req.Tool || seen.ArgsHash != req.ArgsHash {
			t.Fatal("request was not forwarded intact")
		}
	}
}

func TestRemoteAskerInvalidDecisionUnreachable(t *testing.T) {
	a := &RemoteAsker{Send: func(_ context.Context, _ Request) (Decision, error) {
		return Decision(99), nil
	}}
	if d := a.Ask(context.Background(), testRequest()); d != Unreachable {
		t.Fatalf("invalid decision = %v, want Unreachable (never trusted)", d)
	}
}

func TestFillArgsHash(t *testing.T) {
	r := testRequest()
	r.ArgsHash = ""
	if err := r.FillArgsHash(); err != nil {
		t.Fatal(err)
	}
	if r.ArgsHash == "" {
		t.Fatal("ArgsHash not filled")
	}
	// Canonicalization: key order must not change the hash.
	other := Request{ArgsJSON: []byte("{\"repo\": \"a/b\"}")}
	if err := other.FillArgsHash(); err != nil {
		t.Fatal(err)
	}
	if other.ArgsHash != r.ArgsHash {
		t.Fatalf("hash differs across formatting: %q vs %q", other.ArgsHash, r.ArgsHash)
	}

	bad := Request{ArgsJSON: []byte("{broken")}
	bad.ArgsHash = "keep"
	if err := bad.FillArgsHash(); err == nil {
		t.Fatal("FillArgsHash accepted invalid JSON")
	}
	if bad.ArgsHash != "keep" {
		t.Fatal("ArgsHash mutated on error path")
	}
}

func TestDecisionString(t *testing.T) {
	cases := map[Decision]string{
		Denied:       "denied",
		Approved:     "approved",
		Timedout:     "timedout",
		Unreachable:  "unreachable",
		Stale:        "stale",
		Decision(42): "invalid",
	}
	for d, want := range cases {
		if got := d.String(); got != want {
			t.Fatalf("Decision(%d).String() = %q, want %q", int(d), got, want)
		}
	}
	if Decision(0) != Denied {
		t.Fatal("zero value must be Denied (never an approval)")
	}
}
