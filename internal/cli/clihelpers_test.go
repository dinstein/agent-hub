package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp/transport"
)

// TestClassifySecretsErrorSeparatesAWrongKeyFromEverythingElse: a vault that
// cannot be decrypted is an AUTH failure with its own exit code, because the
// fix is a different key rather than a retry. Everything else must pass
// through unchanged rather than be relabelled as an auth problem.
func TestClassifySecretsErrorSeparatesAWrongKeyFromEverythingElse(t *testing.T) {
	if got := classifySecretsError(nil); got != nil {
		t.Fatalf("nil classified as %v", got)
	}

	decryptErr := errors.New("vault: cannot decrypt with the configured key")
	var e *Error
	if !errors.As(classifySecretsError(decryptErr), &e) {
		t.Fatalf("a decrypt failure was not classified: %T", classifySecretsError(decryptErr))
	}
	if e.Code != CodeAuthFailed || e.ExitCode != ExitAuth {
		t.Errorf("code/exit = %s/%d, want %s/%d", e.Code, e.ExitCode, CodeAuthFailed, ExitAuth)
	}
	if !errors.Is(e, decryptErr) {
		t.Error("the original error was not wrapped, so the cause is lost")
	}
	if e.Hint == "" {
		t.Error("no hint: the user is not told which key to check")
	}

	other := errors.New("permission denied")
	if got := classifySecretsError(other); !errors.Is(got, other) {
		t.Errorf("an unrelated error was rewritten to %v", got)
	}
}

// TestShortHashKeepsAPrefixAndMarksTheTruncation: pin identifiers are compared
// by eye, so a shortened one must be visibly shortened — otherwise a truncated
// hash reads as a complete, different hash.
func TestShortHashKeepsAPrefixAndMarksTheTruncation(t *testing.T) {
	full := strings.Repeat("a", 64)
	got := shortHash(full)
	if len([]rune(got)) != 17 || !strings.HasSuffix(got, "…") {
		t.Errorf("shortHash(64 chars) = %q, want a 16-char prefix plus an ellipsis", got)
	}
	if !strings.HasPrefix(full, strings.TrimSuffix(got, "…")) {
		t.Errorf("shortHash(%q) = %q is not a prefix of the input", full, got)
	}
	// Short enough to show whole: returned verbatim, with no ellipsis that
	// would imply bytes were dropped.
	for _, s := range []string{"", "abc", strings.Repeat("b", 16)} {
		if got := shortHash(s); got != s {
			t.Errorf("shortHash(%q) = %q, want it unchanged", s, got)
		}
	}
}

// TestBinaryExistsDistinguishesAPathFromAPathLookup: a command containing a
// separator names a FILE and must be checked as one; a bare name is resolved
// through PATH. Treating a path as a PATH lookup would report a perfectly
// valid absolute command as missing.
func TestBinaryExistsDistinguishesAPathFromAPathLookup(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tool")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	if !binaryExists(exe) {
		t.Errorf("an existing absolute path reported missing: %s", exe)
	}
	if binaryExists(filepath.Join(dir, "absent")) {
		t.Error("a non-existent absolute path reported present")
	}
	// A directory is not a runnable command even though it exists.
	if binaryExists(dir) {
		t.Error("a directory reported as a binary")
	}
	// Bare names go through PATH.
	if !binaryExists("go") {
		t.Error("go not found on PATH; the lookup branch is broken")
	}
	if binaryExists("definitely-not-a-real-binary-xyz") {
		t.Error("an unknown bare name reported present")
	}
}

// TestExpiryColumnSeparatesNeverFromExpired: "no expiry" and "expiry already
// passed" are opposite states, and both would render as an empty or zero
// duration if the column just formatted the number.
func TestExpiryColumnSeparatesNeverFromExpired(t *testing.T) {
	if got := expiryColumn(0, 0); got != "never" {
		t.Errorf("no expiry = %q, want never", got)
	}
	if got := expiryColumn(0, 999); got != "never" {
		t.Errorf("expiresAt 0 must win regardless of expiresIn, got %q", got)
	}
	if got := expiryColumn(12345, 0); got != "expired" {
		t.Errorf("elapsed expiry = %q, want expired", got)
	}
	if got := expiryColumn(12345, 90); got != "1m30s" {
		t.Errorf("live expiry = %q, want 1m30s", got)
	}
}

// TestCtlErrorReportsTheServerMessage: ctlError is what the control plane's
// failures arrive as, and Error() is what surfaces when nothing reclassifies
// it. Returning anything but the server's own message would hide the only
// explanation that exists.
func TestCtlErrorReportsTheServerMessage(t *testing.T) {
	e := &ctlError{Status: 409, Code: "E_CONFLICT", Message: "session already closed"}
	if e.Error() != "session already closed" {
		t.Errorf("Error() = %q, want the server message", e.Error())
	}
}

// TestSilentExitErrorStillNamesItsCode: commands that already rendered their
// outcome return this so Main prints nothing more, but the string must remain
// usable in a wrapped chain rather than being empty.
func TestSilentExitErrorStillNamesItsCode(t *testing.T) {
	e := &silentExitError{code: 3}
	if !strings.Contains(e.Error(), "3") {
		t.Errorf("Error() = %q, want it to name exit code 3", e.Error())
	}
	if got := ExitCodeFor(e); got != 3 {
		t.Errorf("ExitCodeFor = %d, want the carried code 3", got)
	}
}

// TestTestConnectErrorClassifiesByStatusNotByMessage: `server test` tells the
// operator to run `auth login` only when the server actually refused the
// CREDENTIAL. It used to decide that by searching the error text for
// "http 401", and the transport folds the response body into that text — so a
// proxy answering 502 with "upstream returned http 401" produced a login
// suggestion for a failure no login can fix.
func TestTestConnectErrorClassifiesByStatusNotByMessage(t *testing.T) {
	authErr := &transport.Error{
		Class: transport.ClassFatal, StatusCode: 401,
		Err: errors.New("POST https://srv/mcp: http 401 unauthorized"),
	}
	var e *Error
	if !errors.As(testConnectError("gh", authErr), &e) {
		t.Fatal("a 401 was not classified")
	}
	if e.Code != CodeAuthFailed || e.ExitCode != ExitAuth {
		t.Errorf("401 gave %s/%d, want %s/%d", e.Code, e.ExitCode, CodeAuthFailed, ExitAuth)
	}
	if !strings.Contains(e.Hint, "auth login") {
		t.Errorf("no login hint on a real credential rejection: %q", e.Hint)
	}

	// The regression: a proxy error whose BODY mentions 401.
	proxyErr := &transport.Error{
		Class: transport.ClassUnavailable, StatusCode: 502,
		Err: errors.New("POST https://srv/mcp: http 502 upstream returned http 401"),
	}
	e = nil
	if !errors.As(testConnectError("gh", proxyErr), &e) {
		t.Fatal("a 502 produced no typed error")
	}
	if e.Code == CodeAuthFailed {
		t.Errorf("a 502 mentioning 401 in its body was reported as a credential rejection: %+v", e)
	}
	if strings.Contains(e.Hint, "auth login") {
		t.Errorf("a 502 suggested a login that cannot fix it: %q", e.Hint)
	}

	// A plain dial failure keeps the generic classification and stays wrapped.
	dial := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	e = nil
	if !errors.As(testConnectError("gh", dial), &e) {
		t.Fatal("a dial failure produced no typed error")
	}
	if e.Code != CodeGeneral || e.ExitCode != ExitGeneral {
		t.Errorf("dial failure gave %s/%d, want the generic classification", e.Code, e.ExitCode)
	}
	if !errors.Is(e, dial) {
		t.Error("the cause was not wrapped")
	}
}
