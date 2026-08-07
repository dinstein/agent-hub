package ctlapi

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
)

// TestDaemonStopIsAskedForAndAccepted covers the route that exists because
// one platform cannot be signalled.
//
// Windows has no SIGTERM, and a console control event reaches only a process
// group sharing the caller's console — never a daemon started from another
// terminal or by a windowless application. `daemon stop` is already holding
// this socket when it needs the daemon to stop, so it asks over it.
//
// 202 rather than 200 is the contract: the daemon is still draining when the
// answer is written, and this socket is one of the things it closes.
func TestDaemonStopIsAskedForAndAccepted(t *testing.T) {
	var mu sync.Mutex
	var reasons []string
	_, env := startServer(t, func(o *Options) {
		o.RequestStop = func(reason string) {
			mu.Lock()
			defer mu.Unlock()
			reasons = append(reasons, reason)
		}
	})

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/daemon/stop", nil)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (the daemon has not finished when this returns): %s",
			status, http.StatusAccepted, body)
	}
	var wire struct {
		Data DaemonStopAccepted `json:"data"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	got := wire.Data
	if !got.Stopping || got.Pid <= 0 {
		t.Errorf("body = %+v, want stopping with the daemon's own pid", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reasons) != 1 {
		t.Fatalf("RequestStop called %d times, want once", len(reasons))
	}
	if reasons[0] == "" {
		t.Error("the stop carried no reason; the shutdown log reports it")
	}
}

// TestDaemonStopIsNotServedWithoutTheHook pins the package's uniform rule:
// an absent collaborator takes its routes into the same 404 every other miss
// lands in, rather than half-serving them. A daemon assembled without a stop
// handle must not answer as though it will stop.
func TestDaemonStopIsNotServedWithoutTheHook(t *testing.T) {
	_, env := startServer(t, func(o *Options) { o.RequestStop = nil })
	if status, body := nrDo(t, env.sock, http.MethodPost, "/v1/daemon/stop", nil); status != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when no stop hook is wired: %s", status, body)
	}
}

// TestDaemonStopRefusesTheWrongMethod keeps the route from becoming a GET a
// browser preflight or a link could trigger.
func TestDaemonStopRefusesTheWrongMethod(t *testing.T) {
	_, env := startServer(t, func(o *Options) {
		o.RequestStop = func(string) { t.Error("a GET stopped the daemon") }
	})
	if status, _ := nrDo(t, env.sock, http.MethodGet, "/v1/daemon/stop", nil); status != http.StatusNotFound {
		t.Errorf("GET status = %d, want 404", status)
	}
}
