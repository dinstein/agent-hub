package ctlapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/session"
)

const gwTestTimeout = 10 * time.Second

// fakeGateway drives the gateway-face wire protocol (register / link /
// servers) exactly like a real stdio gateway process would, over the same UDS.
type fakeGateway struct {
	t    *testing.T
	hc   *http.Client
	sid  string
	body io.ReadCloser

	frames chan sseFrame
}

type sseFrame struct {
	event string
	data  []byte
}

func registerFakeGateway(t *testing.T, sock, clientID string) *fakeGateway {
	t.Helper()
	fg := &fakeGateway{t: t, hc: rawClient(sock), frames: make(chan sseFrame, 16)}

	body, _ := json.Marshal(GatewayHelloWire{ClientID: clientID, Pid: 123})
	resp, err := fg.hc.Post("http://d/v1/gateway/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var env struct {
		OK   bool              `json:"ok"`
		Data GatewayRegistered `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil || !env.OK {
		t.Fatalf("register decode: ok=%v err=%v status=%d", env.OK, err, resp.StatusCode)
	}
	if env.Data.SessionID == "" {
		t.Fatal("register returned empty session id")
	}
	fg.sid = env.Data.SessionID
	return fg
}

// openLink attaches the SSE link and pumps frames into fg.frames.
func (fg *fakeGateway) openLink() {
	fg.t.Helper()
	resp, err := fg.hc.Get("http://d/v1/gateway/" + fg.sid + "/link")
	if err != nil {
		fg.t.Fatalf("link: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		fg.t.Fatalf("link status = %d, want 200", resp.StatusCode)
	}
	fg.body = resp.Body
	go func() {
		sc := bufio.NewScanner(resp.Body)
		eventName, data := "", []byte(nil)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if len(data) > 0 {
					fg.frames <- sseFrame{event: eventName, data: data}
				}
				eventName, data = "", nil
			case strings.HasPrefix(line, "event: "):
				eventName = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: ")...)
			}
		}
		close(fg.frames)
	}()
}

func (fg *fakeGateway) closeLink() {
	if fg.body != nil {
		_ = fg.body.Close()
	}
}

// waitFor polls cond until true or the timeout expires.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(gwTestTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestGatewayLinkDropClosesSession(t *testing.T) {
	_, env := startServer(t, nil)
	fg := registerFakeGateway(t, env.sock, "cursor")
	fg.openLink()

	fg.closeLink()
	waitFor(t, "session close after link drop", func() bool {
		_, ok := env.mgr.Get(session.SessionID(fg.sid))
		return !ok
	})

	// Re-registration mints a NEW identity: references to the dead session
	// must break, never silently rebind (docs/architecture.md §7).
	fg2 := registerFakeGateway(t, env.sock, "cursor")
	defer fg2.closeLink()
	if fg2.sid != "cursor:2" {
		t.Fatalf("re-registered session id = %q, want cursor:2", fg2.sid)
	}
}

func TestGatewayAttachWatchdogClosesSession(t *testing.T) {
	_, env := startServer(t, func(o *Options) {
		o.LinkAttachTimeout = 50 * time.Millisecond
	})
	fg := registerFakeGateway(t, env.sock, "cursor")
	// Never open the link: the watchdog must reap the session.
	waitFor(t, "attach watchdog close", func() bool {
		_, ok := env.mgr.Get(session.SessionID(fg.sid))
		return !ok
	})
}

func TestGatewaySecondLinkAttachConflicts(t *testing.T) {
	_, env := startServer(t, nil)
	fg := registerFakeGateway(t, env.sock, "cursor")
	defer fg.closeLink()
	fg.openLink()

	resp, err := fg.hc.Get("http://d/v1/gateway/" + fg.sid + "/link")
	if err != nil {
		t.Fatalf("second link: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second link status = %d, want 409", resp.StatusCode)
	}
}

func TestGatewayRegistryEventsForwarded(t *testing.T) {
	_, env := startServer(t, nil)
	fg := registerFakeGateway(t, env.sock, "cursor")
	defer fg.closeLink()
	fg.openLink()

	// The daemon publishes registry changes on TopicRegistry; the link
	// forwards them as `registry` frames. Subscription happens inside the
	// link handler goroutine, so publish until the frame arrives.
	got := make(chan RegistryFrame, 1)
	go func() {
		for f := range fg.frames {
			if f.event != LinkEventRegistry {
				continue
			}
			var rf RegistryFrame
			if json.Unmarshal(f.data, &rf) == nil {
				got <- rf
				return
			}
		}
	}()
	deadline := time.Now().Add(gwTestTimeout)
	for {
		env.bus.Publish(event.Event{
			Topic:   TopicRegistry,
			Key:     string(registry.DocServers),
			Payload: registry.Change{Kind: registry.DocServers, Rev: 7},
		})
		select {
		case rf := <-got:
			if rf.Kind != string(registry.DocServers) || rf.Rev != 7 {
				t.Fatalf("registry frame = %+v, want servers/7", rf)
			}
			return
		case <-time.After(20 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("registry frame never arrived on the link")
			}
		}
	}
}

func TestGatewayRegisterRejectsEmptyClientID(t *testing.T) {
	_, env := startServer(t, nil)
	hc := rawClient(env.sock)
	body, _ := json.Marshal(GatewayHelloWire{})
	resp, err := hc.Post("http://d/v1/gateway/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGatewayUnknownSessionIsUniform404(t *testing.T) {
	_, env := startServer(t, nil)
	hc := rawClient(env.sock)
	for _, req := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/gateway/nope:1/link"},
		// A served action with an unknown session, which is what this test
		// is about: `/ack` stood here until the action was dropped, and an
		// unrouted path would have passed on the uniform 404 alone.
		{http.MethodPost, "/v1/gateway/nope:1/servers"},
	} {
		var body io.Reader
		if req.method == http.MethodPost {
			body = strings.NewReader(`{"id":1,"ok":true}`)
		}
		r, err := http.NewRequest(req.method, "http://d"+req.path, body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := hc.Do(r)
		if err != nil {
			t.Fatalf("%s %s: %v", req.method, req.path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s status = %d, want 404", req.method, req.path, resp.StatusCode)
		}
		if !bytes.Contains(raw, []byte(notFoundMessage)) {
			t.Fatalf("%s %s body %q is not the uniform 404", req.method, req.path, raw)
		}
	}
}

// TestGatewayHelloWireShape pins the frozen wire field names.
func TestGatewayHelloWireShape(t *testing.T) {
	b, err := json.Marshal(GatewayHelloWire{ClientID: "c", Pid: 1, Root: "/r", ScopeHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"client_id":"c","pid":1,"root":"/r","scope_hash":"h"}`
	if string(b) != want {
		t.Fatalf("wire shape = %s, want %s", b, want)
	}
}
