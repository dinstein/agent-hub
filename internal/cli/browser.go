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
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	// The opener forks the real browser and exits; reaping it here keeps no
	// zombie behind and never blocks on the browser itself.
	go func() { _ = cmd.Wait() }()
	return nil
}
