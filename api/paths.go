package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ErrUnsupportedPlatform is returned by DefaultSocketPath on platforms
// without a resolved default (currently anything other than darwin and
// linux; Windows named pipes arrive in M2). Test with errors.Is.
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
)

// DefaultSocketPath resolves the control socket path for the current
// process environment, byte-identically to internal/platform.CtlSocketPath.
func DefaultSocketPath() (string, error) {
	return defaultSocketPath(runtime.GOOS, os.LookupEnv, os.UserHomeDir)
}

// defaultSocketPath re-implements internal/platform's resolution chain:
//
//  1. AGENTHUB_SOCKET, when set and non-empty (any platform).
//  2. Otherwise <run>/ctl.sock, where <run> is:
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
func defaultSocketPath(goos string, lookup func(string) (string, bool), home func() (string, error)) (string, error) {
	if v, ok := lookup(envSocket); ok && v != "" {
		return v, nil
	}
	run, err := runDir(goos, lookup, home)
	if err != nil {
		return "", err
	}
	return filepath.Join(run, ctlSocketName), nil
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
	default:
		return "", fmt.Errorf("api: %s: %w", goos, ErrUnsupportedPlatform)
	}
}
