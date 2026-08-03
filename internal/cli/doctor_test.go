package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

// doctorEnvelope pulls the result envelope out of a `doctor --json` stdout.
//
// Under --json the progress events are NDJSON lines preceding the envelope,
// which is always the LAST line — the same shape `auth login --json` has. So
// the whole of stdout is not itself an envelope and must not be handed to
// decodeEnvelope directly.
func doctorEnvelope(t *testing.T, out string) envelope {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return decodeEnvelope(t, lines[len(lines)-1])
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
	report := decodeDoctor(t, doctorEnvelope(t, out))
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
	if code, _, stderr := runCLI(t, "", "server", "enable", "x", "--no-probe"); code != ExitOK {
		t.Fatalf("server enable: %s", stderr)
	}
	// The server points at a command that does not exist, so the handshake
	// check fails on purpose: exit 1 with the report intact.
	code, out, _ := runCLI(t, "", "doctor", "--json")
	if code != ExitGeneral {
		t.Fatalf("exit = %d, want 1 (server:x cannot hand shake)\n%s", code, out)
	}
	env := doctorEnvelope(t, out)
	if !env.OK {
		t.Fatalf("envelope = %s", out)
	}
	report := decodeDoctor(t, env)

	// Two writes: `server add` records the definition and `server enable`
	// puts it into service. They are separate operations and each bumps the
	// generation.
	meta := findCheck(t, report, "registry:meta")
	if meta.Status != StatusOK || meta.Detail != "generation 2" {
		t.Errorf("meta check = %+v, want ok / generation 2", meta)
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

// TestDoctorProgressGoesToStderrNotStdout pins the split doctor shares with
// `auth login`: progress is a STREAM, the report is a VALUE.
//
// The whole point of the progress lines is that a run with several servers
// spends seconds in the probe phase with nothing to show yet. That is only
// worth having if it costs nothing downstream — so stdout in human mode must
// still be the report and nothing else, or `agenthub doctor > report.txt`
// starts collecting chatter.
func TestDoctorProgressGoesToStderrNotStdout(t *testing.T) {
	setDataDir(t)
	isolateHome(t)
	if code, _, stderr := runCLI(t, "", "server", "add", "x", "--cmd", "no-such-binary"); code != ExitOK {
		t.Fatalf("server add: %s", stderr)
	}
	if code, _, stderr := runCLI(t, "", "server", "enable", "x", "--no-probe"); code != ExitOK {
		t.Fatalf("server enable: %s", stderr)
	}

	_, out, stderr := runCLI(t, "", "doctor")
	// Every progress line this command emits, and none of them may appear on
	// stdout. Spelled out rather than derived, so a new one added without a
	// thought for the stream/value split fails here.
	for _, line := range []string{
		"checking directories and registry...",
		"probing 1 server",
		"checking vault, integrity and skills...",
		"checking client configurations...",
	} {
		if !strings.Contains(stderr, line) {
			t.Errorf("stderr is missing the %q progress line:\n%s", line, stderr)
		}
		if strings.Contains(out, line) {
			t.Errorf("progress line %q leaked onto stdout, which must carry the report alone:\n%s", line, out)
		}
	}
}

// TestDoctorJSONProgressPrecedesTheEnvelope pins the NDJSON contract: under
// --json the progress events are lines on STDOUT before the envelope, and the
// envelope is always the last line.
//
// A consumer reading line by line has to be able to take the last line as the
// result. If a progress event were ever emitted after it — say by a check
// added below the report assembly — every such consumer would break, and the
// human mode would look completely fine.
func TestDoctorJSONProgressPrecedesTheEnvelope(t *testing.T) {
	setDataDir(t)
	isolateHome(t)
	if code, _, stderr := runCLI(t, "", "server", "add", "x", "--cmd", "no-such-binary"); code != ExitOK {
		t.Fatalf("server add: %s", stderr)
	}
	if code, _, stderr := runCLI(t, "", "server", "enable", "x", "--no-probe"); code != ExitOK {
		t.Fatalf("server enable: %s", stderr)
	}

	_, out, _ := runCLI(t, "", "doctor", "--json")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected progress lines before the envelope, got:\n%s", out)
	}
	var sawProbe bool
	for _, line := range lines[:len(lines)-1] {
		var ev struct {
			Event  string `json:"event"`
			Server string `json:"server"`
			Status string `json:"status"`
			OK     *bool  `json:"ok"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("progress line is not JSON: %v\n%s", err, line)
		}
		if ev.OK != nil {
			t.Fatalf("the envelope is not the last line; it appeared at:\n%s", line)
		}
		if ev.Event == "" {
			t.Errorf("progress line carries no event name:\n%s", line)
		}
		if ev.Event == "server_probed" {
			sawProbe = true
			if ev.Server != "x" || ev.Status != StatusFail {
				t.Errorf("server_probed = %+v, want server x / status fail", ev)
			}
		}
	}
	if !sawProbe {
		t.Error("no server_probed event: the per-server line is what tells a slow run which server it is waiting on")
	}
	// And the last line really is the report.
	report := decodeDoctor(t, doctorEnvelope(t, out))
	if len(report.Checks) == 0 {
		t.Error("the final line decoded as an envelope but carried no checks")
	}
}

// TestDoctorProbesEveryServerInSortedOrder covers the concurrent fan-out in
// checkServers, which is invisible to every other doctor test: they configure
// at most one server, and one goroutine cannot race, drop a result or reorder
// anything.
//
// Two properties, and the second is the one worth the test. Completeness — a
// slot left unwritten is a zero-valued DoctorCheck with an empty Name, so a
// dropped result shows up as a missing `server:<id>` rather than as a wrong
// status. And ORDER: results come back in whatever order the handshakes time
// out in, and are placed by index precisely so the report does not reorder
// between runs. Diffing today's doctor output against yesterday's is most of
// what the command is for, and a report that shuffles its own lines makes that
// useless while every individual line stays correct.
//
// Run under -race this also exercises the concurrent writes themselves; the
// mixed enabled/disabled set matters there, because disabled entries fill
// their slot on the calling goroutine while the others are in flight.
func TestDoctorProbesEveryServerInSortedOrder(t *testing.T) {
	setDataDir(t)
	isolateHome(t)
	// More entries than maxServerProbes, so the semaphore actually queues
	// rather than letting every probe start at once.
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}
	for i, id := range ids {
		if code, _, stderr := runCLI(t, "", "server", "add", id, "--cmd", "no-such-binary-"+id); code != ExitOK {
			t.Fatalf("server add %s: %s", id, stderr)
		}
		// Leave every third one disabled: those fill their slot without a
		// goroutine, so the two paths have to agree on placement.
		if i%3 == 2 {
			continue
		}
		if code, _, stderr := runCLI(t, "", "server", "enable", id, "--no-probe"); code != ExitOK {
			t.Fatalf("server enable %s: %s", id, stderr)
		}
	}

	// Every enabled entry names a binary that does not exist, so each probe
	// fails fast rather than sitting on handshakeTimeout — this stays a unit
	// test, not an 88-second one.
	_, out, _ := runCLI(t, "", "doctor", "--json")
	report := decodeDoctor(t, doctorEnvelope(t, out))

	var got []string
	for _, c := range report.Checks {
		if strings.HasPrefix(c.Name, "server:") {
			got = append(got, strings.TrimPrefix(c.Name, "server:"))
		}
	}
	if !slices.Equal(got, ids) {
		t.Errorf("server checks = %v, want %v (sorted, one per configured server).\n"+
			"A missing id means a result slot was never written; a reordered one means the "+
			"report no longer diffs against a previous run.", got, ids)
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
	if code, _, stderr := runCLI(t, "", "server", "enable", "x", "--no-probe"); code != ExitOK {
		t.Fatalf("server enable: %s", stderr)
	}
	if code, _, _ := runCLI(t, "", "server", "disable", "x"); code != ExitOK {
		t.Fatalf("disable failed")
	}
	code, out, _ := runCLI(t, "", "doctor", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	report := decodeDoctor(t, doctorEnvelope(t, out))
	srv := findCheck(t, report, "server:x")
	if srv.Status != StatusOK || !strings.Contains(srv.Detail, "disabled") {
		t.Errorf("server:x = %+v, want ok/disabled", srv)
	}
}

// TestDoctorDanglingActiveProfile pins docs/architecture.md §7: a
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
	pointActiveProfileAt(t, dir, "payments")
	code, out, _ := runCLI(t, "", "doctor", "--json")
	if code != ExitGeneral {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	report := decodeDoctor(t, doctorEnvelope(t, out))
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
	if code, _, stderr := runCLI(t, "", "server", "enable", "x", "--no-probe"); code != ExitOK {
		t.Fatalf("server enable: %s", stderr)
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
	report := decodeDoctor(t, doctorEnvelope(t, out))
	c := findCheck(t, report, "server:x")
	if c.Status != StatusWarn || !strings.Contains(c.Detail, "still installing") {
		t.Errorf("server:x = %+v, want a 'still installing' warn", c)
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
	env := doctorEnvelope(t, out)
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
	if code, _, stderr := runCLI(t, "", "server", "enable", "x", "--no-probe"); code != ExitOK {
		t.Fatalf("server enable: %s", stderr)
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
		c := findCheck(t, decodeDoctor(t, doctorEnvelope(t, out)), "previous-shutdown")
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
		c := findCheck(t, decodeDoctor(t, doctorEnvelope(t, out)), "previous-shutdown")
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
		c := findCheck(t, decodeDoctor(t, doctorEnvelope(t, out)), "previous-shutdown")
		if c.Status != StatusWarn {
			t.Fatalf("check = %+v, want warn (an unclean exit is survivable, not broken)", c)
		}
		if !strings.Contains(c.Detail, "crash") || c.Fix == "" {
			t.Fatalf("check = %+v, want a crash detail with guidance", c)
		}
	})
}

// TestDoctorWarnsAboutRetiredProjectBindings pins the ONE direction that made
// retiring the per-project layer dangerous: `projects` is no longer a modelled
// field, and the registry preserves unknown fields verbatim, so a legacy block
// survives on disk looking exactly as authoritative as it did while it worked
// — but it no longer narrows anything.
//
// The usual reason to have written one was to narrow a particular checkout, so
// its silent retirement WIDENS what that client sees. Doctor is the only thing
// standing between the operator and a widening they never asked for, which is
// why this is a warn rather than an info.
func TestDoctorWarnsAboutRetiredProjectBindings(t *testing.T) {
	dir := setDataDir(t)
	isolateHome(t)

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
	check := findCheck(t, decodeDoctor(t, doctorEnvelope(t, out)), "scope:projects")
	if check.Status != StatusWarn {
		t.Fatalf("status = %q, want warn — a retired rule that still LOOKS applied is a "+
			"silent widening, and reporting it as ok is how an operator never learns: %+v",
			check.Status, check)
	}
	if !strings.Contains(check.Detail, "claude-code") {
		t.Errorf("detail does not name the client: %q", check.Detail)
	}
	// The operator must learn the rule is INERT, not merely present.
	if !strings.Contains(check.Detail, "no longer") {
		t.Errorf("detail does not say the block no longer applies: %q", check.Detail)
	}
	if check.Fix == "" {
		t.Error("a warning the operator cannot act on is noise: want a fix hint")
	}
}

// The check must stay silent when nothing is configured: a diagnostic that
// warns about a feature nobody used is noise, and noise is what makes the
// real warnings get skipped.
func TestDoctorSaysNothingAboutAbsentProjectBindings(t *testing.T) {
	setDataDir(t)
	isolateHome(t)
	_, out, _ := runCLI(t, "", "doctor", "--json")
	if hasCheck(decodeDoctor(t, doctorEnvelope(t, out)), "scope:projects") {
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
	check := findCheck(t, decodeDoctor(t, doctorEnvelope(t, out)), "registry:quarantined")
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
	doc := findCheck(t, decodeDoctor(t, doctorEnvelope(t, out)), "registry:servers")
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
	if hasCheck(decodeDoctor(t, doctorEnvelope(t, out)), "registry:quarantined") {
		t.Error("doctor reported registry:quarantined with an intact registry")
	}
}

// TestDoctorNeverClaimsAbsenceFromAFileItCannotRead: the three states
// between yes and no each get their own report. Reading the server lists
// alone made all of them "no agenthub gateway entry" — an absence asserted
// about a file doctor had not read — and suggested a connect that, for a
// format agenthub will not rewrite, cannot work.
func TestDoctorNeverClaimsAbsenceFromAFileItCannotRead(t *testing.T) {
	requireUnprivilegedCLI(t)
	setDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A format with no reader.
	write(t, filepath.Join(home, ".continue", "config.yaml"), "mcpServers:\n  - name: x\n")
	// A file that may not be read at all.
	blocked := filepath.Join(home, ".cursor", "mcp.json")
	write(t, blocked, `{"mcpServers":{}}`)
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o644) })

	report := decodeDoctor(t, doctorEnvelope(t, mustRun(t, "", "doctor", "--json")))
	checks := map[string]DoctorCheck{}
	for _, c := range report.Checks {
		checks[c.Name] = c
	}

	cont, ok := checks["client:continue"]
	if !ok {
		t.Fatalf("continue not reported: %+v", report.Checks)
	}
	if strings.Contains(cont.Detail, "no agenthub gateway entry") {
		t.Errorf("continue = %q, want an admission that the format is not read", cont.Detail)
	}
	cur, ok := checks["client:cursor"]
	if !ok {
		t.Fatalf("cursor not reported: %+v", report.Checks)
	}
	if strings.Contains(cur.Detail, "no agenthub gateway entry") || cur.Status != StatusWarn {
		t.Errorf("cursor = %+v, want a warning that it could not be read", cur)
	}
	if !strings.Contains(cur.Fix, "client inspect") {
		t.Errorf("cursor fix = %q, want it to point at inspect", cur.Fix)
	}
}

// TestDoctorReadsCodexTOML: the entry codex's own CLI wrote is agenthub's,
// and doctor must say so rather than report the client as unconfigured.
func TestDoctorReadsCodexTOML(t *testing.T) {
	setDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	write(t, filepath.Join(home, ".codex", "config.toml"),
		"[mcp_servers.agenthub]\ncommand = \"/bin/sh\"\n"+
			"args = [\"connect\", \"--client\", \"codex\"]\n")

	report := decodeDoctor(t, doctorEnvelope(t, mustRun(t, "", "doctor", "--json")))
	for _, c := range report.Checks {
		if c.Name != "client:codex" {
			continue
		}
		if !strings.Contains(c.Detail, "gateway entry present") {
			t.Errorf("codex = %+v, want the entry recognised", c)
		}
		return
	}
	t.Errorf("codex not reported: %+v", report.Checks)
}
