package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// shortTempDir returns a temp dir with a short absolute path. t.TempDir()
// paths can exceed the ~104-byte sun_path limit for UDS on macOS.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ahapi")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// newTestDaemon serves h on a fresh Unix socket and returns its path —
// the in-test fake daemon required for UDS round-trip coverage.
func newTestDaemon(t *testing.T, h http.Handler) string {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "ctl.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln) //nolint:errcheck // closed on cleanup
	t.Cleanup(func() { srv.Close() })
	return sock
}

func writeOK(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data}); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}

func TestPingRoundTripOverUDS(t *testing.T) {
	var gotReqID, gotVersion string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		gotReqID = r.Header.Get(HeaderRequestID)
		gotVersion = r.Header.Get(HeaderAPIVersion)
		writeOK(t, w, Hello{Version: "0.1.0", Pid: 4242, Generation: 7})
	})
	c := New(newTestDaemon(t, mux))
	defer c.Close()

	h, err := c.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if h.Version != "0.1.0" || h.Pid != 4242 || h.Generation != 7 {
		t.Errorf("unexpected Hello: %+v", h)
	}
	if gotReqID == "" {
		t.Error("X-Request-Id was not generated")
	}
	if gotVersion != APIVersion {
		t.Errorf("X-Agenthub-Api-Version = %q, want %q", gotVersion, APIVersion)
	}
}

func TestRequestIDOverride(t *testing.T) {
	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(HeaderRequestID)
		writeOK(t, w, Hello{})
	})
	c := New(newTestDaemon(t, mux))
	defer c.Close()

	ctx := WithRequestID(context.Background(), "fixed-id-123")
	if _, err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got != "fixed-id-123" {
		t.Errorf("X-Request-Id = %q, want override fixed-id-123", got)
	}
}

func TestServersList(t *testing.T) {
	want := []Server{{
		ID: "github", Transport: "stdio", Enabled: true, State: "connected",
		Tools: 26, Source: "manual",
		Health: Health{Level: HealthLevelHealthy, AdminState: AdminStateEnabled, Summary: "ok"},
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/servers", func(w http.ResponseWriter, r *http.Request) {
		writeOK(t, w, want)
	})
	c := New(newTestDaemon(t, mux))
	defer c.Close()

	got, err := c.Servers.List(context.Background())
	if err != nil {
		t.Fatalf("Servers.List: %v", err)
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestErrorBodyPassthrough(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/servers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderRequestID, r.Header.Get(HeaderRequestID)) // echo
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": false,
			"error": ErrorBody{
				Code: "E_SERVER_NOT_FOUND", Message: "no server 'gh'", Hint: "did you mean 'github'?",
			},
		})
	})
	c := New(newTestDaemon(t, mux))
	defer c.Close()

	_, err := c.Servers.List(WithRequestID(context.Background(), "req-42"))
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *api.Error, got %T: %v", err, err)
	}
	if apiErr.Code != "E_SERVER_NOT_FOUND" || apiErr.Message != "no server 'gh'" ||
		apiErr.Hint != "did you mean 'github'?" {
		t.Errorf("error body not passed through: %+v", apiErr)
	}
	if apiErr.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want 404", apiErr.Status)
	}
	if apiErr.RequestID != "req-42" {
		t.Errorf("RequestID = %q, want echoed req-42", apiErr.RequestID)
	}
}

// TestGarbledResponseFailsClosed: a body that cannot be identified as a
// success envelope must surface as an error, never as silent success.
func TestGarbledResponseFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
	}{
		{"not_json", "<html>gateway timeout</html>", http.StatusOK},
		{"ok_false_without_error", `{"ok":false}`, http.StatusOK},
		{"http_error_without_error_body", `{"ok":true,"data":[]}`, http.StatusInternalServerError},
		{"success_without_data", `{"ok":true}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/servers", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			})
			c := New(newTestDaemon(t, mux))
			defer c.Close()
			_, err := c.Servers.List(context.Background())
			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("want *api.Error, got %T: %v", err, err)
			}
			if apiErr.Code != ErrCodeBadResponse {
				t.Errorf("Code = %q, want %q", apiErr.Code, ErrCodeBadResponse)
			}
		})
	}
}

func TestDaemonDownIsTransportError(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "absent.sock")
	c := New(sock)
	defer c.Close()
	if _, err := c.Ping(context.Background()); err == nil {
		t.Fatal("Ping against a non-existent socket must fail")
	}
}
