package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrUnsupportedPlatform is returned by DefaultSocketPath on platforms
// without a resolved default (anything other than darwin, linux and
// windows). Test with errors.Is.
var ErrUnsupportedPlatform = errors.New("unsupported platform")

// Frozen AGENTHUB_* environment variable names (ABI since v1). They mirror
// internal/platform, which this package must not import (depguard rule 1).
const (
	envSocket  = "AGENTHUB_SOCKET"
	envDataDir = "AGENTHUB_DATA_DIR"
	// dirName is the data directory name. It must equal internal/platform's
	// dirName — this package cannot import it (depguard rule 1), so the two
	// copies are held together by the contract test in internal/ctlapi
	// rather than by the compiler.
	dirName = "AgentHub"
	// devDirName is the data directory a DEVELOPMENT build uses instead.
	// Same contract as dirName: it must equal internal/platform.DevDirName,
	// and the pair is held together by a test rather than by the compiler.
	//
	// This package needs it because the GUI resolves its paths through here
	// and cannot import internal/platform either. Without it a development
	// GUI reads the installed release's data — which is the separation the
	// dev channel exists to provide, silently absent on one side of it.
	devDirName = "AgentHubDev"
	// ctlSocketName is the frozen control socket file name.
	ctlSocketName = "ctl.sock"
	// windowsAppDataEnv is where Windows keeps roaming application data.
	windowsAppDataEnv = "APPDATA"
	// The two control-pipe names, one per build channel. Frozen identifiers
	// (canonical.md §1), and under the same contract as dirName above: each
	// must equal internal/platform's spelling, and only a test holds them
	// together. Spelled out whole rather than one derived from the other, for
	// the reason internal/platform gives at length: a name that is the output
	// of a concatenation is a name nobody can grep for.
	ctlPipePrefix    = `\\.\pipe\agenthub-ctl-`
	devCtlPipePrefix = `\\.\pipe\agenthub-ctl-dev-`
)

// DefaultSocketPath resolves the control socket path for the current
// process environment, byte-identically to internal/platform.CtlSocketPath.
func DefaultSocketPath() (string, error) {
	return defaultSocketPath(runtime.GOOS, os.LookupEnv, os.UserHomeDir, currentUserSID)
}

// DevSocketPath is DefaultSocketPath for a DEVELOPMENT build, byte-identically
// to internal/platform.DevResolver(nil).CtlSocketPath.
//
// It exists for the same caller as DevDataDir — cmd/agenthub-gui, which cannot
// import internal/platform — and for one platform. Everywhere else the dev
// channel reaches the endpoint for free: the socket sits under the run
// directory, which follows the data directory, which the GUI already overrides
// with DevDataDir. A Windows control endpoint is a named pipe and follows
// nothing, so a development GUI would dial the pipe an INSTALLED RELEASE is
// serving and operate on real user data while looking correct — the exact
// failure the channel split exists to prevent, and one that has already
// happened once on Linux for the same structural reason.
func DevSocketPath() (string, error) {
	return devSocketPath(runtime.GOOS, os.LookupEnv, os.UserHomeDir, currentUserSID)
}

// defaultSocketPath re-implements internal/platform's resolution chain:
//
//  1. AGENTHUB_SOCKET, when set and non-empty (any platform).
//  2. On windows: \\.\pipe\agenthub-ctl-<sha8(SID)>. The endpoint is a named
//     pipe, so it is NOT under the run directory and does not follow the data
//     directory at all — see devSocketPath for what that costs.
//  3. Otherwise <run>/ctl.sock, where <run> is:
//     - linux: ${XDG_RUNTIME_DIR}/AgentHub when XDG_RUNTIME_DIR is an
//     absolute path AND AGENTHUB_DATA_DIR is unset, else <data>/run;
//     - darwin: <data>/run;
//     and <data> is:
//     - AGENTHUB_DATA_DIR, when set and non-empty;
//     - darwin: ~/Library/Application Support/AgentHub;
//     - linux: ${XDG_DATA_HOME}/AgentHub when XDG_DATA_HOME is an absolute
//     path (relative values are ignored per the XDG spec), else
//     ~/.local/share/agenthub.
//
// CONTRACT: this must stay byte-identical to
// internal/platform.(*Resolver).CtlSocketPath. api cannot import
// internal/platform (canonical.md §2 rule 1), so the logic is duplicated
// here on purpose; the cross-package contract test pinning both sides
// together lives on the internal/ctlapi side, which may import both.
func defaultSocketPath(goos string, lookup func(string) (string, bool), home func() (string, error), sid func() (string, error)) (string, error) {
	if v, ok := lookup(envSocket); ok && v != "" {
		return v, nil
	}
	if goos == "windows" {
		return ctlPipeName(ctlPipePrefix, sid)
	}
	run, err := runDir(goos, lookup, home)
	if err != nil {
		return "", err
	}
	return filepath.Join(run, ctlSocketName), nil
}

// devSocketPath is defaultSocketPath for a development build. See
// DevSocketPath for why only one platform needs it.
//
// CONTRACT: byte-identical to
// internal/platform.DevResolver(nil).CtlSocketPath.
func devSocketPath(goos string, lookup func(string) (string, bool), home func() (string, error), sid func() (string, error)) (string, error) {
	// An explicit override still wins, the same precedence DevResolver and
	// DevDataDir use: the caller named an endpoint and must get that endpoint.
	if v, ok := lookup(envSocket); ok && v != "" {
		return v, nil
	}
	if goos == "windows" {
		return ctlPipeName(devCtlPipePrefix, sid)
	}
	// Everywhere else the endpoint follows the data directory, so the dev
	// endpoint is just the release resolution over the dev directory. Note
	// what this does NOT consult: XDG_RUNTIME_DIR. internal/platform skips it
	// whenever the data directory has been relocated, and a dev build has
	// relocated it by definition — putting the socket in the shared runtime
	// directory is precisely how a dev build and a release came to share one.
	data, err := devDataDir(goos, lookup, home)
	if err != nil {
		return "", err
	}
	return filepath.Join(data, "run", ctlSocketName), nil
}

// ctlPipeName renders one of the two frozen pipe names for the current user.
//
// The SID is hashed for the reason internal/platform gives: pipe names live in
// one machine-wide namespace, so without it two users on the same machine race
// for a single name and the loser talks to the winner's daemon.
func ctlPipeName(prefix string, sid func() (string, error)) (string, error) {
	s, err := sid()
	if err != nil {
		return "", fmt.Errorf("api: resolve current user SID: %w", err)
	}
	return prefix + sha8(s), nil
}

func runDir(goos string, lookup func(string) (string, bool), home func() (string, error)) (string, error) {
	switch goos {
	case "linux":
		// XDG_RUNTIME_DIR is consulted only while the data directory is still
		// the platform default. It names one directory per user, so a socket
		// placed there is shared by every agenthub on the machine regardless of
		// which data directory each was pointed at — see the long form on
		// internal/platform.(*Resolver).RunDir.
		if v, ok := lookup(envDataDir); !ok || v == "" {
			if v, ok := lookup("XDG_RUNTIME_DIR"); ok && v != "" && filepath.IsAbs(v) {
				return filepath.Join(v, dirName), nil
			}
		}
	case "darwin":
		// Fall through to <data>/run.
	case "windows":
		// Also <data>\run, and it holds only daemon.json: the control endpoint
		// is a pipe rather than a file in here. There is no XDG equivalent and
		// no tmpfs worth special-casing.
		data, err := dataDir(goos, lookup, home)
		if err != nil {
			return "", err
		}
		return winJoin(data, "run"), nil
	default:
		return "", fmt.Errorf("api: %s: %w", goos, ErrUnsupportedPlatform)
	}
	data, err := dataDir(goos, lookup, home)
	if err != nil {
		return "", err
	}
	return filepath.Join(data, "run"), nil
}

func dataDir(goos string, lookup func(string) (string, bool), home func() (string, error)) (string, error) {
	if v, ok := lookup(envDataDir); ok && v != "" {
		return v, nil
	}
	switch goos {
	case "darwin":
		h, err := home()
		if err != nil {
			return "", fmt.Errorf("api: resolve home: %w", err)
		}
		return filepath.Join(h, "Library", "Application Support", dirName), nil
	case "linux":
		if v, ok := lookup("XDG_DATA_HOME"); ok && v != "" && filepath.IsAbs(v) {
			return filepath.Join(v, dirName), nil
		}
		h, err := home()
		if err != nil {
			return "", fmt.Errorf("api: resolve home: %w", err)
		}
		return filepath.Join(h, ".local", "share", dirName), nil
	case "windows":
		return windowsDataDir(dirName, lookup, home)
	default:
		return "", fmt.Errorf("api: %s: %w", goos, ErrUnsupportedPlatform)
	}
}

// DevDataDir resolves the data directory a DEVELOPMENT build must use,
// byte-identically to internal/platform's dev resolution.
//
// An explicit AGENTHUB_DATA_DIR still wins, exactly as it does for the
// release path: an override that stopped being honoured because a build is a
// development one would be a surprise in the direction nobody wants — the
// caller asked for a specific directory and got a different one.
//
// It is exported for one caller: cmd/agenthub-gui, which has to know where a
// development build keeps its data and cannot import internal/platform.
func DevDataDir() (string, error) {
	return devDataDir(runtime.GOOS, os.LookupEnv, os.UserHomeDir)
}

func devDataDir(goos string, lookup func(string) (string, bool), home func() (string, error)) (string, error) {
	if v, ok := lookup(envDataDir); ok && v != "" {
		return v, nil
	}
	switch goos {
	case "darwin":
		h, err := home()
		if err != nil {
			return "", fmt.Errorf("api: resolve home: %w", err)
		}
		return filepath.Join(h, "Library", "Application Support", devDirName), nil
	case "linux":
		if v, ok := lookup("XDG_DATA_HOME"); ok && v != "" && filepath.IsAbs(v) {
			return filepath.Join(v, devDirName), nil
		}
		h, err := home()
		if err != nil {
			return "", fmt.Errorf("api: resolve home: %w", err)
		}
		return filepath.Join(h, ".local", "share", devDirName), nil
	case "windows":
		return windowsDataDir(devDirName, lookup, home)
	default:
		return "", fmt.Errorf("api: %s: %w", goos, ErrUnsupportedPlatform)
	}
}

// windowsDataDir resolves %APPDATA%\<name>, falling back to the roaming
// directory under the user's home when APPDATA is missing.
//
// WHAT THIS DELIBERATELY OMITS. internal/platform's Windows branch does one
// more thing: when the process turns out to be running inside somebody else's
// MSIX app container, every write under %APPDATA% is silently redirected into
// that package's private shadow store, and it escapes through a loopback-UNC
// twin path (\\127.0.0.1\C$\Users\...). That escape is NOT duplicated here, and
// the omission is the point rather than an oversight.
//
// The escape exists because an MSIX-packaged MCP client SPAWNS the agenthub
// gateway inside its own container. Nothing that reaches this function can be
// in that position: these resolvers serve cmd/agenthub-gui, a desktop
// application the user launches, and the gateway resolves its paths through
// internal/platform instead. Duplicating a container probe and a UNC
// reachability check into the public api package would add two syscalls' worth
// of untestable code to cover a case its only caller cannot be in — and the
// contract test pins this against a platform resolver that is not packaged,
// which is the same statement in executable form.
func windowsDataDir(name string, lookup func(string) (string, bool), home func() (string, error)) (string, error) {
	appData, ok := lookup(windowsAppDataEnv)
	if !ok || appData == "" {
		h, err := home()
		if err != nil {
			return "", fmt.Errorf("api: resolve home: %w", err)
		}
		appData = winJoin(h, "AppData", "Roaming")
	}
	return winJoin(appData, name), nil
}

// winJoin joins Windows path elements with explicit backslashes instead of
// filepath.Join, mirroring internal/platform's helper of the same name and for
// the same reason: these functions must produce identical results whatever host
// they run on, and the contract test that compares them runs on macOS and
// Linux, where filepath.Join would use "/". A path spelling that changes with
// the host is not a path spelling.
func winJoin(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for i, p := range parts {
		p = strings.ReplaceAll(p, "/", `\`)
		if i > 0 {
			p = strings.TrimLeft(p, `\`)
		}
		p = strings.TrimRight(p, `\`)
		if p == "" {
			continue
		}
		cleaned = append(cleaned, p)
	}
	return strings.Join(cleaned, `\`)
}

// sha8 is the first 8 hex characters of the SHA-256 of s, the same digest
// internal/platform hashes a SID with. Both halves of a pipe name are frozen,
// so this must not change either.
func sha8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// isPipePath reports whether p names a Windows named pipe rather than a
// filesystem path — the api-side copy of platform.IsPipePath. Callers that
// would otherwise take the directory of the endpoint, or dial it as a socket,
// have to ask first.
func isPipePath(p string) bool {
	return strings.HasPrefix(p, `\\.\pipe\`) || strings.HasPrefix(p, `\\?\pipe\`)
}
