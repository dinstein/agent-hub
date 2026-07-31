package ctlapi

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/dinstein/agent-hub/internal/confops"
)

// recordingForgetter is a StateForgetter capturing what it was asked to
// clear, with optional fault injection.
type recordingForgetter struct {
	name   string
	forgot []string
	err    error
}

func (f *recordingForgetter) ForgetServer(_ context.Context, id string) error {
	if f.err != nil {
		return f.err
	}
	f.forgot = append(f.forgot, id)
	return nil
}

func (f *recordingForgetter) StateName() string { return f.name }

// TestServerDeleteClearsState pins that the DAEMON strips the same footprint
// `agenthub server rm` does. The two front ends disagreeing is the failure
// this guards: an operator who removes a server through the GUI would
// otherwise leave a re-added server's predecessor state behind for a
// re-added id to inherit, with nothing on screen saying so.
func TestServerDeleteClearsState(t *testing.T) {
	pins := &recordingForgetter{name: "tool pins"}
	grants := &recordingForgetter{name: "approval grants"}
	_, env := startServer(t, func(o *Options) {
		o.ServerStateForgetters = []confops.StateForgetter{pins, grants}
	})
	seedServer(t, env.reg, "gone", true)

	res := doAdmin(t, env.sock, http.MethodDelete, "/v1/servers/gone", nil)
	if res.status != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", res.status)
	}
	for _, f := range []*recordingForgetter{pins, grants} {
		if len(f.forgot) != 1 || f.forgot[0] != "gone" {
			t.Errorf("%s cleared %v, want [gone]", f.name, f.forgot)
		}
	}
}

// TestServerDeleteStateFailureStillDeletes pins the failure direction on the
// daemon side: a state store that cannot be cleaned must not fail the
// request. The registry entry is already committed by then, so a 500 here
// would report "not deleted" for a server that IS deleted.
func TestServerDeleteStateFailureStillDeletes(t *testing.T) {
	broken := &recordingForgetter{name: "tool pins", err: errors.New("state file is locked")}
	_, env := startServer(t, func(o *Options) {
		o.ServerStateForgetters = []confops.StateForgetter{broken}
	})
	seedServer(t, env.reg, "gone", true)

	res := doAdmin(t, env.sock, http.MethodDelete, "/v1/servers/gone", nil)
	if res.status != http.StatusOK {
		t.Fatalf("a state-store failure broke the delete: status %d", res.status)
	}
	if _, ok := env.reg.Snapshot().Servers.V.Servers["gone"]; ok {
		t.Error("the entry survived a delete that answered 200")
	}
}
