package cli

import (
	"errors"
	"fmt"

	"github.com/dinstein/agent-hub/internal/cli/output"
	"github.com/dinstein/agent-hub/internal/registry"
)

// Exit codes, frozen by docs/modules/controlplane.md. The mapping from error values to
// these codes lives in ExitCodeFor and nowhere else.
const (
	ExitOK         = 0 // success
	ExitGeneral    = 1 // generic error (downstream / network / internal)
	ExitUsage      = 2 // usage error (bad args, unknown flag/command)
	ExitNotFound   = 3 // resource not found (server/profile/secret/skill/session/tool/token)
	ExitDaemonDown = 4 // daemon offline but the command requires it
	ExitAuth       = 5 // authentication / authorization failure
	ExitDenied     = 6 // refused by a guard (spawnguard, a skill integrity pin)
	ExitLocked     = 7 // any store's lock contention timeout, or state corrupt
)

// Stable machine-readable error codes for the --json failure envelope.
// New codes may be added; existing ones must never change meaning.
const (
	CodeGeneral              = "E_GENERAL"
	CodeUsage                = "E_USAGE"
	CodeNotFound             = "E_NOT_FOUND"
	CodeServerNotFound       = "E_SERVER_NOT_FOUND"
	CodeServerExists         = "E_SERVER_EXISTS"
	CodeDaemonDown           = "E_DAEMON_DOWN"
	CodeDaemonRunning        = "E_DAEMON_RUNNING"
	CodeDaemonUnowned        = "E_DAEMON_UNOWNED"
	CodeAuthFailed           = "E_AUTH_FAILED"
	CodeDenied               = "E_GOVERNANCE_DENIED"
	CodeLockTimeout          = "E_LOCK_TIMEOUT"
	CodeRegistryCorrupt      = "E_REGISTRY_CORRUPT"
	CodeInvalidJSON          = "E_INVALID_JSON"
	CodeUnsupportedTransport = "E_UNSUPPORTED_TRANSPORT"
	CodeClientUnsupported    = "E_CLIENT_UNSUPPORTED"
	CodeClientNotConnected   = "E_CLIENT_NOT_CONNECTED"
	CodeNotImplemented       = "E_NOT_IMPLEMENTED"
	CodeProfileNotFound      = "E_PROFILE_NOT_FOUND"
	CodeProfileExists        = "E_PROFILE_EXISTS"
	CodeSessionNotFound      = "E_SESSION_NOT_FOUND"
	CodeToolNotFound         = "E_TOOL_NOT_FOUND"
	CodeSkillNotFound        = "E_SKILL_NOT_FOUND"
	CodeSkillExists          = "E_SKILL_EXISTS"
	CodeSecretNotFound       = "E_SECRET_NOT_FOUND"
	CodeConfigKeyUnknown     = "E_CONFIG_KEY_UNKNOWN"
	CodeStateCorrupt         = "E_STATE_CORRUPT"
)

// Error is the typed CLI error: it carries the stable machine code for the
// JSON envelope, the process exit code, and an optional hint for humans.
// Commands should return *Error (or a registry sentinel) for every failure
// they can classify; anything else maps to ExitGeneral.
type Error struct {
	Code     string // stable machine-readable code (Code* constants)
	ExitCode int    // process exit code (Exit* constants)
	Message  string
	Hint     string
	Err      error // optional wrapped cause
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// Usagef builds a usage error (exit 2). Cobra flag/argument errors are
// funneled into this type as well, so "cobra parse error => exit 2" holds by
// construction.
func Usagef(format string, a ...any) *Error {
	return &Error{Code: CodeUsage, ExitCode: ExitUsage, Message: fmt.Sprintf(format, a...)}
}

// NotFoundf builds a resource-not-found error (exit 3) with a specific code
// such as CodeServerNotFound.
func NotFoundf(code, format string, a ...any) *Error {
	return &Error{Code: code, ExitCode: ExitNotFound, Message: fmt.Sprintf(format, a...)}
}

// DaemonDownf builds an "online-only command, daemon offline" error (exit 4).
// Unused until M1 introduces online-only commands; part of the frozen table.
func DaemonDownf(format string, a ...any) *Error {
	return &Error{
		Code: CodeDaemonDown, ExitCode: ExitDaemonDown,
		Message: fmt.Sprintf(format, a...),
		// The hub is the application's now: opening AgentHub starts one, and
		// naming `daemon start` first would send every user down the path
		// that refuses them (daemonowner.go).
		Hint: "open AgentHub, or run an operator-owned hub with 'agenthub daemon start --headless'",
	}
}

// AuthFailedf builds an authentication failure error (exit 5).
//
// It is the table's declaration of that row rather than the way the row is
// normally reached: every production exit 5 — an OAuth flow, a downstream
// answering 401/403 to `server test`, a secret file that will not decrypt —
// carries a Hint as well, which this constructor has no parameter for, so
// those sites build the Error literally. Deleting it would leave the row with
// no named constructor while its siblings keep theirs.
func AuthFailedf(format string, a ...any) *Error {
	return &Error{Code: CodeAuthFailed, ExitCode: ExitAuth, Message: fmt.Sprintf(format, a...)}
}

// Deniedf builds a guard-refusal error (exit 6). Same standing as AuthFailedf
// above: the two things that actually exit 6 — a skill's integrity pin and the
// spawn guard screening a generated `docker run` — both need a Hint and build
// the Error literally.
//
// It used to say "reserved for the M1 governance gates". There are none: the
// approval queue and the runtime scope change were removed rather than left
// half-wired (AGENTS.md), so a reader looking for the gate this constructor
// names would be looking for something that does not exist.
func Deniedf(format string, a ...any) *Error {
	return &Error{Code: CodeDenied, ExitCode: ExitDenied, Message: fmt.Sprintf(format, a...)}
}

// silentExitError carries a bare exit code for commands that have already
// rendered their outcome through the output layer (doctor's per-check
// statuses, for example) and must not have a second error printed by Main.
type silentExitError struct{ code int }

func (e *silentExitError) Error() string { return fmt.Sprintf("exit code %d", e.code) }

// ExitCodeFor maps any error returned by command execution to the frozen
// exit-code table. Classification order:
//
//  1. nil                          -> 0
//  2. *silentExitError             -> its code (already reported)
//  3. *Error                       -> its ExitCode
//  4. registry lock timeout        -> 7
//  5. registry unreadable (corrupt registry that could not be healed,
//     since healed quarantines are downgraded to warnings before they
//     ever reach Main)             -> 7
//  6. anything else               -> 1
func ExitCodeFor(err error) int {
	if err == nil {
		return ExitOK
	}
	var silent *silentExitError
	if errors.As(err, &silent) {
		return silent.code
	}
	var ce *Error
	if errors.As(err, &ce) {
		return ce.ExitCode
	}
	if errors.Is(err, registry.ErrLockTimeout) {
		return ExitLocked
	}
	var ue *registry.UnreadableError
	if errors.As(err, &ue) {
		return ExitLocked
	}
	return ExitGeneral
}

// errorDetailFor renders an error for the failure envelope, mirroring the
// classification of ExitCodeFor.
func errorDetailFor(err error) output.ErrorDetail {
	var ce *Error
	if errors.As(err, &ce) {
		return output.ErrorDetail{Code: ce.Code, Message: ce.Error(), Hint: ce.Hint}
	}
	if errors.Is(err, registry.ErrLockTimeout) {
		return output.ErrorDetail{
			Code:    CodeLockTimeout,
			Message: err.Error(),
			Hint:    "another agenthub process holds the registry lock; retry in a moment",
		}
	}
	var ue *registry.UnreadableError
	if errors.As(err, &ue) {
		return output.ErrorDetail{
			Code:    CodeRegistryCorrupt,
			Message: err.Error(),
			Hint:    "the unreadable file was quarantined, not destroyed; inspect it and re-run",
		}
	}
	return output.ErrorDetail{Code: CodeGeneral, Message: err.Error()}
}

// splitQuarantine separates registry quarantine reports (a document was
// unreadable but has been healed: quarantined + reset to defaults, store
// still fully usable) from fatal errors. Healed quarantines become warnings
// on the success envelope instead of failing the command — exit 7 is
// reserved for "corrupt and could NOT self-heal" (docs/modules/controlplane.md).
func splitQuarantine(err error) (warnings []string, fatal error) {
	var fatals []error
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if multi, ok := e.(interface{ Unwrap() []error }); ok {
			for _, c := range multi.Unwrap() {
				walk(c)
			}
			return
		}
		var ue *registry.UnreadableError
		if errors.As(e, &ue) {
			warnings = append(warnings, ue.Error())
			return
		}
		fatals = append(fatals, e)
	}
	walk(err)
	return warnings, errors.Join(fatals...)
}
