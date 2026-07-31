package cli

import (
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/secrets"
)

// TestBrowserEnvCarriesNoCredential is the regression for an opener that
// inherited the CLI's complete environment.
//
// `auth login` runs with AGENTHUB_SECRET_KEY, the AGENTHUB_SECRET_* values
// and whatever bare secret variables the operator opted in. The browser is
// the one child this process starts that it does not control: the handler
// launches the real browser, the browser launches whatever it likes, and
// every one of them could read the lot out of its own environment.
func TestBrowserEnvCarriesNoCredential(t *testing.T) {
	// Deliberately not parallel: t.Setenv.
	t.Setenv(secrets.EnvEncKey, "key-material")
	t.Setenv(secrets.EnvSecretPrefix+"GITHUB_TOKEN", "ghp_live")
	// A bare opted-in variable: no prefix identifies it, which is why the
	// filter has to be an allow list.
	t.Setenv("GITHUB_TOKEN", "ghp_live_too")
	t.Setenv("PATH", "/usr/bin:/bin")

	for _, kv := range browserEnv() {
		name, value, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "AGENTHUB_") {
			t.Errorf("browser environment carries %q", name)
		}
		if strings.Contains(value, "ghp_live") || value == "key-material" {
			t.Errorf("browser environment carries the value of %q", name)
		}
	}
}

// TestBrowserEnvIsNeverNil pins the one way this filter fails open: os/exec
// reads a nil Env as "inherit the parent's", so an environment holding none
// of the allowlisted names must still produce an empty non-nil slice.
func TestBrowserEnvIsNeverNil(t *testing.T) {
	t.Parallel()
	if browserEnv() == nil {
		t.Fatal("nil environment: os/exec would inherit everything")
	}
}

// TestBrowserEnvNamesAreAnAllowList guards the list itself. A name matching
// a credential prefix added here would be inherited forever after without
// anything else noticing.
func TestBrowserEnvNamesAreAnAllowList(t *testing.T) {
	t.Parallel()
	for _, name := range browserEnvNames() {
		if strings.HasPrefix(name, "AGENTHUB_") {
			t.Errorf("%q must not be handed to a browser", name)
		}
	}
}

// TestBrowserEnvKeepsWhatTheOpenerNeeds: the failure direction of an allow
// list is a handler that cannot run, so the names it must have are pinned.
func TestBrowserEnvKeepsWhatTheOpenerNeeds(t *testing.T) {
	// Deliberately not parallel: t.Setenv.
	t.Setenv("PATH", "/usr/bin:/bin")
	got := map[string]string{}
	for _, kv := range browserEnv() {
		name, value, _ := strings.Cut(kv, "=")
		got[name] = value
	}
	if got["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("PATH missing from the browser environment: %v", got)
	}
}
