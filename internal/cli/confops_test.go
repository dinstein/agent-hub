package cli

import (
	"errors"
	"testing"

	"github.com/dinstein/agent-hub/internal/confops"
)

// TestConfopsCodesMatchTheFrozenCLIVocabulary: confops and the CLI each name
// the stable machine codes, and the --json failure envelope is a contract
// keyed on those strings. Two spellings of one code is exactly the drift
// this phase exists to prevent, so the two vocabularies are pinned together.
func TestConfopsCodesMatchTheFrozenCLIVocabulary(t *testing.T) {
	pairs := []struct {
		ops, cli string
	}{
		{confops.CodeUsage, CodeUsage},
		{confops.CodeNotFound, CodeNotFound},
		{confops.CodeServerNotFound, CodeServerNotFound},
		{confops.CodeServerExists, CodeServerExists},
		{confops.CodeProfileNotFound, CodeProfileNotFound},
		{confops.CodeProfileExists, CodeProfileExists},
		{confops.CodeToolNotFound, CodeToolNotFound},
		{confops.CodeConfigKeyUnknown, CodeConfigKeyUnknown},
		{confops.CodeUnsupportedTransport, CodeUnsupportedTransport},
		{confops.CodeDenied, CodeDenied},
		{confops.CodeStateCorrupt, CodeStateCorrupt},
	}
	for _, p := range pairs {
		if p.ops != p.cli {
			t.Errorf("code drift: confops %q vs cli %q", p.ops, p.cli)
		}
	}
}

// TestOpsErrorMapsEveryKindToTheFrozenExitTable pins the translation the
// whole CLI now depends on: a confops Kind must land on the exit code the
// command used to return by hand.
func TestOpsErrorMapsEveryKindToTheFrozenExitTable(t *testing.T) {
	cases := []struct {
		kind confops.Kind
		want int
	}{
		{confops.KindUsage, ExitUsage},
		{confops.KindNotFound, ExitNotFound},
		{confops.KindConflict, ExitGeneral},
		{confops.KindDenied, ExitDenied},
		{confops.KindState, ExitLocked},
		{confops.KindStale, ExitGeneral},
	}
	for _, tc := range cases {
		err := opsError(&confops.Error{Kind: tc.kind, Code: "E_X", Message: "m", Hint: "h"})
		if got := ExitCodeFor(err); got != tc.want {
			t.Errorf("kind %s -> exit %d, want %d", tc.kind, got, tc.want)
		}
		detail := errorDetailFor(err)
		if detail.Code != "E_X" || detail.Message != "m" || detail.Hint != "h" {
			t.Errorf("kind %s lost its rendering: %+v", tc.kind, detail)
		}
	}
}

// TestOpsErrorPassesForeignErrorsThrough: the registry and integrity
// sentinels keep their own classifiers (exit 7 for a corrupt store, for
// example), so the bridge must not swallow them.
func TestOpsErrorPassesForeignErrorsThrough(t *testing.T) {
	sentinel := errors.New("some other failure")
	if got := opsError(sentinel); !errors.Is(got, sentinel) {
		t.Errorf("opsError rewrote a foreign error: %v", got)
	}
	if opsError(nil) != nil {
		t.Error("opsError(nil) must stay nil")
	}
}

// TestOpsErrorMapsStalePrecondition: unreachable from the CLI today (it
// never sends a precondition), but a coded envelope beats E_GENERAL the day
// a non-interactive caller does.
func TestOpsErrorMapsStalePrecondition(t *testing.T) {
	err := opsError(&confops.StaleError{Want: 3, Got: 7})
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("stale error was not classified: %v", err)
	}
	if ce.Code != confops.CodeStalePrecondition || ce.ExitCode != ExitGeneral {
		t.Errorf("classified as %+v", ce)
	}
}
