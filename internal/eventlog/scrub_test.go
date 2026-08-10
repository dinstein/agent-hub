package eventlog

import (
	"os"
	"strings"
	"testing"
)

// TestAppendScrubsDetailBeforePersisting is the regression for the 2026-08-10
// sweep's two-sink asymmetry: Emit's slog twin was scrubbed by logx while
// Append wrote Record.Detail to events.jsonl verbatim, so a credential a
// downstream error or a child's stderr carried persisted in the clear in the
// file `agenthub events` and the GUI serve.
func TestAppendScrubsDetailBeforePersisting(t *testing.T) {
	s, path := openTest(t)

	// A child's stderr / a downstream error echoing a resolved credential —
	// the shapes logx's scrubber is built to catch, the same scrubbing the
	// slog twin already gets.
	const kvSecret = "hunter2SECRETpw"
	const tokenSecret = "ghp_0123456789abcdefghij01"
	s.Append(Record{
		Scope:  ScopeServer,
		Kind:   KindConnectFailed,
		Server: "github",
		Detail: "child stderr: password=" + kvSecret + "; sent Bearer " + tokenSecret,
	})
	s.Sync()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if strings.Contains(got, kvSecret) {
		t.Errorf("password= value persisted in the clear:\n%s", got)
	}
	if strings.Contains(got, tokenSecret) {
		t.Errorf("token persisted in the clear:\n%s", got)
	}

	// The record itself must survive — scrubbing redacts, it does not drop the
	// line — so the diagnostic is still there, minus the secret.
	res, err := Read(path, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 1 || res.Records[0].Kind != KindConnectFailed {
		t.Fatalf("records = %+v, want one connect_failed", res.Records)
	}
	if res.Records[0].Server != "github" {
		t.Errorf("server = %q, want the untouched non-secret field", res.Records[0].Server)
	}
}
