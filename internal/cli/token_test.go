package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/httpbridge"
)

// createToken runs `token create` and returns the decoded result.
func createToken(t *testing.T, args ...string) TokenCreated {
	t.Helper()
	code, out, errOut := runCLI(t, "", append([]string{"token", "create", "--json"}, args...)...)
	if code != ExitOK {
		t.Fatalf("token create: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	var got TokenCreated
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &got); err != nil {
		t.Fatalf("decode create result: %v (%s)", err, out)
	}
	return got
}

// The value is printed exactly once, and it is the only place it ever
// appears: `ls` must show the prefix and nothing more.
func TestTokenCreateShowsTheValueOnceAndLsNeverDoes(t *testing.T) {
	dir := setDataDir(t)
	created := createToken(t, "ci", "--tier", "write")

	if !strings.HasPrefix(created.Value, httpbridge.TokenPrefix) {
		t.Fatalf("value %q is not an agent token", created.Value)
	}
	if created.Token.Tier != "write" || created.Token.State != "active" {
		t.Errorf("created token = %+v, want tier write / state active", created.Token)
	}

	code, out, _ := runCLI(t, "", "token", "ls", "--json")
	if code != ExitOK {
		t.Fatalf("token ls: exit %d (%s)", code, out)
	}
	if strings.Contains(out, created.Value) {
		t.Fatal("token ls printed the token value")
	}
	var list TokenList
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tokens) != 1 || list.Tokens[0].Name != "ci" {
		t.Fatalf("token ls = %+v, want the one token", list.Tokens)
	}
	if list.Tokens[0].Prefix != created.Value[:httpbridge.DisplayPrefixLen] {
		t.Errorf("prefix = %q, want the display prefix of the value", list.Tokens[0].Prefix)
	}

	// The human rendering is bound by the same rule.
	_, human, _ := runCLI(t, "", "token", "ls")
	if strings.Contains(human, created.Value) {
		t.Fatal("human token ls printed the token value")
	}
	if !strings.Contains(human, "never recoverable") {
		t.Errorf("human ls does not state the one-shot rule: %s", human)
	}

	// The store lives under the data dir, and the key beside it.
	for _, name := range []string{httpbridge.TokensFileName, httpbridge.KeyFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s under the data dir: %v", name, err)
		}
	}
}

func TestTokenRevokeThroughTheCLI(t *testing.T) {
	setDataDir(t)
	createToken(t, "agent", "--tier", "read")

	code, out, _ := runCLI(t, "", "token", "revoke", "agent", "--json")
	if code != ExitOK {
		t.Fatalf("token revoke: exit %d (%s)", code, out)
	}
	var revoked TokenRevoked
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &revoked); err != nil {
		t.Fatal(err)
	}
	if revoked.Name != "agent" || revoked.RevokedAt.IsZero() {
		t.Fatalf("revoke result = %+v", revoked)
	}

	_, lsOut, _ := runCLI(t, "", "token", "ls", "--json")
	var list TokenList
	if err := json.Unmarshal(decodeEnvelope(t, lsOut).Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tokens) != 1 || list.Tokens[0].State != "revoked" {
		t.Fatalf("ls after revoke = %+v, want one revoked row", list.Tokens)
	}

	// Revoking an unknown name is a not-found, not a generic failure.
	code, _, _ = runCLI(t, "", "token", "revoke", "nope")
	if code != ExitNotFound {
		t.Errorf("revoke of an unknown token: exit %d, want %d", code, ExitNotFound)
	}
}

func TestTokenCreateValidationExitCodes(t *testing.T) {
	setDataDir(t)
	createToken(t, "taken", "--tier", "read")

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"duplicate name", []string{"taken", "--tier", "read"}, ExitUsage},
		{"bad tier", []string{"other", "--tier", "root"}, ExitUsage},
		{"bad name", []string{"has space", "--tier", "read"}, ExitUsage},
	}
	for _, tc := range cases {
		code, out, _ := runCLI(t, "", append([]string{"token", "create", "--json"}, tc.args...)...)
		if code != tc.want {
			t.Errorf("%s: exit %d, want %d (%s)", tc.name, code, tc.want, out)
		}
	}
}

// The allowlist's nil-vs-empty three-state must survive the flag layer: an
// omitted --server means "every server", never "none".
func TestTokenServerFlagThreeState(t *testing.T) {
	setDataDir(t)
	all := createToken(t, "all", "--tier", "read")
	if all.Token.Servers != nil {
		t.Errorf("omitted --server produced %v, want null (every server)", all.Token.Servers)
	}
	narrow := createToken(t, "narrow", "--tier", "read", "--server", "git", "--server", "fs")
	if len(narrow.Token.Servers) != 2 {
		t.Errorf("--server produced %v, want two entries", narrow.Token.Servers)
	}
	pinned := createToken(t, "pinned", "--tier", "read", "--profile", "readonly")
	if pinned.Token.Profile != "readonly" {
		t.Errorf("profile pin = %q, want readonly", pinned.Token.Profile)
	}
}

// docs/conventions.md#command-naming: singular canonical name, plural alias, `ls` for lists.
func TestTokenGroupNaming(t *testing.T) {
	setDataDir(t)
	if code, _, _ := runCLI(t, "", "tokens", "ls", "--json"); code != ExitOK {
		t.Errorf("the plural alias does not resolve")
	}
	if code, _, _ := runCLI(t, "", "token", "ls", "--json"); code != ExitOK {
		t.Errorf("the singular canonical name does not resolve")
	}
}

// A token minted by the CLI must authenticate against the store the daemon
// opens — same directory, same key, no second convention.
func TestCLIMintedTokenAuthenticatesAgainstTheStore(t *testing.T) {
	dir := setDataDir(t)
	created := createToken(t, "agent", "--tier", "destructive")

	store, err := httpbridge.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tok, ok, err := store.Lookup(created.Value, time.Now())
	if err != nil || !ok {
		t.Fatalf("Lookup = (%+v, %v, %v), want a match", tok, ok, err)
	}
	if tok.Name != "agent" || string(tok.Tier) != "destructive" {
		t.Fatalf("looked-up token = %+v", tok)
	}
}
