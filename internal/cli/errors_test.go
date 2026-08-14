package cli

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/registry"
)

// TestExitCodeFor is the table-driven pin of the frozen exit-code table
// (docs/subsystems/cli.md): every code 0-7 has at least one producing error value.
func TestExitCodeFor(t *testing.T) {
	lockErr := &registry.LockTimeoutError{Path: "/tmp/reg/.lock", Timeout: time.Second}
	unreadable := &registry.UnreadableError{
		Kind: registry.DocServers, Path: "servers.json",
		QuarantinePath: "servers.json.unreadable-1", Err: errors.New("bad json"),
	}

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil is success", nil, ExitOK},
		{"untyped error is general", errors.New("boom"), ExitGeneral},
		{"wrapped untyped error is general", fmt.Errorf("ctx: %w", errors.New("boom")), ExitGeneral},
		{"typed general error", &Error{Code: CodeServerExists, ExitCode: ExitGeneral, Message: "exists"}, ExitGeneral},
		{"usage error", Usagef("bad flag"), ExitUsage},
		{"not found", NotFoundf(CodeServerNotFound, "no server %q", "gh"), ExitNotFound},
		{"daemon down", DaemonDownf("daemon required"), ExitDaemonDown},
		{"auth failed", AuthFailedf("token expired"), ExitAuth},
		{"governance denied", Deniedf("HITL deny"), ExitDenied},
		{"lock timeout typed", lockErr, ExitLocked},
		{"lock timeout wrapped", fmt.Errorf("update: %w", lockErr), ExitLocked},
		{"lock timeout sentinel", registry.ErrLockTimeout, ExitLocked},
		{"registry unreadable", unreadable, ExitLocked},
		{"registry unreadable joined", errors.Join(errors.New("other"), unreadable), ExitLocked},
		{"silent exit passthrough", &silentExitError{code: ExitNotFound}, ExitNotFound},
		{"typed error wins over join order", errors.Join(unreadable, Usagef("bad")), ExitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCodeFor(tc.err); got != tc.want {
				t.Errorf("ExitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestErrorDetailFor(t *testing.T) {
	d := errorDetailFor(&Error{Code: CodeServerNotFound, ExitCode: ExitNotFound, Message: "no server 'gh'", Hint: "try ls"})
	if d.Code != CodeServerNotFound || d.Message != "no server 'gh'" || d.Hint != "try ls" {
		t.Errorf("detail = %+v", d)
	}

	d = errorDetailFor(&registry.LockTimeoutError{Path: "p", Timeout: time.Second})
	if d.Code != CodeLockTimeout || d.Hint == "" {
		t.Errorf("lock detail = %+v", d)
	}

	d = errorDetailFor(&registry.UnreadableError{Kind: registry.DocMeta, Path: "meta.json", QuarantinePath: "q", Err: errors.New("x")})
	if d.Code != CodeRegistryCorrupt {
		t.Errorf("unreadable detail = %+v", d)
	}

	d = errorDetailFor(errors.New("boom"))
	if d.Code != CodeGeneral || d.Message != "boom" {
		t.Errorf("general detail = %+v", d)
	}
}

func TestSplitQuarantine(t *testing.T) {
	unreadable := &registry.UnreadableError{Kind: registry.DocServers, Path: "s", QuarantinePath: "q", Err: errors.New("x")}

	warnings, fatal := splitQuarantine(nil)
	if len(warnings) != 0 || fatal != nil {
		t.Errorf("nil: warnings=%v fatal=%v", warnings, fatal)
	}

	warnings, fatal = splitQuarantine(unreadable)
	if len(warnings) != 1 || fatal != nil {
		t.Errorf("healed quarantine must be a warning, got warnings=%v fatal=%v", warnings, fatal)
	}

	boom := errors.New("boom")
	warnings, fatal = splitQuarantine(errors.Join(unreadable, boom))
	if len(warnings) != 1 || !errors.Is(fatal, boom) {
		t.Errorf("mixed join: warnings=%v fatal=%v", warnings, fatal)
	}

	// Nested joins flatten.
	warnings, fatal = splitQuarantine(errors.Join(errors.Join(unreadable, unreadable), boom))
	if len(warnings) != 2 || !errors.Is(fatal, boom) {
		t.Errorf("nested join: warnings=%v fatal=%v", warnings, fatal)
	}
}
