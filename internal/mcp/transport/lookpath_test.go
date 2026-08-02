package transport

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stub writes an executable stand-in named name into dir and returns its path.
func stub(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", p, err)
	}
	return p
}

func pathEnv(dirs ...string) []string {
	return []string{"HOME=/home/u", "PATH=" + strings.Join(dirs, string(os.PathListSeparator))}
}

func TestLookPathUsesTheChildsPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("LookPath defers to exec.LookPath on Windows")
	}
	dir := t.TempDir()
	want := stub(t, dir, "npx")

	// The regression: this process cannot see dir at all, which is exactly
	// the shape of the launchd bug — the child's PATH knows where npx is and
	// the parent's does not.
	t.Setenv("PATH", "/nonexistent-for-this-test")

	got, err := LookPath("npx", pathEnv(dir))
	if err != nil {
		t.Fatalf("LookPath: %v", err)
	}
	if got != want {
		t.Fatalf("resolved to %q, want %q", got, want)
	}
}

func TestLookPathTakesTheFirstMatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("LookPath defers to exec.LookPath on Windows")
	}
	first, second := t.TempDir(), t.TempDir()
	want := stub(t, first, "tool")
	stub(t, second, "tool")

	got, err := LookPath("tool", pathEnv(first, second))
	if err != nil {
		t.Fatalf("LookPath: %v", err)
	}
	if got != want {
		t.Fatalf("resolved to %q, want the earlier entry %q", got, want)
	}
}

func TestLookPathSkipsNonExecutablesAndDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("LookPath defers to exec.LookPath on Windows")
	}
	shadow, real := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(shadow, "tool"), 0o755); err != nil {
		t.Fatalf("mkdir shadow: %v", err)
	}
	notExec := t.TempDir()
	if err := os.WriteFile(filepath.Join(notExec, "tool"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write non-executable: %v", err)
	}
	want := stub(t, real, "tool")

	got, err := LookPath("tool", pathEnv(shadow, notExec, real))
	if err != nil {
		t.Fatalf("LookPath: %v", err)
	}
	if got != want {
		t.Fatalf("resolved to %q, want %q", got, want)
	}
}

// An empty PATH entry is the current directory to POSIX and to
// exec.LookPath, and deliberately nothing here.
func TestLookPathIgnoresEmptyPATHEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("LookPath defers to exec.LookPath on Windows")
	}
	cwd := t.TempDir()
	stub(t, cwd, "tool")
	t.Chdir(cwd)

	if _, err := LookPath("tool", pathEnv("", "/nonexistent")); err == nil {
		t.Fatal("an empty PATH entry resolved a command out of the working directory")
	}
}

func TestLookPathNotFoundNamesWhatWasSearched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("LookPath defers to exec.LookPath on Windows")
	}
	dir := t.TempDir()
	_, err := LookPath("absent-tool", pathEnv(dir))
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	// The message has one job the bare exec error could not do: say where it
	// looked, so an operator can tell a missing tool from a truncated PATH.
	if !strings.Contains(err.Error(), "absent-tool") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("error names neither the command nor the search path: %v", err)
	}
}

func TestLookPathPassThrough(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		command string
		env     []string
	}{
		{"nil env inherits ours, so exec asks the right question", "npx", nil},
		{"a path separator means exec resolves it, not PATH", "./local/npx", pathEnv(dir)},
		{"an absolute command is already resolved", "/usr/bin/env", pathEnv(dir)},
		{"no PATH in env is the caller's decision to keep", "npx", []string{"HOME=/home/u"}},
		{"an empty command is the caller's error to report", "", pathEnv(dir)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LookPath(tc.command, tc.env)
			if err != nil {
				t.Fatalf("LookPath: %v", err)
			}
			if got != tc.command {
				t.Fatalf("rewrote %q to %q", tc.command, got)
			}
		})
	}
}

func TestPathFromEnv(t *testing.T) {
	cases := []struct {
		name  string
		env   []string
		want  string
		found bool
	}{
		{"absent", []string{"HOME=/h"}, "", false},
		{"present", []string{"HOME=/h", "PATH=/a"}, "/a", true},
		{"the last occurrence wins, as os/exec dedups", []string{"PATH=/a", "PATH=/b"}, "/b", true},
		{"an empty value is still a value", []string{"PATH="}, "", true},
		{"an entry with no = is not a variable", []string{"PATH", "PATH=/a"}, "/a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := pathFromEnv(tc.env)
			if got != tc.want || found != tc.found {
				t.Fatalf("pathFromEnv = (%q, %v), want (%q, %v)", got, found, tc.want, tc.found)
			}
		})
	}
}

// TestSpawnStdioFindsCommandOnlyOnTheChildsPATH is the end-to-end shape of
// the bug: the process PATH cannot resolve the command and the spawn must
// succeed anyway.
func TestSpawnStdioFindsCommandOnlyOnTheChildsPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("LookPath defers to exec.LookPath on Windows")
	}
	dir := t.TempDir()
	stub(t, dir, "quiet-tool")
	t.Setenv("PATH", "/nonexistent-for-this-test")

	tr, err := SpawnStdio(StdioConfig{Command: "quiet-tool", Env: pathEnv(dir)})
	if err != nil {
		t.Fatalf("SpawnStdio: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
}

func TestSpawnStdioReportsAnUnresolvableCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("LookPath defers to exec.LookPath on Windows")
	}
	_, err := SpawnStdio(StdioConfig{Command: "absent-tool", Env: pathEnv(t.TempDir())})
	if err == nil {
		t.Fatal("expected a spawn failure")
	}
	// ClassUnavailable, as a failed exec.Start already was: a missing binary
	// stays a breaker-visible spawn failure rather than becoming fatal.
	var terr *Error
	if !errors.As(err, &terr) {
		t.Fatalf("not a transport error: %v", err)
	}
	if terr.Class != ClassUnavailable {
		t.Fatalf("class = %v, want ClassUnavailable", terr.Class)
	}
}
