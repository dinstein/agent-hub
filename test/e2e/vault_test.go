package e2e_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// A downstream credential can reach agenthub by four routes, and until this
// file the suite exercised exactly one of them: TestHTTPDownstreamSecretHeader
// sets AGENTHUB_SECRET_<KEY> in the child environment, which is level 1 of the
// chain and the level that involves no vault at all.
//
// The level that matters on a real installation is the one an operator gets
// from `agenthub secret set`: a value they typed once, sealed on disk, read
// back by a DIFFERENT process at connect time. Three things have to agree for
// that to work — the composite (server, scope, key) the writer used, the key
// the placeholder names, and the enc-file key material both processes derive —
// and each lives in a different package. A single process cannot disagree with
// itself about any of them.
//
// AGENTHUB_SECRET_KEY is what pins the backend here. With it set, both the read
// and the write path resolve to secrets.enc unconditionally
// (Chain.encForRead/encForWrite) and the OS keyring is never consulted, which
// is also what keeps this test off a developer's real macOS keychain.

// vaultEnv is the child environment with the encrypted vault active.
func vaultEnv(dataDir string) []string {
	return append(testEnv(dataDir), "AGENTHUB_SECRET_KEY=e2e-vault-passphrase")
}

// findFile walks root for the first file named name, or "" if there is none.
// The vault's directory is an internal layout detail; what this test asserts
// is about the file's CONTENT, so locating it is worth a walk and hardcoding
// the path is not.
func findFile(t *testing.T, root, name string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil //nolint:nilerr // an unreadable subtree is not this test's subject
		}
		if !d.IsDir() && d.Name() == name {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return found
}

// TestVaultResolvesADownstreamCredentialForALiveGateway is the vault's
// end-to-end path: `secret set` writes, a spawned gateway reads, and the
// downstream sees the value.
//
// The gateway is the point. `server test` would be cheaper and would resolve
// the placeholder inside the CLI process that has just written it — the one
// assembly where the write and the read cannot disagree. The connection a
// client actually uses is made by a different process from a different
// assembly, and that is where a resolver left unwired shows up.
func TestVaultResolvesADownstreamCredentialForALiveGateway(t *testing.T) {
	dataDir := t.TempDir()
	env := vaultEnv(dataDir)
	const value = "vaulted-bearer-value"
	fake := newFakeHTTPServerAuth(t, fakemcp.Minimal(), value)

	// The registry holds the placeholder and never the value; the connector
	// refuses a loopback URL without --local.
	runAgenthubEnv(t, env, "", "server", "add", "remote",
		"--url", fake.url(), "--transport", "http", "--local",
		"--header", "Authorization=Bearer ${SECRET_REMOTE_TOKEN}", "--json")
	runAgenthubEnv(t, env, "", "server", "enable", "remote", "--no-probe")
	runAgenthubEnv(t, env, "", "config", "set", "discovery", "full")

	// The value arrives on stdin, which is the form a CI pipeline uses; the
	// terminal form reads it without echo and cannot be driven from here.
	runAgenthubEnv(t, env, value, "secret", "set", "remote", "REMOTE_TOKEN", "--stdin")

	// Sealed at rest. This is cheap and it is the claim the whole backend is
	// for: an enc file with the plaintext in it would satisfy every other
	// assertion in this test.
	encPath := findFile(t, dataDir, "secrets.enc")
	if encPath == "" {
		t.Fatal("no secrets.enc was written; the value did not reach the encrypted backend")
	}
	sealed, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatalf("reading %s: %v", encPath, err)
	}
	if bytes.Contains(sealed, []byte(value)) {
		t.Fatalf("%s holds the credential in plaintext", encPath)
	}
	// Nor may it have leaked sideways into the registry, which is plain
	// configuration and is copied around, diffed and pasted into issues.
	if raw := findFile(t, dataDir, "servers.json"); raw != "" {
		data, err := os.ReadFile(raw)
		if err != nil {
			t.Fatalf("reading %s: %v", raw, err)
		}
		if bytes.Contains(data, []byte(value)) {
			t.Fatalf("%s holds the credential", raw)
		}
		if !bytes.Contains(data, []byte("${SECRET_REMOTE_TOKEN}")) {
			t.Fatalf("%s no longer carries the placeholder verbatim", raw)
		}
	}

	// `secret ls` names the entry and its backend, and never the value.
	out, _ := runAgenthubEnv(t, env, "", "secret", "ls", "--json")
	e := lastEnvelope(t, out)
	if !e.OK {
		t.Fatalf("secret ls: %s", out)
	}
	var list struct {
		Secrets []struct {
			Server  string `json:"server"`
			Key     string `json:"key"`
			Backend string `json:"backend"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(e.Data, &list); err != nil {
		t.Fatalf("secret ls data: %v\n%s", err, e.Data)
	}
	if strings.Contains(out, value) {
		t.Fatalf("secret ls printed the value: %s", out)
	}
	var seen bool
	for _, row := range list.Secrets {
		if row.Server == "remote" && row.Key == "REMOTE_TOKEN" {
			seen = true
			if row.Backend != "enc-file" {
				t.Fatalf("the value went to the %q backend, not the encrypted file", row.Backend)
			}
		}
	}
	if !seen {
		t.Fatalf("the stored entry is not in `secret ls`: %+v", list.Secrets)
	}

	// And now the read, from a process that did none of the above. The fake
	// answers only a request carrying the exact bearer value, so a tool result
	// is the proof — there is no weaker way for this call to succeed.
	c := startGatewayEnv(t, env, "vaultclient")
	c.initialize()
	c.waitForTool("remote__echo", 45*time.Second)
	if got := c.textContent(c.callTool("remote__echo", map[string]any{"marker": "from-the-vault"}, 45*time.Second)); !strings.Contains(got, "from-the-vault") {
		t.Fatalf("the downstream did not answer through the vaulted credential: %q", got)
	}
	c.close()
}

// TestVaultWithoutItsKeyFailsClosed is the other direction: the sealed file is
// still there and the key that opens it is not.
//
// The failure has to be a refusal to connect, not a request carrying the
// literal placeholder text. Sending "${SECRET_REMOTE_TOKEN}" as an
// Authorization header would reach the downstream, be rejected by it, and be
// reported as an authentication problem at the far end — sending an operator
// to the provider's dashboard for a key they never lost.
func TestVaultWithoutItsKeyFailsClosed(t *testing.T) {
	dataDir := t.TempDir()
	env := vaultEnv(dataDir)
	const value = "vaulted-bearer-value"
	fake := newFakeHTTPServerAuth(t, fakemcp.Minimal(), value)

	runAgenthubEnv(t, env, "", "server", "add", "remote",
		"--url", fake.url(), "--transport", "http", "--local",
		"--header", "Authorization=Bearer ${SECRET_REMOTE_TOKEN}", "--json")
	runAgenthubEnv(t, env, value, "secret", "set", "remote", "REMOTE_TOKEN", "--stdin")

	// It works with the key: without this the negative below would pass just
	// as happily against a fixture that was never set up.
	if out, _ := runAgenthubEnv(t, env, "", "server", "test", "remote", "--json"); !lastEnvelope(t, out).OK {
		t.Fatalf("the vaulted credential does not work even with its key: %s", out)
	}

	before := fake.calls.Load()
	code, out := runAgenthubExit(t, dataDir, "", "server", "test", "remote", "--json")
	if code == 0 {
		t.Fatalf("a server whose credential cannot be unsealed connected anyway: %s", out)
	}
	if fake.calls.Load() != before {
		t.Fatal("a request went out while the credential was unresolvable")
	}
}
