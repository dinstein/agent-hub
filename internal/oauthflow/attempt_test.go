package oauthflow

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A bare URL list could not diagnose anything: four candidates and a failure
// read the same whether every one 404'd, one answered with a broken document,
// or the first was refused and the rest were never reached. The outcome is
// what separates them.
func TestAttemptsRecordWhatEachCandidateAnswered(t *testing.T) {
	as := newFakeAS(t)
	as.issuerPath = "tenant1"
	// Only the third candidate answers, so the first two are misses.
	as.servedMetadata = map[string]bool{
		"/tenant1/.well-known/openid-configuration": true,
	}
	d := NewDiscoverer(as.client())

	res, err := d.DiscoverFromIssuer(context.Background(), as.issuer())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(res.Attempted) != 3 {
		t.Fatalf("recorded %d attempts, want one per candidate: %+v", len(res.Attempted), res.Attempted)
	}
	want := []string{AttemptNoDocument, AttemptNoDocument, AttemptOK}
	for i, w := range want {
		if res.Attempted[i].Outcome != w {
			t.Errorf("attempt %d (%s) outcome = %q, want %q",
				i, res.Attempted[i].URL, res.Attempted[i].Outcome, w)
		}
		if res.Attempted[i].URL == "" {
			t.Errorf("attempt %d has no URL", i)
		}
	}
}

// A document that parses but cannot drive a flow stops the chain, and the
// operator needs the URL that was wrong. "Unusable" is the outcome that says
// the provider answered — as opposed to a miss, where it did not.
func TestAnUnusableDocumentIsRecordedAsSuchAndCarriedOnTheError(t *testing.T) {
	// A document with neither an authorization nor a token endpoint: it
	// parses, so the chain reaches it, and it cannot drive a flow.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"issuer":"whatever"}`)
	}))
	defer srv.Close()
	d := NewDiscoverer(NewClient(Config{AllowLoopback: true, Timeout: 5 * time.Second}))

	_, err := d.DiscoverFromIssuer(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("an unusable metadata document was accepted")
	}
	var fe *FlowError
	if !errors.As(err, &fe) {
		t.Fatalf("error is not a *FlowError: %v", err)
	}
	// The trace has to survive onto the ERROR: a failed discovery is the case
	// that needs it most, and the status alone never said which URL was wrong.
	if len(fe.Attempted) == 0 {
		t.Fatal("the failing discovery carried no attempt trace")
	}
	last := fe.Attempted[len(fe.Attempted)-1]
	if last.Outcome != AttemptUnusable {
		t.Errorf("last outcome = %q, want %q", last.Outcome, AttemptUnusable)
	}
}

// Every candidate absent is an outcome rather than an error: providers 404
// the forms they do not implement, which is what the synthesized-endpoints
// fallback exists for. The trace must show the misses that led there.
func TestAllCandidatesMissingIsRecordedBeforeTheFallback(t *testing.T) {
	as := newFakeAS(t)
	as.servedMetadata = map[string]bool{} // nothing is served
	d := NewDiscoverer(as.client())

	res, err := d.DiscoverFromIssuer(context.Background(), as.issuer())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if res.Status != DiscoveryDefaults {
		t.Fatalf("status = %s, want %s", res.Status, DiscoveryDefaults)
	}
	if len(res.Attempted) == 0 {
		t.Fatal("the fallback recorded no attempts, so nothing explains why it was taken")
	}
	for _, a := range res.Attempted {
		if a.Outcome != AttemptNoDocument {
			t.Errorf("%s: outcome = %q, want every candidate to be a miss", a.URL, a.Outcome)
		}
	}
}

func TestAttemptURLsRendersTheChainForMessages(t *testing.T) {
	got := attemptURLs([]Attempt{
		{URL: "https://a/.well-known/x", Outcome: AttemptNoDocument},
		{URL: "https://b/.well-known/y", Outcome: AttemptOK},
	})
	if len(got) != 2 || got[0] != "https://a/.well-known/x" || got[1] != "https://b/.well-known/y" {
		t.Fatalf("attemptURLs = %q", got)
	}
	if attemptURLs(nil) == nil {
		t.Fatal("attemptURLs(nil) must return an empty slice, never nil: it is joined into a message")
	}
}

// A protected-resource document that lists no authorization_servers ends the
// chain, and the walk that reached it is what says where it came from: the URL
// the 401 advertised, or one of the forms guessed after it. Every other
// discovery failure carries the trace; this branch was the one that did not,
// so the status arrived with nothing under it.
func TestANoAuthorizationServersErrorCarriesTheChain(t *testing.T) {
	// The path-scoped candidate 404s and the origin-root one answers, so a
	// trace that survives has more than one entry — a single-element list
	// would pass while still losing the walk.
	const served = "/.well-known/oauth-protected-resource"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != served {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"resource":"https://rs.example/mcp"}`)
	}))
	defer srv.Close()
	d := NewDiscoverer(NewClient(Config{AllowLoopback: true, Timeout: 5 * time.Second}))

	_, err := d.DiscoverFromResource(context.Background(), srv.URL+"/mcp", "")
	if err == nil {
		t.Fatal("a protected-resource document with no authorization_servers was accepted")
	}
	var fe *FlowError
	if !errors.As(err, &fe) {
		t.Fatalf("error is not a FlowError: %v", err)
	}
	if fe.Discovery != DiscoveryProtected {
		t.Fatalf("discovery status = %q, want %q", fe.Discovery, DiscoveryProtected)
	}
	if len(fe.Attempted) < 2 {
		t.Fatalf("the error carries %d attempts, so the walk that found the document was lost: %+v",
			len(fe.Attempted), fe.Attempted)
	}
	last := fe.Attempted[len(fe.Attempted)-1]
	if last.Outcome != AttemptOK {
		t.Errorf("the candidate that answered is recorded as %q, want %q", last.Outcome, AttemptOK)
	}
	if !strings.HasSuffix(last.URL, served) {
		t.Errorf("the answering candidate is %q, want the one that served the document", last.URL)
	}
}
