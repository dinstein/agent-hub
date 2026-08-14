// Docker isolation spawner (docs/subsystems/protocol.md, M2).
//
// Positioning: the baseline spawn guard is anti-smuggling, not a sandbox.
// This file is the resource/namespace isolation half — a stdio transport
// variant whose host process is `docker run -i --rm ...` and whose MCP peer
// speaks newline-delimited JSON-RPC over the container's stdin/stdout,
// exactly like the host runtime.
//
// It drives the docker CLI with os/exec and imports no SDK: internal/mcp is
// standard-library only (canonical.md §2 rule 2), and shelling out is also
// what makes DOCKER_HOST, contexts and credential helpers work without
// re-implementing any of them.
//
// Isolation defaults (deny by default; every relaxation is explicit config):
//
//   - network `none` — an MCP server that needs the network says so,
//   - only explicitly declared directories are mounted, read-only unless
//     the mount sets Write,
//   - no --privileged, no host namespaces, no capability grants are ever
//     emitted; the flags this file owns cannot be overridden through
//     ExtraRunArgs,
//   - container names carry the "agenthub-" prefix and an
//     `agenthub.managed=true` label so strays are findable and sweepable.
//
// Secrets: container environment is passed as `-e NAME` (value inherited
// from the docker CLI's own environment), never as `-e NAME=VALUE`. Argv is
// world-readable through ps(1); the CLI's environment is not.
package transport

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Typed docker failures. They exist so an operator sees the actual cause
// instead of a downstream deadline: the failure modes below are the ones
// that otherwise look identical (a container that never speaks).
var (
	// ErrDockerUnavailable: the docker CLI could not be located.
	ErrDockerUnavailable = errors.New("docker CLI not found")
	// ErrDockerDaemon: the CLI is present but the daemon refused or is
	// not running.
	ErrDockerDaemon = errors.New("docker daemon unreachable")
	// ErrDockerImage: the image does not exist locally and could not be
	// pulled.
	ErrDockerImage = errors.New("docker image unavailable")
	// ErrDockerConfig: the container configuration is invalid — refused
	// before anything is spawned.
	ErrDockerConfig = errors.New("invalid docker runtime configuration")
)

// NetworkNone is the default container network: no interfaces at all.
const NetworkNone = "none"

// containerNamePrefix marks every container agenthub creates. Grep-able and
// sweepable; paired with the managed label below.
const containerNamePrefix = "agenthub-"

// Labels stamped on every managed container.
const (
	LabelManaged = "agenthub.managed"
	LabelServer  = "agenthub.server"
)

// dockerStopGrace bounds the best-effort container removal in Close. It is
// short on purpose: --rm already handles the normal path, and a slow docker
// daemon must not hold a gateway shutdown hostage.
const dockerStopGrace = 5 * time.Second

// Mount is one explicitly declared host directory exposed to the container.
// Read-only is the default: Write is opt-in per mount, so a config that
// forgets to think about it lands on the safe side.
type Mount struct {
	Source string // absolute host path
	Target string // absolute container path; empty means "same as Source"
	Write  bool   // false (the zero value) mounts read-only
}

// target returns the effective container path.
func (m Mount) target() string {
	if m.Target != "" {
		return m.Target
	}
	return m.Source
}

// DockerConfig describes the container a stdio downstream runs in.
type DockerConfig struct {
	// Image is the container image reference (required).
	Image string
	// Network is the docker network. Empty means NetworkNone.
	Network string
	// Mounts are the only host paths the container can see.
	Mounts []Mount
	// Env names the environment variables forwarded INTO the container.
	// Values are passed through the docker CLI's own environment, never
	// through argv (see the package comment).
	Env map[string]string
	// Memory and CPUs are the resource limits (`--memory`, `--cpus`).
	// Empty means "no limit configured".
	Memory string
	CPUs   string
	// User and Workdir map to `--user` / `--workdir`.
	User    string
	Workdir string
	// ServerID labels the container so strays trace back to a config entry
	// and seeds the generated container name.
	ServerID string
	// ContainerName overrides the generated name. Generated names are
	// unique per spawn on purpose: agenthub runs one gateway process per
	// client, so several processes legitimately run the same server at the
	// same time and a deterministic per-server name would collide. (This
	// is the one place the mcpproxy recipe does not transfer: its
	// "idempotent pre-clean by fixed name" assumes a single daemon.)
	ContainerName string
	// CIDFile is where docker writes the container id, so Close can remove
	// the exact container even if --rm never ran. Empty means "generate
	// one under os.TempDir()".
	CIDFile string
	// ExtraRunArgs are appended verbatim after the flags this file owns.
	// They cannot re-specify an owned flag (that would silently weaken the
	// isolation defaults) and they are screened by StdioConfig.Screen.
	ExtraRunArgs []string
	// Binary overrides docker CLI discovery.
	Binary string
}

// network returns the effective network name.
func (c *DockerConfig) network() string {
	if c.Network == "" {
		return NetworkNone
	}
	return c.Network
}

// SpawnDocker starts cfg.Command/cfg.Args inside a container and returns the
// stdio Transport speaking to it. cfg.Docker must be non-nil.
//
// Failure directions:
//   - docker CLI missing            → ClassFatal wrapping ErrDockerUnavailable
//     (a missing runtime is a configuration fact, not a flaky downstream:
//     retrying it and burning the breaker budget helps nobody),
//   - invalid container config      → ClassFatal wrapping ErrDockerConfig,
//   - spawn failure                 → ClassUnavailable,
//   - image missing / daemon down   → the container exits immediately, every
//     pending call fails ClassUnavailable, and Stderr() carries an explicit
//     "agenthub: ..." diagnosis line instead of a bare timeout.
func SpawnDocker(cfg StdioConfig) (Transport, error) {
	if cfg.Docker == nil {
		return nil, &Error{Class: ClassFatal, Err: fmt.Errorf("docker transport: nil DockerConfig")}
	}
	dc := *cfg.Docker // copy: generated name/cidfile must not leak into the caller's config

	// StdioConfig.Cwd means "the directory the server runs in". For a host
	// spawn that is the child's working directory; for a container it is
	// --workdir, because the child's directory is a path INSIDE the image.
	// Applying it to the docker CLI process instead would be a silent no-op
	// for the workload — the entry asks for a working directory and gets
	// none, with nothing to notice. An explicit DockerConfig.Workdir is the
	// more specific statement and wins.
	if dc.Workdir == "" {
		dc.Workdir = cfg.Cwd
	}

	bin, err := DockerBinary(dc.Binary)
	if err != nil {
		return nil, &Error{Class: ClassFatal, Err: err}
	}
	if dc.ContainerName == "" {
		dc.ContainerName = generateContainerName(dc.ServerID)
	}
	cleanupCID := func() {}
	if dc.CIDFile == "" {
		path, dir, cerr := newCIDFilePath()
		if cerr != nil {
			return nil, &Error{Class: ClassUnavailable, Err: cerr}
		}
		dc.CIDFile = path
		cleanupCID = func() { _ = os.RemoveAll(dir) }
	}

	args, err := BuildDockerRunArgs(dc, cfg.Command, cfg.Args)
	if err != nil {
		cleanupCID()
		return nil, &Error{Class: ClassFatal, Err: err}
	}

	// The docker CLI's own environment: the caller's env (PATH/HOME/
	// DOCKER_HOST/...) plus the values named by DockerConfig.Env, plus the
	// CLI's directory prepended to PATH. That last one is the launchd/
	// systemd lesson: a service-launched process has a truncated PATH and
	// docker's credential helpers live next to the binary.
	env := dockerEnv(cfg.Env, dc.Env, filepath.Dir(bin))

	if err := screen(cfg.Screen, bin, args, env); err != nil {
		cleanupCID()
		return nil, err
	}

	// Deliberately NOT cmd.Dir = cfg.Cwd: that directory was interpreted
	// above as a path inside the container, and it need not exist on this
	// host. The docker CLI is a control-plane client — where it runs from
	// does not affect the workload — so it inherits the gateway's directory.
	cmd := exec.Command(bin, args...)
	cmd.Env = env

	cid := dc.CIDFile
	cleanup := func() {
		removeContainer(bin, env, cid, dc.ContainerName)
		cleanupCID()
	}
	return launch(cmd, bin+" run "+dc.Image, diagnoseDocker(dc.Image), cleanup)
}

// BuildDockerRunArgs renders the `docker run` argument vector. It is pure
// and total: same config in, same argv out (golden-tested) — nothing here
// reads the clock, the environment or the filesystem.
//
// command/args are the process started inside the container; an empty
// command uses the image's own entrypoint/CMD.
func BuildDockerRunArgs(cfg DockerConfig, command string, args []string) ([]string, error) {
	if err := validateDockerConfig(cfg); err != nil {
		return nil, err
	}
	out := []string{"run", "-i", "--rm"}
	if cfg.ContainerName != "" {
		// SpawnDocker always names the container; a name is optional here
		// so callers can render (and screen) the run line for a config
		// before an instance of it exists.
		out = append(out, "--name", cfg.ContainerName)
	}
	out = append(out, "--label", LabelManaged+"=true")
	if cfg.ServerID != "" {
		out = append(out, "--label", LabelServer+"="+cfg.ServerID)
	}
	if cfg.CIDFile != "" {
		out = append(out, "--cidfile", cfg.CIDFile)
	}
	out = append(out, "--network", cfg.network())
	if cfg.Memory != "" {
		out = append(out, "--memory", cfg.Memory)
	}
	if cfg.CPUs != "" {
		out = append(out, "--cpus", cfg.CPUs)
	}
	if cfg.User != "" {
		out = append(out, "--user", cfg.User)
	}
	if cfg.Workdir != "" {
		out = append(out, "--workdir", cfg.Workdir)
	}
	for _, m := range sortedMounts(cfg.Mounts) {
		mode := ":ro"
		if m.Write {
			mode = ":rw"
		}
		out = append(out, "-v", m.Source+":"+m.target()+mode)
	}
	for _, name := range sortedKeys(cfg.Env) {
		// `-e NAME` (no value): docker inherits it from the CLI's own
		// environment, keeping secrets out of argv.
		out = append(out, "-e", name)
	}
	out = append(out, cfg.ExtraRunArgs...)
	out = append(out, cfg.Image)
	if command != "" {
		out = append(out, command)
	}
	out = append(out, args...)
	return out, nil
}

// ownedRunFlags are the flags BuildDockerRunArgs emits itself. ExtraRunArgs
// may not re-specify them: docker's last-wins semantics would let an extra
// `--network host` silently undo the isolation default, and a config that
// contradicts itself is a bug, not an override.
var ownedRunFlags = map[string]bool{
	"-i": true, "--interactive": true, "--rm": true,
	"--name": true, "--cidfile": true, "--network": true, "--net": true,
	"--memory": true, "-m": true, "--cpus": true,
	"--user": true, "-u": true, "--workdir": true, "-w": true,
	"--volume": true, "-v": true, "--mount": true,
	"--env": true, "-e": true, "--env-file": true,
	"--label": true, "-l": true,
}

var (
	containerNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	memoryRe        = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[bkmgBKMG]?$`)
	cpusRe          = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	envNameRe       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	networkNameRe   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:/-]*$`)
	sanitizeNameRe  = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)
)

func configErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrDockerConfig, fmt.Sprintf(format, a...))
}

// validateDockerConfig rejects, before any process is started, every shape
// that would either be silently mis-parsed by docker or would smuggle a
// second flag through a value slot (a mount source containing ':', an image
// starting with '-', ...).
func validateDockerConfig(cfg DockerConfig) error {
	if strings.TrimSpace(cfg.Image) == "" {
		return configErr("image is required")
	}
	if strings.HasPrefix(cfg.Image, "-") {
		return configErr("image %q must not start with '-'", cfg.Image)
	}
	if strings.ContainsAny(cfg.Image, " \t\n") {
		return configErr("image %q must not contain whitespace", cfg.Image)
	}
	if cfg.ContainerName != "" {
		if !strings.HasPrefix(cfg.ContainerName, containerNamePrefix) {
			return configErr("container name %q must start with %q", cfg.ContainerName, containerNamePrefix)
		}
		if !containerNameRe.MatchString(cfg.ContainerName) {
			return configErr("container name %q is not a valid docker name", cfg.ContainerName)
		}
	}
	if n := cfg.network(); !networkNameRe.MatchString(n) {
		return configErr("network %q is not a valid docker network name", n)
	}
	if cfg.Memory != "" && !memoryRe.MatchString(cfg.Memory) {
		return configErr("memory limit %q is not a docker size (e.g. 512m, 2g)", cfg.Memory)
	}
	if cfg.CPUs != "" && !cpusRe.MatchString(cfg.CPUs) {
		return configErr("cpu limit %q is not a decimal number (e.g. 1.5)", cfg.CPUs)
	}
	if cfg.CIDFile != "" && !filepath.IsAbs(cfg.CIDFile) {
		return configErr("cidfile %q must be an absolute path", cfg.CIDFile)
	}
	for _, m := range cfg.Mounts {
		if !isAbsMountPath(m.Source) {
			return configErr("mount source %q must be an absolute path", m.Source)
		}
		if !isAbsMountPath(m.target()) {
			return configErr("mount target %q must be an absolute path", m.target())
		}
		if strings.ContainsAny(m.Source, ":") || strings.ContainsAny(m.target(), ":") {
			return configErr("mount %q:%q must not contain ':'", m.Source, m.target())
		}
	}
	for name := range cfg.Env {
		if !envNameRe.MatchString(name) {
			return configErr("environment variable name %q is not valid", name)
		}
	}
	for _, a := range cfg.ExtraRunArgs {
		if owned, why := ownsRunFlag(a); owned {
			return configErr("extra run arg %q re-specifies %s, a flag the isolation defaults own", a, why)
		}
	}
	return nil
}

// dockerShortWithValue are `docker run` shorthand flags that consume a
// value. They matter only for scanning PAST them: in a cluster like `-td`
// the scan continues, but in `-p8080:80` everything after `p` is the port
// spec and holds no further flags.
//
// Kept small on purpose — it needs to cover the shorthands that can precede
// an owned one in a cluster, not all of docker's surface.
var dockerShortWithValue = map[byte]bool{
	'a': true, 'c': true, 'e': true, 'h': true, 'l': true,
	'm': true, 'p': true, 'u': true, 'v': true, 'w': true,
}

// ownsRunFlag reports whether one ExtraRunArgs token re-specifies a flag
// BuildDockerRunArgs emits itself, in ANY spelling docker accepts, and
// names the flag it matched.
//
// Comparing the token up to '=' was not enough. docker's flag parser takes
// a shorthand's value attached to the letter, so `--user 0:0`, `--user=0:0`,
// `-u 0:0` and `-u0:0` are four spellings of one flag and only the first
// three have `-u` as a prefix of a separate token. The isolation defaults
// are emitted first and docker's last-wins parsing means the extra arg
// won — `-u0:0` ran the container as root under a config that said
// `user: 1000:1000`, and `-v/home/user/.ssh:/keys` added a host mount the
// declared mounts never contained. That is the "isolation a config claims
// must be delivered or refused" rule failing in the silent direction.
//
// Failure direction: FAIL-CLOSED for every flag it recognises — a doubtful
// spelling is refused rather than passed to docker. It is fail-open past an
// unrecognised shorthand, because guessing whether an unknown letter takes
// a value would reject working configurations as docker's surface grows.
// That residue is not the only gate: spawnguard inspects the assembled
// command line for container-escape flags without consulting this table.
func ownsRunFlag(arg string) (bool, string) {
	key, _, _ := strings.Cut(arg, "=")
	if ownedRunFlags[key] {
		return true, key
	}
	// Long flags carry no attached-value spelling beyond `=`, already cut.
	if strings.HasPrefix(arg, "--") || !strings.HasPrefix(arg, "-") || len(arg) < 2 {
		return false, ""
	}
	for i := 1; i < len(key); i++ {
		short := "-" + string(key[i])
		if ownedRunFlags[short] {
			return true, short
		}
		if dockerShortWithValue[key[i]] {
			break // the rest of the token is this flag's value
		}
	}
	return false, ""
}

// isAbsMountPath accepts POSIX absolute paths on every host: container
// targets are always POSIX, and a Windows host still spells its sources
// C:\... — filepath.IsAbs alone would reject "/work" when cross-checked on
// a Windows host, so both spellings are accepted.
func isAbsMountPath(p string) bool {
	return strings.HasPrefix(p, "/") || filepath.IsAbs(p)
}

// sortedMounts orders mounts by (target, source) so the argv is a function
// of the config and not of map/slice authoring order.
func sortedMounts(in []Mount) []Mount {
	out := append([]Mount(nil), in...)
	slices.SortStableFunc(out, func(a, b Mount) int {
		return cmp.Or(cmp.Compare(a.target(), b.target()), cmp.Compare(a.Source, b.Source))
	})
	return out
}

func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}

// dockerEnv builds the docker CLI's own environment: base, minus any name
// the container config overrides, plus those names in sorted order, with
// binDir prepended to PATH.
func dockerEnv(base []string, containerEnv map[string]string, binDir string) []string {
	if base == nil {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+len(containerEnv))
	pathSeen := false
	for _, kv := range base {
		name, val, _ := strings.Cut(kv, "=")
		if _, ok := containerEnv[name]; ok {
			continue
		}
		if isPathVar(name) {
			pathSeen = true
			kv = name + "=" + prependPath(val, binDir)
		}
		out = append(out, kv)
	}
	if !pathSeen && binDir != "" && binDir != "." {
		out = append(out, pathVarName()+"="+binDir)
	}
	for _, k := range sortedKeys(containerEnv) {
		out = append(out, k+"="+containerEnv[k])
	}
	return out
}

func pathVarName() string {
	if runtime.GOOS == "windows" {
		return "Path"
	}
	return "PATH"
}

func isPathVar(name string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(name, "PATH")
	}
	return name == "PATH"
}

func prependPath(current, dir string) string {
	if dir == "" || dir == "." {
		return current
	}
	sep := string(os.PathListSeparator)
	for _, p := range strings.Split(current, sep) {
		if p == dir {
			return current
		}
	}
	if current == "" {
		return dir
	}
	return dir + sep + current
}

// dockerFallbackPaths are the well-known install locations checked when PATH
// does not have docker. A launchd/systemd-launched gateway inherits a
// truncated PATH and Docker Desktop keeps its CLI inside the app bundle;
// failing there with "not found" while the user has docker running is the
// single most confusing outcome this list prevents.
var dockerFallbackPaths = []string{
	"/usr/local/bin/docker",
	"/opt/homebrew/bin/docker",
	"/Applications/Docker.app/Contents/Resources/bin/docker",
	"/usr/bin/docker",
	"/snap/bin/docker",
}

// DockerBinary resolves the docker CLI. override wins when non-empty;
// otherwise PATH is searched, then the well-known locations. The result is
// an absolute path so the child's PATH can be extended with its directory.
//
// Returns ErrDockerUnavailable (wrapped, with a remediation hint) when the
// CLI cannot be found anywhere.
func DockerBinary(override string) (string, error) {
	if override != "" {
		if abs, err := exec.LookPath(override); err == nil {
			return absOrSame(abs), nil
		}
		return "", fmt.Errorf("%w: configured docker binary %q is not executable", ErrDockerUnavailable, override)
	}
	if p, err := exec.LookPath("docker"); err == nil {
		return absOrSame(p), nil
	}
	for _, p := range dockerFallbackPaths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: install Docker (or set the server runtime back to host) — "+
		"searched PATH and %s", ErrDockerUnavailable, strings.Join(dockerFallbackPaths, ", "))
}

func absOrSame(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// DockerVersion probes the docker daemon and returns the server version.
// It is the doctor-facing liveness check: DockerBinary proves the CLI
// exists, this proves the daemon answers.
func DockerVersion(ctx context.Context, override string) (string, error) {
	bin, err := DockerBinary(override)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, bin, "version", "--format", "{{.Server.Version}}")
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderrOf(err))
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("%w: %s", ErrDockerDaemon, firstLine(detail))
	}
	return strings.TrimSpace(string(out)), nil
}

// DockerContainer is one agenthub-managed container as reported by
// StrayContainers.
type DockerContainer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Server string `json:"server,omitempty"`
}

// StrayContainers lists every container carrying the agenthub managed label,
// running or not. `--rm` normally leaves none behind; anything listed here
// survived a kill -9 of the gateway and is reported by doctor.
func StrayContainers(ctx context.Context, override string) ([]DockerContainer, error) {
	bin, err := DockerBinary(override)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, "ps", "--all",
		"--filter", "label="+LabelManaged+"=true",
		"--format", "{{.ID}}\t{{.Names}}\t{{.Status}}\t{{.Label \""+LabelServer+"\"}}")
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderrOf(err))
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("%w: %s", ErrDockerDaemon, firstLine(detail))
	}
	var list []DockerContainer
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		c := DockerContainer{ID: f[0]}
		if len(f) > 1 {
			c.Name = f[1]
		}
		if len(f) > 2 {
			c.Status = f[2]
		}
		if len(f) > 3 {
			c.Server = f[3]
		}
		list = append(list, c)
	}
	return list, nil
}

func stderrOf(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(ee.Stderr)
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// generateContainerName builds a unique, prefixed, docker-legal name.
func generateContainerName(serverID string) string {
	id := sanitizeNameRe.ReplaceAllString(serverID, "-")
	id = strings.Trim(id, "-._")
	if id == "" {
		id = "server"
	}
	if len(id) > 40 {
		id = id[:40]
	}
	return fmt.Sprintf("%s%s-%d-%d", containerNamePrefix, id, os.Getpid(), time.Now().UnixNano()%1e9)
}

// newCIDFilePath returns a path docker may create (docker refuses an
// existing cidfile) inside a private 0700 directory, plus that directory so
// the caller can remove it.
func newCIDFilePath() (path, dir string, err error) {
	dir, err = os.MkdirTemp("", "agenthub-cid-")
	if err != nil {
		return "", "", fmt.Errorf("docker transport: cidfile dir: %w", err)
	}
	return filepath.Join(dir, "cid"), dir, nil
}

// removeContainer is the belt-and-braces half of container lifecycle: --rm
// covers the normal exit, this covers "the CLI died before docker cleaned
// up". It removes by container id when the cidfile was written and falls
// back to the name otherwise. Every failure is ignored on purpose — the
// container may legitimately be gone already, and shutdown must not fail.
func removeContainer(bin string, env []string, cidFile, name string) {
	target := name
	if cidFile != "" {
		if data, err := os.ReadFile(cidFile); err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				target = id
			}
		}
	}
	if target == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerStopGrace)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "rm", "--force", "--volumes", target)
	cmd.Env = env
	_ = cmd.Run()
}

// dockerDiagnosis maps a stderr fragment to a typed cause. Order matters:
// daemon problems are checked first because a dead daemon also produces
// image-ish wording ("error during connect ... pull").
var dockerDiagnosis = []struct {
	needles []string
	err     error
	hint    string
}{
	{
		needles: []string{
			"cannot connect to the docker daemon",
			"is the docker daemon running",
			"error during connect",
			"docker daemon is not running",
			"permission denied while trying to connect to the docker daemon socket",
		},
		err:  ErrDockerDaemon,
		hint: "start Docker (or run 'agenthub doctor' for the full runtime check)",
	},
	{
		needles: []string{
			"unable to find image",
			"manifest unknown",
			"manifest for", // "manifest for X not found"
			"pull access denied",
			"repository does not exist",
			"no such image",
			"image not found",
			"not found: manifest unknown",
		},
		err:  ErrDockerImage,
		hint: "pull it first ('docker pull %s') or fix the image reference",
	},
}

// DiagnoseDockerStderr classifies a docker CLI stderr tail. ok=false means
// the failure is not one of the known runtime-level causes and the raw
// stderr is the best explanation available.
func DiagnoseDockerStderr(image, stderr string) (err error, ok bool) {
	low := strings.ToLower(stderr)
	for _, d := range dockerDiagnosis {
		for _, n := range d.needles {
			if strings.Contains(low, n) {
				hint := d.hint
				if strings.Contains(hint, "%s") {
					hint = fmt.Sprintf(hint, image)
				}
				return fmt.Errorf("%w: %s", d.err, hint), true
			}
		}
	}
	return nil, false
}

// diagnoseDocker decorates the stderr tail with an explicit diagnosis line.
// This is what keeps a missing image from reaching the operator as a bare
// "context deadline exceeded": internal/downstream embeds Stderr() in the
// initialization failure, so the cause travels with the error.
func diagnoseDocker(image string) func(string) string {
	return func(tail string) string {
		diag, ok := DiagnoseDockerStderr(image, tail)
		if !ok {
			return tail
		}
		if tail == "" {
			return "agenthub: " + diag.Error()
		}
		return tail + "\nagenthub: " + diag.Error()
	}
}
