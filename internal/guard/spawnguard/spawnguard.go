// Package spawnguard checks downstream spawn command lines before the
// process is started (docs/subsystems/guard.md, internal/guard/spawnguard).
//
// Positioning — ANTI-SMUGGLING, NOT A SANDBOX: the guard catches command
// shapes that smuggle arbitrary code execution through an innocuous-looking
// server entry (inline shell/interpreter eval, loader-hijacking environment
// variables, container-escape flags). It is pattern matching on the command
// line, not an execution boundary: a config the operator legitimately wrote
// to run code will run code. Regular launcher usage — npx, uvx, docker run
// with ordinary project mounts — passes untouched.
//
// Failure direction: shape detection FAILS OPEN — a command line the guard
// cannot confidently parse is allowed (blocking would turn every unusual but
// legitimate launcher into an outage; the sandbox story lives elsewhere,
// M2 Docker Spawner). The deterministic checks (denylist, dangerous env
// names) always block on match.
//
// Dependency constraint (canonical.md §2 rule 4, depguard-enforced): only
// the standard library plus internal/guard.
package spawnguard

import (
	"fmt"
	"strings"

	"github.com/dinstein/agent-hub/internal/guard"
)

// Stable machine-readable block codes.
const (
	CodeDenylisted      = "denylisted"
	CodeInlineEval      = "inline_eval"
	CodeEnvSmuggling    = "env_smuggling"
	CodeContainerEscape = "container_escape"
)

// Blocked is the typed rejection. It unwraps to guard.ErrBlocked so callers
// can classify with errors.Is without importing this package.
type Blocked struct {
	// Code is one of the Code* constants.
	Code string
	// Reason is the human-readable explanation naming the offending token.
	Reason string
	// EnvVar names the variable that triggered a CodeEnvSmuggling block, and
	// is empty for every other code. It is a separate field rather than
	// something to parse back out of Reason because the caller is the only
	// one who can finish the diagnosis: this package receives one flat
	// []string and cannot tell an inherited variable from one the server
	// entry declared, which is exactly the question the operator has.
	EnvVar string
}

// Error implements error.
func (b *Blocked) Error() string {
	return fmt.Sprintf("spawnguard: blocked (%s): %s", b.Code, b.Reason)
}

// Unwrap ties Blocked to the guard.ErrBlocked sentinel.
func (b *Blocked) Unwrap() error { return guard.ErrBlocked }

func blockedf(code, format string, a ...any) *Blocked {
	return &Blocked{Code: code, Reason: fmt.Sprintf(format, a...)}
}

// Config configures a Guard. All lists are optional.
type Config struct {
	// Allowlist contains command basenames trusted to bypass the SHAPE
	// checks (inline-eval, container-escape). It does NOT bypass the env
	// check: dangerous env vars subvert the trusted binary itself.
	Allowlist []string
	// Denylist contains command basenames that are always blocked.
	Denylist []string
	// ExtraDangerousEnv adds env var names to the built-in dangerous set.
	ExtraDangerousEnv []string
	// AllowEnv removes env var names from the dangerous set (exact match,
	// checked before the built-in names and prefixes).
	AllowEnv []string
}

// Guard is an immutable, concurrency-safe checker.
type Guard struct {
	allow         map[string]bool
	deny          map[string]bool
	envDeny       map[string]bool
	envDenyPrefix []string
	envAllow      map[string]bool
}

// Env var names whose presence redirects what code the child process loads
// or runs. Values are compared exactly (POSIX env is case-sensitive).
var defaultDangerousEnv = []string{
	"LD_PRELOAD", "LD_AUDIT", "LD_LIBRARY_PATH", // ELF loader hijack
	"NODE_OPTIONS",        // --require/--import arbitrary modules
	"PYTHONSTARTUP",       // runs a file on interpreter start
	"PERL5OPT", "RUBYOPT", // interpreter option smuggling
	"BASH_ENV", "ENV", "ZDOTDIR", // shell startup-file redirection
	"IFS", // word-splitting subversion
}

var defaultDangerousEnvPrefixes = []string{
	"DYLD_", // macOS dynamic linker (INSERT_LIBRARIES and friends)
}

// New builds a Guard from cfg.
func New(cfg Config) *Guard {
	toSet := func(names []string, lower bool) map[string]bool {
		m := make(map[string]bool, len(names))
		for _, n := range names {
			if lower {
				n = strings.ToLower(n)
			}
			m[n] = true
		}
		return m
	}
	g := &Guard{
		allow:         toSet(cfg.Allowlist, true),
		deny:          toSet(cfg.Denylist, true),
		envDeny:       toSet(defaultDangerousEnv, false),
		envDenyPrefix: defaultDangerousEnvPrefixes,
		envAllow:      toSet(cfg.AllowEnv, false),
	}
	for _, n := range cfg.ExtraDangerousEnv {
		g.envDeny[n] = true
	}
	return g
}

// maxWrapperDepth bounds wrapper unwrapping (env nohup nice ... cmd). Deeper
// nesting stops unwrapping and the remaining wrapper passes the shape checks
// as-is (fail-open, see package doc).
const maxWrapperDepth = 4

// Check inspects one spawn (command, args, env) and returns nil to allow or
// a *Blocked to reject. env entries are "KEY=VALUE" strings as in os/exec.
//
// Check order: env smuggling (deterministic, always applies) → denylist →
// allowlist (shape-check bypass) → wrapper unwrapping → inline-eval →
// container-escape.
func (g *Guard) Check(command string, args, env []string) error {
	for _, kv := range env {
		if err := g.checkEnvEntry(kv); err != nil {
			return err
		}
	}
	cmd, rest := command, args
	for range maxWrapperDepth {
		base := basename(cmd)
		if g.deny[base] {
			return blockedf(CodeDenylisted, "command %q is denylisted", base)
		}
		if g.allow[base] {
			return nil // allowlisted: shape checks bypassed; env already checked
		}
		next, nextArgs, unwrapped, err := g.unwrap(base, rest)
		if err != nil {
			return err
		}
		if !unwrapped {
			break
		}
		if next == "" {
			return nil // wrapper with no discernible command: nothing to run
		}
		cmd, rest = next, nextArgs
	}
	base := basename(cmd)
	if g.deny[base] {
		return blockedf(CodeDenylisted, "command %q is denylisted", base)
	}
	if g.allow[base] {
		return nil
	}
	if err := checkInlineEval(base, rest); err != nil {
		return err
	}
	return checkContainer(base, rest)
}

// checkEnvEntry blocks dangerous env names with non-empty values. An empty
// value is inert (explicit unset) and allowed.
func (g *Guard) checkEnvEntry(kv string) error {
	name, val, ok := strings.Cut(kv, "=")
	if !ok || val == "" {
		return nil
	}
	if g.envAllow[name] {
		return nil
	}
	if g.envDeny[name] {
		return blockedEnv(name)
	}
	for _, p := range g.envDenyPrefix {
		if strings.HasPrefix(name, p) {
			return blockedEnv(name)
		}
	}
	return nil
}

// blockedEnv builds the CodeEnvSmuggling rejection. The reason says what the
// variable does rather than only that it is "dangerous": the operator who
// meets this message usually set the variable for an unrelated reason years
// ago and needs to know why a loader path is a code-execution question at all.
func blockedEnv(name string) *Blocked {
	return &Blocked{
		Code:   CodeEnvSmuggling,
		Reason: fmt.Sprintf("env var %s redirects what the spawned process loads or runs", name),
		EnvVar: name,
	}
}

// basename extracts the lowercase command basename, tolerating both path
// separators and a trailing .exe.
func basename(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if i := strings.LastIndexAny(cmd, `/\`); i >= 0 {
		cmd = cmd[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(cmd), ".exe")
}
