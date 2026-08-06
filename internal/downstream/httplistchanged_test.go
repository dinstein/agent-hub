package downstream_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
)

// listChangedFake is a streamable-http MCP server that grows a second tool
// and announces it on the GET stream — the only channel by which a
// streamable-http downstream can say its tool set moved.
type listChangedFake struct {
	srv   *httptest.Server
	tools atomic.Int32 // how many tools tools/list currently returns
	gets  atomic.Int32
	push  chan struct{} // closed by the test to trigger the notification
}

func newListChangedFake(t *testing.T) *listChangedFake {
	t.Helper()
	f := &listChangedFake{push: make(chan struct{})}
	f.tools.Store(1)
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *listChangedFake) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		f.gets.Add(1)
		flusher, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		select {
		case <-f.push:
		case <-r.Context().Done():
			return
		}
		data, _ := json.Marshal(mcp.NewNotification(mcp.NotificationToolsListChanged, nil))
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
		flusher.Flush()
		<-r.Context().Done()
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	msg, err := mcp.ParseMessage(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	req, ok := msg.(*mcp.Request)
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch req.Method {
	case mcp.MethodInitialize:
		writeMsg(w, mcp.NewResponse(req.ID, mustJSON(mcp.InitializeResult{
			ProtocolVersion: mcp.ProtocolVersion,
			Capabilities:    json.RawMessage(`{"tools":{"listChanged":true}}`),
			ServerInfo:      mcp.Implementation{Name: "grower", Version: "1"},
		})))
	case mcp.MethodToolsList:
		defs := []mcp.ToolDef{{Name: "one", InputSchema: json.RawMessage(`{"type":"object"}`)}}
		if f.tools.Load() > 1 {
			defs = append(defs, mcp.ToolDef{Name: "two", InputSchema: json.RawMessage(`{"type":"object"}`)})
		}
		writeMsg(w, mcp.NewResponse(req.ID, mustJSON(mcp.ListToolsResult{Tools: defs})))
	default:
		writeMsg(w, mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeMethodNotFound, Message: req.Method}))
	}
}

// TestHTTPListChangedRefreshesTheCatalog: a streamable-http downstream's
// tools/list_changed reaches this process and the catalog follows it.
//
// The machinery for this existed and worked; nothing in production turned it
// on, so a remote server's catalog was fixed from connect until the
// connection was rebuilt, and no log line, status field or doctor check said
// so. Catalog refresh has no other trigger — no poll, no TTL, no re-list on
// anything but a reconnect — and a healthy HTTP downstream never reconnects.
//
// The GET is the assertion as much as the tool count: without it there is no
// channel at all, and a server answering `application/json` to every POST
// can never volunteer a notification.
func TestHTTPListChangedRefreshesTheCatalog(t *testing.T) {
	t.Parallel()
	f := newListChangedFake(t)

	srv, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:         "grower",
		Kind:       transport.StreamableHTTP,
		URL:        f.srv.URL + "/mcp",
		Provenance: downstream.ProvenanceLocal,
	}, downstream.Deps{ConnectTimeout: 10 * time.Second, NotificationStream: true})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer srv.Close()

	changed := make(chan struct{}, 4)
	srv.OnListChanged(func(transport.ChangeMask) { changed <- struct{}{} })

	if got := len(srv.Tools()); got != 1 {
		t.Fatalf("tools at connect = %d, want 1", got)
	}
	// Wait for the stream to be open before the server changes anything: a
	// notification pushed before the GET lands would be lost, and the test
	// would then be timing rather than behaviour.
	deadline := time.Now().Add(5 * time.Second)
	for f.gets.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no GET stream was opened; a streamable-http downstream then has no channel for list_changed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	f.tools.Store(2)
	close(f.push)

	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("tools/list_changed never arrived")
	}
	for time.Now().Before(deadline.Add(10 * time.Second)) {
		if len(srv.Tools()) == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("catalog still has %d tools after list_changed", len(srv.Tools()))
}
