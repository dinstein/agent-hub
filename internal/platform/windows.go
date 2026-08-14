package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
)

// ────────────────────────────────────────────────────────────────────────────
// Windows path resolution (M2, ruling A.5 #23).
//
// NOT VERIFIED ON REAL HARDWARE. Everything below cross-compiles, is unit
// tested through the injectable Resolver hooks on macOS/Linux, and follows
// the MSIX lesson recorded in docs/status/windows.md — but no part of it has run on
// a Windows machine, let alone inside an MSIX container. Treat a surprise
// here as expected, not as a regression. See docs/status/windows.md.
//
// The MSIX problem, restated so the code below is readable:
//
//	An MSIX-packaged client (several MCP clients ship that way) spawns the
//	agenthub gateway INSIDE its own app container. Every write under
//	%APPDATA% is then silently redirected into that package's private
//	shadow directory — under any spelling of the path. The gateway reads
//	and writes the registry, the vault and the logs, so a redirected data
//	directory does not degrade gracefully: it forks the user's
//	configuration per client, invisibly.
//
//	Detection: GetCurrentPackageFamilyName. rc == APPMODEL_ERROR_NO_PACKAGE
//	(15700) means "no package identity" — the normal case, because agenthub
//	never ships as an MSIX package itself. ANY OTHER result means the
//	process inherited SOMEONE ELSE'S container.
//
//	Escape: the redirection filter keys on local paths, so the same
//	directory reached through a loopback UNC path (\\127.0.0.1\C$\Users\...)
//	is the real one. It is probed before being adopted (administrative
//	shares can be disabled), and a failed probe is announced loudly rather
//	than silently falling back to a redirected — i.e. wrong — directory.
// ────────────────────────────────────────────────────────────────────────────

// loopbackHost is the UNC host used for the twin path. 127.0.0.1 rather
// than "localhost" so no name resolution is involved.
const loopbackHost = "127.0.0.1"

// windowsAppDataEnv is the environment variable Windows uses for the
// roaming application data directory.
const windowsAppDataEnv = "APPDATA"

// PackageIdentity is the MSIX package identity of the current process.
type PackageIdentity struct {
	// Packaged is true when the process runs inside an app container.
	// agenthub is never packaged itself, so true always means "inherited
	// from the client that spawned us".
	Packaged bool
	// Family is the package family name when known ("" otherwise).
	Family string
}

// packageIdentity returns the process's package identity, using the
// injected hook when present. The real implementation is per-GOOS
// (packageid_windows.go / packageid_other.go) and is memoized: package
// identity cannot change during a process lifetime.
func (r *Resolver) packageIdentity() PackageIdentity {
	if r.PackageIdentity != nil {
		return r.PackageIdentity()
	}
	return currentPackageIdentity()
}

// probe reports whether a directory (or its nearest existing ancestor) is
// reachable. The default implementation stats; tests inject.
func (r *Resolver) probe(path string) error {
	if r.ProbePath != nil {
		return r.ProbePath(path)
	}
	return defaultProbePath(path)
}

// warn emits a loud, deduplicated operator warning. A redirected data
// directory is silent data loss, so this path must never be quiet.
func (r *Resolver) warn(msg string) {
	if r.Warn != nil {
		r.Warn(msg)
		return
	}
	defaultWarn(msg)
}

var (
	warnMu   sync.Mutex
	warnSeen = map[string]bool{}
)

// defaultWarn writes to stderr once per distinct message. Stderr, not
// stdout: on a stdio gateway stdout is the JSON-RPC frame stream and a
// stray line there corrupts the protocol.
func defaultWarn(msg string) {
	warnMu.Lock()
	seen := warnSeen[msg]
	if !seen {
		warnSeen[msg] = true
	}
	warnMu.Unlock()
	if !seen {
		fmt.Fprintln(os.Stderr, "agenthub: WARNING: "+msg)
	}
}

// defaultProbePath reports whether path — or the closest ancestor that
// exists — can be stat'ed. Walking up matters because the data directory
// itself usually does not exist yet on first run; what is being tested is
// whether the UNC route works at all.
func defaultProbePath(path string) error {
	p := path
	for range 8 {
		if _, err := os.Stat(p); err == nil {
			return nil
		}
		parent := parentWindowsPath(p)
		if parent == p || parent == "" {
			break
		}
		p = parent
	}
	return fmt.Errorf("platform: %s is not reachable", path)
}

// windowsDataDirNamed resolves %APPDATA%\<name>, applying the MSIX twin-path
// escape when this process turns out to be inside someone else's container.
// Both directory names run this one function, so the escape cannot apply to
// one build flavour and not the other.
//
// Callers reach it through dataDirNamed, below DataDir's AGENTHUB_DATA_DIR
// check — an explicit override is always obeyed verbatim, on every platform,
// including inside a container.
func (r *Resolver) windowsDataDirNamed(name string) (string, error) {
	appData, ok := r.lookup(windowsAppDataEnv)
	if !ok || appData == "" {
		home, err := r.home()
		if err != nil {
			return "", fmt.Errorf("platform: resolve home: %w", err)
		}
		appData = winJoin(home, "AppData", "Roaming")
	}
	base := winJoin(appData, name)

	id := r.packageIdentity()
	if !id.Packaged {
		return base, nil
	}
	twin, ok := loopbackUNCPath(base)
	if !ok {
		r.warn(fmt.Sprintf(
			"running inside MSIX package container %q and %s has no drive letter, so the "+
				"redirection escape does not apply; agenthub data may be written to the "+
				"package's private shadow directory", identityName(id), base))
		return base, nil
	}
	if err := r.probe(twin); err != nil {
		r.warn(fmt.Sprintf(
			"running inside MSIX package container %q: %%APPDATA%% is redirected to that "+
				"package's private store, and the loopback-UNC escape %s is unreachable (%v). "+
				"agenthub is falling back to %s, which this client will NOT share with your "+
				"other clients. Fix: enable the administrative share, or set AGENTHUB_DATA_DIR "+
				"to a path outside %%APPDATA%%",
			identityName(id), twin, err, base))
		return base, nil
	}
	return twin, nil
}

func identityName(id PackageIdentity) string {
	if id.Family != "" {
		return id.Family
	}
	return "<unknown family>"
}

// windowsRunDir returns <data>\run. Windows has no XDG runtime directory
// and no tmpfs equivalent worth special-casing: the control endpoint is a
// named pipe, not a file, so the run directory only holds daemon.json.
func (r *Resolver) windowsRunDir() (string, error) { return r.dataSub("run") }

// ctlPipePrefix and devCtlPipePrefix are the two control-pipe names, one per
// build channel. Both are FROZEN identifiers (docs/conventions.md#frozen-identifiers/§2).
//
// Two spelled-out constants rather than one with the channel spliced in. The
// pipe name must not move when the data directory is renamed — deriving it
// from dirName was tried once, and the result was that "rename the data
// directory" silently became "rename the protocol". A dev channel obtained by
// interpolating into the release name has the same defect one level down: the
// release name stops being a literal you can grep for and starts being an
// output of string concatenation.
const (
	ctlPipePrefix    = `\\.\pipe\agenthub-ctl-`
	devCtlPipePrefix = `\\.\pipe\agenthub-ctl-dev-`
)

// windowsCtlEndpoint returns the control-plane named pipe path
// \\.\pipe\agenthub-ctl-<sha8(SID)>, or its dev sibling
// \\.\pipe\agenthub-ctl-dev-<sha8(SID)> on a Resolver from DevResolver.
//
// The SID hash is not obfuscation: pipe names live in a single machine-wide
// namespace, so on a multi-user machine two users would otherwise race for
// the same name and the loser would be talking to the winner's daemon. The
// hash keeps the name stable per user and unique across users; the actual
// access control is the SDDL on the pipe (see CtlPipeSDDL).
//
// WHY THE CHANNEL IS A FIELD AND NOT A LOOKUP. On Unix the split reaches the
// endpoint for free: the socket is <run>/ctl.sock, the run directory follows
// the data directory, and DevResolver moves the data directory. A pipe name is
// not a filesystem path, so that chain does not exist here and the resolver
// has to be told. Asking "does the data directory end in AgentHubDev?" would
// re-introduce exactly the dirName derivation the constants above refuse.
func (r *Resolver) windowsCtlEndpoint() (string, error) {
	sid, err := r.userSID()
	if err != nil {
		return "", err
	}
	if r.devChannel {
		return devCtlPipePrefix + sha8(sid), nil
	}
	return ctlPipePrefix + sha8(sid), nil
}

func (r *Resolver) userSID() (string, error) {
	if r.UserSID != nil {
		return r.UserSID()
	}
	sid, err := currentUserSID()
	if err != nil {
		return "", fmt.Errorf("platform: resolve current user SID: %w", err)
	}
	return sid, nil
}

// sha8 is the first 8 hex characters of the SHA-256 of s.
func sha8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// CtlPipeSDDL returns the security descriptor for the control pipe: full
// control for the pipe's owner (the current user) and NOTHING for anyone
// else — no Administrators ACE, no SYSTEM ACE.
//
// This is the Windows equivalent of the Unix pair "0700 directory +
// SO_PEERCRED", and it is deliberately stricter than the usual Windows
// default: the control plane hands out every downstream credential and
// approves tool calls, so "an administrator can also connect" is not a
// property worth having. Local single user is the whole threat model
// (docs/architecture.md#the-processes).
//
// D:P               discretionary ACL, protected (no inherited ACEs)
// (A;;GA;;;<SID>)   allow, generic all, to that SID exactly
func CtlPipeSDDL(sid string) string {
	return "D:P(A;;GA;;;" + sid + ")"
}

// CtlPipeSDDL resolves the current user's SID and renders the control-pipe
// security descriptor. It is the seam the Windows control-plane listener
// consumes; see docs/status/windows.md for why the listener itself is not built
// yet (it needs a named-pipe implementation, i.e. a new dependency).
func (r *Resolver) CtlPipeSDDL() (string, error) {
	sid, err := r.userSID()
	if err != nil {
		return "", err
	}
	return CtlPipeSDDL(sid), nil
}

// IsPipePath reports whether p names a Windows named pipe rather than a
// filesystem path. Callers that create parent directories or chmod the
// endpoint must skip both for a pipe.
func IsPipePath(p string) bool {
	return strings.HasPrefix(p, `\\.\pipe\`) || strings.HasPrefix(p, `\\?\pipe\`)
}

// ── path helpers ────────────────────────────────────────────────────────────
//
// Windows paths are built with explicit backslashes instead of
// filepath.Join, because these functions must produce identical results
// whatever host they run on: the cross-platform unit tests resolve Windows
// paths from macOS and Linux, and filepath.Join would join them with "/"
// there. A path spelling that changes with the host is not a path spelling.

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

func parentWindowsPath(p string) string {
	p = strings.TrimRight(p, `\`)
	i := strings.LastIndex(p, `\`)
	if i < 0 {
		return ""
	}
	parent := p[:i]
	// \\host\share is the shortest meaningful UNC prefix; do not walk into
	// \\host, which is not a path.
	if strings.HasPrefix(parent, `\\`) && strings.Count(parent, `\`) < 4 {
		return ""
	}
	return parent
}

func isDriveLetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// loopbackUNCPath rewrites C:\Users\alice\... into
// \\127.0.0.1\C$\Users\alice\..., the administrative-share twin of the same
// directory. ok=false for anything that is not a drive-letter path (a UNC
// path is already outside the redirection filter, and a relative path is
// not a location).
func loopbackUNCPath(p string) (string, bool) {
	p = strings.ReplaceAll(p, "/", `\`)
	if len(p) < 3 || p[1] != ':' || p[2] != '\\' {
		return "", false
	}
	drive := p[0]
	if !isDriveLetter(drive) {
		return "", false
	}
	return `\\` + loopbackHost + `\` + strings.ToUpper(string(drive)) + `$\` + p[3:], true
}
