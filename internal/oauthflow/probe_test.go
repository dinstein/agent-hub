package oauthflow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A resource server is allowed to publish its protected-resource metadata
// anywhere; RFC 9728 says it names the location in the 401 it returns to an
// unauthenticated request. Guessing from a candidate list only works for
// servers that happen to use a conventional path, so these tests pin that
// the probe actually asks — and that a server which answers unhelpfully
// costs nothing beyond the request.

// probeServer is a resource server that answers 401 with the given
// WWW-Authenticate header and records what was requested.
func probeServer(t *testing.T, header string, status int) (*httptest.Server, *[]string) {
	t.Helper()
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		if header != "" {
			w.Header().Set("WWW-Authenticate", header)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func probeDiscoverer(t *testing.T) *Discoverer {
	t.Helper()
	return NewDiscoverer(NewClient(Config{AllowLoopback: true}))
}

func TestProbeReadsTheAdvertisedPointer(t *testing.T) {
	srv, hits := probeServer(t,
		`Bearer resource_metadata="https://example.test/auth/prm", realm="mcp"`,
		http.StatusUnauthorized)

	got := probeDiscoverer(t).ProbeResourceMetadataURL(context.Background(), srv.URL+"/mcp")
	if got != "https://example.test/auth/prm" {
		t.Fatalf("probe = %q, want the advertised pointer", got)
	}
	if len(*hits) != 1 || (*hits)[0] != "/mcp" {
		t.Fatalf("probe requested %v, want exactly one request to /mcp", *hits)
	}
}

func TestProbeFailsSoft(t *testing.T) {
	cases := []struct {
		name   string
		header string
		status int
	}{
		{"401 without the parameter", `Bearer realm="mcp"`, http.StatusUnauthorized},
		{"401 without any challenge", "", http.StatusUnauthorized},
		{"server answers 200", "", http.StatusOK},
		{"server answers 500", "", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := probeServer(t, tc.header, tc.status)
			if got := probeDiscoverer(t).ProbeResourceMetadataURL(context.Background(), srv.URL+"/mcp"); got != "" {
				t.Fatalf("probe = %q, want empty so discovery falls back to its candidates", got)
			}
		})
	}
}

func TestProbeSurvivesAnUnreachableServer(t *testing.T) {
	// Nothing is listening: the probe must return empty rather than fail the
	// login, because discovery still has candidates to try.
	got := probeDiscoverer(t).ProbeResourceMetadataURL(context.Background(), "http://127.0.0.1:1/mcp")
	if got != "" {
		t.Fatalf("probe = %q, want empty", got)
	}
}

func TestProbeScreensTheTarget(t *testing.T) {
	// The resource URL itself is screened like every other destination: a
	// discoverer without the loopback exemption must not dial one.
	d := NewDiscoverer(NewClient(Config{}))
	srv, hits := probeServer(t, `Bearer resource_metadata="https://example.test/prm"`, http.StatusUnauthorized)
	if got := d.ProbeResourceMetadataURL(context.Background(), srv.URL+"/mcp"); got != "" {
		t.Fatalf("probe = %q, want empty for a screened target", got)
	}
	if len(*hits) != 0 {
		t.Fatalf("screened target was still contacted: %v", *hits)
	}
}

func TestProbedPointerIsTriedFirst(t *testing.T) {
	// End to end: the 401 names a non-conventional location, and discovery
	// must try it before any guessed candidate.
	var hits []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		switch r.URL.Path {
		case "/mcp":
			w.Header().Set("WWW-Authenticate",
				`Bearer resource_metadata="`+srv.URL+`/auth/prm"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/auth/prm":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"resource":"` + srv.URL + `/mcp","authorization_servers":["` + srv.URL + `"]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	d := probeDiscoverer(t)
	ptr := d.ProbeResourceMetadataURL(context.Background(), srv.URL+"/mcp")
	if ptr != srv.URL+"/auth/prm" {
		t.Fatalf("probe = %q", ptr)
	}
	_, _ = d.DiscoverFromResource(context.Background(), srv.URL+"/mcp", ptr)

	var prmIndex = -1
	for i, h := range hits {
		if h == "/auth/prm" {
			prmIndex = i
			break
		}
	}
	if prmIndex < 0 {
		t.Fatalf("the advertised document was never fetched: %v", hits)
	}
	for _, h := range hits[:prmIndex] {
		if strings.HasPrefix(h, "/.well-known/") {
			t.Fatalf("a guessed candidate was tried before the advertised one: %v", hits)
		}
	}
}
