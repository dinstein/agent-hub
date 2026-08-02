package downstream

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubCommand writes an executable stand-in into a fresh directory and
// returns that directory.
func stubCommand(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nread line\n"), 0o755); err != nil {
		t.Fatalf("write stand-in %s: %v", name, err)
	}
	return dir
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the truncated-PATH problem is launchd's and systemd's")
	}
}

func TestWidenPATHIfNeededRepairsATruncatedPATH(t *testing.T) {
	skipOnWindows(t)
	dir := stubCommand(t, "npx")
	env := []string{"HOME=/home/u", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}

	got := widenPATHIfNeeded(env, "npx", nil, func() string { return dir })
	path, _ := envValueOf(got, "PATH")
	want := "/usr/bin:/bin:/usr/sbin:/sbin" + string(os.PathListSeparator) + dir
	if path != want {
		t.Fatalf("PATH = %q, want %q", path, want)
	}
}

// The precondition is the whole design: a PATH that already resolves the
// command must not cost a login shell, because the first stdio dial is the
// gateway's most timing-sensitive moment.
func TestWidenPATHIfNeededSpawnsNoShellWhenTheCommandResolves(t *testing.T) {
	skipOnWindows(t)
	dir := stubCommand(t, "npx")
	env := []string{"PATH=" + dir}

	called := false
	got := widenPATHIfNeeded(env, "npx", nil, func() string { called = true; return "/opt/homebrew/bin" })
	if called {
		t.Fatal("captured a login PATH for a command that already resolved")
	}
	if path, _ := envValueOf(got, "PATH"); path != dir {
		t.Fatalf("PATH = %q, want it untouched at %q", path, dir)
	}
}

// A configuration that states a PATH has said what it means — it is neither
// probed nor widened.
func TestWidenPATHIfNeededLeavesAnExplicitPATHAlone(t *testing.T) {
	skipOnWindows(t)
	env := []string{"PATH=/only/this"}
	called := false

	got := widenPATHIfNeeded(env, "npx", map[string]string{"PATH": "/only/this"},
		func() string { called = true; return "/opt/homebrew/bin" })
	if called {
		t.Fatal("captured a login PATH for a spec that stated its own")
	}
	if path, _ := envValueOf(got, "PATH"); path != "/only/this" {
		t.Fatalf("PATH = %q, want the configured value verbatim", path)
	}
}

func TestWidenPATHIfNeededKeepsExistingPrecedence(t *testing.T) {
	skipOnWindows(t)
	sep := string(os.PathListSeparator)
	// The command resolves nowhere on env's PATH, so the widening runs; the
	// login PATH then offers two copies and the earlier one must win.
	first, second := stubCommand(t, "tool"), stubCommand(t, "tool")
	env := []string{"PATH=/nonexistent"}

	got := widenPATHIfNeeded(env, "tool", nil, func() string { return first + sep + second })
	path, _ := envValueOf(got, "PATH")
	if want := "/nonexistent" + sep + first + sep + second; path != want {
		t.Fatalf("PATH = %q, want %q", path, want)
	}
}

func TestWidenPATHIfNeededAddsPATHWhenTheEnvHasNone(t *testing.T) {
	skipOnWindows(t)
	dir := stubCommand(t, "npx")
	got := widenPATHIfNeeded([]string{"HOME=/home/u"}, "npx", nil, func() string { return dir })
	if path, ok := envValueOf(got, "PATH"); !ok || path != dir {
		t.Fatalf("PATH = %q (present=%v), want the login shell's", path, ok)
	}
}

// A login shell that reports nothing leaves the environment exactly as it
// was, rather than writing an empty PATH over a usable one.
func TestWidenPATHIfNeededToleratesAnEmptyCapture(t *testing.T) {
	skipOnWindows(t)
	env := []string{"PATH=/usr/bin"}
	got := widenPATHIfNeeded(env, "npx", nil, func() string { return "" })
	if path, _ := envValueOf(got, "PATH"); path != "/usr/bin" {
		t.Fatalf("PATH = %q, want it untouched", path)
	}
}

func TestBuildEnvStillStripsAgenthubVars(t *testing.T) {
	t.Setenv("AGENTHUB_DATA_DIR", "/d")
	for _, kv := range buildEnv(nil) {
		if strings.HasPrefix(kv, envPrefix) {
			t.Fatalf("AGENTHUB_ variable leaked to the child: %s", kv)
		}
	}
}

// TestDialStdioSpawnsACommandOnlyTheLoginPATHCanFind is the whole bug in one
// test: the process PATH is the four-entry launchd one, the command lives
// somewhere only the login shell knows about, and the spawn must succeed.
func TestDialStdioSpawnsACommandOnlyTheLoginPATHCanFind(t *testing.T) {
	skipOnWindows(t)
	dir := stubCommand(t, "shim-server")
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

	deps := Deps{LoginPATH: func() string { return dir }}
	tr, err := deps.dialStdio(context.Background(), Spec{ID: "s", Kind: "stdio", Command: "shim-server"})
	if err != nil {
		t.Fatalf("dialStdio: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
}

// The healthy case must not reach for a login shell at all.
func TestDialStdioSpawnsNoLoginShellWhenPATHIsFine(t *testing.T) {
	skipOnWindows(t)
	dir := stubCommand(t, "shim-server")
	t.Setenv("PATH", dir)

	called := false
	deps := Deps{LoginPATH: func() string { called = true; return dir }}
	tr, err := deps.dialStdio(context.Background(), Spec{ID: "s", Kind: "stdio", Command: "shim-server"})
	if err != nil {
		t.Fatalf("dialStdio: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	if called {
		t.Fatal("captured a login PATH on a machine whose PATH was never truncated")
	}
}
