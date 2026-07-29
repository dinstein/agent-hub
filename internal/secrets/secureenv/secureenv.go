// Package secureenv builds hardened environments for spawned downstream
// processes: allowlist filtering (deny by default),
// login-shell PATH capture, and proxy-variable userinfo redaction.
//
// It exposes pure functions only. Wiring into internal/downstream happens
// in a later task; Filter stacks with downstream's own AGENTHUB_* strip
// (both deny AGENTHUB_*, so the composition is idempotent).
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

// CaptureLoginPATH runs `shell -l -c 'echo $PATH'` and returns the last
// non-empty line of output. launchd/systemd-spawned processes inherit a
// truncated PATH; the login shell's is what interactive users actually
// have (this bit mcpproxy three times). The last line is
// used because login profiles may print greetings before the echo.
// An empty shell argument falls back to $SHELL, then /bin/sh.
func CaptureLoginPATH(ctx context.Context, shell string) (string, error) {
	if shell == "" {
		shell = os.Getenv("SHELL")
	}
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.CommandContext(ctx, shell, "-l", "-c", "echo $PATH")
	// Children of the login shell inherit the stdout pipe; after the
	// context kills the shell, Output would otherwise block until every
	// descendant exits. WaitDelay force-closes the pipes shortly after
	// cancellation so a wedged login profile cannot stall the caller.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("secureenv: capture login PATH via %s: %w", shell, err)
	}
	lines := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l, nil
		}
	}
	return "", fmt.Errorf("secureenv: login shell %s printed no PATH", shell)
}

// loginPATHTimeout bounds the one-shot capture; a wedged login profile
// must not stall the first spawn for long.
const loginPATHTimeout = 3 * time.Second

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
