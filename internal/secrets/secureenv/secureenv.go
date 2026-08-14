// Package secureenv builds hardened environments for spawned downstream
// processes: allowlist filtering (deny by default),
// login-shell PATH capture, and proxy-variable userinfo redaction.
//
// It exposes pure functions only, and only SOME of them are wired up.
//
// LIVE: LoginPATH and MergePATH, called from internal/downstream/spec.go to
// widen a truncated PATH before a spawn.
//
// NOT WIRED: Filter, Config, RedactProxyValue and CaptureLoginPATH have no
// caller outside this package's own tests. A spawned downstream therefore
// receives the parent environment minus the AGENTHUB_* prefix, which
// internal/downstream strips itself (spec.go, envPrefix) — a deny list. The
// allowlist below and the proxy redaction beside it describe what Filter
// WOULD admit, not what a downstream currently gets; read them as a design
// that is built and waiting, not as a filter in force.
// docs/subsystems/credentials.md records the gap.
package secureenv

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// defaultAllowNames are the exact variable names forwarded by default:
// process basics, temp dirs, identity, and time/locale.
var defaultAllowNames = map[string]struct{}{
	"PATH":     {},
	"HOME":     {},
	"TMPDIR":   {},
	"TMP":      {},
	"TEMP":     {},
	"USER":     {},
	"LOGNAME":  {},
	"SHELL":    {},
	"TERM":     {},
	"TZ":       {},
	"LANG":     {},
	"LANGUAGE": {},
}

// defaultAllowPrefixes are the variable-name prefixes forwarded by
// default (locale and XDG base directories).
var defaultAllowPrefixes = []string{"LC_", "XDG_"}

// proxyNames are handled specially: denied by default, forwarded with
// userinfo redaction under Config.ForwardProxy.
var proxyNames = map[string]struct{}{
	"HTTP_PROXY": {}, "http_proxy": {},
	"HTTPS_PROXY": {}, "https_proxy": {},
	"ALL_PROXY": {}, "all_proxy": {},
	"NO_PROXY": {}, "no_proxy": {},
}

// deniedPrefix is stripped unconditionally, even when allowlisted via
// Config (fail-closed: our own control variables must never leak into a
// downstream). This stacks with the identical strip in
// internal/downstream.
const deniedPrefix = "AGENTHUB_"

// Config customizes Filter beyond the built-in allowlist.
type Config struct {
	// Allow adds exact variable names to the allowlist (per-server
	// config).
	Allow []string
	// AllowPrefixes adds name prefixes to the allowlist.
	AllowPrefixes []string
	// ForwardProxy forwards the proxy variables (HTTP_PROXY etc.), with
	// userinfo stripped from their URLs. Default false: proxy endpoints
	// (which often embed credentials) are not a downstream's business
	// unless asked.
	ForwardProxy bool
}

// Filter returns the subset of environ ("KEY=value" entries) that passes
// the allowlist, preserving order. Deny by default: anything not
// explicitly allowed is dropped.
func Filter(environ []string, cfg Config) []string {
	extra := make(map[string]struct{}, len(cfg.Allow))
	for _, n := range cfg.Allow {
		extra[n] = struct{}{}
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, val, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		if strings.HasPrefix(name, deniedPrefix) {
			continue // hard deny, not overridable
		}
		if _, isProxy := proxyNames[name]; isProxy {
			if !cfg.ForwardProxy {
				continue
			}
			red, ok := RedactProxyValue(name, val)
			if !ok {
				continue // fail-closed: unredactable credential-bearing value is dropped
			}
			out = append(out, name+"="+red)
			continue
		}
		if allowed(name, extra, cfg.AllowPrefixes) {
			out = append(out, kv)
		}
	}
	return out
}

func allowed(name string, extra map[string]struct{}, extraPrefixes []string) bool {
	if _, ok := defaultAllowNames[name]; ok {
		return true
	}
	if _, ok := extra[name]; ok {
		return true
	}
	for _, p := range defaultAllowPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	for _, p := range extraPrefixes {
		if p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// RedactProxyValue strips userinfo (user:password@) from a proxy variable
// value. Returns ok=false when the value carries an '@' that cannot be
// positively identified and removed as URL userinfo — such values are
// dropped rather than forwarded (fail-closed: never pass a value we could
// not prove credential-free). NO_PROXY is a host list without
// credentials and passes through verbatim.
func RedactProxyValue(name, val string) (string, bool) {
	if strings.EqualFold(name, "NO_PROXY") {
		return val, true
	}
	if !strings.Contains(val, "@") {
		return val, true
	}
	u, err := url.Parse(val)
	if err != nil || u.User == nil {
		// '@' present but not parseable as userinfo (e.g. scheme-less
		// "user:pass@host" parses with an opaque part) — drop it.
		return "", false
	}
	u.User = nil
	return u.String(), true
}

// captureModes are tried in order, most complete first.
//
// **`-l` alone is not enough, and that is the whole reason this list is a
// list.** A login shell sources the login profile (.zprofile, .bash_profile)
// and nothing else, while the line that puts Homebrew, nvm, pyenv or a
// language toolchain on PATH conventionally lives in the *interactive* rc
// file (.zshrc, .bashrc) — so the directory holding `npx` is exactly the one
// `-l` does not find. `-i -l` sources both.
//
// This is easy to measure wrongly. Running `zsh -l -c 'echo $PATH'` from a
// terminal prints a complete PATH and appears to prove `-l` sufficient; it
// proves nothing, because the shell inherited the already-complete PATH of
// the interactive shell that launched it and merely appended to it. The
// launchd case has no such inheritance, and is the only case that matters
// here.
//
// `-i` is kept fallible rather than assumed: a shell that refuses to be
// interactive without a tty, or an rc file that fails under one, must not
// cost us the plain login capture that would have worked.
var captureModes = [][]string{
	{"-i", "-l", "-c", "echo $PATH"},
	{"-l", "-c", "echo $PATH"},
}

// CaptureLoginPATH runs the shell as an interactive login shell (falling back
// to a plain login shell) and returns the PATH it reports — the last
// non-empty line of output, because a profile may print a greeting before the
// echo. launchd/systemd-spawned processes inherit a truncated PATH; this is
// what an interactive user actually has (this bit mcpproxy three times).
//
// An empty shell argument falls back to $SHELL, then /bin/sh.
func CaptureLoginPATH(ctx context.Context, shell string) (string, error) {
	if shell == "" {
		shell = os.Getenv("SHELL")
	}
	if shell == "" {
		shell = "/bin/sh"
	}
	var err error
	for _, args := range captureModes {
		var path string
		if path, err = captureWith(ctx, shell, args); err == nil {
			return path, nil
		}
		// A cancelled context will not be any kinder to the next mode, and
		// retrying spends the caller's remaining budget on a second timeout.
		if ctx.Err() != nil {
			break
		}
	}
	return "", err
}

func captureWith(ctx context.Context, shell string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, shell, args...)
	// Children of the login shell inherit the stdout pipe; after the
	// context kills the shell, Output would otherwise block until every
	// descendant exits. WaitDelay force-closes the pipes shortly after
	// cancellation so a wedged login profile cannot stall the caller.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("secureenv: capture login PATH via %s %s: %w", shell, strings.Join(args[:len(args)-2], " "), err)
	}
	lines := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l, nil
		}
	}
	return "", fmt.Errorf("secureenv: login shell %s printed no PATH", shell)
}

// loginPATHTimeout bounds the one-shot capture, across both modes; a wedged
// profile must not stall the first spawn for long. It is the budget for an
// interactive rc file, which does real work — version managers, completions —
// where a login profile mostly exports variables.
const loginPATHTimeout = 5 * time.Second

var loginPATH struct {
	once sync.Once
	val  string
}

// LoginPATH returns the login-shell PATH, captured once per process
// (sync.Once) with a hard timeout. On any failure it falls back to the
// current process PATH (fail-open by design: a broken login shell must
// not block spawning — the worst case is keeping the truncated PATH we
// already had, never less).
func LoginPATH() string {
	loginPATH.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), loginPATHTimeout)
		defer cancel()
		p, err := CaptureLoginPATH(ctx, "")
		if err != nil {
			loginPATH.val = os.Getenv("PATH")
			return
		}
		loginPATH.val = p
	})
	return loginPATH.val
}

// MergePATH returns base with every directory of extra that base does not
// already list appended, in extra's order.
//
// base is preserved byte for byte, and that is the point rather than an
// implementation detail: the result is a strict superset in which every
// command that already resolved under base still resolves to the same file.
// A process whose PATH was never truncated therefore spawns exactly what it
// spawned before, so this can be applied unconditionally instead of behind a
// guess at whether the current PATH "looks truncated".
//
// Empty entries in extra are dropped — POSIX reads an empty entry as the
// current directory, which is not something a login shell should be able to
// add to a spawn. An empty entry already in base is left alone; removing it
// would change what base resolves.
//
// Deduplication is by exact string match. On Windows that will miss a
// difference in case or in trailing separator and append a duplicate
// directory, which costs a wasted stat during lookup and nothing else.
func MergePATH(base, extra string) string {
	sep := string(os.PathListSeparator)
	seen := make(map[string]struct{})
	for _, dir := range strings.Split(base, sep) {
		seen[dir] = struct{}{}
	}
	out := base
	for _, dir := range strings.Split(extra, sep) {
		if dir == "" {
			continue
		}
		if _, dup := seen[dir]; dup {
			continue
		}
		seen[dir] = struct{}{}
		if out == "" {
			out = dir
			continue
		}
		out += sep + dir
	}
	return out
}
