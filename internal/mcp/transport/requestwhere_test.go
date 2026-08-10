package transport

import (
	"net/http"
	"strings"
	"testing"
)

// TestRequestWhereDropsQueryAndUserinfo is the regression for the 2026-08-10
// sweep's finding that httpError's "where" kept the URL query. url.URL.Redacted
// masks only the userinfo password, so a legacy-SSE ?sessionId= or a
// configured ?api_key= reached the error string, and from there Warn logs and
// trace frames.
func TestRequestWhereDropsQueryAndUserinfo(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost,
		"https://user:sekritPW@svc.example/mcp/post?sessionId=SESSIONSECRET&api_key=KEYSECRET", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := requestWhere(req)

	for _, secret := range []string{"SESSIONSECRET", "KEYSECRET", "sekritPW", "sessionId", "?"} {
		if strings.Contains(got, secret) {
			t.Errorf("requestWhere leaked %q: %s", secret, got)
		}
	}
	if want := "POST https://svc.example/mcp/post"; got != want {
		t.Errorf("requestWhere = %q, want %q", got, want)
	}
}
