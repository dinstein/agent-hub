package transport

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// LookPath returns the absolute path of command as found in the PATH
// carried by env — the environment the child is about to be given — rather
// than in the one this process happens to have.
//
// That distinction is the whole reason this exists. exec.Command resolves its
// first argument through exec.LookPath, which reads THIS process's PATH;
// cmd.Env is only ever handed to the child, and never consulted for the
// lookup. So a caller that repairs the child's PATH — internal/downstream
// does, from the login shell, because launchd hands a GUI-launched process a
// four-entry PATH — would still watch every spawn fail with "executable file
// not found in $PATH" against a PATH the child was never going to run with.
//
// It is deliberately not a general exec.LookPath replacement:
//
//   - On Windows it returns command unchanged. Resolution there means PATHEXT
//     and its ordering, which this would have to reproduce exactly and which
//     no gate in this repository can verify on a real machine (docs/windows.md).
//     The truncated-PATH problem it exists for is launchd's and systemd's.
//   - A command containing a path separator is returned unchanged: exec
//     resolves those against Cwd and never consults PATH, so there is nothing
//     here to correct.
//   - A nil env is returned unchanged. Under exec.Cmd semantics that means the
//     child inherits this process's environment, so the two PATHs are one PATH
//     and exec's own lookup is already asking the right question.
func LookPath(command string, env []string) (string, error) {
	if runtime.GOOS == "windows" || command == "" || env == nil {
		return command, nil
	}
	if strings.ContainsAny(command, `/\`) {
		return command, nil
	}
	path, ok := pathFromEnv(env)
	if !ok {
		// The child is being given no PATH at all. Whatever exec would have
		// done with that is still the caller's decision, not ours to change.
		return command, nil
	}
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		// An empty entry is the current directory to POSIX and to
		// exec.LookPath. It is not one here: this resolves commands for a
		// spawn, and letting a directory nobody named decide which binary
		// runs is the one deviation worth taking, in the closed direction.
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, command)
		if executableFile(candidate) {
			return absOrSame(candidate), nil
		}
	}
	return "", fmt.Errorf("executable %q not found in the child's PATH (searched %s)", command, path)
}

// pathFromEnv returns the PATH value of a child environment. The LAST
// occurrence wins, matching os/exec's own deduplication of a slice that names
// the same variable twice.
func pathFromEnv(env []string) (string, bool) {
	val, found := "", false
	for _, kv := range env {
		name, v, ok := strings.Cut(kv, "=")
		if !ok || !isPathVar(name) {
			continue
		}
		val, found = v, true
	}
	return val, found
}

// executableFile reports whether p is a regular file with an execute bit.
// Symlinks are followed, since that is how most package managers publish a
// command.
func executableFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
