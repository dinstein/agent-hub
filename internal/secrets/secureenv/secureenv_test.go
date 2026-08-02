package secureenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFilterTable(t *testing.T) {
	cases := []struct {
		name    string
		environ []string
		cfg     Config
		want    []string
	}{
		{
			name:    "default allowlist passes basics",
			environ: []string{"PATH=/usr/bin", "HOME=/home/u", "TMPDIR=/tmp", "LANG=en_US.UTF-8", "TZ=UTC"},
			want:    []string{"PATH=/usr/bin", "HOME=/home/u", "TMPDIR=/tmp", "LANG=en_US.UTF-8", "TZ=UTC"},
		},
		{
			name:    "deny by default drops unknown vars",
			environ: []string{"PATH=/usr/bin", "AWS_SECRET_ACCESS_KEY=x", "DATABASE_URL=y", "SSH_AUTH_SOCK=z"},
			want:    []string{"PATH=/usr/bin"},
		},
		{
			name:    "locale and XDG prefixes pass",
			environ: []string{"LC_ALL=C", "LC_CTYPE=UTF-8", "XDG_DATA_HOME=/d", "XDGX=v"},
			want:    []string{"LC_ALL=C", "LC_CTYPE=UTF-8", "XDG_DATA_HOME=/d"},
		},
		{
			name:    "AGENTHUB_ hard denied even when allowlisted",
			environ: []string{"AGENTHUB_SECRET_FOO=x", "AGENTHUB_DATA_DIR=/d", "PATH=/p"},
			cfg:     Config{Allow: []string{"AGENTHUB_SECRET_FOO"}, AllowPrefixes: []string{"AGENTHUB_"}},
			want:    []string{"PATH=/p"},
		},
		{
			name:    "custom exact names and prefixes",
			environ: []string{"MY_TOKEN_FILE=/f", "APP_MODE=dev", "APP2=x"},
			cfg:     Config{Allow: []string{"MY_TOKEN_FILE"}, AllowPrefixes: []string{"APP_"}},
			want:    []string{"MY_TOKEN_FILE=/f", "APP_MODE=dev"},
		},
		{
			name:    "proxy vars dropped by default",
			environ: []string{"HTTP_PROXY=http://p:3128", "https_proxy=http://p:3128", "NO_PROXY=localhost", "PATH=/p"},
			want:    []string{"PATH=/p"},
		},
		{
			name:    "proxy forwarded with userinfo stripped",
			environ: []string{"HTTP_PROXY=http://user:pass@proxy:3128", "NO_PROXY=localhost,.corp"},
			cfg:     Config{ForwardProxy: true},
			want:    []string{"HTTP_PROXY=http://proxy:3128", "NO_PROXY=localhost,.corp"},
		},
		{
			name:    "proxy without credentials forwarded verbatim",
			environ: []string{"https_proxy=http://proxy:3128"},
			cfg:     Config{ForwardProxy: true},
			want:    []string{"https_proxy=http://proxy:3128"},
		},
		{
			name: "unredactable credential-bearing proxy dropped",
			// scheme-less form parses with an opaque part, not userinfo —
			// fail-closed: dropped rather than forwarded.
			environ: []string{"HTTP_PROXY=user:pass@proxy:3128"},
			cfg:     Config{ForwardProxy: true},
			want:    []string{},
		},
		{
			name:    "malformed entries dropped",
			environ: []string{"NOEQUALS", "=novalue", "PATH=/p"},
			want:    []string{"PATH=/p"},
		},
		{
			name:    "empty input",
			environ: nil,
			want:    []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Filter(tc.environ, tc.cfg)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Filter = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRedactProxyValue(t *testing.T) {
	cases := []struct {
		name, varName, val string
		want               string
		wantOK             bool
	}{
		{"no credentials", "HTTP_PROXY", "http://proxy:3128", "http://proxy:3128", true},
		{"userinfo stripped", "HTTP_PROXY", "http://u:p@proxy:3128", "http://proxy:3128", true},
		{"user only stripped", "HTTPS_PROXY", "socks5://u@proxy:1080", "socks5://proxy:1080", true},
		{"no_proxy verbatim", "NO_PROXY", "a@b,localhost", "a@b,localhost", true},
		{"no_proxy lowercase verbatim", "no_proxy", "a@b", "a@b", true},
		{"schemeless credentials dropped", "HTTP_PROXY", "u:p@proxy:3128", "", false},
		{"empty value", "HTTP_PROXY", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := RedactProxyValue(tc.varName, tc.val)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("RedactProxyValue(%q, %q) = (%q, %v), want (%q, %v)",
					tc.varName, tc.val, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// writeFakeShell writes an executable script usable as the login shell.
func writeFakeShell(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-shell tests are unix-only")
	}
	path := filepath.Join(t.TempDir(), "fakeshell")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCaptureLoginPATHLastLine(t *testing.T) {
	// Login profiles may print greetings before the echo; the last
	// non-empty line wins.
	shell := writeFakeShell(t, "echo 'welcome banner'\necho '/fake/bin:/usr/bin'\n")
	got, err := CaptureLoginPATH(context.Background(), shell)
	if err != nil {
		t.Fatalf("CaptureLoginPATH: %v", err)
	}
	if got != "/fake/bin:/usr/bin" {
		t.Fatalf("got %q, want /fake/bin:/usr/bin", got)
	}
}

func TestCaptureLoginPATHEmptyOutput(t *testing.T) {
	shell := writeFakeShell(t, "true\n")
	if _, err := CaptureLoginPATH(context.Background(), shell); err == nil {
		t.Fatal("expected error for shell printing nothing")
	}
}

func TestCaptureLoginPATHTimeout(t *testing.T) {
	shell := writeFakeShell(t, "sleep 5\necho /never\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := CaptureLoginPATH(ctx, shell)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("context state: %v", ctx.Err())
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("capture blocked %v past its context", elapsed)
	}
}

func TestCaptureLoginPATHMissingShell(t *testing.T) {
	if _, err := CaptureLoginPATH(context.Background(), "/nonexistent/shell-xyz"); err == nil {
		t.Fatal("expected error for missing shell")
	}
}

// TestLoginPATHCached: the process-wide capture is stable across calls
// and never empty when the process has a PATH (fail-open fallback).
func TestLoginPATHCached(t *testing.T) {
	first := LoginPATH()
	second := LoginPATH()
	if first != second {
		t.Fatalf("LoginPATH not cached: %q vs %q", first, second)
	}
	if os.Getenv("PATH") != "" && first == "" {
		t.Fatal("LoginPATH empty despite process PATH being set")
	}
}

func TestMergePATHTable(t *testing.T) {
	sep := string(os.PathListSeparator)
	j := func(parts ...string) string { return strings.Join(parts, sep) }

	cases := []struct {
		name        string
		base, extra string
		want        string
	}{
		{
			name:  "appends only what is missing, in extra's order",
			base:  j("/usr/bin", "/bin"),
			extra: j("/usr/bin", "/opt/homebrew/bin", "/bin", "/Users/u/.cargo/bin"),
			want:  j("/usr/bin", "/bin", "/opt/homebrew/bin", "/Users/u/.cargo/bin"),
		},
		{
			name:  "a base that already contains everything is unchanged",
			base:  j("/a", "/b", "/c"),
			extra: j("/c", "/a"),
			want:  j("/a", "/b", "/c"),
		},
		{
			name:  "base keeps its precedence rather than extra's",
			base:  j("/first", "/second"),
			extra: j("/second", "/first"),
			want:  j("/first", "/second"),
		},
		{
			name:  "empty base takes extra without a leading empty entry",
			base:  "",
			extra: j("/a", "/b"),
			want:  j("/a", "/b"),
		},
		{
			name:  "empty entries in extra are dropped",
			base:  "/a",
			extra: j("", "/b", ""),
			want:  j("/a", "/b"),
		},
		{
			name:  "an empty entry already in base is left alone",
			base:  j("/a", "", "/b"),
			extra: j("/c"),
			want:  j("/a", "", "/b", "/c"),
		},
		{
			name:  "extra repeating itself contributes one entry",
			base:  "/a",
			extra: j("/b", "/b"),
			want:  j("/a", "/b"),
		},
		{
			name:  "empty extra is a no-op",
			base:  j("/a", "/b"),
			extra: "",
			want:  j("/a", "/b"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergePATH(tc.base, tc.extra); got != tc.want {
				t.Fatalf("MergePATH(%q, %q) = %q, want %q", tc.base, tc.extra, got, tc.want)
			}
		})
	}
}

// TestMergePATHKeepsBaseResolution is the invariant the callers rely on:
// whatever base resolved before the merge resolves to the same file after
// it. A machine whose PATH was never truncated must spawn what it always
// spawned, which is what lets the merge run unconditionally.
func TestMergePATHKeepsBaseResolution(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	name := "shadowed-tool"
	if runtime.GOOS == "windows" {
		name += ".bat"
	}
	for _, dir := range []string{dirA, dirB} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write stand-in: %v", err)
		}
	}

	// dirA wins under base; the merge must not let extra's dirB take over.
	merged := MergePATH(dirA, dirB)
	t.Setenv("PATH", merged)
	got, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("LookPath under merged PATH: %v", err)
	}
	if want := filepath.Join(dirA, name); got != want {
		t.Fatalf("merge changed resolution: got %q, want %q", got, want)
	}
}
