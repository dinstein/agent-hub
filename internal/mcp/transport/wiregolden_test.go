package transport

import (
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// updateGolden rewrites the wire golden files:
//
//	go test ./internal/mcp/transport -update
//
// The golden files pin the exact bytes this package puts on the wire —
// method, path, protocol headers and body — because "determinism is a
// contract" (ruling #27). Fix the code, never the golden, unless the
// protocol itself changed.
var updateGolden = flag.Bool("update", false, "rewrite testdata golden files")

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	got = append(got, '\n')
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("wire shape drifted from %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func recordedJSON(t *testing.T, reqs []recordedRequest) []byte {
	t.Helper()
	data, err := json.MarshalIndent(reqs, "", "  ")
	if err != nil {
		t.Fatalf("marshal records: %v", err)
	}
	return data
}

// TestStreamableHTTPWireGolden pins a full streamable-http session:
// initialize → notifications/initialized → tools/list → DELETE.
func TestStreamableHTTPWireGolden(t *testing.T) {
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodGet:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		msg := readRPC(t, r)
		switch m := msg.(type) {
		case *mcp.Request:
			if m.Method == mcp.MethodInitialize {
				w.Header().Set(headerSessionID, "golden-session")
				writeJSONRPC(t, w, mcp.NewResponse(m.ID, initResult(t, mcp.ProtocolVersion)))
				return
			}
			s := startSSE(t, w)
			s.message("g1", mcp.NewResponse(m.ID, json.RawMessage(`{"tools":[]}`)))
		case *mcp.Notification:
			w.WriteHeader(http.StatusAccepted)
		}
	})

	tr := dialStreamable(t, HTTPConfig{
		URL:    fs.URL + "/mcp",
		Header: http.Header{"Authorization": []string{"Bearer golden-token"}},
	})
	if _, err := initializeLegacy(testCtx(t), tr, mcp.Implementation{Name: "agenthub", Version: "test"}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := tr.Call(testCtx(t), mcp.MethodToolsList, mcp.ListToolsParams{}); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkGolden(t, "streamablehttp_wire.json", recordedJSON(t, fs.recorded()))
}

// TestHTTPSSEWireGolden pins a full legacy HTTP+SSE session: the GET
// handshake, then initialize and notifications/initialized POSTed to the
// server-supplied endpoint.
//
// DEPRECATED-UPSTREAM(http+sse, earliest-removal: none) — deprecated
// 2025-03-26; kept on the read side, see httpsse.go
func TestHTTPSSEWireGolden(t *testing.T) {
	ls := newLegacyServer(t, "/messages?sid=golden")
	tr, err := DialHTTPSSE(testCtx(t), HTTPConfig{
		URL:    ls.URL + "/sse",
		Header: http.Header{"Authorization": []string{"Bearer golden-token"}},
	})
	if err != nil {
		t.Fatalf("DialHTTPSSE: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	go func() {
		req, ok := ls.nextPost(t).(*mcp.Request)
		if !ok {
			return
		}
		ls.events <- mcp.NewResponse(req.ID, initResult(t, mcp.ProtocolVersion))
	}()
	if _, err := initializeLegacy(testCtx(t), tr, mcp.Implementation{Name: "agenthub", Version: "test"}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// The initialized notification is fire-and-forget; wait for the server
	// to have seen it so the record set is deterministic.
	if _, ok := ls.nextPost(t).(*mcp.Notification); !ok {
		t.Fatal("expected notifications/initialized")
	}
	checkGolden(t, "httpsse_wire.json", recordedJSON(t, ls.recorded()))
}
