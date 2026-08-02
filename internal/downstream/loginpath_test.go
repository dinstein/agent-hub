package downstream

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// envValue returns the value of name in a "KEY=value" slice, taking the last
// occurrence as os/exec does.
func envValue(env []string, name string) (string, bool) {
	val, found := "", false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			val, found = v, true
		}
	}
	return val, found
}

func TestBuildEnvWidensPATH(t *testing.T) {
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", "/usr/bin"+sep+"/bin")
	login := func() string { return "/usr/bin" + sep + "/opt/homebrew/bin" }

	got, ok := envValue(buildEnv(nil, login), "PATH")
	if !ok {
		t.Fatal("no PATH in the child environment")
	}
	want := "/usr/bin" + sep + "/bin" + sep + "/opt/homebrew/bin"
	if got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
}

// The truncated PATH keeps its precedence: widening is additive, so a machine
// that never had the launchd problem spawns what it always spawned.
func TestBuildEnvWidenKeepsExistingPrecedence(t *testing.T) {
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", "/first"+sep+"/second")
	login := func() string { return "/second" + sep + "/first" + sep + "/third" }

	got, _ := envValue(buildEnv(nil, login), "PATH")
	want := "/first" + sep + "/second" + sep + "/third"
	if got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
}

// A configuration that states a PATH has said what it means.
func TestBuildEnvLeavesAnExplicitPATHAlone(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	called := false
	login := func() string { called = true; return "/opt/homebrew/bin" }

	env := buildEnv(map[string]string{"PATH": "/only/this"}, login)
	got, _ := envValue(env, "PATH")
	if got != "/only/this" {
		t.Fatalf("PATH = %q, want the configured value verbatim", got)
	}
	if called {
		t.Fatal("captured a login PATH for a spec that stated its own")
	}
	count := 0
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && k == "PATH" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("PATH appears %d times in the child environment, want 1", count)
	}
}

func TestBuildEnvAddsPATHWhenTheParentHasNone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unsetenv of PATH is not the same statement on Windows")
	}
	t.Setenv("PATH", "")
	if err := os.Unsetenv("PATH"); err != nil {
		t.Fatalf("unset PATH: %v", err)
	}
	login := func() string { return "/opt/homebrew/bin" }

	got, ok := envValue(buildEnv(nil, login), "PATH")
	if !ok {
		t.Fatal("no PATH given to a child whose parent had none")
	}
	if got != "/opt/homebrew/bin" {
		t.Fatalf("PATH = %q, want the login shell's", got)
	}
}

// nil means production's process-wide capture, not "skip the widening".
func TestBuildEnvNilLoginPATHFallsBackToTheRealCapture(t *testing.T) {
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", "/usr/bin"+sep+"/bin")
	got, ok := envValue(buildEnv(nil, nil), "PATH")
	if !ok {
		t.Fatal("no PATH in the child environment")
	}
	if !strings.HasPrefix(got, "/usr/bin"+sep+"/bin") {
		t.Fatalf("PATH = %q, want it to start with the process PATH", got)
	}
}

func TestBuildEnvStillStripsAgenthubVars(t *testing.T) {
	t.Setenv("AGENTHUB_DATA_DIR", "/d")
	t.Setenv("PATH", "/usr/bin")
	for _, kv := range buildEnv(nil, func() string { return "/usr/bin" }) {
		if strings.HasPrefix(kv, envPrefix) {
			t.Fatalf("AGENTHUB_ variable leaked to the child: %s", kv)
		}
	}
}

// TestDialStdioSpawnsACommandOnlyTheLoginPATHCanFind is the whole bug in one
// test: the process PATH is the four-entry launchd one, the command lives
// somewhere only the login shell knows about, and the spawn must succeed.
func TestDialStdioSpawnsACommandOnlyTheLoginPATHCanFind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the truncated-PATH problem is launchd's and systemd's")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "shim-server")
	// Reads one line and exits; enough to prove exec found and started it.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nread line\n"), 0o755); err != nil {
		t.Fatalf("write stand-in server: %v", err)
	}
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

	deps := Deps{LoginPATH: func() string { return dir }}
	tr, err := deps.dialStdio(context.Background(), Spec{ID: "s", Kind: "stdio", Command: "shim-server"})
	if err != nil {
		t.Fatalf("dialStdio: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
}
