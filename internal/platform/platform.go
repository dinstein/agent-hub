// Package platform resolves per-user filesystem locations (data, registry,
// logs, cache, state, run directories and the control socket path) for
// agenthub.
//
// Two constraints, from different places and held by different means:
//   - Zero business dependencies: this package imports only the standard
//     library and must never import other internal packages. canonical.md §2,
//     and depguard fails the build on a violation.
//   - The AGENTHUB_* environment variable names are frozen identifiers:
//     renaming the product must never rename them. canonical.md §1, where the
//     ABI is listed. NOTHING enforces this one — depguard reads imports, not
//     string constants — so it holds only as long as a reader knows it is a
//     rule. Which is why it is stated here, next to the constants.
//
// Platform support: macOS and Linux are implemented and exercised in CI.
// Windows is implemented here as of M2 (ruling A.5 #23 put the seam in this
// package) but is NOT VERIFIED ON REAL HARDWARE — see windows.go and
// docs/windows.md. Everything Windows-specific is reachable from this
// package alone, so the day a Windows machine exists the verification
// surface is one package plus the control-plane listener.
//
// All lookups go through a Resolver so tests can inject the OS, environment
// and home directory instead of mutating process state. The zero value of
// Resolver uses runtime.GOOS, os.LookupEnv and os.UserHomeDir.
package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ErrUnsupportedPlatform is returned by path-resolution functions on
// platforms that are not yet supported (currently anything other than
// darwin, linux and windows). Callers must test for it with errors.Is.
var ErrUnsupportedPlatform = errors.New("unsupported platform")

// Frozen environment variable names. These are ABI, listed in canonical.md §1
// alongside the module path and the binary names: renaming the product must
// never rename them. docs/modules/controlplane.md describes what each one
// does; the ruling that they cannot move is in canonical.md.
const (
	// EnvDataDir overrides the whole data directory.
	EnvDataDir = "AGENTHUB_DATA_DIR"
	// EnvRegistry overrides the registry directory independently of the
	// data directory.
	EnvRegistry = "AGENTHUB_REGISTRY"
	// EnvSocket overrides the control socket path (tests / multi-instance).
	EnvSocket = "AGENTHUB_SOCKET"
	// EnvHTTPToken carries the operator's own bearer for the daemon's MCP
	// data plane. It lives in the environment rather than on argv because a
	// credential passed as a command-line argument is readable by every
	// process on the machine.
	EnvHTTPToken = "AGENTHUB_HTTP_TOKEN"
	// EnvNoClientCLI set to "1" forbids agenthub from running another
	// application's configuration CLI on the user's behalf (the delegation
	// that connects clients whose file format agenthub will not rewrite).
	// Connect and Disconnect then explain what to run instead.
	EnvNoClientCLI = "AGENTHUB_NO_CLIENT_CLI"
)

// dirName is the release data directory name. DevDirName below is its
// development sibling.
const dirName = "AgentHub"

// DevDirName is the data directory name a development build uses instead of
// dirName. It is a SIBLING of the release directory, not a subdirectory of
// it: a dev run must not be able to corrupt an installed copy's registry by
// walking up one level, and a stray `rm -rf` on one must not take the other.
//
// This constant lives here because the directory layout is this package's
// business, but NOTHING here selects it. platform stays what it is — given an
// environment, resolve a path — and the choice of which name applies is made
// by the binary's entry point, which is the only place that knows whether it
// was built for release. See DevResolver.
const DevDirName = "AgentHubDev"

// DevResolver returns a Resolver that resolves the development data directory
// instead of the release one, unless the environment already says otherwise.
//
// Precedence is deliberate: an explicit AGENTHUB_DATA_DIR still wins. CI, the
// e2e suite and anyone debugging two sandboxes at once set that variable, and
// a build flavour that quietly overrode it would break them in a way that
// looks like the tests are wrong.
//
// Failure direction: a build that forgets to declare its channel gets the DEV
// directory, never the release one. The cost of guessing wrong that way is an
// extra sandbox; guessing the other way spends a one-time OAuth refresh token
// belonging to the user's real installation, which is not recoverable.
//
// Answering the AGENTHUB_DATA_DIR lookup, rather than carrying the dev
// directory in a separate field, is what makes the separation reach the run
// directory too: RunDir asks that same question to decide whether it may put
// the control socket in the shared XDG_RUNTIME_DIR (see dataDirRelocated).
func DevResolver(base *Resolver) *Resolver {
	r := &Resolver{}
	if base != nil {
		*r = *base
	}
	// Windows only: the control endpoint is a pipe name, so it cannot follow
	// the data directory the way <run>/ctl.sock does. See windowsCtlEndpoint.
	r.devChannel = true
	inner := r.LookupEnv
	if inner == nil {
		inner = os.LookupEnv
	}
	r.LookupEnv = func(key string) (string, bool) {
		if v, ok := inner(key); ok && v != "" {
			return v, ok
		}
		if key == EnvDataDir {
			if dir, err := devDataDir(r); err == nil {
				return dir, true
			}
		}
		return inner(key)
	}
	return r
}

// devDataDir is the development sibling of DataDir. It reuses the same
// platform branches so the two directories can never end up in different
// parents — the dev copy must be findable by anyone who knows where the real
// one lives.
func devDataDir(r *Resolver) (string, error) {
	switch r.goos() {
	case "darwin":
		home, err := r.home()
		if err != nil {
			return "", fmt.Errorf("platform: resolve home: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", DevDirName), nil
	case "linux":
		if v, ok := r.lookup("XDG_DATA_HOME"); ok && v != "" && filepath.IsAbs(v) {
			return filepath.Join(v, DevDirName), nil
		}
		home, err := r.home()
		if err != nil {
			return "", fmt.Errorf("platform: resolve home: %w", err)
		}
		return filepath.Join(home, ".local", "share", DevDirName), nil
	case "windows":
		return r.windowsDevDataDir()
	default:
		return "", fmt.Errorf("platform: %s: %w", r.goos(), ErrUnsupportedPlatform)
	}
}

// Resolver resolves agenthub filesystem locations. Fields left nil/empty
// fall back to the real process environment, so the zero value behaves like
// Default(). Resolvers are stateless and safe for concurrent use.
type Resolver struct {
	// GOOS overrides runtime.GOOS ("darwin", "linux", "windows", ...).
	GOOS string
	// LookupEnv overrides os.LookupEnv.
	LookupEnv func(key string) (string, bool)
	// UserHomeDir overrides os.UserHomeDir.
	UserHomeDir func() (string, error)

	// The three hooks below exist for the Windows branch (see windows.go).
	// They are injectable so the MSIX escape can be unit tested on the
	// machines that actually run the tests — none of which are Windows.

	// PackageIdentity overrides the MSIX app-container probe.
	PackageIdentity func() PackageIdentity
	// ProbePath overrides the reachability check applied to the
	// loopback-UNC twin path before it is adopted.
	ProbePath func(path string) error
	// UserSID overrides the current-user SID lookup used for the control
	// pipe name and its SDDL.
	UserSID func() (string, error)
	// Warn receives operator warnings (a redirected data directory that
	// could not be escaped). nil writes one line per distinct message to
	// stderr — never stdout, which carries JSON-RPC frames on a gateway.
	Warn func(msg string)

	// devChannel marks a Resolver built by DevResolver. It exists for ONE
	// caller — the Windows control-pipe name, which is not derived from any
	// directory and so cannot inherit the channel split the way <run>/ctl.sock
	// does (see windowsCtlEndpoint).
	//
	// Unexported deliberately. Every other part of this package answers "given
	// an environment, resolve a path" with no opinion about which build is
	// asking, and DevResolver is the single place that opinion enters. An
	// exported field would be a second entrance, and a Resolver could then
	// claim the dev endpoint while resolving the release data directory.
	devChannel bool
}

// Default returns a Resolver backed by the real process environment.
func Default() *Resolver { return &Resolver{} }

func (r *Resolver) goos() string {
	if r.GOOS != "" {
		return r.GOOS
	}
	return runtime.GOOS
}

func (r *Resolver) lookup(key string) (string, bool) {
	if r.LookupEnv != nil {
		return r.LookupEnv(key)
	}
	return os.LookupEnv(key)
}

func (r *Resolver) home() (string, error) {
	if r.UserHomeDir != nil {
		return r.UserHomeDir()
	}
	return os.UserHomeDir()
}

// DataDir resolves the root data directory.
//
// Resolution order:
//  1. AGENTHUB_DATA_DIR, when set and non-empty (honored on every platform:
//     an explicit override requires no platform knowledge).
//  2. macOS: ~/Library/Application Support/AgentHub.
//  3. Linux: ${XDG_DATA_HOME}/AgentHub when XDG_DATA_HOME is set to an
//     absolute path (relative values are ignored per the XDG spec),
//     otherwise ~/.local/share/AgentHub.
//  4. Windows: %APPDATA%\AgentHub, subject to the MSIX container escape
//     described in windows.go (UNVERIFIED — see docs/windows.md).
//
// Any other platform returns ErrUnsupportedPlatform.
func (r *Resolver) DataDir() (string, error) {
	if v, ok := r.lookup(EnvDataDir); ok && v != "" {
		return v, nil
	}
	switch r.goos() {
	case "darwin":
		home, err := r.home()
		if err != nil {
			return "", fmt.Errorf("platform: resolve home: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", dirName), nil
	case "linux":
		if v, ok := r.lookup("XDG_DATA_HOME"); ok && v != "" && filepath.IsAbs(v) {
			return filepath.Join(v, dirName), nil
		}
		home, err := r.home()
		if err != nil {
			return "", fmt.Errorf("platform: resolve home: %w", err)
		}
		return filepath.Join(home, ".local", "share", dirName), nil
	case "windows":
		return r.windowsDataDir()
	default:
		return "", fmt.Errorf("platform: %s: %w", r.goos(), ErrUnsupportedPlatform)
	}
}

// RegistryDir resolves the registry directory: AGENTHUB_REGISTRY when set
// and non-empty, otherwise <data>/registry.
func (r *Resolver) RegistryDir() (string, error) {
	if v, ok := r.lookup(EnvRegistry); ok && v != "" {
		return v, nil
	}
	return r.dataSub("registry")
}

// LogsDir resolves <data>/logs.
func (r *Resolver) LogsDir() (string, error) { return r.dataSub("logs") }

// CacheDir resolves <data>/cache.
func (r *Resolver) CacheDir() (string, error) { return r.dataSub("cache") }

// StateDir resolves <data>/state.
func (r *Resolver) StateDir() (string, error) { return r.dataSub("state") }

// RunDir resolves the runtime directory (sockets, daemon.json).
//
// It is <data>/run on every platform, with one exception: on Linux, when the
// data directory is this platform's DEFAULT location, ${XDG_RUNTIME_DIR}/AgentHub
// is preferred instead (tmpfs, per-user 0700, cleared on logout). macOS and
// Windows have no equivalent worth special-casing. The directory itself must
// be created with EnsureDir, which enforces 0700 permissions.
//
// Why the exception is conditional. XDG_RUNTIME_DIR names ONE directory per
// user, so a run directory pinned to it is shared by every agenthub on the
// machine no matter which data directory each was told to use. A development
// build and an installed release, or two sandboxed test runs, would then all
// resolve the same <run>/ctl.sock: whoever binds first owns it and everyone
// else talks to a daemon that is not theirs, holding a registry that is not
// theirs. Making the run directory follow a relocated data directory is what
// "AGENTHUB_DATA_DIR moves everything, the socket included" always claimed —
// it was simply true only on macOS, where <data>/run is unconditional.
//
// The rule is deliberately a property of the ENVIRONMENT and not of the
// binary: a release-channel agenthub spawned by a development build (they
// share a PATH) computes the same run directory as its parent, because both
// read the same relocated data directory. A rule keyed on the build channel
// instead would make the two disagree exactly when one execs the other.
//
// AGENTHUB_SOCKET still outranks all of this — see CtlSocketPath.
func (r *Resolver) RunDir() (string, error) {
	switch r.goos() {
	case "linux":
		if !r.dataDirRelocated() {
			if v, ok := r.lookup("XDG_RUNTIME_DIR"); ok && v != "" && filepath.IsAbs(v) {
				return filepath.Join(v, dirName), nil
			}
		}
		return r.dataSub("run")
	case "darwin":
		return r.dataSub("run")
	case "windows":
		return r.windowsRunDir()
	default:
		return "", fmt.Errorf("platform: %s: %w", r.goos(), ErrUnsupportedPlatform)
	}
}

// dataDirRelocated reports whether the data directory is somewhere other than
// this platform's default location.
//
// It is true both when the operator exported AGENTHUB_DATA_DIR and when this
// is a development build, because DevResolver answers that same lookup. That
// conflation is the point rather than an accident of the implementation: the
// run-directory question is identical in the two cases — this process does not
// share a data directory with the default installation, so it must not share a
// control socket with it either.
func (r *Resolver) dataDirRelocated() bool {
	v, ok := r.lookup(EnvDataDir)
	return ok && v != ""
}

// CtlSocketPath resolves the control endpoint: AGENTHUB_SOCKET when set and
// non-empty, otherwise <run>/ctl.sock — except on Windows, where the
// endpoint is the named pipe \\.\pipe\agenthub-ctl-<sha8(SID)> and not a
// filesystem path at all (use IsPipePath before creating directories or
// changing permissions on the result).
func (r *Resolver) CtlSocketPath() (string, error) {
	if v, ok := r.lookup(EnvSocket); ok && v != "" {
		return v, nil
	}
	if r.goos() == "windows" {
		return r.windowsCtlEndpoint()
	}
	run, err := r.RunDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(run, "ctl.sock"), nil
}

// dataSub joins a subdirectory onto the data directory. Windows is spelled
// with backslashes explicitly (winJoin) rather than through filepath.Join,
// because the cross-platform tests resolve Windows paths from macOS/Linux
// and filepath.Join would separate them with "/" there — a path spelling
// that depends on the host is not a path spelling.
func (r *Resolver) dataSub(name string) (string, error) {
	data, err := r.DataDir()
	if err != nil {
		return "", err
	}
	if r.goos() == "windows" {
		return winJoin(data, name), nil
	}
	return filepath.Join(data, name), nil
}

// EnsureDir creates dir (and any missing parents) with 0700 permissions.
// If the leaf directory already exists with looser permissions it is
// tightened to 0700 — run/state directories hold sockets and credentials
// and must never be group/world accessible.
//
// Windows caveat (M2, unverified): Go's permission bits do not map onto
// Windows ACLs — os.Chmod there only toggles the read-only attribute — so
// this function does NOT restrict access on Windows. %APPDATA% is already
// per-user, and the control endpoint's protection is the pipe SDDL
// (CtlPipeSDDL), not a directory mode. Tightening the data directory ACL
// explicitly is tracked in docs/windows.md.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("platform: ensure dir: %w", err)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("platform: ensure dir: %w", err)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("platform: ensure dir: %w", err)
		}
	}
	return nil
}

// EnsureDirs applies EnsureDir to every path in order, stopping at the
// first error.
func EnsureDirs(dirs ...string) error {
	for _, d := range dirs {
		if err := EnsureDir(d); err != nil {
			return err
		}
	}
	return nil
}

// Package-level convenience wrappers over Default(). Prefer injecting a
// *Resolver in code that needs testability.

// DataDir resolves the data directory using the real process environment.
func DataDir() (string, error) { return Default().DataDir() }

// RegistryDir resolves the registry directory using the real process environment.
func RegistryDir() (string, error) { return Default().RegistryDir() }

// LogsDir resolves the logs directory using the real process environment.
func LogsDir() (string, error) { return Default().LogsDir() }

// CacheDir resolves the cache directory using the real process environment.
func CacheDir() (string, error) { return Default().CacheDir() }

// StateDir resolves the state directory using the real process environment.
func StateDir() (string, error) { return Default().StateDir() }

// RunDir resolves the run directory using the real process environment.
func RunDir() (string, error) { return Default().RunDir() }

// CtlSocketPath resolves the control socket path using the real process environment.
func CtlSocketPath() (string, error) { return Default().CtlSocketPath() }
