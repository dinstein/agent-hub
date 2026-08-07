package daemon_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/daemon"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
	"github.com/dinstein/agent-hub/internal/tier"
)

// These are the acceptance tests for the HTTP face's server→client direction,
// and they are the only ones that run the WHOLE chain: a real downstream
// announcing a tool-set change, a real gateway refreshing its catalog, the
// real per-credential Conn fanning the notification out, and a real HTTP
// client reading it off an SSE body.
//
// internal/httpbridge's own stream tests drive a fake dispatcher, so they
// prove the HTTP shape and nothing about where a notification comes from;
// internal/gateway's prove the delivery discipline and nothing about how it
// leaves the process. The bug that survives both is a chain that is never
// connected, which is exactly the state this face was in before.

// listChangedOnCall is a downstream that announces a tool-set change whenever
// a tool is called, then answers the call normally. Calling a tool is the
// controllable moment in a test: it happens after the stream is open, unlike
// anything that fires during connect.
func listChangedOnCall() downstream.DialFunc {
	return scriptedDial(map[string]*fakemcp.Script{
		"fake": fakemcp.Minimal("echo").With(
			fakemcp.ListChangedStorm(mcp.MethodToolsCall, 1, time.Millisecond)),
	})
}

// openSSE issues one streaming request and returns a reader over its body.
func openSSE(t *testing.T, d *httpDaemon, method, bearer, session string, body []byte) (*http.Response, *bufio.Reader) {
	t.Helper()
	url := "http://" + d.addr + httpbridge.DefaultPath
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	var req *http.Request
	var err error
	if reader != nil {
		req, err = http.NewRequest(method, url, reader)
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "text/event-stream")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(httpbridge.MethodHeader, mcp.MethodSubscriptionsListen)
	}
	if session != "" {
		req.Header.Set(httpbridge.SessionHeader, session)
	}
	// No client timeout: the whole point of this response is that it stays
	// open. The deadline that matters is the per-read one in awaitNotification.
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%s: HTTP %d", method, res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	return res, bufio.NewReader(res.Body)
}

// awaitNotification reads events until one carries method, or the deadline
// passes. Keep-alive comments and other methods are skipped.
func awaitNotification(t *testing.T, br *bufio.Reader, method string) *mcp.Notification {
	t.Helper()
	type result struct {
		n   *mcp.Notification
		err error
	}
	found := make(chan result, 1)
	go func() {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				found <- result{err: err}
				return
			}
			data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data: ")
			if !ok {
				continue // keep-alive comment, event name, or blank separator
			}
			var n mcp.Notification
			if err := json.Unmarshal([]byte(data), &n); err != nil {
				found <- result{err: fmt.Errorf("event data is not a JSON-RPC message: %w (%s)", err, data)}
				return
			}
			if n.Method == method {
				found <- result{n: &n}
				return
			}
		}
	}()
	select {
	case r := <-found:
		if r.err != nil {
			t.Fatalf("stream ended before %s arrived: %v", method, r.err)
		}
		return r.n
	case <-time.After(testTimeout):
		t.Fatalf("%s never arrived on the stream", method)
	}
	return nil
}

// finishHandshake sends notifications/initialized, which a ≤ 2025-11-25
// client owes the server after initialize.
//
// It is not optional decoration here: the gateway defers every
// tools/list_changed until the upstream session is initialized, and this
// notification is what sets that. A test that skips it watches a stream that
// is working perfectly and carries nothing.
func finishHandshake(t *testing.T, d *httpDaemon, bearer, session string) {
	t.Helper()
	status, _, body := d.mcpPost(t, bearer, session, mcp.NewNotification(mcp.NotificationInitialized, nil))
	if status != http.StatusAccepted {
		t.Fatalf("notifications/initialized: HTTP %d (%s), want 202", status, body)
	}
}

// callEcho drives the downstream so it announces a change. The call itself is
// allowed to fail while the catalog is still warming; what the test needs is
// that it eventually reaches the downstream at all.
func callEcho(t *testing.T, d *httpDaemon, bearer, session string) {
	t.Helper()
	waitForDetail(t, "tools/call to reach the downstream", func() (bool, string) {
		res := d.rpc(t, bearer, session, mcp.MethodToolsCall, mcp.CallToolParams{
			Name:      "fake__echo",
			Arguments: json.RawMessage(`{"marker":"stream"}`),
		})
		if res.Error != nil {
			return false, fmt.Sprintf("tools/call returned error %+v", res.Error)
		}
		return true, ""
	})
}

// TestHTTPGetStreamDeliversToolsListChanged is the ≤ 2025-11-25 acceptance:
// the client that used to be told listChanged:true and then never heard
// anything now hears it.
func TestHTTPGetStreamDeliversToolsListChanged(t *testing.T) {
	var value string
	d := startHTTPDaemon(t, func(cfg *daemon.Config) {
		dir, err := cfg.Resolver.DataDir()
		if err != nil {
			t.Fatal(err)
		}
		value = mintToken(t, dir, httpbridge.CreateSpec{Name: "agent", Tier: tier.Destructive})
		cfg.Dial = listChangedOnCall()
	})

	session := d.initialize(t, value)
	finishHandshake(t, d, value, session)
	_, br := openSSE(t, d, http.MethodGet, value, session, nil)

	callEcho(t, d, value, session)
	awaitNotification(t, br, mcp.NotificationToolsListChanged)
}

// TestHTTPListenStreamDeliversToolsListChanged is the 2026-07-28 acceptance,
// over the transport shape that replaced the GET: the acknowledgement first,
// then the real notification carrying the subscription id.
func TestHTTPListenStreamDeliversToolsListChanged(t *testing.T) {
	var value string
	d := startHTTPDaemon(t, func(cfg *daemon.Config) {
		dir, err := cfg.Resolver.DataDir()
		if err != nil {
			t.Fatal(err)
		}
		value = mintToken(t, dir, httpbridge.CreateSpec{Name: "agent", Tier: tier.Destructive})
		cfg.Dial = listChangedOnCall()
	})

	// A session is still minted, because the tool call below is a ≤ 2025-11-25
	// request. The stream itself presents no session id — 2026-07-28 removed
	// the header — which is what makes this the other generation's path.
	session := d.initialize(t, value)
	finishHandshake(t, d, value, session)

	listen := mustJSON(t, mcp.NewRequest(mcp.NewIntID(42), mcp.MethodSubscriptionsListen,
		mustJSON(t, mcp.SubscriptionsListenParams{
			Notifications: mcp.SubscriptionFilter{ToolsListChanged: true},
		})))
	_, br := openSSE(t, d, http.MethodPost, value, "", listen)

	ack := awaitNotification(t, br, mcp.NotificationSubscriptionsAcknowledged)
	var acked mcp.SubscriptionsAcknowledgedParams
	if err := json.Unmarshal(ack.Params, &acked); err != nil {
		t.Fatalf("decode acknowledgement: %v", err)
	}
	if !acked.Notifications.ToolsListChanged {
		t.Fatal("the stream did not acknowledge tools/list_changed, so a client would stop waiting for it")
	}

	callEcho(t, d, value, session)
	got := awaitNotification(t, br, mcp.NotificationToolsListChanged)

	var params struct {
		Meta *mcp.NotificationMeta `json:"_meta"`
	}
	if err := json.Unmarshal(got.Params, &params); err != nil {
		t.Fatalf("decode delivered params: %v", err)
	}
	if params.Meta == nil || params.Meta.SubscriptionID.Key() != mcp.NewIntID(42).Key() {
		t.Fatalf("delivered notification does not carry the listen request's id: %s", got.Params)
	}
}
