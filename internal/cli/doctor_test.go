package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/cli/output"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
)

func decodeDoctor(t *testing.T, env envelope) DoctorReport {
	t.Helper()
	var report DoctorReport
	if err := json.Unmarshal(env.Data, &report); err != nil {
		t.Fatalf("data is not a doctor report: %v\n%s", err, env.Data)
	}
	return report
}

func findCheck(t *testing.T, r DoctorReport, name string) DoctorCheck {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not found in %+v", name, r.Checks)
	return DoctorCheck{}
}

func hasCheck(r DoctorReport, name string) bool {
	for _, c := range r.Checks {
		if c.Name == name {
			return true
		}
	}
	return false
}

// isolateHome points HOME at a temp dir so the client-drift check cannot
// read (or, on macOS, prompt for) the developer's real client
// configurations. Every doctor test uses it: a diagnostic that touches the
// user's home from a unit test is a bug in the test, not a feature.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func TestDoctorFreshEnvironment(t *testing.T) {
	setDataDir(t)
	isolateHome(t)
	code, out, stderr := runCLI(t, "", "doctor")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr: %s\n%s", code, stderr, out)
	}
	for _, want := range []string{
		"data-dir", "override via AGENTHUB_DATA_DIR",
		"registry:meta", "missing (created on first use)", "registry:lock",
		"ctl-socket", "vault", "path",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
}

// TestDoctorIsReadOnly pins that doctor without --fix never materializes
// any directory: the diagnostic must not be able to change what it reports.
func TestDoctorIsReadOnly(t *testing.T) {
	dir := setDataDir(t)
	isolateHome(t)
	if code, _, stderr := runCLI(t, "", "doctor"); code != ExitOK {
		t.Fatalf("doctor exit = %d: %s", code, stderr)
	}
	for _, sub := range []string{"registry", "state", "secrets", "logs", "cache", "run", "skills"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); !os.IsNotExist(err) {
			t.Errorf("doctor must not create %s/ (err=%v)", sub, err)
		}
	}
}

// TestDoctorFixCreatesDirectories pins the other half: --fix performs the
// SAFE repair (creating a missing directory is idempotent and destroys
// nothing) and says so in the report.
func TestDoctorFixCreatesDirectories(t *testing.T) {
	dir := setDataDir(t)
	isolateHome(t)
	code, out, _ := runCLI(t, "", "doctor", "--fix", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	report := decodeDoctor(t, decodeEnvelope(t, out))
	if report.Summary.Fixed == 0 {
		t.Fatalf("--fix reported no repairs: %+v", report.Summary)
	}
	reg := findCheck(t, report, "registry-dir")
	if reg.Status != StatusOK || !reg.Fixed {
		t.Errorf("registry-dir = %+v, want ok+fixed", reg)
	}
	if _, err := os.Stat(filepath.Join(dir, "registry")); err != nil {
		t.Errorf("--fix did not create the registry directory: %v", err)
	}
}

func TestDoctorJSONAfterWrite(t *testing.T) {
	setDataDir(t)
	isolateHome(t)
	if code, _, _ := runCLI(t, "", "server", "add", "x", "--cmd", "foo"); code != ExitOK {
		t.Fatalf("add failed")
	}
	// The server points at a command that does not exist, so the handshake
	// check fails on purpose: exit 1 with the report intact.
	code, out, _ := runCLI(t, "", "doctor", "--json")
	if code != ExitGeneral {
		t.Fatalf("exit = %d, want 1 (server:x cannot hand shake)\n%s", code, out)
	}
	env := decodeEnvelope(t, out)
	if !env.OK {
		t.Fatalf("envelope = %s", out)
	}
	report := decodeDoctor(t, env)

	meta := findCheck(t, report, "registry:meta")
	if meta.Status != StatusOK || meta.Detail != "generation 1" {
		t.Errorf("meta check = %+v, want ok / generation 1", meta)
	}
	if servers := findCheck(t, report, "registry:servers"); servers.Status != StatusOK {
		t.Errorf("servers check = %+v", servers)
	}
	lock := findCheck(t, report, "registry:lock")
	if lock.Status != StatusOK || !strings.HasPrefix(lock.Detail, "present") {
		t.Errorf("lock check = %+v", lock)
	}
	srv := findCheck(t, report, "server:x")
	if srv.Status != StatusFail || srv.Fix == "" {
		t.Errorf("server:x = %+v, want fail with a suggested fix", srv)
	}
}

// TestDoctorDisabledServerIsNotProbed: intentionally off is not broken, so
// a disabled server must never be dialed nor reported as a failure.
func TestDoctorDisabledServerIsNotProbed(t *testing.T) {
	setDataDir(t)
	isolateHome(t)
	if code, _, _ := runCLI(t, "", "server", "add", "x", "--cmd", "foo"); code != ExitOK {
		t.Fatalf("add failed")
	}
	if code, _, _ := runCLI(t, "", "server", "disable", "x"); code != ExitOK {
		t.Fatalf("disable failed")
	}
	code, out, _ := runCLI(t, "", "doctor", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	report := decodeDoctor(t, decodeEnvelope(t, out))
	srv := findCheck(t, report, "server:x")
	if srv.Status != StatusOK || !strings.Contains(srv.Detail, "disabled") {
		t.Errorf("server:x = %+v, want ok/disabled", srv)
	}
}

// TestDoctorDanglingActiveProfile pins docs/architecture.md §7 improvement 5: a
// dangling profile reference fail-closes to an EMPTY scope, and doctor must
// say so out loud instead of leaving it silent.
func TestDoctorDanglingActiveProfile(t *testing.T) {
	dir := setDataDir(t)
	isolateHome(t)
	if code, _, _ := runCLI(t, "", "profile", "create", "payments"); code != ExitOK {
		t.Fatalf("create failed")
	}
	if code, _, _ := runCLI(t, "", "profile", "use", "payments"); code != ExitOK {
		t.Fatalf("use failed")
	}
	if code, _, _ := runCLI(t, "", "profile", "rm", "payments"); code != ExitOK {
		t.Fatalf("rm failed")
	}
	// `profile rm` clears the active marker; re-point it at the missing
	// profile by hand to simulate a stale / hand-edited governance document.
	// The marker lives in the registry, not a state file, because scope
	// resolution is pure and never reads one.
	govPath := filepath.Join(dir, "registry", "governance.json")
	gov, err := os.ReadFile(govPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(gov, &doc); err != nil {
		t.Fatal(err)
	}
	doc["activeProfile"] = "payments"
	edited, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(govPath, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "", "doctor", "--json")
	if code != ExitGeneral {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	report := decodeDoctor(t, decodeEnvelope(t, out))
	c := findCheck(t, report, "active-profile")
	if c.Status != StatusFail || !strings.Contains(c.Detail, "EMPTY scope") {
		t.Errorf("active-profile = %+v, want a loud fail", c)
	}
}

// TestDoctorColdCacheIsNotAFailure: a server whose tool cache entry is
// still missing while the cache itself is fresh is reported as "still
// installing", not as a broken server — the classic npx/uvx first-run false
// positive (docs/modules/controlplane.md).
func TestDoctorColdCacheIsNotAFailure(t *testing.T) {
	dir := setDataDir(t)
	isolateHome(t)
	if code, _, _ := runCLI(t, "", "server", "add", "x", "--cmd", "foo"); code != ExitOK {
		t.Fatalf("add failed")
	}
	cacheDir := filepath.Join(dir, "cache", "tools")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "other.json"), []byte(`{"server":"other"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "", "doctor", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (a cold cache must not fail)\n%s", code, out)
	}
	report := decodeDoctor(t, decodeEnvelope(t, out))
	c := findCheck(t, report, "server:x")
	if c.Status != StatusWarn || !strings.Contains(c.Detail, "still installing") {
		t.Errorf("server:x = %+v, want a 'still installing' warn", c)
	}
}

// TestDoctorCorruptOverrideStoreFails pins the fail direction of the
// override store: unreadable must never read as "no overrides".
func TestDoctorCorruptOverrideStoreFails(t *testing.T) {
	dir := setDataDir(t)
	isolateHome(t)
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, toolOverridesFileName), []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "", "doctor", "--json")
	if code != ExitGeneral {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	report := decodeDoctor(t, decodeEnvelope(t, out))
	if c := findCheck(t, report, "integrity:overrides"); c.Status != StatusFail {
		t.Errorf("integrity:overrides = %+v, want fail", c)
	}
}

func TestDoctorFailingCheckExit1(t *testing.T) {
	dir := setDataDir(t)
	isolateHome(t)
	regDir := filepath.Join(dir, "registry")
	if err := os.MkdirAll(regDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(regDir, "meta.json"), []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, _ := runCLI(t, "", "doctor", "--json")
	if code != ExitGeneral {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	env := decodeEnvelope(t, out)
	// The report itself succeeded: envelope stays ok:true; the findings and
	// the exit code carry the failure (a second error envelope would corrupt
	// single-line JSON consumption).
	if !env.OK {
		t.Fatalf("envelope = %s", out)
	}
	report := decodeDoctor(t, env)
	if meta := findCheck(t, report, "registry:meta"); meta.Status != StatusFail {
		t.Errorf("meta check = %+v, want fail", meta)
	}
	if report.Summary.Fail == 0 {
		t.Errorf("summary = %+v", report.Summary)
	}

	code, humanOut, _ := runCLI(t, "", "doctor")
	if code != ExitGeneral {
		t.Fatalf("human exit = %d, want 1", code)
	}
	if !strings.Contains(humanOut, "[fail]") {
		t.Errorf("human output missing [fail]:\n%s", humanOut)
	}
}

// TestDoctorHumanAndJSONSameSource renders ONE report through both paths:
// running doctor twice would compare two different observations (timings,
// mtimes), which proves nothing about the rendering. This proves it.
func TestDoctorHumanAndJSONSameSource(t *testing.T) {
	dir := setDataDir(t)
	isolateHome(t)
	if code, _, _ := runCLI(t, "", "server", "add", "x", "--cmd", "foo"); code != ExitOK {
		t.Fatalf("add failed")
	}
	app := &App{
		version:  "test",
		stdin:    strings.NewReader(""),
		stdout:   &bytes.Buffer{},
		stderr:   &bytes.Buffer{},
		resolver: platform.Default(),
	}
	if got, _ := app.resolver.DataDir(); got != dir {
		t.Fatalf("resolver data dir = %q, want %q", got, dir)
	}
	report := app.runDoctor(context.Background(), false)

	var jsonBuf, jsonErr bytes.Buffer
	if err := output.New(&jsonBuf, &jsonErr, true).Emit(report); err != nil {
		t.Fatal(err)
	}
	var humanBuf, humanErr bytes.Buffer
	if err := output.New(&humanBuf, &humanErr, false).Emit(report); err != nil {
		t.Fatal(err)
	}
	decoded := decodeDoctor(t, decodeEnvelope(t, jsonBuf.String()))
	if len(decoded.Checks) != len(report.Checks) {
		t.Fatalf("json report has %d checks, source has %d", len(decoded.Checks), len(report.Checks))
	}
	human := humanBuf.String()
	for _, c := range decoded.Checks {
		for _, want := range []string{c.Name, c.Status, c.Detail} {
			if !strings.Contains(human, want) {
				t.Errorf("human output missing %q from the JSON report:\n%s", want, human)
			}
		}
	}
	if !hasCheck(decoded, "server:x") {
		t.Errorf("report has no per-server handshake check: %+v", decoded.Checks)
	}
}

// crash marker: doctor reports how the PREVIOUS long-running
// process exited. Three states, and the ambiguous one must never read as
// clean (that would hand out a bill of health nobody earned).
func TestDoctorPreviousShutdown(t *testing.T) {
	t.Run("no marker", func(t *testing.T) {
		setDataDir(t)
		isolateHome(t)
		_, out, _ := runCLI(t, "", "doctor", "--json")
		c := findCheck(t, decodeDoctor(t, decodeEnvelope(t, out)), "previous-shutdown")
		if c.Status != StatusOK || !strings.Contains(c.Detail, "unknown") {
			t.Fatalf("check = %+v, want ok/unknown on a fresh directory", c)
		}
	})

	t.Run("clean", func(t *testing.T) {
		dir := setDataDir(t)
		isolateHome(t)
		regDir := filepath.Join(dir, "registry")
		m, _, err := registry.ArmRunMarker(regDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Resolve(); err != nil {
			t.Fatal(err)
		}
		_, out, _ := runCLI(t, "", "doctor", "--json")
		c := findCheck(t, decodeDoctor(t, decodeEnvelope(t, out)), "previous-shutdown")
		if c.Status != StatusOK || c.Detail != "clean" {
			t.Fatalf("check = %+v, want ok/clean", c)
		}
	})

	t.Run("crash", func(t *testing.T) {
		dir := setDataDir(t)
		isolateHome(t)
		regDir := filepath.Join(dir, "registry")
		// Armed and never resolved: exactly what a kill -9 leaves behind.
		if _, _, err := registry.ArmRunMarker(regDir); err != nil {
			t.Fatal(err)
		}
		_, out, _ := runCLI(t, "", "doctor", "--json")
		c := findCheck(t, decodeDoctor(t, decodeEnvelope(t, out)), "previous-shutdown")
		if c.Status != StatusWarn {
			t.Fatalf("check = %+v, want warn (an unclean exit is survivable, not broken)", c)
		}
		if !strings.Contains(c.Detail, "crash") || c.Fix == "" {
			t.Fatalf("check = %+v, want a crash detail with guidance", c)
		}
	})
}

// TestDoctorNamesProjectBindings pins that the check reports CONDITIONAL, not
// broken. Per-project bindings are selected by matching a session's root
// against the roots under clients.json#<client>/projects, and the stdio
// gateway now reports one (the client's first MCP root), so a binding whose
// prefix matches does apply.
//
// What doctor still cannot verify from the registry alone is whether the
// client reports a root at all. That is why the check survives rather than
// being deleted: a client reporting none matches nothing, and because project
// bindings sit ABOVE the client binding in precedence, the miss leaves the
// WIDER client-level binding in force.
func TestDoctorNamesProjectBindings(t *testing.T) {
	dir := setDataDir(t)
	isolateHome(t)

	// A project binding can only be written by hand: no CLI command and no
	// control-plane route creates one.
	regDir := filepath.Join(dir, "registry")
	if err := os.MkdirAll(regDir, 0o700); err != nil {
		t.Fatal(err)
	}
	clientsJSON := `{"clients":{"claude-code":{"projects":{"/Users/someone/work/payments":{"servers":["fs"]}}}}}`
	if err := os.WriteFile(filepath.Join(regDir, "clients.json"), []byte(clientsJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	// The exit code is deliberately NOT asserted: doctor inspects the ambient
	// client configuration too, so unrelated findings decide it. What this
	// test owns is the status of its own check.
	_, out, _ := runCLI(t, "", "doctor", "--json")
	check := findCheck(t, decodeDoctor(t, decodeEnvelope(t, out)), "scope:projects")
	if check.Status != StatusOK {
		t.Fatalf("status = %q, want ok — the binding is live for a client that reports a "+
			"matching root, and reporting a working configuration as a warning is how a "+
			"diagnostic teaches operators to ignore it: %+v", check.Status, check)
	}
	if !strings.Contains(check.Detail, "claude-code") {
		t.Errorf("detail does not name the client: %q", check.Detail)
	}
	// The operator must learn what the binding DEPENDS ON, since that is the
	// part doctor cannot check for them.
	if !strings.Contains(check.Detail, "root") {
		t.Errorf("detail does not say the match depends on the reported root: %q", check.Detail)
	}
}

// The check must stay silent when nothing is configured: a diagnostic that
// warns about a feature nobody used is noise, and noise is what makes the
// real warnings get skipped.
func TestDoctorSaysNothingAboutAbsentProjectBindings(t *testing.T) {
	setDataDir(t)
	isolateHome(t)
	_, out, _ := runCLI(t, "", "doctor", "--json")
	if hasCheck(decodeDoctor(t, decodeEnvelope(t, out)), "scope:projects") {
		t.Error("doctor reported scope:projects with no project bindings configured")
	}
}

// TestDoctorReportsQuarantinedRegistryDocs covers the one registry event that
// costs the operator DATA, and the reason the per-document checks cannot see
// it: quarantine renames the corrupt file and writes a fresh empty one in its
// place, so registry:servers afterwards reports "readable" — true, and exactly
// the wrong thing to read when every server has just disappeared.
//
// The warning is issued once, at the moment of quarantine, by whichever
// command triggered it. Someone running doctor afterwards to find out where
// their configuration went is the case this check exists for.
func TestDoctorReportsQuarantinedRegistryDocs(t *testing.T) {
	dir := setDataDir(t)
	isolateHome(t)
	if code, _, stderr := runCLI(t, "", "server", "add", "fs", "--cmd", "npx"); code != ExitOK {
		t.Fatalf("server add: %s", stderr)
	}

	// Corrupt the document the way a crashed writer or a bad merge would,
	// then let a normal command trigger the quarantine.
	regDir := filepath.Join(dir, "registry")
	if err := os.WriteFile(filepath.Join(regDir, "servers.json"),
		[]byte(`{"servers":{"fs":{"command":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _ = runCLI(t, "", "server", "ls"); true {
		// The exit status is not the subject; the quarantine side effect is.
		_ = dir
	}

	quarantined, err := filepath.Glob(filepath.Join(regDir, "*.unreadable-*"))
	if err != nil || len(quarantined) == 0 {
		t.Fatalf("nothing was quarantined, so this test is not exercising the case: %v", err)
	}

	_, out, _ := runCLI(t, "", "doctor", "--json")
	check := findCheck(t, decodeDoctor(t, decodeEnvelope(t, out)), "registry:quarantined")
	if check.Status != StatusWarn {
		t.Fatalf("status = %q, want warn: %+v", check.Status, check)
	}
	if !strings.Contains(check.Detail, filepath.Base(quarantined[0])) {
		t.Errorf("detail does not name the set-aside file: %q", check.Detail)
	}
	// The operator needs to be told where the good copy is, or the finding is
	// only bad news without a next step.
	if !strings.Contains(check.Fix, "backups") {
		t.Errorf("fix does not point at the backup chain: %q", check.Fix)
	}

	// The per-document check still reports the RESET file as readable, which
	// is precisely why this separate finding has to exist.
	doc := findCheck(t, decodeDoctor(t, decodeEnvelope(t, out)), "registry:servers")
	if doc.Status != StatusOK {
		t.Errorf("registry:servers = %q; the premise of this test is that the reset file reads fine", doc.Status)
	}
}

// A registry nobody has damaged must produce no finding at all: a diagnostic
// that warns about a situation that has not happened is the noise that makes
// real warnings get skipped.
func TestDoctorSilentWithoutQuarantinedDocs(t *testing.T) {
	setDataDir(t)
	isolateHome(t)
	if code, _, stderr := runCLI(t, "", "server", "add", "fs", "--cmd", "npx"); code != ExitOK {
		t.Fatalf("server add: %s", stderr)
	}
	_, out, _ := runCLI(t, "", "doctor", "--json")
	if hasCheck(decodeDoctor(t, decodeEnvelope(t, out)), "registry:quarantined") {
		t.Error("doctor reported registry:quarantined with an intact registry")
	}
}
