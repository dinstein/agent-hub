package downstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/guard"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
)

// dialBlocked reports whether the spec's dialer refuses addr. The dial is
// expected to fail inside the Control hook, before any packet leaves, so the
// timeout below is a backstop against a hang rather than the mechanism.
func dialBlocked(t *testing.T, spec Spec, addr string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dial := dialContextFor(spec)
	if dial == nil {
		t.Fatal("dialContextFor returned no dialer: the HTTP client would be built UNSCREENED")
	}
	conn, err := dial(ctx, "tcp", addr)
	if conn != nil {
		_ = conn.Close()
	}
	return err
}

// TestEveryHTTPSpecGetsAScreenedDialer guards the seam the whole SSRF design
// rests on.
//
// internal/mcp is standard-library only, so it cannot screen addresses itself;
// it takes an injected dialer and, given none, builds a plain one with NO
// screening at all (newHTTPClient). That combination is documented as
// tests-only, which makes internal/downstream the single place responsible for
// never producing it — and nothing tested that it did.
//
// The check is per spec SHAPE rather than a single case, because the carve-out
// below means the dialer is built differently for different specs, and "some
// specs are screened" is not the property that matters.
func TestEveryHTTPSpecGetsAScreenedDialer(t *testing.T) {
	specs := []struct {
		name string
		spec Spec
	}{
		{"bare remote", Spec{ID: "s", URL: "https://x.example/mcp"}},
		{"explicitly remote", Spec{ID: "s", URL: "https://x.example/mcp", Provenance: ProvenanceRemote}},
		{"local provenance", Spec{ID: "s", URL: "http://127.0.0.1:9/mcp", Provenance: ProvenanceLocal}},
		{"unknown provenance string", Spec{ID: "s", URL: "https://x.example/mcp", Provenance: "who-knows"}},
		{"zero value", Spec{}},
	}
	// 169.254.169.254 is the cloud metadata address: the single destination an
	// SSRF is most often pointed at, and one no spec shape may reach.
	for _, tc := range specs {
		t.Run(tc.name, func(t *testing.T) {
			err := dialBlocked(t, tc.spec, "169.254.169.254:80")
			if err == nil {
				t.Fatal("the link-local metadata address was dialable")
			}
			if !errors.Is(err, guard.ErrBlocked) {
				t.Fatalf("refused with %v, which does not unwrap to guard.ErrBlocked", err)
			}
		})
	}
}

// TestLocalProvenanceCarveOutIsOnlyLoopback pins how narrow the carve-out is.
// ProvenanceLocal buys a literal loopback address and nothing else: RFC1918,
// CGNAT and link-local stay blocked even for a server the operator called
// local, because those are the ranges cloud metadata services and intranet
// hosts live in.
func TestLocalProvenanceCarveOutIsOnlyLoopback(t *testing.T) {
	local := Spec{ID: "s", URL: "http://127.0.0.1:9/mcp", Provenance: ProvenanceLocal}

	// Refused even with the carve-out active.
	for _, addr := range []string{
		"10.0.0.1:80", "192.168.1.1:80", "172.16.0.1:80",
		"169.254.169.254:80", "100.64.0.1:80",
		"[::127.0.0.1]:80", // v4-embedding form: not a literal loopback
	} {
		t.Run("refused/"+addr, func(t *testing.T) {
			if err := dialBlocked(t, local, addr); !errors.Is(err, guard.ErrBlocked) {
				t.Fatalf("%s was not blocked for a local-provenance spec: %v", addr, err)
			}
		})
	}

	// A literal loopback IS allowed past the guard — port 9 (discard) is
	// closed, so the dial fails, but it must NOT fail as a guard block.
	for _, addr := range []string{"127.0.0.1:9", "[::1]:9"} {
		t.Run("permitted/"+addr, func(t *testing.T) {
			if err := dialBlocked(t, local, addr); errors.Is(err, guard.ErrBlocked) {
				t.Fatalf("%s was blocked despite the local carve-out", addr)
			}
		})
	}

	// Without ProvenanceLocal the same loopback address is refused.
	remote := Spec{ID: "s", URL: "http://127.0.0.1:9/mcp"}
	if err := dialBlocked(t, remote, "127.0.0.1:9"); !errors.Is(err, guard.ErrBlocked) {
		t.Fatalf("loopback was reachable without local provenance: %v", err)
	}
}

// TestSSEDialLogsUnderTheServerBinding pins the seam that makes the
// transport's records usable: internal/mcp cannot reach logx, so a logger
// with the server already bound has to be handed down from here. Without it
// the endpoint address is recorded under no server at all, which in a merged
// gateway stream is the same as not recording it.
func TestSSEDialLogsUnderTheServerBinding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: endpoint\ndata: /messages\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	var buf bytes.Buffer
	deps := Deps{Log: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	spec := Spec{
		ID: "elk-mcp", Kind: transport.SSE, URL: srv.URL + "/sse",
		Provenance: ProvenanceLocal, // httptest is loopback
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := deps.dialHTTP(ctx, spec)
	if err != nil {
		t.Fatalf("dialHTTP: %v", err)
	}
	defer func() { _ = tr.Close() }()

	line := buf.String()
	if !strings.Contains(line, "sse endpoint") {
		t.Fatalf("the endpoint negotiation was not recorded: %q", line)
	}
	if !strings.Contains(line, `"`+logx.FieldServer+`":"elk-mcp"`) {
		t.Fatalf("record is not bound to the server: %s", line)
	}
}

// How a dial was assembled decides how the connection behaves, and none of it
// was recoverable afterwards: an HTTP downstream either worked or produced an
// error naming neither the wire protocol it spoke nor where its Authorization
// came from. The event log says a connect happened; only this says how it was
// set up.
func TestHTTPDialRecordsHowItWasAssembled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	deps := Deps{Log: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	spec := Spec{
		ID: "remote-mcp", Kind: transport.StreamableHTTP, URL: srv.URL + "/mcp",
		Provenance: ProvenanceLocal, // httptest is loopback
		Headers:    map[string]string{"Authorization": "Basic dXNlcjpwYXNz"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := deps.dialHTTP(ctx, spec)
	if err != nil {
		t.Fatalf("dialHTTP: %v", err)
	}
	defer func() { _ = tr.Close() }()

	line := buf.String()
	for _, want := range []string{
		`"level":"INFO"`,
		`"msg":"dialing an http downstream"`,
		`"transport":"` + string(transport.StreamableHTTP) + `"`,
		// An operator-set Authorization always beats the vault credential, and
		// which of the two won is the whole of a 401 investigation.
		`"auth":"explicit header"`,
		`"` + logx.FieldServer + `":"remote-mcp"`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("dial record missing %s: %s", want, line)
		}
	}
	// The host, so a wrong port or an http:// that should have been https://
	// is visible — but never the path or query, which is where tokens get put.
	if !strings.Contains(line, `"host":"`+srv.URL+`"`) {
		t.Fatalf("dial record does not carry the endpoint host: %s", line)
	}
	if strings.Contains(line, "/mcp") {
		t.Fatalf("dial record carries the url path, which may hold a secret: %s", line)
	}
	if strings.Contains(line, "dXNlcjpwYXNz") {
		t.Fatalf("the credential itself leaked into the dial record: %s", line)
	}
}

// endpointHost must never echo something it could not parse: a URL this
// package cannot read is not one it should quote back into a log.
func TestEndpointHostDropsWhatItCannotParse(t *testing.T) {
	for _, raw := range []string{"://nope", "not a url at all", ""} {
		if got := endpointHost(raw); got != "" {
			t.Errorf("endpointHost(%q) = %q, want empty", raw, got)
		}
	}
	// Userinfo is a credential and lives outside Host, so reducing to
	// scheme://host drops it by construction rather than by a filter.
	if got := endpointHost("https://user:pass@example.com/mcp?token=abc"); got != "https://example.com" {
		t.Errorf("endpointHost kept more than scheme://host: %q", got)
	}
}
