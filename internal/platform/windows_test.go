package platform_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/platform"
)

// These tests run on macOS/Linux: they exercise the Windows resolution
// logic through the Resolver hooks, which is the whole point of putting the
// seam in this package (ruling A.5 #23). They prove the LOGIC, not the
// platform — no assertion here says anything about how Windows actually
// behaves inside an MSIX container. See docs/status/windows.md.

const appData = `C:\Users\alice\AppData\Roaming`

func winResolver(t *testing.T, id platform.PackageIdentity, probe func(string) error) (*platform.Resolver, *[]string) {
	t.Helper()
	var warnings []string
	r := &platform.Resolver{
		GOOS: "windows",
		LookupEnv: func(key string) (string, bool) {
			if key == "APPDATA" {
				return appData, true
			}
			return "", false
		},
		UserHomeDir:     func() (string, error) { return `C:\Users\alice`, nil },
		PackageIdentity: func() platform.PackageIdentity { return id },
		ProbePath:       probe,
		UserSID:         func() (string, error) { return "S-1-5-21-1111-2222-3333-1001", nil },
		Warn:            func(msg string) { warnings = append(warnings, msg) },
	}
	return r, &warnings
}

// TestWindowsUnpackagedUsesAppData: the normal case — agenthub is never an
// MSIX package itself, so an unpackaged process writes straight to %APPDATA%.
func TestWindowsUnpackagedUsesAppData(t *testing.T) {
	r, warnings := winResolver(t, platform.PackageIdentity{}, func(string) error {
		t.Fatal("no probe expected when the process is not packaged")
		return nil
	})
	got, err := r.DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if got != appData+`\AgentHub` {
		t.Fatalf("DataDir = %q", got)
	}
	if len(*warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", *warnings)
	}
}

// TestWindowsPackagedAdoptsLoopbackUNC: spawned inside a client's MSIX
// container, %APPDATA% is redirected into that package's private store, so
// the reachable loopback-UNC twin wins.
func TestWindowsPackagedAdoptsLoopbackUNC(t *testing.T) {
	var probed string
	r, warnings := winResolver(t,
		platform.PackageIdentity{Packaged: true, Family: "SomeClient_8wekyb3d8bbwe"},
		func(p string) error { probed = p; return nil })

	got, err := r.DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	want := `\\127.0.0.1\C$\Users\alice\AppData\Roaming\AgentHub`
	if got != want {
		t.Fatalf("DataDir = %q, want %q", got, want)
	}
	if probed != want {
		t.Fatalf("adopted %q without probing it (probed %q)", got, probed)
	}
	if len(*warnings) != 0 {
		t.Fatalf("a successful escape must be silent: %v", *warnings)
	}
}

// TestWindowsPackagedUnreachableTwinWarnsLoudly: the failure direction. A
// redirected data directory is silent per-client configuration forking, so
// falling back must never be quiet.
func TestWindowsPackagedUnreachableTwinWarnsLoudly(t *testing.T) {
	r, warnings := winResolver(t,
		platform.PackageIdentity{Packaged: true, Family: "SomeClient_8wekyb3d8bbwe"},
		func(string) error { return errors.New("network path not found") })

	got, err := r.DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if got != appData+`\AgentHub` {
		t.Fatalf("DataDir = %q, want the local fallback", got)
	}
	if len(*warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", *warnings)
	}
	w := (*warnings)[0]
	for _, want := range []string{"MSIX", "SomeClient_8wekyb3d8bbwe", "AGENTHUB_DATA_DIR", "NOT share"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning does not mention %q: %s", want, w)
		}
	}
}

// TestWindowsDataDirOverrideBypassesEverything: an explicit override is
// obeyed verbatim on every platform, including inside a container — it is
// also the documented workaround when the UNC escape is unavailable.
func TestWindowsDataDirOverrideBypassesEverything(t *testing.T) {
	r := &platform.Resolver{
		GOOS: "windows",
		LookupEnv: func(key string) (string, bool) {
			if key == platform.EnvDataDir {
				return `D:\hub`, true
			}
			return "", false
		},
		PackageIdentity: func() platform.PackageIdentity {
			t.Fatal("an explicit override must not probe package identity")
			return platform.PackageIdentity{}
		},
	}
	got, err := r.DataDir()
	if err != nil || got != `D:\hub` {
		t.Fatalf("DataDir = %q, %v", got, err)
	}
}

// TestWindowsCtlPipePath pins the frozen control endpoint spelling
// (docs/conventions.md#frozen-identifiers) and the reason the SID is hashed into it.
func TestWindowsCtlPipePath(t *testing.T) {
	r, _ := winResolver(t, platform.PackageIdentity{}, nil)
	got, err := r.CtlSocketPath()
	if err != nil {
		t.Fatalf("CtlSocketPath: %v", err)
	}
	const prefix = `\\.\pipe\agenthub-ctl-`
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("pipe path = %q, want prefix %q", got, prefix)
	}
	suffix := strings.TrimPrefix(got, prefix)
	if len(suffix) != 8 {
		t.Fatalf("SID hash %q is not 8 hex chars", suffix)
	}
	if !platform.IsPipePath(got) {
		t.Fatalf("IsPipePath(%q) = false", got)
	}

	// Different users must not collide on the machine-wide pipe namespace.
	r2, _ := winResolver(t, platform.PackageIdentity{}, nil)
	r2.UserSID = func() (string, error) { return "S-1-5-21-1111-2222-3333-1002", nil }
	other, err := r2.CtlSocketPath()
	if err != nil {
		t.Fatalf("CtlSocketPath: %v", err)
	}
	if other == got {
		t.Fatalf("two SIDs produced the same pipe name %q", got)
	}

	// Stability: the same SID always yields the same name.
	again, err := r.CtlSocketPath()
	if err != nil || again != got {
		t.Fatalf("pipe name is not stable: %q vs %q (%v)", again, got, err)
	}
}

// TestWindowsDevCtlPipePath pins the SECOND frozen endpoint: the dev channel's
// own pipe name.
//
// The release name is the one that may never move, and the trap is the cheap
// way to obtain a dev name — splice "dev" into whatever the release name
// resolved to, or derive both from the data directory. Deriving from the
// directory was tried once and turned "rename the data directory" into "rename
// the protocol"; splicing has the same defect one level down, because the
// release name stops being a literal anyone can grep for and becomes the output
// of a concatenation. Both spellings are therefore asserted whole.
func TestWindowsDevCtlPipePath(t *testing.T) {
	const sid = "S-1-5-21-1111-2222-3333-1001"

	base, _ := winResolver(t, platform.PackageIdentity{}, nil)
	base.UserSID = func() (string, error) { return sid, nil }
	dev := platform.DevResolver(base)

	release, err := base.CtlSocketPath()
	if err != nil {
		t.Fatalf("release CtlSocketPath: %v", err)
	}
	devPipe, err := dev.CtlSocketPath()
	if err != nil {
		t.Fatalf("dev CtlSocketPath: %v", err)
	}

	hash := strings.TrimPrefix(release, `\\.\pipe\agenthub-ctl-`)
	if want := `\\.\pipe\agenthub-ctl-` + hash; release != want {
		t.Errorf("release pipe = %q, want %q", release, want)
	}
	if want := `\\.\pipe\agenthub-ctl-dev-` + hash; devPipe != want {
		t.Errorf("dev pipe = %q, want %q", devPipe, want)
	}
	// Same user, same hash: what separates the channels is the name, not the
	// identity. A dev build that hashed something else would also "differ",
	// while quietly ceasing to be per-user — and two users would then share
	// the dev pipe in the machine-wide namespace.
	if !strings.HasSuffix(devPipe, hash) {
		t.Errorf("dev pipe %q does not hash the same SID as %q", devPipe, release)
	}
	if !platform.IsPipePath(devPipe) {
		t.Errorf("IsPipePath(%q) = false", devPipe)
	}
}

func TestIsPipePath(t *testing.T) {
	yes := []string{`\\.\pipe\agenthub-ctl-abcdef01`, `\\?\pipe\x`}
	no := []string{"/run/user/1000/agenthub/ctl.sock", `C:\Users\a\ctl.sock`, ""}
	for _, p := range yes {
		if !platform.IsPipePath(p) {
			t.Errorf("IsPipePath(%q) = false", p)
		}
	}
	for _, p := range no {
		if platform.IsPipePath(p) {
			t.Errorf("IsPipePath(%q) = true", p)
		}
	}
}

// TestCtlPipeSDDL pins the security descriptor: the owner and nobody else.
// No Administrators ACE is intentional — the control plane hands out every
// downstream credential (docs/architecture.md#the-processes).
func TestCtlPipeSDDL(t *testing.T) {
	const sid = "S-1-5-21-1111-2222-3333-1001"
	got := platform.CtlPipeSDDL(sid)
	if got != "D:P(A;;GA;;;"+sid+")" {
		t.Fatalf("SDDL = %q", got)
	}
	for _, forbidden := range []string{"BA", "SY", "WD", "AU"} {
		if strings.Contains(got, ";"+forbidden+")") {
			t.Fatalf("SDDL grants %s: %q", forbidden, got)
		}
	}

	r, _ := winResolver(t, platform.PackageIdentity{}, nil)
	fromResolver, err := r.CtlPipeSDDL()
	if err != nil || fromResolver != got {
		t.Fatalf("Resolver.CtlPipeSDDL = %q, %v", fromResolver, err)
	}
}

// TestWindowsDerivedDirsUseBackslashes: a path spelling must not depend on
// the host that computed it.
func TestWindowsDerivedDirsUseBackslashes(t *testing.T) {
	r, _ := winResolver(t, platform.PackageIdentity{}, nil)
	for name, fn := range map[string]func() (string, error){
		"registry": r.RegistryDir,
		"logs":     r.LogsDir,
		"cache":    r.CacheDir,
		"state":    r.StateDir,
		"run":      r.RunDir,
	} {
		got, err := fn()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(got, "/") {
			t.Errorf("%s dir %q contains a forward slash", name, got)
		}
		if !strings.HasPrefix(got, appData+`\AgentHub\`) {
			t.Errorf("%s dir = %q", name, got)
		}
	}
}
