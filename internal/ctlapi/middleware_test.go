package ctlapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/session"
)

func newBareServer(t *testing.T) *Server {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(Options{
		Registry: reg,
		Sessions: session.NewMemoryManager(session.Options{}),
		Bus:      event.NewBus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// TestPanicBeforeWriteBecomes500 proves the request id survives a panic:
// the header is written before the handler runs, and the recovery path
// emits a full 500 envelope.
func TestPanicBeforeWriteBecomes500(t *testing.T) {
	srv := newBareServer(t)
	h := srv.withMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	req.Header.Set(api.HeaderRequestID, "panic-req-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
	if got := rec.Header().Get(api.HeaderRequestID); got != "panic-req-1" {
		t.Errorf("request id lost across panic: %q", got)
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("500 body is not an envelope: %v (%s)", err, rec.Body.String())
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj == nil || errObj["code"] != CodeInternal || errObj["requestId"] != "panic-req-1" {
		t.Errorf("error body = %v", env)
	}
}

// TestPanicAfterWriteAborts: once the response has started, recovery must
// not append an envelope to a half-written body — it aborts the connection
// (http.ErrAbortHandler).
func TestPanicAfterWriteAborts(t *testing.T) {
	srv := newBareServer(t)
	h := srv.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("mid-stream")
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	rec := httptest.NewRecorder()
	defer func() {
		p := recover()
		if p != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", p)
		}
		if body := rec.Body.String(); strings.Contains(body, CodeInternal) {
			t.Errorf("500 envelope appended to a started response: %q", body)
		}
	}()
	h.ServeHTTP(rec, req)
	t.Fatal("handler did not panic through")
}

func TestActorParsing(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"cli", "cli"},
		{"gui", "gui"},
		{"gateway:cursor:3", "gateway:cursor:3"},
		{"", "cli"},
		{"root", "cli"},
		{"gateway:", "cli"},
		{"GUI", "cli"},
		{"gateway:" + strings.Repeat("x", 300), "cli"}, // oversized rejected
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.header != "" {
			r.Header.Set(HeaderActor, tc.header)
		}
		if got := actor(r); got != tc.want {
			t.Errorf("actor(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}
