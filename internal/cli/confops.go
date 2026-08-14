package cli

import (
	"context"
	"errors"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
)

// Bridge between the CLI (a front end) and internal/confops (the single
// implementation of every semantic registry write).
//
// The CLI owns flag parsing, rendering and the exit-code table; it owns no
// rules. Everything below is translation: confops speaks Kind + a stable
// machine code, the CLI turns that into its own typed *Error so the frozen
// exit-code table and the --json failure envelope stay exactly what they
// were before the rules moved out of this package.
//
// The CLI always passes an EMPTY Precondition: an offline command has no
// earlier read to be stale against, and the registry's cross-process lock
// already prevents a torn write. Optimistic concurrency exists for the
// long-lived GUI window, which does hold a possibly minutes-old view.

// noPrecondition spells out the "do not check" case at every call site, so
// that a caller who DOES need a precondition has to say so.
var noPrecondition = confops.Precondition{}

// exitCodeForOps maps a confops failure class onto the frozen exit-code
// table (docs/subsystems/cli.md).
func exitCodeForOps(kind confops.Kind) int {
	switch kind {
	case confops.KindUsage:
		return ExitUsage
	case confops.KindNotFound:
		return ExitNotFound
	case confops.KindDenied:
		return ExitDenied
	case confops.KindState:
		return ExitLocked
	case confops.KindConflict, confops.KindStale:
		return ExitGeneral
	}
	return ExitGeneral
}

// opsError translates a confops failure into the CLI's typed error. Anything
// else (a registry sentinel, an I/O error) is passed
// through untouched so the existing classifiers keep owning it.
func opsError(err error) error {
	if err == nil {
		return nil
	}
	var oe *confops.Error
	if errors.As(err, &oe) {
		return &Error{
			Code:     oe.Code,
			ExitCode: exitCodeForOps(oe.Kind),
			Message:  oe.Message,
			Hint:     oe.Hint,
			Err:      oe.Err,
		}
	}
	var se *confops.StaleError
	if errors.As(err, &se) {
		// Unreachable from the CLI (it never sends a precondition), but
		// mapped rather than left to the generic branch so a future
		// non-interactive caller gets a coded envelope, not E_GENERAL.
		return &Error{
			Code:     confops.CodeStalePrecondition,
			ExitCode: ExitGeneral,
			Message:  se.Error(),
			Hint:     "re-read the configuration and retry against the current generation",
		}
	}
	return err
}

// opsStore opens the registry and returns it together with the healed-
// quarantine warnings, ready for a confops operation.
//
// It also retires the pre-migration <state>/active-profile.json marker. That
// happens HERE, on the write path, and not in openStore: doctor and the
// read-only commands open the registry through openStore, and a diagnostic
// that writes is a diagnostic that can change what it reports
// (TestDoctorIsReadOnly). A confops caller is about to mutate the registry
// anyway.
//
// A failed migration is reported rather than swallowed — losing the marker
// silently WIDENS what the operator's clients see.
func (a *App) opsStore() (*registry.Store, []string, error) {
	store, warnings, err := a.openStore()
	if err != nil {
		return store, warnings, err
	}
	if stateDir, serr := a.stateDir(); serr == nil {
		moved, merr := confops.MigrateActiveProfile(context.Background(), store, stateDir)
		switch {
		case merr != nil:
			warnings = append(warnings, "active-profile marker could not be migrated: "+merr.Error())
		case moved:
			warnings = append(warnings, "active profile migrated from state file into governance.json")
		}
	}
	return store, warnings, nil
}
