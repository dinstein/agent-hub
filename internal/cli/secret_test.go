package cli

import (
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/secrets"
)

// secretEnv pins the vault to the encrypted file so no test touches the
// real OS keychain (and no test can pop a confirmation dialog).
func secretEnv(t *testing.T) string {
	t.Helper()
	dir := setDataDir(t)
	t.Setenv(secrets.EnvEncKey, "test-passphrase-for-cli-secrets")
	return dir
}

// theSecret is the sentinel value: no output of any command may contain it.
const theSecret = "sk-live-DO-NOT-ECHO-4f2b9c"

func TestSecretRoundTrip(t *testing.T) {
	secretEnv(t)

	var change SecretChange
	decodeInto(t, mustRun(t, theSecret+"\n", "secret", "set", "github", "GITHUB_TOKEN", "--stdin", "--json"), &change)
	if change.Action != "stored" || change.Scope != secrets.DefaultScope {
		t.Errorf("set result = %+v", change)
	}

	var list SecretList
	decodeInto(t, mustRun(t, "", "secret", "ls", "--json"), &list)
	if len(list.Secrets) != 1 {
		t.Fatalf("ls = %+v, want one entry", list.Secrets)
	}
	row := list.Secrets[0]
	if row.Server != "github" || row.Key != "GITHUB_TOKEN" || !row.Set {
		t.Errorf("row = %+v", row)
	}
	if row.Backend != "enc-file" {
		t.Errorf("backend = %q, want enc-file (AGENTHUB_SECRET_KEY is set)", row.Backend)
	}

	// Filtering by server.
	decodeInto(t, mustRun(t, "", "secret", "ls", "other", "--json"), &list)
	if len(list.Secrets) != 0 {
		t.Errorf("ls other = %+v, want empty", list.Secrets)
	}

	mustRun(t, "", "secret", "rm", "github", "GITHUB_TOKEN")
	decodeInto(t, mustRun(t, "", "secret", "ls", "--json"), &list)
	if len(list.Secrets) != 0 {
		t.Errorf("secret survived rm: %+v", list.Secrets)
	}
}

// TestSecretIsNeverEchoed is the docs/modules/controlplane.md rule-5 guard: there is no
// command, mode or flag that puts a stored value back on a stream. It runs
// every reading surface and asserts the sentinel never appears.
func TestSecretIsNeverEchoed(t *testing.T) {
	secretEnv(t)
	mustRun(t, theSecret+"\n", "secret", "set", "github", "GITHUB_TOKEN", "--stdin")
	mustRun(t, "", "server", "add", "github", "--cmd", "gh-mcp", "--env", "GITHUB_TOKEN=${GITHUB_TOKEN}")

	surfaces := [][]string{
		{"secret", "ls"},
		{"secret", "ls", "--json"},
		{"secret", "ls", "github", "--json"},
		{"secret", "set", "github", "GITHUB_TOKEN", "--stdin", "--json"},
		{"secret", "rm", "github", "GITHUB_TOKEN", "--json"},
		{"server", "ls", "--json"},
		{"server", "inspect", "github", "--json"},
		{"doctor", "--json"},
	}
	for _, args := range surfaces {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, out, errOut := runCLI(t, theSecret+"\n", args...)
			if strings.Contains(out, theSecret) {
				t.Errorf("stdout of %v leaked the secret value:\n%s", args, out)
			}
			if strings.Contains(errOut, theSecret) {
				t.Errorf("stderr of %v leaked the secret value:\n%s", args, errOut)
			}
		})
	}

	// And there is no escape hatch to add one to.
	for _, flag := range []string{"--show", "--reveal", "--value", "--plaintext"} {
		if code, _, _ := runCLI(t, "", "secret", "ls", flag); code != ExitUsage {
			t.Errorf("secret ls %s exit = %d, want %d (no reveal flag may exist)", flag, code, ExitUsage)
		}
	}
}

// TestSecretSetRefusesEmptyAndNonTTY: an empty value is refused (it would
// look "set" while resolving to nothing), and a non-terminal stdin without
// --stdin is a usage error rather than an echoing read.
func TestSecretSetRefusesEmptyAndNonTTY(t *testing.T) {
	secretEnv(t)
	if code, _, _ := runCLI(t, "\n", "secret", "set", "github", "TOKEN", "--stdin"); code != ExitUsage {
		t.Errorf("empty value exit = %d, want %d", code, ExitUsage)
	}
	code, _, stderr := runCLI(t, "value", "secret", "set", "github", "TOKEN")
	if code != ExitUsage {
		t.Errorf("non-terminal stdin without --stdin exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "--stdin") {
		t.Errorf("the error must point at --stdin, got %q", stderr)
	}
}

// TestSecretScopeIsComposite pins the (serverID, scope) vault key: two
// scopes of the same server are two independent entries.
func TestSecretScopeIsComposite(t *testing.T) {
	secretEnv(t)
	mustRun(t, "a\n", "secret", "set", "github", "TOKEN", "--stdin")
	mustRun(t, "b\n", "secret", "set", "github", "TOKEN", "--stdin", "--scope", "team")

	var list SecretList
	decodeInto(t, mustRun(t, "", "secret", "ls", "github", "--json"), &list)
	if len(list.Secrets) != 2 {
		t.Fatalf("ls = %+v, want two scoped entries", list.Secrets)
	}
	scopes := map[string]bool{}
	for _, s := range list.Secrets {
		scopes[s.Scope] = true
	}
	if !scopes[secrets.DefaultScope] || !scopes["team"] {
		t.Errorf("scopes = %v, want _global and team", scopes)
	}
}

// TestServerRmPurgesCredentials is the end-to-end guard for the default:
// `server rm` removes the server's stored credentials, through a real vault
// and a real registry rather than a fake.
//
// The regression it pins had two faces. The obvious one is a refresh token
// left in the vault after the operator believed they had cleaned up. The
// subtler one is revival: the vault is keyed by server id, so re-adding the
// same id later would silently reuse the old credentials — against what may
// be an entirely different provider.
func TestServerRmPurgesCredentials(t *testing.T) {
	secretEnv(t)
	mustRun(t, "", "server", "add", "gone", "--cmd", "gone-mcp")
	mustRun(t, "", "server", "add", "stays", "--cmd", "stays-mcp")
	mustRun(t, theSecret+"\n", "secret", "set", "gone", "TOKEN", "--stdin")
	mustRun(t, theSecret+"\n", "secret", "set", "stays", "TOKEN", "--stdin")

	mustRun(t, "", "server", "rm", "gone")

	var list SecretList
	decodeInto(t, mustRun(t, "", "secret", "ls", "--json"), &list)
	for _, row := range list.Secrets {
		if row.Server == "gone" {
			t.Errorf("credential survived server rm: %+v", row)
		}
	}
	// The neighbour's credential must be untouched: the purge is scoped to
	// one server id, not "everything that looks unused".
	var kept bool
	for _, row := range list.Secrets {
		if row.Server == "stays" && row.Key == "TOKEN" {
			kept = true
		}
	}
	if !kept {
		t.Errorf("the purge crossed into another server: %+v", list.Secrets)
	}
}

// TestServerRmWarnsWhenTheVaultIsUnreadable pins the loud failure of the one
// case where a purge CANNOT be complete.
//
// The setup is not exotic, which is the point: `secret set` writes to
// secrets.enc whenever AGENTHUB_SECRET_KEY is set, but the vault listing can
// only see that file when the same key is present. Removing the server from a
// shell without the key therefore enumerates nothing and deletes nothing.
// Before this warning existed that path reported a clean removal over a
// surviving refresh token — and because the vault is keyed by server id,
// re-adding the id later revived it against a possibly different provider.
//
// The credential still surviving is expected here (nothing can decrypt it);
// what is asserted is that the operator is TOLD, and told how to finish.
func TestServerRmWarnsWhenTheVaultIsUnreadable(t *testing.T) {
	secretEnv(t)
	mustRun(t, "", "server", "add", "gone", "--cmd", "gone-mcp")
	mustRun(t, theSecret+"\n", "secret", "set", "gone", "TOKEN", "--stdin")

	// The same operator, a shell without the passphrase.
	t.Setenv(secrets.EnvEncKey, "")

	_, stdout, stderr := runCLI(t, "", "server", "rm", "gone")
	out := stdout + stderr
	if !strings.Contains(out, "secrets.enc") {
		t.Errorf("a purge that could not read the vault stayed silent: %q", out)
	}
	if !strings.Contains(out, secrets.EnvEncKey) && !strings.Contains(out, "auth logout") {
		t.Errorf("the warning does not say how to finish the cleanup: %q", out)
	}
}
