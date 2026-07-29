package downstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// --- a minimal streamable-http MCP server -------------------------------

// httpFake is a one-endpoint MCP Streamable HTTP server: every JSON-RPC
// request is answered with application/json. It exists to exercise the
// downstream wiring (screening, header/secret expansion, bearer injection,
// 401 retry); the transport's own protocol surface is covered in
// internal/mcp/transport.
type httpFake struct {
	srv *httptest.Server
	// wantToken, when non-empty, is the credential the server accepts; every
	// other value gets a 401.
	wantToken atomic.Value // string
	// seen records the Authorization values observed, in order.
	mu   sync.Mutex
	seen []string
}

func newHTTPFake(t *testing.T) *httpFake {
	t.Helper()
	f := &httpFake{}
	f.wantToken.Store("")
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *httpFake) url() string { return f.srv.URL + "/mcp" }

func (f *httpFake) authSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *httpFake) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed) // no GET notification stream
		return
	}
	auth := r.Header.Get("Authorization")
	f.mu.Lock()
	f.seen = append(f.seen, auth)
	f.mu.Unlock()

	if want, _ := f.wantToken.Load().(string); want != "" && auth != "Bearer "+want {
		w.Header().Set("WWW-Authenticate", `Bearer realm="fake"`)
		w.WriteHeader(http.StatusUnauthorized)
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
		w.WriteHeader(http.StatusAccepted) // notification
		return
	}
	var result json.RawMessage
	switch req.Method {
	case mcp.MethodInitialize:
		result = mustJSON(mcp.InitializeResult{
			ProtocolVersion: mcp.ProtocolVersion,
			Capabilities:    json.RawMessage(`{"tools":{}}`),
			ServerInfo:      mcp.Implementation{Name: "http-fake", Version: "1"},
		})
	case mcp.MethodToolsList:
		result = mustJSON(mcp.ListToolsResult{Tools: []mcp.ToolDef{{
			Name:        "echo",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}}})
	case mcp.MethodToolsCall:
		var p mcp.CallToolParams
		_ = json.Unmarshal(req.Params, &p)
		result = mustJSON(mcp.CallResult{
			Content: json.RawMessage(fmt.Sprintf(`[{"type":"text","text":%q}]`, string(p.Arguments))),
		})
	default:
		w.Header().Set("Content-Type", "application/json")
		writeMsg(w, mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeMethodNotFound, Message: req.Method}))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	writeMsg(w, mcp.NewResponse(req.ID, result))
}

func writeMsg(w http.ResponseWriter, v any) {
	data, _ := json.Marshal(v)
	_, _ = w.Write(data)
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

// --- connect / call ------------------------------------------------------

// TestHTTPConnectListAndCall pins the whole http path end to end through the
// REAL screened dialer: the loopback endpoint is reachable only because the
// spec declares provenance=local.
func TestHTTPConnectListAndCall(t *testing.T) {
	t.Parallel()
	f := newHTTPFake(t)

	srv, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:         "remote",
		Kind:       transport.StreamableHTTP,
		URL:        f.url(),
		Provenance: downstream.ProvenanceLocal,
	}, downstream.Deps{ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer srv.Close()

	tools := srv.Tools()
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, want one echo tool", tools)
	}
	res, err := srv.Call(context.Background(), "echo", json.RawMessage(`{"marker":"http-ok"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(res.Content), "http-ok") {
		t.Fatalf("call result = %s", res.Content)
	}
}

// TestHTTPScreeningRefusesPrivateWithoutProvenance proves the fail-closed
// direction: the same loopback endpoint is refused when the entry does not
// declare it local.
func TestHTTPScreeningRefusesPrivateWithoutProvenance(t *testing.T) {
	t.Parallel()
	f := newHTTPFake(t)

	_, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:   "remote",
		Kind: transport.StreamableHTTP,
		URL:  f.url(),
	}, downstream.Deps{ConnectTimeout: 5 * time.Second})
	if err == nil {
		t.Fatal("connect to a loopback endpoint succeeded without provenance=local")
	}
	if !strings.Contains(err.Error(), "private") {
		t.Fatalf("error = %v, want a private-address refusal", err)
	}
}

// TestHTTPProvenanceLocalOnlyCoversLoopback proves the carve-out is narrow:
// declaring a non-loopback host local does not unlock it.
func TestHTTPProvenanceLocalOnlyCoversLoopback(t *testing.T) {
	t.Parallel()
	_, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:         "intranet",
		Kind:       transport.StreamableHTTP,
		URL:        "http://10.1.2.3:8080/mcp",
		Provenance: downstream.ProvenanceLocal,
	}, downstream.Deps{ConnectTimeout: 5 * time.Second})
	if err == nil {
		t.Fatal("RFC1918 endpoint accepted under provenance=local")
	}
	if !strings.Contains(err.Error(), "literal loopback") {
		t.Fatalf("error = %v, want the loopback-only refusal", err)
	}
}

// --- credentials ---------------------------------------------------------

// TestHTTPBearerFromVault covers the happy path of credential injection:
// the token comes from the (serverID, _global, __http_auth__) vault entry
// and rides on every request.
func TestHTTPBearerFromVault(t *testing.T) {
	t.Parallel()
	f := newHTTPFake(t)
	f.wantToken.Store("tok-1")

	resolve := staticResolver(map[secrets.Ref]string{
		secrets.HTTPAuthRef("remote"): "tok-1",
	})
	srv, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:         "remote",
		Kind:       transport.StreamableHTTP,
		URL:        f.url(),
		Provenance: downstream.ProvenanceLocal,
	}, downstream.Deps{
		Secrets:        resolve,
		Auth:           downstream.NewVaultTokenSource("remote", resolve, nil),
		ConnectTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer srv.Close()
	for _, got := range f.authSeen() {
		if got != "Bearer tok-1" {
			t.Fatalf("Authorization = %q, want Bearer tok-1", got)
		}
	}
}

// TestHTTPPassiveRefreshRetriesExactlyOnce is the core of the 401 contract:
// one refresh, one replay, and no second refresh when the fresh credential
// is rejected too.
func TestHTTPPassiveRefreshRetriesExactlyOnce(t *testing.T) {
	t.Parallel()
	f := newHTTPFake(t)
	f.wantToken.Store("fresh")

	var refreshes atomic.Int64
	resolve := staticResolver(map[secrets.Ref]string{
		secrets.HTTPAuthRef("remote"): "stale",
	})
	auth := downstream.NewVaultTokenSource("remote", resolve, func(context.Context) (string, error) {
		refreshes.Add(1)
		return "fresh", nil
	})

	srv, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:         "remote",
		Kind:       transport.StreamableHTTP,
		URL:        f.url(),
		Provenance: downstream.ProvenanceLocal,
	}, downstream.Deps{Secrets: resolve, Auth: auth, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Connect through a 401 + refresh: %v", err)
	}
	defer srv.Close()

	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refreshes = %d, want exactly 1 (the cached token serves every later request)", got)
	}
	seen := f.authSeen()
	if len(seen) < 2 || seen[0] != "Bearer stale" || seen[1] != "Bearer fresh" {
		t.Fatalf("authorization sequence = %v, want [stale fresh …]", seen)
	}
	for _, got := range seen[1:] {
		if got != "Bearer fresh" {
			t.Fatalf("request after refresh carried %q", got)
		}
	}
}

// TestHTTPRefreshFailureSurfacesOriginal401 pins the failure direction: a
// refresh that cannot happen must not hide the server's own rejection.
func TestHTTPRefreshFailureSurfacesOriginal401(t *testing.T) {
	t.Parallel()
	f := newHTTPFake(t)
	f.wantToken.Store("never-issued")

	resolve := staticResolver(map[secrets.Ref]string{
		secrets.HTTPAuthRef("remote"): "stale",
	})
	auth := downstream.NewVaultTokenSource("remote", resolve, func(context.Context) (string, error) {
		return "", errors.New("authorization server said no")
	})
	_, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:         "remote",
		Kind:       transport.StreamableHTTP,
		URL:        f.url(),
		Provenance: downstream.ProvenanceLocal,
	}, downstream.Deps{Secrets: resolve, Auth: auth, ConnectTimeout: 10 * time.Second})
	if err == nil {
		t.Fatal("connect succeeded against a server that rejects every credential")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want the downstream 401 to survive the failed refresh", err)
	}
}

// TestHTTPVaultMissIsNotCached is the regression test for the order the CLI
// recommends: `server add` (disabled) → `server enable` (connects, 401,
// enabled anyway) → `auth login` (writes the vault). The connection is built
// while the vault holds nothing, so caching that miss would pin the empty
// string for the life of the process and 401 forever — the gateway would
// list the server and fail every call until it was restarted.
//
// The refresher is nil here on purpose: this proves the plain vault re-read
// recovers on its own, with no OAuth grant available to paper over it.
func TestHTTPVaultMissIsNotCached(t *testing.T) {
	t.Parallel()
	f := newHTTPFake(t)
	f.wantToken.Store("late-token")

	var mu sync.Mutex
	stored := "" // the vault is empty when the connection is dialed
	resolve := func(_ context.Context, ref secrets.Ref) (string, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if ref != secrets.HTTPAuthRef("remote") || stored == "" {
			return "", false, nil
		}
		return stored, true, nil
	}

	// The server accepts anything until the credential lands, so the
	// connection survives its dial and stays up — this must be ONE live
	// connection throughout, because a redial would build a fresh round
	// tripper and prove nothing.
	f.wantToken.Store("")
	srv, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:         "remote",
		Kind:       transport.StreamableHTTP,
		URL:        f.url(),
		Provenance: downstream.ProvenanceLocal,
	}, downstream.Deps{
		Secrets:        resolve,
		Auth:           downstream.NewVaultTokenSource("remote", resolve, nil),
		ConnectTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Connect against an anonymous server: %v", err)
	}
	defer srv.Close()
	if first := f.authSeen(); len(first) == 0 || first[0] != "" {
		t.Fatalf("first request carried %v, want no Authorization (the vault was empty)", first)
	}

	// `agenthub auth login` lands in another process, and the server starts
	// demanding the token it just issued.
	mu.Lock()
	stored = "late-token"
	mu.Unlock()
	f.wantToken.Store("late-token")

	if err := srv.RefreshTools(context.Background()); err != nil {
		t.Fatalf("call on the live connection after the credential landed: %v", err)
	}
	if last := f.authSeen(); last[len(last)-1] != "Bearer late-token" {
		t.Fatalf("last request carried %q, want Bearer late-token", last[len(last)-1])
	}
}

// TestHTTPExplicitAuthorizationHeaderWins covers the hand-pasted-token
// configuration: an operator-set Authorization header suppresses the vault
// injection entirely.
func TestHTTPExplicitAuthorizationHeaderWins(t *testing.T) {
	t.Parallel()
	f := newHTTPFake(t)
	f.wantToken.Store("pasted")

	resolve := staticResolver(map[secrets.Ref]string{
		secrets.HTTPAuthRef("remote"):    "vault-token",
		secrets.UserRef("remote", "PAT"): "pasted",
	})
	srv, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:         "remote",
		Kind:       transport.StreamableHTTP,
		URL:        f.url(),
		Headers:    map[string]string{"Authorization": "Bearer ${SECRET_PAT}"},
		Provenance: downstream.ProvenanceLocal,
	}, downstream.Deps{
		Secrets:        resolve,
		Auth:           downstream.NewVaultTokenSource("remote", resolve, nil),
		ConnectTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer srv.Close()
	for _, got := range f.authSeen() {
		if got != "Bearer pasted" {
			t.Fatalf("Authorization = %q, want the explicit header to win", got)
		}
	}
}

// TestHTTPUnresolvedHeaderSecretFailsClosed proves a missing secret never
// reaches the wire as literal placeholder text.
func TestHTTPUnresolvedHeaderSecretFailsClosed(t *testing.T) {
	t.Parallel()
	f := newHTTPFake(t)

	_, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:         "remote",
		Kind:       transport.StreamableHTTP,
		URL:        f.url(),
		Headers:    map[string]string{"X-Api-Key": "${SECRET_MISSING}"},
		Provenance: downstream.ProvenanceLocal,
	}, downstream.Deps{
		Secrets:        staticResolver(nil),
		ConnectTimeout: 5 * time.Second,
	})
	if !errors.Is(err, downstream.ErrUnresolvedSecret) {
		t.Fatalf("error = %v, want ErrUnresolvedSecret", err)
	}
	if len(f.authSeen()) != 0 {
		t.Fatal("a request went out despite the unresolved placeholder")
	}
}

// staticResolver is an in-memory secrets.Resolver.
func staticResolver(m map[secrets.Ref]string) secrets.Resolver {
	return func(_ context.Context, ref secrets.Ref) (string, bool, error) {
		v, ok := m[ref]
		return v, ok, nil
	}
}

// --- registry mapping ----------------------------------------------------

func TestSpecFromEntry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entry   registry.ServerEntry
		wantErr bool
		want    transport.Kind
	}{
		{name: "stdio default", entry: registry.ServerEntry{Command: "npx"}, want: transport.Stdio},
		{name: "stdio explicit", entry: registry.ServerEntry{Transport: "stdio", Command: "npx"}, want: transport.Stdio},
		{name: "stdio without command", entry: registry.ServerEntry{Transport: "stdio"}, wantErr: true},
		{name: "http", entry: registry.ServerEntry{Transport: "http", URL: "https://x/mcp"}, want: transport.StreamableHTTP},
		{name: "http without url", entry: registry.ServerEntry{Transport: "http"}, wantErr: true},
		{name: "sse", entry: registry.ServerEntry{Transport: "sse", URL: "https://x/sse"}, want: transport.SSE},
		{name: "unknown", entry: registry.ServerEntry{Transport: "grpc"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := downstream.SpecFromEntry("s", tc.entry)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SpecFromEntry(%+v) = %+v, want an error", tc.entry, spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("SpecFromEntry: %v", err)
			}
			if spec.Kind != tc.want {
				t.Errorf("kind = %q, want %q", spec.Kind, tc.want)
			}
			if spec.ID != "s" {
				t.Errorf("id = %q", spec.ID)
			}
		})
	}
}

// TestSpecFromEntryCopiesMaps proves the spec does not alias the registry
// snapshot: a connected server must not observe a later registry edit.
func TestSpecFromEntryCopiesMaps(t *testing.T) {
	t.Parallel()
	entry := registry.ServerEntry{
		Transport: "http",
		URL:       "https://x/mcp",
		Headers:   map[string]string{"X": "1"},
		Env:       map[string]string{"E": "1"},
	}
	spec, err := downstream.SpecFromEntry("s", entry)
	if err != nil {
		t.Fatal(err)
	}
	entry.Headers["X"] = "mutated"
	entry.Env["E"] = "mutated"
	if spec.Headers["X"] != "1" || spec.Env["E"] != "1" {
		t.Fatalf("spec aliases the registry maps: %+v", spec)
	}
}
