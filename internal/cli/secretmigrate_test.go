package cli

import (
	"strings"
	"testing"
)

// These tests run with only the enc-file backend available: the OS keyring is
// off-limits to the suite (secret_test.go pins the vault to secrets.enc so no
// test can pop a keychain dialog). The real backend-to-backend round trip is
// therefore covered in internal/secrets, where a fake keyring can be injected
// — TestBackendMigrateRoundTrip and TestBackendIgnoresEnvironmentLevels.
// What belongs HERE is the command surface: flag validation, the failure
// direction of an unavailable backend, and the no-values invariant.

func TestSecretMigrateRequiresBothBackends(t *testing.T) {
	secretEnv(t)
	for _, args := range [][]string{
		{"secret", "migrate", "--json"},
		{"secret", "migrate", "--from", "keyring", "--json"},
		{"secret", "migrate", "--to", "keyring", "--json"},
	} {
		code, out, _ := runCLI(t, "", args...)
		if code != ExitUsage {
			t.Errorf("%v exit = %d, want %d (usage)", args, code, ExitUsage)
		}
		env := decodeEnvelope(t, out)
		if env.Error == nil || !strings.Contains(env.Error.Message, "required") {
			t.Errorf("%v error = %+v, want a 'required' usage error", args, env.Error)
		}
	}
}

// An unknown backend must be REJECTED, never quietly resolved to a default:
// the spellings are frozen CLI surface, and guessing which backend an
// operator meant is how credentials move somewhere they did not intend.
func TestSecretMigrateRejectsUnknownBackend(t *testing.T) {
	secretEnv(t)
	code, out, _ := runCLI(t, "", "secret", "migrate", "--from", "env", "--to", "enc-file", "--json")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, ExitUsage)
	}
	env := decodeEnvelope(t, out)
	if env.Error == nil || !strings.Contains(env.Error.Message, "unknown backend") {
		t.Fatalf("error = %+v, want an unknown-backend usage error", env.Error)
	}
	// The message must list what IS accepted; "env" is a plausible guess
	// precisely because environment variables do resolve secrets.
	if !strings.Contains(env.Error.Message, "keyring") {
		t.Errorf("error message does not list the valid backends: %q", env.Error.Message)
	}
}

func TestSecretMigrateRejectsSameBackend(t *testing.T) {
	secretEnv(t)
	code, out, _ := runCLI(t, "", "secret", "migrate", "--from", "enc-file", "--to", "enc-file", "--json")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, ExitUsage)
	}
	env := decodeEnvelope(t, out)
	if env.Error == nil || !strings.Contains(env.Error.Message, "nothing to migrate") {
		t.Errorf("error = %+v, want a same-backend usage error", env.Error)
	}
}

// TestSecretMigrateNeverPrintsValues is the group-wide invariant from
// secret.go: no command here prints a secret VALUE. Migration is the command
// most likely to break it by accident, since it holds plaintext in memory to
// verify the copy.
func TestSecretMigrateNeverPrintsValues(t *testing.T) {
	secretEnv(t)
	mustRun(t, theSecret+"\n", "secret", "set", "github", "GITHUB_TOKEN", "--stdin")

	// A dry run against the only available backend as the SOURCE: it lists
	// the refs that would move without touching either side.
	_, out, errOut := runCLI(t, "", "secret", "migrate",
		"--from", "enc-file", "--to", "keyring", "--dry-run")
	if strings.Contains(out, theSecret) || strings.Contains(errOut, theSecret) {
		t.Fatal("secret migrate leaked the credential value into its output")
	}
	// Whatever the outcome, the KEY name may appear — that is what the
	// operator needs to see. Only the value is forbidden.
	_ = out
}

// TestSecretMigrateDryRunListsWithoutMoving: --dry-run must leave both
// backends untouched, so the operator can see the blast radius first.
func TestSecretMigrateDryRunListsWithoutMoving(t *testing.T) {
	secretEnv(t)
	mustRun(t, theSecret+"\n", "secret", "set", "github", "GITHUB_TOKEN", "--stdin")

	// enc-file → enc-file is rejected, so use enc-file as the destination and
	// let the source resolution decide. When the keyring is unavailable the
	// command must fail with guidance rather than half-migrate.
	code, out, _ := runCLI(t, "", "secret", "migrate",
		"--from", "keyring", "--to", "enc-file", "--dry-run", "--json")
	if code == ExitOK {
		// A machine with a working keyring: the dry run must report that
		// nothing changed, and the secret must still be listed afterwards.
		var res SecretMigrateResult
		decodeInto(t, out, &res)
		if !res.DryRun {
			t.Error("dry_run flag missing from a --dry-run result")
		}
	} else {
		// No keyring here: the failure must name the backend and guide,
		// never leave a partial migration behind.
		env := decodeEnvelope(t, out)
		if env.Error == nil || !strings.Contains(env.Error.Message, "keyring") {
			t.Fatalf("error = %+v, want an unavailable-keyring message", env.Error)
		}
	}

	// Either way the credential is still exactly where it was.
	var list SecretList
	decodeInto(t, mustRun(t, "", "secret", "ls", "--json"), &list)
	if len(list.Secrets) != 1 || list.Secrets[0].Key != "GITHUB_TOKEN" {
		t.Errorf("ls after a dry run = %+v, want the untouched entry", list.Secrets)
	}
}
