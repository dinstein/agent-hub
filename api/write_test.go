package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
)

// capture records exactly what the client put on the wire for one call.
type capture struct {
	method string
	path   string
	query  string
	body   []byte
}

// newCapturingDaemon serves any path over UDS, records the request and
// answers with a success envelope carrying data.
func newCapturingDaemon(t *testing.T, data any) (*capture, *Client) {
	t.Helper()
	var got capture
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath, not Path: the daemon matches ids on the escaped
		// path so a %2F cannot smuggle an extra segment, and a client that
		// failed to escape one would be invisible in the decoded form.
		got.method, got.path, got.query = r.Method, r.URL.EscapedPath(), r.URL.RawQuery
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading body: %v", err)
		}
		got.body = b
		writeOK(t, w, data)
	})
	c := New(newTestDaemon(t, mux))
	t.Cleanup(c.Close)
	return &got, c
}

// newFailingDaemon answers every request with the given status and error
// body (raw JSON), so the client's error mapping can be tested verbatim.
func newFailingDaemon(t *testing.T, status int, errBody string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderRequestID, r.Header.Get(HeaderRequestID))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":false,"error":` + errBody + `}`))
	})
	c := New(newTestDaemon(t, mux))
	t.Cleanup(c.Close)
	return c
}

// TestPreconditionOnTheWire: expectedGeneration travels as the
// expected_generation query parameter, and a ZERO generation sends nothing —
// "do not check" must be spelled by absence, never by the literal 0.
func TestPreconditionOnTheWire(t *testing.T) {
	got, c := newCapturingDaemon(t, ServerWrite{})

	if _, err := c.Servers.Delete(context.Background(), "github", 42); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got.query != "expected_generation=42" {
		t.Errorf("query = %q, want expected_generation=42", got.query)
	}
	if got.method != http.MethodDelete || got.path != "/v1/servers/github" {
		t.Errorf("wire = %s %s, want DELETE /v1/servers/github", got.method, got.path)
	}

	if _, err := c.Servers.Delete(context.Background(), "github", 0); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got.query != "" {
		t.Errorf("generation 0 must send no parameter, got %q", got.query)
	}
}

// TestStalePreconditionBecomesConflictError: a 409 E_STALE_PRECONDITION is
// the one refusal a caller answers with "re-read and retry", so it must
// arrive typed and carrying the generation to retry against.
func TestStalePreconditionBecomesConflictError(t *testing.T) {
	c := newFailingDaemon(t, http.StatusConflict, `{
		"code":"E_STALE_PRECONDITION",
		"message":"the configuration changed since it was read (it is now at generation 9)",
		"hint":"re-read the configuration and retry against the current generation",
		"generation":9}`)

	_, err := c.Servers.SetEnabled(context.Background(), "github", false, 7)
	conflict, ok := AsConflict(err)
	if !ok {
		t.Fatalf("want *ConflictError, got %T: %v", err, err)
	}
	if !IsConflict(err) {
		t.Error("IsConflict must report true for a stale precondition")
	}
	if conflict.CurrentGeneration != 9 {
		t.Errorf("CurrentGeneration = %d, want 9", conflict.CurrentGeneration)
	}
	// The daemon's body still passes through verbatim underneath.
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatal("a ConflictError must unwrap to the *Error it was built from")
	}
	if apiErr.Code != ErrCodeStalePrecondition || apiErr.Hint == "" {
		t.Errorf("error body not passed through: %+v", apiErr)
	}
	if !IsCode(err, ErrCodeStalePrecondition) {
		t.Error("IsCode must still match through the conflict wrapper")
	}
}

// TestNonStaleConflictsAreNotConflictErrors is the anti-retry-loop test: 409
// alone does not mean "your view was stale". A duplicate name or a drifted
// skill target answers 409 too, and re-reading fixes neither — a frontend
// that retried on them would loop forever.
func TestNonStaleConflictsAreNotConflictErrors(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"duplicate_server_name", "E_SERVER_EXISTS"},
		{"duplicate_profile_name", "E_PROFILE_EXISTS"},
		{"skill_target_drifted", ErrCodeConflict},
		{"duplicate_token_name", "E_TOKEN_EXISTS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newFailingDaemon(t, http.StatusConflict,
				`{"code":"`+tc.code+`","message":"already taken"}`)
			_, err := c.Servers.Create(context.Background(), ServerSpec{ID: "x"}, 3)
			if IsConflict(err) {
				t.Errorf("%s must NOT be reported as a stale precondition", tc.code)
			}
			if !IsCode(err, tc.code) {
				t.Errorf("code not passed through: %v", err)
			}
		})
	}
}

// TestConflictErrorMessage keeps the operator-facing string honest in both
// the reported and the unreported-generation case.
func TestConflictErrorMessage(t *testing.T) {
	base := &Error{ErrorBody: ErrorBody{Code: ErrCodeStalePrecondition, Message: "changed"}}
	with := &ConflictError{Err: base, CurrentGeneration: 4}
	if got, want := with.Error(),
		"agenthub api: E_STALE_PRECONDITION: changed (registry is now at generation 4; re-read and retry)"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	without := &ConflictError{Err: base}
	if got, want := without.Error(),
		"agenthub api: E_STALE_PRECONDITION: changed (registry generation moved; re-read and retry)"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// TestErrorBodyGenerationIgnoredOnOtherErrors: the generation field is
// meaningful ONLY on a stale precondition. A daemon that sets it elsewhere
// must not make a caller believe it knows the current generation.
func TestErrorBodyGenerationIgnoredOnOtherErrors(t *testing.T) {
	c := newFailingDaemon(t, http.StatusBadRequest,
		`{"code":"E_BAD_REQUEST","message":"nope","generation":12}`)
	_, err := c.Servers.Create(context.Background(), ServerSpec{ID: "x"}, 1)
	if IsConflict(err) {
		t.Fatal("a 400 is never a conflict")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *Error, got %T", err)
	}
	if apiErr.Generation != 12 {
		t.Errorf("Generation = %d, want the body value 12 (carried, not interpreted)", apiErr.Generation)
	}
}

// TestWriteResultDecoding pins the {generation, changed, warnings} tail every
// mutation answers with — including the warnings, which report fail-closed
// side effects a frontend must show rather than swallow.
func TestWriteResultDecoding(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/profiles/{name}", func(w http.ResponseWriter, r *http.Request) {
		writeOK(t, w, json.RawMessage(`{
			"generation":11,"changed":true,
			"warnings":["client \"claude\" still references profile \"dev\"; it now resolves to an EMPTY scope"],
			"name":"dev","dangling":["claude"],"deleted":true}`))
	})
	c := New(newTestDaemon(t, mux))
	defer c.Close()

	out, err := c.Profiles.Delete(context.Background(), "dev", 10)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if out.Generation != 11 || !out.Changed || !out.Deleted {
		t.Errorf("write tail not decoded: %+v", out)
	}
	if len(out.Warnings) != 1 {
		t.Fatalf("warnings must survive a successful write, got %v", out.Warnings)
	}
	if len(out.Dangling) != 1 || out.Dangling[0] != "claude" {
		t.Errorf("dangling clients must be reported: %v", out.Dangling)
	}
}
