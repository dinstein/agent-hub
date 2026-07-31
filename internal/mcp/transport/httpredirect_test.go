package transport

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestDefaultClientRefusesCrossOriginRedirect covers the half of the
// credential-redirect defect that does not involve the vault at all.
//
// HTTPConfig.Header is applied to every request, and an operator-configured
// Authorization header lives there (Deps.buildHeader's "an explicit header
// wins" path). net/http drops sensitive headers only when a redirect leaves
// the DOMAIN, so without a policy of our own it would carry that header to
// a subdomain, or down from https to http, because the downstream said so.
func TestDefaultClientRefusesCrossOriginRedirect(t *testing.T) {
	endpoint, err := url.Parse("https://vendor.example/mcp")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	client := newHTTPClient(nil, endpoint)
	if client.CheckRedirect == nil {
		t.Fatal("the default client has no redirect policy: caller headers would follow a 3xx anywhere")
	}

	for _, target := range []string{
		"https://attacker.example/collect",
		"https://sub.vendor.example/mcp", // net/http alone would keep Authorization here
		"http://vendor.example/mcp",      // and here
		"https://vendor.example:8443/mcp",
	} {
		u, perr := url.Parse(target)
		if perr != nil {
			t.Fatalf("parse %s: %v", target, perr)
		}
		if err := client.CheckRedirect(&http.Request{URL: u}, nil); err == nil {
			t.Errorf("redirect to %s was permitted", target)
		}
	}

	same, err := url.Parse("https://vendor.example/mcp/v2")
	if err != nil {
		t.Fatalf("parse same-origin target: %v", err)
	}
	if err := client.CheckRedirect(&http.Request{URL: same}, nil); err != nil {
		t.Fatalf("a same-origin redirect was refused: %v", err)
	}
}

// TestDefaultClientCarriesHeadersOnlyToItsOwnOrigin is the end-to-end form:
// a real redirect, a real header, and an assertion about what arrived.
func TestDefaultClientCarriesHeadersOnlyToItsOwnOrigin(t *testing.T) {
	seen := make(chan string, 4)
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/collect", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	endpoint, err := url.Parse(origin.URL + "/mcp")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint.String(), strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer OPERATOR-TOKEN")

	resp, err := newHTTPClient(nil, endpoint).Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Error("the cross-origin redirect was followed")
	}
	select {
	case got := <-seen:
		t.Fatalf("the redirect target was reached with Authorization=%q", got)
	default:
	}
}
