package clients

import (
	"errors"
	"fmt"
	"io/fs"
)

// ParseError reports an existing configuration file that could not be
// parsed. The file is left untouched: refusing to write is the whole point
// of this error. Hint carries actionable text (e.g. "this file uses JSONC
// comments") and Snippet the fragment to paste by hand.
type ParseError struct {
	Path    string
	Client  string
	Err     error
	Hint    string
	Snippet string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("client configuration %s is not valid JSON: %v", e.Path, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// NotConnectedError reports a Disconnect that found nothing to remove:
// either the file does not exist or no entry in it is owned by agenthub.
type NotConnectedError struct {
	Path string
}

func (e *NotConnectedError) Error() string {
	return fmt.Sprintf("no agenthub-owned entry in %s", e.Path)
}

// TooLargeError reports a configuration file above MaxConfigSize. The
// limit is checked from the stat size before any read, so a runaway file
// never costs more than one stat.
type TooLargeError struct {
	Path  string
	Size  int64
	Limit int64
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("client configuration %s is %d bytes, above the %d byte limit", e.Path, e.Size, e.Limit)
}

// PermissionError reports a configuration file agenthub is not allowed to
// touch. On macOS this is overwhelmingly TCC: the process
// lacks Full Disk Access / App Data access for the owning application.
//
// This type must never be collapsed into "not found". A missing file means
// "the client is not installed, nothing to do"; a denied file means "the
// client IS installed and you must grant access" — opposite user actions,
// and opposite HTTP statuses (403, not 404) once ctlapi surfaces them.
type PermissionError struct {
	// Path is the file that could not be accessed.
	Path string
	// Client is the owning client ID ("" when unknown).
	Client string
	// Op is "stat", "read" or "write" — which access was refused.
	Op string
	// Remediation is user-facing text explaining how to grant access.
	Remediation string
	// Err is the underlying syscall error.
	Err error
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("permission denied (%s) for client configuration %s: %v", e.Op, e.Path, e.Err)
}

func (e *PermissionError) Unwrap() error { return e.Err }

// HTTPStatus is the status ctlapi must map this error to. It is stated
// here rather than in ctlapi so the "403, never 404" rule travels with the
// error type.
func (e *PermissionError) HTTPStatus() int { return 403 }

// UnsupportedError reports an operation agenthub deliberately does not
// perform on this client: rewriting a non-JSON document (codex TOML,
// continue YAML) or a client with no local configuration at all
// (open-webui). Snippet always carries what the user should do instead —
// this error is a redirection, not a dead end.
type UnsupportedError struct {
	Client string
	Op     string // "connect" | "disconnect" | "import"
	Shape  Shape
	Path   string
	// Snippet is the manual configuration fragment or instructions.
	Snippet string
}

func (e *UnsupportedError) Error() string {
	if e.Shape == ShapeRemote {
		return fmt.Sprintf("client %s has no local MCP configuration file; %s is not supported", e.Client, e.Op)
	}
	return fmt.Sprintf("client %s stores MCP configuration as %s (%s); agenthub does not rewrite it, %s is not supported",
		e.Client, e.Shape, e.Path, e.Op)
}

// UnknownClientError reports an unregistered client ID.
type UnknownClientError struct {
	Client string
}

func (e *UnknownClientError) Error() string {
	return fmt.Sprintf("unknown client %q", e.Client)
}

// IsPermission reports whether err is (or wraps) a *PermissionError.
func IsPermission(err error) bool {
	var pe *PermissionError
	return errors.As(err, &pe)
}

// classifyAccess converts a filesystem error into *PermissionError when it
// is a denial, and returns nil otherwise (including for not-exist, which
// callers handle as "client not installed"). Failure direction: anything
// ambiguous is NOT reported as a denial, so a transient I/O error can never
// masquerade as a TCC prompt.
func (t *Table) classifyAccess(err error, path, client, op string) *PermissionError {
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return nil
	}
	return &PermissionError{
		Path:        path,
		Client:      client,
		Op:          op,
		Remediation: remediation(t.goos()),
		Err:         err,
	}
}

// remediation returns the platform-specific text for a denied access.
func remediation(goos string) string {
	if goos == "darwin" {
		return "macOS denied access to another application's data. " +
			"Grant Full Disk Access to the program running agenthub in " +
			"System Settings > Privacy & Security > Full Disk Access, then retry. " +
			"agenthub only reads MCP server definitions and never reads a client's data on its own."
	}
	return "the file exists but is not readable by this user; check its ownership and mode, then retry"
}
