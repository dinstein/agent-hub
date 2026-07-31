package cli

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// This file answers two questions for `auth login`: can this host show a
// browser, and how does it open one. Both are deliberately conservative —
// a wrong "yes" strands the user in front of a loopback listener that will
// never receive a callback, while a wrong "no" only costs one paste.

// canOpenBrowser reports whether launching a browser on THIS host would put
// a window in front of the user who is running the command.
//
// Failure direction: answer NO when unsure. A remote session is the case
// that matters — `xdg-open` on an SSH host either fails or, worse,
// succeeds on a display nobody is looking at, and the CLI would then wait
// out the full loopback timeout for a callback that cannot arrive.
func canOpenBrowser() bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CLIENT") != "" {
		return false
	}
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	default:
		// A Unix desktop without a display server has no browser to open.
		return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	}
}

// openBrowser launches the platform handler for url.
//
// It refuses anything that is not http(s): the URL comes from an
// authorization-server metadata document, and handing a `file://` or custom
// scheme to the system opener is arbitrary local-handler invocation with a
// remote-controlled argument.
func openBrowser(url string) error {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return fmt.Errorf("refusing to open browser for a non-http url")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	// Detach the streams: a handler that chatters on stdout would corrupt
	// the NDJSON progress stream this command is writing.
	cmd.Stdout = nil
	cmd.Stderr = nil
	// And detach the environment, which is the same discipline one field
	// over. This process holds AGENTHUB_SECRET_KEY, every AGENTHUB_SECRET_*
	// value and any bare secret variable the operator opted in; inheriting
	// them would hand the lot to the opener, to the browser it launches and
	// to everything the browser itself spawns, readable through
	// /proc/<pid>/environ for as long as any of them live.
	cmd.Env = browserEnv()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	// The opener forks the real browser and exits; reaping it here keeps no
	// zombie behind and never blocks on the browser itself.
	go func() { _ = cmd.Wait() }()
	return nil
}

// browserEnvNames is what a platform handler is given, and nothing else.
//
// An ALLOW list rather than a deny list, for the reason AGENTS.md gives for
// tool selectors: the two answer "a variable that appears tomorrow" in
// opposite directions, and here tomorrow's variable is a credential — a
// bare secret name the operator opts in to is chosen by the operator, so no
// deny list can enumerate them. The cost of an omission is a handler that
// misbehaves; the cost of an over-broad rule is a token in a browser's
// environment.
//
// Each name is here because the opener needs it: the handler must be found
// (PATH, COMSPEC, PATHEXT), it must know whose session this is (HOME, USER,
// LOGNAME, the Windows profile directories), it must be able to reach the
// display server (DISPLAY, WAYLAND_DISPLAY, XAUTHORITY, XDG_*,
// DBUS_SESSION_BUS_ADDRESS — xdg-open goes through the desktop portal), and
// it must have somewhere to write (TMPDIR, TEMP, TMP). Go's os/exec adds
// SYSTEMROOT itself on Windows when it is absent.
func browserEnvNames() []string {
	common := []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR"}
	switch runtime.GOOS {
	case "darwin":
		// __CF_USER_TEXT_ENCODING is what Core Foundation reads to pick the
		// session's encoding; `open` inherits it from a real login session.
		return append(common, "__CF_USER_TEXT_ENCODING")
	case "windows":
		return []string{
			"PATH", "PATHEXT", "COMSPEC", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR",
			"USERPROFILE", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA",
			"PROGRAMFILES", "PROGRAMFILES(X86)", "TEMP", "TMP", "USERNAME",
		}
	default:
		return append(common,
			"DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "DBUS_SESSION_BUS_ADDRESS",
			"XDG_RUNTIME_DIR", "XDG_SESSION_TYPE", "XDG_CURRENT_DESKTOP",
			"XDG_DATA_DIRS", "XDG_CONFIG_DIRS", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
		)
	}
}

// browserEnv renders browserEnvNames from this process's environment.
//
// It never returns nil: os/exec reads a nil Env as "inherit everything",
// which is the failure this function exists to prevent, so an environment
// holding none of these names must still produce an empty non-nil slice.
func browserEnv() []string {
	names := browserEnvNames()
	env := make([]string, 0, len(names))
	for _, k := range names {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}
