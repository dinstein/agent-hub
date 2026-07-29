package ctlapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/scope"
	"github.com/dinstein/agent-hub/internal/session"
)

const gwTestTimeout = 10 * time.Second

// fakeGateway drives the gateway-face wire protocol (register / link / ack)
// exactly like a real stdio gateway process would, over the same UDS.
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

func (fg *fakeGateway) nextFrame() sseFrame {
	fg.t.Helper()
	select {
	case f, ok := <-fg.frames:
		if !ok {
			fg.t.Fatal("link stream closed while waiting for a frame")
		}
		return f
	case <-time.After(gwTestTimeout):
		fg.t.Fatal("timed out waiting for a link frame")
	}
	return sseFrame{}
}

func (fg *fakeGateway) ack(ack GatewayAck) {
	fg.t.Helper()
	body, _ := json.Marshal(ack)
	resp, err := fg.hc.Post("http://d/v1/gateway/"+fg.sid+"/ack", "application/json", bytes.NewReader(body))
	if err != nil {
		fg.t.Fatalf("ack: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fg.t.Fatalf("ack status = %d, want 200", resp.StatusCode)
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

func TestGatewayRegisterOverlayPushAck(t *testing.T) {
	_, env := startServer(t, nil)

	fg := registerFakeGateway(t, env.sock, "cursor")
	defer fg.closeLink()
	if fg.sid != "cursor:1" {
		t.Fatalf("session id = %q, want cursor:1", fg.sid)
	}
	sess, ok := env.mgr.Get(session.SessionID(fg.sid))
	if !ok {
		t.Fatal("session not registered with the manager")
	}
	if sess.Origin != session.OriginStdioGateway {
		t.Fatalf("origin = %v, want stdio", sess.Origin)
	}
	fg.openLink()

	// Push-then-commit round trip: Mutate must block until the gateway
	// acks, and must commit exactly what was pushed.
	mutDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), gwTestTimeout)
		defer cancel()
		mutDone <- env.mgr.Mutate(ctx, session.SessionID(fg.sid), func(ov *scope.Overlay) {
			ov.Servers = []string{"github"}
		})
	}()

	f := fg.nextFrame()
	if f.event != LinkEventOverlay {
		t.Fatalf("frame event = %q, want overlay", f.event)
	}
	var frame OverlayFrame
	if err := json.Unmarshal(f.data, &frame); err != nil {
		t.Fatalf("overlay frame decode: %v", err)
	}
	var pushed scope.Overlay
	if err := json.Unmarshal(frame.Overlay, &pushed); err != nil {
		t.Fatalf("overlay decode: %v", err)
	}
	if len(pushed.Servers) != 1 || pushed.Servers[0] != "github" {
		t.Fatalf("pushed overlay servers = %v, want [github]", pushed.Servers)
	}

	select {
	case err := <-mutDone:
		t.Fatalf("Mutate returned before ack: %v", err)
	case <-time.After(50 * time.Millisecond):
		// Still blocked on the ack: correct.
	}

	fg.ack(GatewayAck{ID: frame.ID, OK: true})
	select {
	case err := <-mutDone:
		if err != nil {
			t.Fatalf("Mutate: %v", err)
		}
	case <-time.After(gwTestTimeout):
		t.Fatal("Mutate did not return after ack")
	}
	ov := env.mgr.Overlay(session.SessionID(fg.sid))
	if ov == nil || ov.Version != 1 {
		t.Fatalf("committed overlay = %+v, want version 1", ov)
	}
}

func TestGatewayNackCommitsNothing(t *testing.T) {
	_, env := startServer(t, nil)
	fg := registerFakeGateway(t, env.sock, "cursor")
	defer fg.closeLink()
	fg.openLink()

	mutDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), gwTestTimeout)
		defer cancel()
		mutDone <- env.mgr.Mutate(ctx, session.SessionID(fg.sid), func(ov *scope.Overlay) {
			ov.Servers = []string{}
		})
	}()
	f := fg.nextFrame()
	var frame OverlayFrame
	if err := json.Unmarshal(f.data, &frame); err != nil {
		t.Fatalf("overlay frame decode: %v", err)
	}
	fg.ack(GatewayAck{ID: frame.ID, OK: false, Error: "cannot apply"})

	select {
	case err := <-mutDone:
		if err == nil {
			t.Fatal("Mutate succeeded despite gateway nack")
		}
		if !strings.Contains(err.Error(), "cannot apply") {
			t.Fatalf("Mutate error %q does not carry the gateway reason", err)
		}
	case <-time.After(gwTestTimeout):
		t.Fatal("Mutate did not return after nack")
	}
	if ov := env.mgr.Overlay(session.SessionID(fg.sid)); ov != nil {
		t.Fatalf("overlay committed despite nack: %+v", ov)
	}
}

func TestGatewayAckTimeoutFailsMutate(t *testing.T) {
	_, env := startServer(t, func(o *Options) {
		o.LinkAckTimeout = 50 * time.Millisecond
	})
	fg := registerFakeGateway(t, env.sock, "cursor")
	defer fg.closeLink()
	fg.openLink()

	ctx, cancel := context.WithTimeout(context.Background(), gwTestTimeout)
	defer cancel()
	err := env.mgr.Mutate(ctx, session.SessionID(fg.sid), func(ov *scope.Overlay) {
		ov.Servers = []string{}
	})
	if err == nil {
		t.Fatal("Mutate succeeded without any ack")
	}
	if ov := env.mgr.Overlay(session.SessionID(fg.sid)); ov != nil {
		t.Fatalf("overlay committed despite missing ack: %+v", ov)
	}
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
		{http.MethodPost, "/v1/gateway/nope:1/ack"},
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

func TestGatewayRegisterIsAudited(t *testing.T) {
	_, env := startServer(t, nil)
	fg := registerFakeGateway(t, env.sock, "cursor")
	defer fg.closeLink()

	waitFor(t, "audit record", func() bool { return len(env.aud.records()) > 0 })
	rec := env.aud.records()[0]
	if rec.Tool != "gateway/register" || rec.Client != "cursor" || rec.Session != fg.sid {
		t.Fatalf("audit record = %+v", rec)
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
