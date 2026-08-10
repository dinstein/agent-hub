package spawnguard

import (
	"errors"
	"testing"

	"github.com/dinstein/agent-hub/internal/guard"
)

// TestContainerEscapeFlagsAreMatchedByMeaningNotSpelling is the regression
// for two findings of the 2026-07-31 sweep. Both were the same mistake:
// the guard compared the flag's TEXT against one spelling, while the
// container runtime normalizes before acting, so a differently-spelled
// value reached docker meaning exactly what the blocked one meant.
//
//   - --cap-add stripped "CAP_" case-sensitively and only then upper-cased,
//     so cap_sys_admin survived both steps as CAP_SYS_ADMIN — absent from
//     dangerousCaps, which holds SYS_ADMIN.
//   - --security-opt matched only the '=' separator and the exact string
//     "label=disable", while moby still accepts ':' and reads a bare
//     "disable" as SELinux label-disable.
func TestContainerEscapeFlagsAreMatchedByMeaningNotSpelling(t *testing.T) {
	g := New(Config{})

	blocked := []struct {
		name string
		args []string
	}{
		// --cap-add: every spelling docker accepts for one capability.
		{"cap-add upper, prefixed", []string{"run", "--cap-add=CAP_SYS_ADMIN", "img"}},
		{"cap-add upper, bare", []string{"run", "--cap-add=SYS_ADMIN", "img"}},
		{"cap-add lower, prefixed", []string{"run", "--cap-add=cap_sys_admin", "img"}},
		{"cap-add lower, bare", []string{"run", "--cap-add=sys_admin", "img"}},
		{"cap-add mixed, prefixed", []string{"run", "--cap-add=Cap_Sys_Ptrace", "img"}},
		{"cap-add lower dac_read_search", []string{"run", "--cap-add=cap_dac_read_search", "img"}},
		{"cap-add lower all", []string{"run", "--cap-add=all", "img"}},
		{"cap-add separate arg", []string{"run", "--cap-add", "cap_sys_module", "img"}},

		// --security-opt: both separators, and the bare shorthand.
		{"seccomp with =", []string{"run", "--security-opt", "seccomp=unconfined", "img"}},
		{"seccomp with :", []string{"run", "--security-opt", "seccomp:unconfined", "img"}},
		{"apparmor with =", []string{"run", "--security-opt", "apparmor=unconfined", "img"}},
		{"apparmor with :", []string{"run", "--security-opt", "apparmor:unconfined", "img"}},
		{"label disable with =", []string{"run", "--security-opt", "label=disable", "img"}},
		{"label disable with :", []string{"run", "--security-opt", "label:disable", "img"}},
		{"bare disable", []string{"run", "--security-opt", "disable", "img"}},
		{"upper-cased", []string{"run", "--security-opt", "SECCOMP:UNCONFINED", "img"}},
		// systempaths=unconfined unmasks /proc kernel-memory paths
		// (/proc/kcore, /proc/keys, /sys/firmware); it disables a confinement
		// layer exactly like the seccomp/apparmor forms above.
		{"systempaths with =", []string{"run", "--security-opt", "systempaths=unconfined", "img"}},
		{"systempaths with :", []string{"run", "--security-opt", "systempaths:unconfined", "img"}},
		{"systempaths attached in cluster", []string{"run", "-it", "--security-opt=systempaths=unconfined", "img"}},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			err := g.Check("docker", tc.args, nil)
			if err == nil {
				t.Fatalf("docker %v was allowed", tc.args)
			}
			if !errors.Is(err, guard.ErrBlocked) {
				t.Errorf("refusal does not unwrap to guard.ErrBlocked: %v", err)
			}
		})
	}

	// Confinement being ADDED, or a profile being named, must still pass —
	// a guard that refused these would push operators away from using them.
	allowed := []struct {
		name string
		args []string
	}{
		{"dropping caps", []string{"run", "--cap-drop=ALL", "img"}},
		{"a harmless cap", []string{"run", "--cap-add=NET_BIND_SERVICE", "img"}},
		{"a named seccomp profile", []string{"run", "--security-opt", "seccomp=/etc/agenthub/profile.json", "img"}},
		{"no-new-privileges", []string{"run", "--security-opt", "no-new-privileges", "img"}},
		{"a named apparmor profile", []string{"run", "--security-opt", "apparmor=docker-default", "img"}},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			if err := g.Check("docker", tc.args, nil); err != nil {
				t.Fatalf("docker %v was refused: %v", tc.args, err)
			}
		})
	}
}

// TestContainerEscapeShortFlagClusters is the regression for the 2026-08-10
// sweep's critical finding. A combined short-flag cluster like -it is not in
// containerBoolFlags (which lists -i and -t separately), so it fell to the
// generic "assume it takes a value" branch and SWALLOWED the following
// argument — the very escape flag or mount value the guard exists to judge.
// The variant -itv additionally hid a mount because checkContainerFlag never
// saw the -v key.
func TestContainerEscapeShortFlagClusters(t *testing.T) {
	g := New(Config{})

	blocked := []struct {
		name string
		args []string
	}{
		{"cluster hides privileged", []string{"run", "-it", "--privileged", "img"}},
		{"cluster hides separate volume", []string{"run", "-it", "-v", "/:/host", "img"}},
		{"cluster ends in v, next-arg volume", []string{"run", "-itv", "/:/host", "img"}},
		{"cluster ends in v, attached volume", []string{"run", "-itv/:/host", "img"}},
		{"v-first attached volume", []string{"run", "-v/:/host", "img"}},
		{"cluster then privileged", []string{"run", "-itd", "--privileged", "img"}},
		{"exec cluster hides privileged", []string{"exec", "-it", "--privileged", "ctr", "sh"}},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			err := g.Check("docker", tc.args, nil)
			if err == nil {
				t.Fatalf("docker %v was allowed", tc.args)
			}
			if !errors.Is(err, guard.ErrBlocked) {
				t.Errorf("refusal does not unwrap to guard.ErrBlocked: %v", err)
			}
		})
	}

	// Benign clusters and attached values must still pass, so the fix does not
	// push operators off ordinary invocations. An unrecognized short letter is
	// tolerated (fail open, matching this layer's shape-check direction).
	allowed := []struct {
		name string
		args []string
	}{
		{"plain interactive tty", []string{"run", "-it", "img"}},
		{"detached interactive tty", []string{"run", "-itd", "img"}},
		{"cluster with project mount", []string{"run", "-itv", "./data:/data", "img"}},
		{"memory attached in cluster", []string{"run", "-itm512m", "img"}},
		{"publish attached in cluster", []string{"run", "-itp8080:80", "img"}},
		{"hostname short takes a value", []string{"run", "-h", "myhost", "img"}},
		{"unknown short letter tolerated", []string{"run", "-itZ", "img"}},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			if err := g.Check("docker", tc.args, nil); err != nil {
				t.Fatalf("docker %v was refused: %v", tc.args, err)
			}
		})
	}
}
