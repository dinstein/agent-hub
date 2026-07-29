package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/clients"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/integrity"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
	"github.com/dinstein/agent-hub/internal/skills"
)

// Doctor check statuses. "warn" is informational (e.g. a file that will be
// created on first write); only "fail" affects the exit code.
const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusFail = "fail"
)

// handshakeTimeout bounds one per-server connectivity probe.
const handshakeTimeout = 8 * time.Second

// dockerProbeTimeout bounds the docker daemon probes. A wedged Docker
// Desktop must not hang the whole diagnostic.
const dockerProbeTimeout = 5 * time.Second

// coldCacheGrace is how long after the tool cache was last touched a
// missing entry is reported as "still installing" rather than as a broken
// server. A launcher (npx/uvx) downloading a package on first run is the
// single most common false positive in this whole report (docs/modules/controlplane.md:
// "a cold launcher cache reports 'still installing' instead of
// falsely flagging a broken server").
const coldCacheGrace = 10 * time.Minute

// DoctorCheck is a single diagnostic finding.
type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
	// Fix describes the repair --fix performed, or the command the operator
	// should run when the repair is destructive and doctor refuses to do it
	// (docs/modules/controlplane.md: destructive repairs are suggested, never executed).
	Fix string `json:"fix,omitempty"`
	// Fixed marks a check that --fix actually repaired.
	Fixed bool `json:"fixed,omitempty"`
}

// DoctorSummary counts checks by status.
type DoctorSummary struct {
	OK    int `json:"ok"`
	Warn  int `json:"warn"`
	Fail  int `json:"fail"`
	Fixed int `json:"fixed,omitempty"`
}

// DoctorReport is the `doctor` result, rendered identically (from this one
// value) in both output modes.
type DoctorReport struct {
	Checks  []DoctorCheck `json:"checks"`
	Summary DoctorSummary `json:"summary"`
}

// Human renders one line per check plus a summary line.
func (r DoctorReport) Human(w io.Writer) error {
	for _, c := range r.Checks {
		if _, err := fmt.Fprintf(w, "[%-4s] %s: %s\n", c.Status, c.Name, c.Detail); err != nil {
			return err
		}
		if c.Fix != "" {
			prefix := "       suggested fix: "
			if c.Fixed {
				prefix = "       fixed: "
			}
			if _, err := fmt.Fprintf(w, "%s%s\n", prefix, c.Fix); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(w, "%d ok, %d warn, %d fail",
		r.Summary.OK, r.Summary.Warn, r.Summary.Fail); err != nil {
		return err
	}
	if r.Summary.Fixed > 0 {
		if _, err := fmt.Fprintf(w, ", %d fixed", r.Summary.Fixed); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// newDoctorCmd builds the full diagnostic of docs/modules/controlplane.md.
//
// Doctor is read-only WITHOUT --fix: it never creates directories, files or
// locks (unlike registry.Open), so running it cannot change what it reports.
// With --fix it performs only SAFE self-healing (recreate a missing
// directory, repoint a stale gateway entry); anything destructive is
// reported as a command to run, never executed.
//
// Exit code: 0 when no check failed, 1 otherwise (statuses carry the detail;
// the JSON envelope stays ok:true because the report itself succeeded).
func (a *App) newDoctorCmd() *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use:   "doctor [--fix]",
		Short: "Diagnose the local agenthub installation (--fix performs safe repairs only)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			report := a.runDoctor(cmd.Context(), fix)
			if err := a.printer().Emit(report); err != nil {
				return err
			}
			if report.Summary.Fail > 0 {
				return &silentExitError{code: ExitGeneral}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false,
		"perform safe self-healing (recreate missing directories, repoint stale client entries)")
	return cmd
}

// doctorRun accumulates checks for one invocation.
type doctorRun struct {
	app    *App
	fix    bool
	cfg    doctorConfig
	checks []DoctorCheck
}

// doctorConfig is the registry content as read from disk WITHOUT opening
// the store. Unreadable documents come back empty; checkRegistryDoc has
// already reported them, so the later checks simply have nothing to say
// about a file that could not be parsed.
type doctorConfig struct {
	servers  map[string]registry.Doc[registry.ServerEntry]
	profiles map[string]registry.Doc[registry.Profile]
	clients  map[string]registry.Doc[registry.ClientEntry]
	// activeProfile is read from the governance document rather than through
	// the store: opening the store CREATES the registry directory, and
	// doctor without --fix must not materialize anything (it would change
	// what it reports).
	activeProfile string
}

// readDoctorConfig loads the routing documents read-only.
func readDoctorConfig(regDir string) doctorConfig {
	var cfg doctorConfig
	if regDir == "" {
		return cfg
	}
	var servers registry.ServersDoc
	if readDocFile(regDir, registry.DocServers, &servers) {
		cfg.servers = servers.Servers
	}
	var profiles registry.ProfilesDoc
	if readDocFile(regDir, registry.DocProfiles, &profiles) {
		cfg.profiles = profiles.Profiles
	}
	var clientsDoc registry.ClientsDoc
	if readDocFile(regDir, registry.DocClients, &clientsDoc) {
		cfg.clients = clientsDoc.Clients
	}
	var gov registry.GovernanceDoc
	if readDocFile(regDir, registry.DocGovernance, &gov) {
		cfg.activeProfile = gov.ActiveProfile
	}
	return cfg
}

func readDocFile(regDir string, kind registry.DocKind, out any) bool {
	b, err := os.ReadFile(filepath.Join(regDir, string(kind)+".json"))
	if err != nil {
		return false
	}
	return json.Unmarshal(b, out) == nil
}

// add appends a check and returns a pointer to it so the caller can attach
// a Fix without repeating the whole literal.
func (d *doctorRun) add(name, status, detail string) *DoctorCheck {
	d.checks = append(d.checks, DoctorCheck{Name: name, Status: status, Detail: detail})
	return &d.checks[len(d.checks)-1]
}

func (a *App) runDoctor(ctx context.Context, fix bool) DoctorReport {
	d := &doctorRun{app: a, fix: fix}

	dirs := d.checkDirectories()
	// Read the configuration WITHOUT registry.Open: opening the store
	// creates the directory, the five documents and a lock file, which
	// would make the diagnostic a writer and let it "fix" by accident the
	// very state it is reporting on. Everything below reads plain files.
	d.cfg = readDoctorConfig(dirs.registry)
	d.checkSocket(ctx)
	d.checkRegistry(dirs.registry)
	d.checkServers(ctx)
	d.checkVault(ctx)
	d.checkIntegrity(ctx, dirs.state)
	d.checkSkills(ctx, dirs.data)
	d.checkClientDrift(ctx)
	d.checkPath()

	report := DoctorReport{Checks: d.checks}
	for _, c := range d.checks {
		switch c.Status {
		case StatusOK:
			report.Summary.OK++
		case StatusWarn:
			report.Summary.Warn++
		case StatusFail:
			report.Summary.Fail++
		}
		if c.Fixed {
			report.Summary.Fixed++
		}
	}
	return report
}

// doctorDirs holds the resolved directory set so later checks do not
// re-resolve (and re-report) it.
type doctorDirs struct {
	data, registry, state, logs, run, cache string
}

// checkDirectories reports the three-state data-directory resolution
// (env override / platform default / unsupported platform) and the
// existence plus permissions of every derived directory.
func (d *doctorRun) checkDirectories() doctorDirs {
	var dirs doctorDirs
	data, err := d.app.resolver.DataDir()
	if err != nil {
		d.add("data-dir", StatusFail, err.Error()).Fix =
			"agenthub supports macOS and Linux in M1; set " + platform.EnvDataDir + " explicitly"
		return dirs
	}
	dirs.data = data
	origin := "platform default (" + runtime.GOOS + ")"
	if v, ok := os.LookupEnv(platform.EnvDataDir); ok && v != "" {
		origin = "override via " + platform.EnvDataDir
	}
	d.add("data-dir", StatusOK, fmt.Sprintf("%s: %s%s", origin, data, existsSuffix(data)))

	subs := []struct {
		name string
		get  func() (string, error)
		set  func(string)
	}{
		{"registry-dir", d.app.resolver.RegistryDir, func(s string) { dirs.registry = s }},
		{"state-dir", d.app.resolver.StateDir, func(s string) { dirs.state = s }},
		{"logs-dir", d.app.resolver.LogsDir, func(s string) { dirs.logs = s }},
		{"run-dir", d.app.resolver.RunDir, func(s string) { dirs.run = s }},
		{"cache-dir", d.app.resolver.CacheDir, func(s string) { dirs.cache = s }},
	}
	for _, s := range subs {
		path, gerr := s.get()
		if gerr != nil {
			d.add(s.name, StatusFail, gerr.Error())
			continue
		}
		s.set(path)
		d.checkOneDir(s.name, path)
	}
	return dirs
}

// checkOneDir reports one directory's existence and mode. A missing
// directory is a warning (it is created on first use) and is exactly the
// kind of thing --fix may recreate: making a directory is idempotent and
// destroys nothing. Widened permissions are NOT chmod'ed automatically:
// relaxing or tightening a mode the user may have set on purpose is not a
// repair we can prove safe, so it is only suggested.
func (d *doctorRun) checkOneDir(name, path string) {
	info, err := os.Stat(path)
	switch {
	case err == nil && info.IsDir():
		mode := info.Mode().Perm()
		status, detail := StatusOK, fmt.Sprintf("%s (mode %04o)", path, mode)
		if mode&0o077 != 0 {
			status = StatusWarn
			detail += " — group/other have access; agenthub creates its directories 0700"
		}
		c := d.add(name, status, detail)
		if status == StatusWarn {
			c.Fix = fmt.Sprintf("chmod 700 %s", path)
		}
	case err == nil:
		d.add(name, StatusFail, path+" exists but is not a directory").Fix =
			"move it aside: mv " + path + " " + path + ".bak"
	case errors.Is(err, fs.ErrNotExist):
		c := d.add(name, StatusWarn, path+" (not created yet)")
		if !d.fix {
			c.Fix = "re-run with --fix to create it"
			return
		}
		if mkErr := platform.EnsureDir(path); mkErr != nil {
			c.Status, c.Detail = StatusFail, mkErr.Error()
			return
		}
		c.Status, c.Detail, c.Fixed = StatusOK, path+" (created)", true
		c.Fix = "created the missing directory"
	default:
		d.add(name, StatusFail, err.Error())
	}
}

// checkSocket reports the control socket's reachability AND its
// permissions. The permission bits ARE the authentication mechanism (0600
// on a UDS is the whole reason the control plane ships no token), so a
// widened mode is a failure, not a cosmetic note.
func (d *doctorRun) checkSocket(ctx context.Context) {
	socket, err := d.app.resolver.CtlSocketPath()
	if err != nil {
		d.add("ctl-socket", StatusFail, err.Error())
		return
	}
	info, statErr := os.Lstat(socket)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		d.add("ctl-socket", StatusWarn, socket+" absent (daemon not running)").Fix =
			"start it with 'agenthub daemon start' if you want the shared pool"
		return
	case statErr != nil:
		d.add("ctl-socket", StatusFail, statErr.Error())
		return
	}
	if info.Mode()&os.ModeSocket == 0 {
		d.add("ctl-socket", StatusFail, socket+" exists but is not a socket").Fix =
			"remove the stale file: rm " + socket
		return
	}
	d.add("ctl-socket", StatusOK, socket+" present")
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		// Deliberately NOT chmod'ed under --fix: a live daemon owns this
		// file, and changing its mode underneath it is not provably safe.
		d.add("ctl-socket-perms", StatusFail, fmt.Sprintf(
			"%s is mode %04o; the control plane relies on 0600 for authentication", socket, mode)).Fix =
			"stop and restart the daemon: agenthub daemon restart"
	} else {
		d.add("ctl-socket-perms", StatusOK, fmt.Sprintf("%s mode %04o", socket, info.Mode().Perm()))
	}

	hello, perr := pingDaemon(ctx, socket)
	if perr != nil {
		d.add("daemon", StatusWarn, "socket exists but no daemon answered: "+perr.Error()).Fix =
			"remove the stale socket and restart: agenthub daemon restart"
		return
	}
	d.add("daemon", StatusOK, fmt.Sprintf("pid %d, version %s, registry generation %d",
		hello.Pid, hello.Version, hello.Generation))
}

// checkRegistry reports every document's readability, the lock file and the
// backup chain, plus the active profile's resolvability.
func (d *doctorRun) checkRegistry(regDir string) {
	if regDir == "" {
		return
	}
	for _, kind := range []registry.DocKind{
		registry.DocMeta, registry.DocServers, registry.DocProfiles,
		registry.DocClients, registry.DocGovernance,
	} {
		d.checks = append(d.checks, checkRegistryDoc(regDir, kind))
	}
	d.checkLock(regDir)
	d.checkQuarantinedDocs(regDir)
	d.checkBackups(regDir)
	d.checkPreviousShutdown(regDir)
	d.checkActiveProfile()
	d.checkRetiredProjectBindings()
}

// checkRetiredProjectBindings reports `projects` blocks left in clients.json
// after the per-project scope layer was retired.
//
// The registry preserves unknown fields verbatim (registry/envelope.go), which
// is normally the right call — it is what lets a newer agenthub's config
// survive an older binary. Here it is precisely the hazard: `projects` is no
// longer modelled, so a legacy block sits on disk looking exactly as
// authoritative as it did while it worked, and nothing about reading the file
// reveals that it stopped applying.
//
// Warn rather than info, and the direction is why: the reason to write a
// project binding was to NARROW one checkout, so its retirement widens what
// that client sees. An operator who is not told inherits a widening they never
// asked for. Deleting the block for them would be the other kind of mistake —
// doctor reports, the operator decides.
func (d *doctorRun) checkRetiredProjectBindings() {
	var stale []string
	for clientID, doc := range d.cfg.clients {
		if doc.HasUnknownField("projects") {
			stale = append(stale, clientID)
		}
	}
	if len(stale) == 0 {
		return // nothing configured: no finding, and no noise either
	}
	sort.Strings(stale)
	d.add("scope:projects", StatusWarn, fmt.Sprintf(
		"clients.json still carries per-project bindings (%s); that layer was retired and the "+
			"block no longer applies, so a rule written to narrow one checkout is now inert "+
			"and the client sees its full profile",
		strings.Join(stale, ", "))).Fix =
		"bind the client to a narrower profile instead: agenthub client bind <client> <profile>, " +
			"then delete the 'projects' block from clients.json"
}

// checkPreviousShutdown reports the crash marker: a
// long-running process arms it on start and resolves it on a clean stop, so
// a still-armed marker at read time means the last run died without saying
// goodbye. Read-only by construction (PreviousShutdown never arms).
//
// An unclean exit is a WARN, not a FAIL: agenthub survives it by design
// (atomic writes, O_APPEND audit lines, quarantine on unparsable files), so
// the finding is "here is what explains the odd state you may be seeing",
// not "something is broken right now".
func (d *doctorRun) checkPreviousShutdown(regDir string) {
	switch registry.PreviousShutdown(regDir) {
	case registry.ShutdownClean:
		d.add("previous-shutdown", StatusOK, "clean")
	case registry.ShutdownCrash:
		d.add("previous-shutdown", StatusWarn,
			"crash — the previous run never resolved its marker (kill -9, panic or power loss)").Fix =
			"nothing to repair: state is written atomically. Check 'agenthub server logs <id>' " +
				"and the daemon log for what the previous run was doing"
	default:
		d.add("previous-shutdown", StatusOK, "unknown (no marker yet — no long-running process has started here)")
	}
}

// checkLock reports the .lock file. flock state cannot be probed without
// TAKING the lock, which doctor must not do (that would make the diagnostic
// itself a writer and could block a live daemon), so what is reported is
// the file's existence and age — enough to recognise the situation an
// operator actually gets stuck in.
func (d *doctorRun) checkLock(regDir string) {
	path := filepath.Join(regDir, ".lock")
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		d.add("registry:lock", StatusOK, "absent (no writer has run yet)")
	case err != nil:
		d.add("registry:lock", StatusFail, err.Error())
	default:
		d.add("registry:lock", StatusOK, fmt.Sprintf(
			"present, last touched %s (advisory flock state cannot be probed without taking the lock)",
			info.ModTime().UTC().Format(time.RFC3339)))
	}
}

// checkQuarantinedDocs reports registry documents that were set aside as
// unreadable, which is the one registry event that costs the operator DATA.
//
// It exists because the per-document checks above cannot see it. Quarantine
// renames the corrupt file and writes a fresh empty one in its place, so
// `registry:servers` afterwards reports "readable" — perfectly true, and
// exactly the wrong thing to read when every server has just disappeared.
// The warning is issued once, at the moment of the quarantine, by whichever
// command happened to trigger it; a user who runs doctor later to find out
// where their configuration went would otherwise be told everything is fine.
//
// Warn rather than fail: nothing is presently broken, the registry works. What
// is true is that data was set aside and has not been recovered, which is why
// the finding persists until the operator deals with the file — that is what
// makes it actionable rather than a permanent nag.
func (d *doctorRun) checkQuarantinedDocs(regDir string) {
	matches, err := filepath.Glob(filepath.Join(regDir, "*.unreadable-*"))
	if err != nil || len(matches) == 0 {
		return // nothing set aside: no finding, and no noise
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, filepath.Base(m))
	}
	sort.Strings(names)
	d.add("registry:quarantined", StatusWarn, fmt.Sprintf(
		"%d registry document(s) were unreadable and set aside, and the working copy was reset: %s",
		len(names), strings.Join(names, ", "))).Fix =
		"the previous good copy is usually in " + filepath.Join(regDir, "backups") +
			"; recover what you need, then delete the .unreadable-* file to clear this check"
}

// checkBackups reports the registry backup chain. An empty chain is normal
// before the first write; an unreadable backup directory is not.
func (d *doctorRun) checkBackups(regDir string) {
	dir := filepath.Join(regDir, "backups")
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		d.add("registry:backups", StatusOK, "no backup chain yet (created on first write)")
		return
	case err != nil:
		d.add("registry:backups", StatusFail, err.Error())
		return
	}
	var newest time.Time
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		count++
		if info, ierr := e.Info(); ierr == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if count == 0 {
		d.add("registry:backups", StatusOK, "backup directory present, still empty")
		return
	}
	d.add("registry:backups", StatusOK, fmt.Sprintf("%d backup file(s), newest %s",
		count, newest.UTC().Format(time.RFC3339)))
}

// checkActiveProfile surfaces a dangling active profile, which fail-closes
// every followActive binding to an EMPTY scope. toolport left this silent;
// making it loud is the point (docs/architecture.md §7, improvement 5).
func (d *doctorRun) checkActiveProfile() {
	active := d.cfg.activeProfile
	if active == "" {
		d.add("active-profile", StatusOK, "none set (every registered server is visible)")
		return
	}
	if _, ok := d.cfg.profiles[active]; !ok {
		d.add("active-profile", StatusFail, fmt.Sprintf(
			"active profile %q does not exist; every followActive binding resolves to an EMPTY scope",
			active)).Fix = "agenthub profile use -   (or recreate it: agenthub profile create " + active + ")"
		return
	}
	d.add("active-profile", StatusOK, active)
}

// checkServers times a handshake against every ENABLED server.
//
// The cold-cache carve-out is the important part: a stdio server launched
// through npx/uvx spends its first run downloading a package, and reporting
// that as a broken server is the classic false positive.
func (d *doctorRun) checkServers(ctx context.Context) {
	cached, cacheErr := gateway.LoadToolCache(d.app.resolver, nil)
	if cacheErr != nil {
		d.add("tool-cache", StatusWarn, cacheErr.Error())
	}
	cacheAge := d.app.toolCacheAge()

	enabled := 0
	for _, id := range sortedKeys(d.cfg.servers) {
		entry := d.cfg.servers[id].V
		if !entry.Enabled {
			d.add("server:"+id, StatusOK, "disabled (intentionally off is not broken)")
			continue
		}
		enabled++
		d.checkOneServer(ctx, id, entry, cached[id], cacheAge)
	}
	d.checkDockerRuntime(ctx)
	if enabled == 0 {
		d.add("servers", StatusWarn, "no enabled servers configured").Fix =
			"add one with 'agenthub server add <name> --cmd ...'"
	}
}

func (d *doctorRun) checkOneServer(
	ctx context.Context, id string, entry registry.ServerEntry,
	cachedTools []mcp.ToolDef, cacheAge time.Duration,
) {
	name := "server:" + id
	// A container entry is handshaked like any other (the dial spawns the
	// container, not the host command), but an invalid `docker run` line is
	// worth catching before the dial: the spawn failure it produces names the
	// symptom, and this names the flag to fix.
	if entry.IsDocker() {
		if err := validateDockerEntry(id, entry); err != nil {
			d.add(name, StatusFail, "docker runtime configuration is invalid: "+err.Error()).Fix =
				"agenthub server rm " + id + "   (then re-add it with corrected --image/--mount flags)"
			return
		}
	}
	start := time.Now()
	tools, protocol, err := d.app.probeServer(ctx, id, entry)
	elapsed := time.Since(start).Round(time.Millisecond)
	switch {
	case err == nil:
		d.add(name, StatusOK, fmt.Sprintf("handshake %s in %s, %d tool(s)", protocol, elapsed, tools))
	case len(cachedTools) == 0 && cacheAge >= 0 && cacheAge < coldCacheGrace:
		// Cold launcher cache: the package is most likely still downloading.
		d.add(name, StatusWarn, fmt.Sprintf(
			"no handshake yet and the tool cache is still cold (%s old) — the launcher is probably still installing: %v",
			cacheAge.Round(time.Second), err))
	case len(cachedTools) > 0:
		d.add(name, StatusFail, fmt.Sprintf(
			"handshake failed after %s (%d tool(s) cached from an earlier run): %v",
			elapsed, len(cachedTools), err)).Fix = "agenthub server test " + id
	default:
		d.add(name, StatusFail, fmt.Sprintf("handshake failed after %s: %v", elapsed, err)).Fix =
			"agenthub server test " + id
	}
}

// checkDockerRuntime reports on the container runtime, but only when a
// server actually asks for it: shelling out to docker on a machine that has
// none is noise, and doctor output that people scroll past is doctor output
// that does not work.
func (d *doctorRun) checkDockerRuntime(ctx context.Context) {
	wanted := 0
	for _, id := range sortedKeys(d.cfg.servers) {
		e := d.cfg.servers[id].V
		if e.Enabled && e.IsDocker() {
			wanted++
		}
	}
	if wanted == 0 {
		return
	}
	bin, err := transport.DockerBinary("")
	if err != nil {
		d.add("docker-runtime", StatusFail, fmt.Sprintf(
			"%d server(s) use the docker runtime but the docker CLI was not found: %v", wanted, err)).Fix =
			"install Docker, or switch those servers back to the host runtime"
		return
	}
	dctx, cancel := context.WithTimeout(ctx, dockerProbeTimeout)
	defer cancel()
	version, err := transport.DockerVersion(dctx, "")
	if err != nil {
		d.add("docker-runtime", StatusFail, fmt.Sprintf("%s found at %s but %v", "docker CLI", bin, err)).Fix =
			"start Docker Desktop (or the docker service) and re-run 'agenthub doctor'"
		return
	}
	d.add("docker-runtime", StatusOK, fmt.Sprintf("%s, daemon %s, %d server(s) isolated", bin, version, wanted))

	// Strays are the kill -9 residue: --rm cleans up the normal exit, so
	// anything still labelled here outlived its gateway.
	strays, err := transport.StrayContainers(dctx, "")
	if err != nil {
		d.add("docker-containers", StatusWarn, "could not list agenthub containers: "+err.Error())
		return
	}
	if len(strays) == 0 {
		d.add("docker-containers", StatusOK, "no leftover agenthub containers")
		return
	}
	names := make([]string, 0, len(strays))
	for _, c := range strays {
		names = append(names, c.Name)
	}
	d.add("docker-containers", StatusWarn, fmt.Sprintf(
		"%d leftover agenthub container(s): %s", len(strays), strings.Join(names, ", "))).Fix =
		"docker rm -f $(docker ps -aq --filter label=" + transport.LabelManaged + "=true)"
}

// probeServer connects to one server, counts its tools and disconnects.
// The connection is short-lived by design: doctor answers "does the
// handshake work right now", it does not join the shared pool.
func (a *App) probeServer(ctx context.Context, id string, entry registry.ServerEntry) (tools int, protocol string, err error) {
	spec, err := downstream.SpecFromEntry(id, entry)
	if err != nil {
		return 0, "", err
	}
	deps, err := a.newOAuthDeps(entry.Provenance == registry.ProvenanceLocal)
	if err != nil {
		return 0, "", err
	}
	ddeps := downstream.Deps{Secrets: deps.chain.Resolver(), ConnectTimeout: handshakeTimeout}
	if spec.IsHTTP() {
		ddeps.Auth = deps.tokenSource(id)
	}
	pctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	srv, err := downstream.Connect(pctx, spec, ddeps)
	if err != nil {
		return 0, "", err
	}
	defer srv.Close()
	if ir := srv.InitializeResult(); ir != nil {
		protocol = ir.ProtocolVersion
	}
	return len(srv.Tools()), protocol, nil
}

// checkVault self-tests the credential backend with a READ-ONLY probe.
//
// It must never prompt: a `doctor` run that pops a keychain dialog is a
// command people stop running. The probe is a Get of a ref that cannot
// exist, which internal/secrets answers from the cheap levels and — for the
// keyring level — with a READ, never a write (a write probe is what
// triggers the destructive-confirmation prompt on macOS).
func (d *doctorRun) checkVault(ctx context.Context) {
	dir, err := d.app.secretsDir()
	if err != nil {
		d.add("vault", StatusFail, err.Error())
		return
	}
	// The chain CREATES its directory on first use; probing before the
	// first secret exists would make the diagnostic a writer. Nothing
	// stored means nothing to self-test.
	if _, serr := os.Stat(dir); errors.Is(serr, fs.ErrNotExist) {
		d.add("vault", StatusOK, "no vault directory yet (created when the first secret is stored)")
		return
	}
	chain, _, err := d.app.secretChain()
	if err != nil {
		d.add("vault", StatusFail, err.Error())
		return
	}
	backend := "keyring (with the enc-file fallback)"
	switch {
	case os.Getenv(secrets.EnvEncKey) != "":
		backend = "enc-file (" + secrets.EnvEncKey + " is set)"
	case os.Getenv(secrets.EnvDevSecrets) == "1":
		backend = "enc-file, dev key (" + secrets.EnvDevSecrets + "=1)"
	}
	pctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if _, _, gerr := chain.Get(pctx, secrets.Ref{ServerID: "__agenthub_doctor__", Key: "__probe__"}); gerr != nil {
		d.add("vault", StatusFail, fmt.Sprintf("%s: read-only probe failed: %v", backend, gerr)).Fix =
			"check " + secrets.EnvEncKey + "; agenthub never overwrites a store it cannot read"
		return
	}
	refs, lerr := chain.List(pctx)
	if lerr != nil {
		d.add("vault", StatusFail, fmt.Sprintf("%s: cannot enumerate stored keys: %v", backend, lerr))
		return
	}
	d.add("vault", StatusOK, fmt.Sprintf("%s, %s stored (values are never read), dir %s",
		backend, plural(len(refs), "key", "keys"), dir))
}

// checkIntegrity reports the <state> stores. A corrupt store is a FAILURE
// and never reads as "nothing recorded": treating it as empty would
// silently un-quarantine every isolated tool.
func (d *doctorRun) checkIntegrity(ctx context.Context, stateDir string) {
	if stateDir == "" {
		return
	}
	// Opening a store CREATES <state>; skip while it does not exist so the
	// diagnostic stays read-only.
	if _, serr := os.Stat(stateDir); errors.Is(serr, fs.ErrNotExist) {
		d.add("integrity:quarantine", StatusOK, "no integrity state yet")
		return
	}
	q, err := integrity.OpenQuarantineStore(stateDir, integrity.Options{LockTimeout: d.app.lockTimeout})
	if err != nil {
		d.add("integrity:quarantine", StatusFail, err.Error())
		return
	}
	snap, err := q.Snapshot(ctx)
	if err != nil {
		d.add("integrity:quarantine", StatusFail, err.Error()).Fix =
			"inspect " + filepath.Join(stateDir, "quarantine.json") +
				" (agenthub never rewrites a store it cannot read)"
		return
	}
	if len(snap) == 0 {
		d.add("integrity:quarantine", StatusOK, "empty")
	} else {
		names := sortedKeys(snap)
		d.add("integrity:quarantine", StatusWarn, fmt.Sprintf("%s quarantined: %s",
			plural(len(names), "tool", "tools"), strings.Join(names, ", "))).Fix =
			"review with 'agenthub tool quarantine ls', then release with 'agenthub tool quarantine release <exposed>'"
	}
	if _, oerr := d.app.loadOverrides(); oerr != nil {
		d.add("integrity:overrides", StatusFail, oerr.Error())
	} else {
		d.add("integrity:overrides", StatusOK, "readable")
	}
}

// checkSkills reports the skill library's health. List is read-only by
// contract, so this cannot materialize or re-record anything.
func (d *doctorRun) checkSkills(ctx context.Context, dataDir string) {
	if dataDir == "" {
		return
	}
	dir := filepath.Join(dataDir, "skills")
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		d.add("skills", StatusOK, "no skill library yet")
		return
	}
	mgr, err := skills.Open(dir, skills.Options{LockTimeout: d.app.lockTimeout})
	if err != nil {
		d.add("skills", StatusFail, err.Error())
		return
	}
	views, err := mgr.List(ctx, skills.ListOptions{})
	if err != nil {
		d.add("skills", StatusFail, err.Error())
		return
	}
	var tampered []string
	for _, v := range views {
		if v.Library == skills.LibraryTampered {
			tampered = append(tampered, v.Skill.ID)
		}
	}
	if len(tampered) > 0 {
		d.add("skills", StatusFail,
			"library copies no longer match their pin: "+strings.Join(tampered, ", ")).Fix =
			"agenthub skill verify"
		return
	}
	d.add("skills", StatusOK, plural(len(views), "library entry", "library entries"))
}

// checkClientDrift verifies every gateway entry agenthub wrote into a
// client configuration: does the binary it points at still exist, and does
// the profile the client's scope binding names still exist.
func (d *doctorRun) checkClientDrift(ctx context.Context) {
	found := clients.Default().Detect(ctx, "")
	if len(found) == 0 {
		d.add("clients", StatusOK, "no client configurations found on this machine")
		return
	}
	seen := map[string]bool{}
	for _, det := range found {
		if seen[det.Client] {
			continue
		}
		seen[det.Client] = true
		if det.Denied {
			d.add("client:"+det.Client, StatusWarn,
				det.Path+" is not readable: "+det.Remediation)
			continue
		}
		d.checkOneClient(det.Client)
	}
}

func (d *doctorRun) checkOneClient(clientID string) {
	name := "client:" + clientID
	insp, err := clients.Inspect(clientID)
	if err != nil && len(insp.Files) == 0 {
		d.add(name, StatusWarn, err.Error())
		return
	}
	var issues []string
	state, _ := insp.ConnectState()
	for _, f := range insp.Files {
		for _, s := range f.Servers {
			if !s.Owned || s.Command == "" {
				continue
			}
			if !binaryExists(s.Command) {
				issues = append(issues, fmt.Sprintf("%s points at %q, which is not executable", f.Path, s.Command))
			}
		}
	}
	// The states between yes and no are reported as themselves. Walking the
	// server lists alone made every one of them "no agenthub gateway entry"
	// — an absence asserted about a file doctor had not read — and then
	// suggested a connect, which for an unreadable or unwritable format is
	// a command that cannot work.
	switch state {
	case clients.ConnectedNo:
		d.add(name, StatusOK, "configured, no agenthub gateway entry").Fix =
			"agenthub client connect " + clientID
		return
	case clients.ConnectedUnknown:
		d.add(name, StatusOK,
			"configured; agenthub does not read this client's configuration format").Fix =
			"agenthub client connect " + clientID + " --dry-run   (prints what to add by hand)"
		return
	case clients.ConnectedDenied, clients.ConnectedUnreadable:
		c := d.add(name, StatusWarn, "cannot tell whether it is connected: "+firstFileError(insp))
		c.Fix = "agenthub client inspect " + clientID
		return
	}
	issues = append(issues, d.danglingProfileIssue(clientID)...)
	if len(issues) == 0 {
		d.add(name, StatusOK, "gateway entry present and resolvable")
		return
	}
	c := d.add(name, StatusFail, strings.Join(issues, "; "))
	if !d.fix {
		c.Fix = "agenthub client connect " + clientID + "   (or re-run: agenthub doctor --fix)"
		return
	}
	// Repointing a stale entry is safe self-healing: the write is the same
	// idempotent merge `client connect` performs and it only ever rewrites
	// the entry agenthub itself owns. A dangling profile is NOT auto-fixed
	// — guessing which profile the operator meant is exactly the kind of
	// repair that must stay a suggestion.
	if fixErr := d.app.repointClient(clientID); fixErr != nil {
		c.Fix = "repoint failed (" + fixErr.Error() + "); run: agenthub client connect " + clientID
		return
	}
	c.Status, c.Fixed = StatusWarn, true
	c.Fix = "repointed the gateway entry at " + d.app.executable() +
		" (a dangling profile, if any, still needs 'agenthub client bind')"
}

// danglingProfileIssue reports a client scope binding whose profile does
// not exist. Such a binding fail-closes to an EMPTY scope, which looks
// exactly like "the agent has no tools" — the failure this check exists to
// name.
func (d *doctorRun) danglingProfileIssue(clientID string) []string {
	entry, ok := d.cfg.clients[clientID]
	if !ok {
		return nil
	}
	b := entry.V.Binding()
	if b.Kind != registry.BindingNamed {
		return nil
	}
	if _, exists := d.cfg.profiles[b.Name]; exists {
		return nil
	}
	return []string{fmt.Sprintf(
		"scope binding names missing profile %q -> EMPTY scope (fail-closed)", b.Name)}
}

// binaryExists reports whether a command written into a client entry can
// still be executed: an absolute/relative path is stat'ed, a bare name is
// looked up on PATH.
func binaryExists(cmd string) bool {
	if strings.ContainsRune(cmd, os.PathSeparator) {
		info, err := os.Stat(cmd)
		return err == nil && !info.IsDir()
	}
	_, err := exec.LookPath(cmd)
	return err == nil
}

// checkPath reports whether the running binary is reachable through PATH,
// which is what every client configuration snippet and doc example assumes.
func (d *doctorRun) checkPath() {
	exe, err := os.Executable()
	if err != nil {
		d.add("path", StatusWarn, "cannot resolve the running executable: "+err.Error())
		return
	}
	resolved, lerr := exec.LookPath("agenthub")
	if lerr != nil {
		d.add("path", StatusWarn,
			"'agenthub' is not on PATH; client entries fall back to the absolute path "+exe).Fix =
			"add " + filepath.Dir(exe) + " to PATH"
		return
	}
	if sameFile(resolved, exe) {
		d.add("path", StatusOK, resolved)
		return
	}
	d.add("path", StatusWarn, fmt.Sprintf(
		"PATH resolves 'agenthub' to %s but this process is %s", resolved, exe)).Fix =
		"make sure clients invoke the build you expect"
}

// repointClient rewrites clientID's gateway entry to point at this binary.
//
// It repoints THE FILES THAT HOLD A STALE ENTRY, not the file a fresh
// connect would choose. The drift the doctor found lives in a specific file
// (the check reports it by path), and that file may not be the current
// default target — writing a correct entry into the default while leaving the
// broken one in place would clear the diagnosis without fixing the client.
func (a *App) repointClient(clientID string) error {
	format, _, err := a.clientTarget(clientID, "", "")
	if err != nil {
		return err
	}
	insp, err := clients.Inspect(clientID)
	if err != nil && len(insp.Files) == 0 {
		return err
	}
	plan := ConnectSnippet(a.executable(), clientID)
	entry := clients.Entry{Command: plan.Entry.Command, Args: plan.Entry.Args}
	repointed := 0
	for _, f := range insp.Files {
		if !f.Connected {
			continue
		}
		if _, err := format.Connect(f.Path, entry); err != nil {
			return err
		}
		repointed++
	}
	if repointed == 0 {
		// Nothing owned anywhere: connect the default target, which is what
		// "there is no entry" calls for.
		_, err = format.Connect(format.DefaultPath(a.clientBaseDir()), entry)
		return err
	}
	return nil
}

func sameFile(x, y string) bool {
	xi, err := os.Stat(x)
	if err != nil {
		return false
	}
	yi, err := os.Stat(y)
	if err != nil {
		return false
	}
	return os.SameFile(xi, yi)
}

// checkRegistryDoc reads one registry document without side effects:
// missing files are a warning (registry.Open creates them on first write),
// unparseable files are a failure. meta.json additionally reports the
// current generation.
func checkRegistryDoc(regDir string, kind registry.DocKind) DoctorCheck {
	name := "registry:" + string(kind)
	path := filepath.Join(regDir, string(kind)+".json")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return DoctorCheck{Name: name, Status: StatusWarn, Detail: "missing (created on first use)"}
	}
	if err != nil {
		return DoctorCheck{Name: name, Status: StatusFail, Detail: err.Error()}
	}
	if kind == registry.DocMeta {
		var meta registry.MetaDoc
		if jerr := json.Unmarshal(b, &meta); jerr != nil {
			return DoctorCheck{Name: name, Status: StatusFail, Detail: fmt.Sprintf("unparseable: %v", jerr)}
		}
		return DoctorCheck{Name: name, Status: StatusOK, Detail: fmt.Sprintf("generation %d", meta.Generation)}
	}
	var v any
	if jerr := json.Unmarshal(b, &v); jerr != nil {
		return DoctorCheck{
			Name: name, Status: StatusFail, Detail: fmt.Sprintf("unparseable: %v", jerr),
			Fix: "fix the JSON by hand; agenthub quarantines (never deletes) a file it cannot read",
		}
	}
	return DoctorCheck{Name: name, Status: StatusOK, Detail: fmt.Sprintf("readable (%d bytes)", len(b))}
}

// existsSuffix annotates a path with its existence state; a missing
// directory is normal before first use, so it is informational, not a
// warning.
func existsSuffix(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return " (not created yet)"
	} else if err != nil {
		return fmt.Sprintf(" (stat: %v)", err)
	}
	return " (exists)"
}

// toolCacheAge returns the age of the newest tool-cache entry, or -1 when
// there is no cache at all. It is what tells "still installing" apart from
// "broken".
func (a *App) toolCacheAge() time.Duration {
	dir, err := a.resolver.CacheDir()
	if err != nil {
		return -1
	}
	var newest time.Time
	walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, werr error) error {
		if werr != nil || entry.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree is not a cache age
		}
		if info, ierr := entry.Info(); ierr == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	if walkErr != nil || newest.IsZero() {
		return -1
	}
	return time.Since(newest)
}

// firstFileError names the location that stopped an inspection reaching an
// answer. Reporting "something failed" without the path leaves the operator
// with nothing to act on.
func firstFileError(insp clients.Inspection) string {
	for _, f := range insp.Files {
		if f.Error != "" {
			return f.Error
		}
	}
	return "no location could be read"
}
