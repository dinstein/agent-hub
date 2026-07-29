package cli

import (
	"fmt"
	"strings"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/registry"
)

// Docker runtime surface of the CLI (docs/modules/foundation.md, M2).
//
// Everything here happens at CONFIGURATION time — when the operator can
// still fix what they typed. `server add` renders the exact `docker run`
// line the spawner would produce, validates it, and screens it with the
// spawn guard. That is the same predicate the spawner injects at spawn
// time, so a config accepted here is a config that will not be refused
// later for a reason the operator never saw.

// dockerFlags is the docker half of `server add`.
type dockerFlags struct {
	runtime string
	image   string
	mounts  []string
	network string
	memory  string
	cpus    string
	user    string
	workdir string
	extra   []string
}

// any reports whether the operator used any docker flag, which is what
// makes `--image` alone enough to select the docker runtime.
func (f dockerFlags) any() bool {
	return f.runtime != "" || f.image != "" || len(f.mounts) > 0 || f.network != "" ||
		f.memory != "" || f.cpus != "" || f.user != "" || f.workdir != "" || len(f.extra) > 0
}

// applyDockerFlags fills the runtime half of a stdio entry.
//
// Inference mirrors the transport inference above it: --image with no
// explicit --runtime means docker. An explicit --runtime host together with
// docker flags is a contradiction and is refused rather than resolved —
// silently ignoring an isolation flag is the failure this rejects.
func applyDockerFlags(entry *registry.ServerEntry, f dockerFlags, usage func(string, ...any) error) error {
	switch f.runtime {
	case "":
		if !f.any() {
			return nil // no runtime flags at all: the host default stands
		}
		// --image (or any container flag) with no explicit --runtime.
	case registry.RuntimeDocker:
	case registry.RuntimeHost:
		if hasDockerOnlyFlags(f) {
			return usage("--image/--mount/--network/--memory/--cpus apply to '--runtime docker' only")
		}
		entry.Runtime = registry.RuntimeHost
		return nil
	default:
		return &Error{
			Code: CodeUsage, ExitCode: ExitUsage,
			Message: fmt.Sprintf("unknown runtime %q", f.runtime),
			Hint:    "supported runtimes: host (default), docker",
		}
	}

	mounts, err := parseMountFlags(f.mounts)
	if err != nil {
		return err
	}
	entry.Runtime = registry.RuntimeDocker
	entry.Docker = &registry.DockerRuntime{
		Image:     f.image,
		Network:   f.network,
		Mounts:    mounts,
		Memory:    f.memory,
		CPUs:      f.cpus,
		User:      f.user,
		Workdir:   f.workdir,
		ExtraArgs: f.extra,
	}
	return nil
}

func hasDockerOnlyFlags(f dockerFlags) bool {
	return f.image != "" || len(f.mounts) > 0 || f.network != "" || f.memory != "" ||
		f.cpus != "" || f.user != "" || f.workdir != "" || len(f.extra) > 0
}

// parseMountFlags parses `SRC[:DST][:ro|rw]`. Read-only is the default, so
// omitting the mode lands on the safe side.
func parseMountFlags(specs []string) ([]registry.DockerMount, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]registry.DockerMount, 0, len(specs))
	for _, s := range specs {
		parts := strings.Split(s, ":")
		m := registry.DockerMount{Source: parts[0]}
		switch len(parts) {
		case 1:
		case 2:
			if mode, ok := mountMode(parts[1]); ok {
				m.Write = mode
			} else {
				m.Target = parts[1]
			}
		case 3:
			m.Target = parts[1]
			mode, ok := mountMode(parts[2])
			if !ok {
				return nil, Usagef("--mount %q: mode must be 'ro' or 'rw'", s)
			}
			m.Write = mode
		default:
			return nil, Usagef("--mount %q: expected SRC[:DST][:ro|rw]", s)
		}
		if strings.TrimSpace(m.Source) == "" {
			return nil, Usagef("--mount %q: source is required", s)
		}
		out = append(out, m)
	}
	return out, nil
}

func mountMode(s string) (write bool, ok bool) {
	switch s {
	case "ro":
		return false, true
	case "rw":
		return true, true
	default:
		return false, false
	}
}

// dockerConfigFor, dockerRunLine and validateDockerEntry delegate to
// internal/confops: the generated `docker run` line the operator is shown
// must be the exact line the spawn guard judged and the spawner will run, so
// there is one renderer, not one per front end.
func dockerConfigFor(id string, e registry.ServerEntry) transport.DockerConfig {
	return confops.DockerConfigFor(id, e)
}

func dockerRunLine(id string, e registry.ServerEntry) ([]string, error) {
	return confops.DockerRunLine(id, e)
}

// validateDockerEntry runs the configuration-time check chain for one entry:
// runtime shape → container config → spawn-guard screen.
func validateDockerEntry(id string, e registry.ServerEntry) error {
	return opsError(confops.ValidateDockerEntry(id, e))
}

// Probing a docker-runtime entry needs no special case here, and used to.
//
// The probe paths (`server test`, `doctor`, `server enable`) reach a
// container through the same SpecFromEntry -> downstream.Connect path the
// gateway uses, because the runtime dimension rides INSIDE the stdio
// transport rather than beside it: Spec.Docker is a pointer whose nil value
// is the only thing meaning "host", and dialStdio hands a non-nil one to
// SpawnDocker. A container entry therefore cannot degrade into a host spawn
// by defaulting — which is the exact failure these paths once refused over.
