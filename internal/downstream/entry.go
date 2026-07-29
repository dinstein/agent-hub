package downstream

import (
	"fmt"
	"maps"
	"slices"

	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/registry"
)

// SpecFromEntry maps one registry entry onto a connection spec. It is the
// SINGLE place that translation happens — gateway, daemon and CLI all call
// it — so a new transport or a new connection-relevant field cannot land in
// one caller and be silently dropped by another.
//
// Validation is deliberate and fail-closed: an entry that names a transport
// this build cannot speak, or that is missing the field its transport
// requires, is an error here rather than a confusing dial failure later.
// Secret placeholders are NOT resolved (that happens at dial time); the
// spec still carries them verbatim.
func SpecFromEntry(id string, e registry.ServerEntry) (Spec, error) {
	mode, err := ParseDeriveMode(e.Derive)
	if err != nil {
		return Spec{}, fmt.Errorf("server %q: %w", id, err)
	}
	// Validated for EVERY transport, not just the one that spawns: an entry
	// naming a runtime this build cannot honor must fail here rather than
	// dial with the isolation quietly dropped.
	if err := e.ValidateRuntime(); err != nil {
		return Spec{}, fmt.Errorf("server %q: %w", id, err)
	}
	spec := Spec{
		ID:         id,
		Derive:     mode,
		Command:    e.Command,
		Args:       e.Args,
		Env:        maps.Clone(e.Env),
		Cwd:        e.Cwd,
		URL:        e.URL,
		Headers:    maps.Clone(e.Headers),
		Provenance: e.Provenance,
	}
	switch e.TransportName() {
	case registry.TransportStdio:
		spec.Kind = transport.Stdio
		if e.Command == "" {
			return Spec{}, fmt.Errorf("server %q: the stdio transport needs a command", id)
		}
		spec.Docker = dockerConfigFor(e)
	case registry.TransportHTTP:
		spec.Kind = transport.StreamableHTTP
		if e.URL == "" {
			return Spec{}, fmt.Errorf("server %q: the http transport needs a url", id)
		}
	case registry.TransportSSE:
		spec.Kind = transport.SSE
		if e.URL == "" {
			return Spec{}, fmt.Errorf("server %q: the sse transport needs a url", id)
		}
	default:
		return Spec{}, fmt.Errorf("server %q: unknown transport %q (want %q, %q or %q)",
			id, e.Transport, registry.TransportStdio, registry.TransportHTTP, registry.TransportSSE)
	}
	return spec, nil
}

// dockerConfigFor translates the registry's container block into the
// spawner's configuration, or returns nil for a host runtime. It runs only
// after SpecFromEntry has validated the runtime, so every entry reaching it
// is already known to be honorable.
//
// nil means host and nothing else. The refusal that protects this — an
// entry asking for isolation it cannot get is an error, never a host spawn
// — lives in ValidateRuntime at the top of SpecFromEntry, because it must
// also cover transports that never reach this function.
func dockerConfigFor(e registry.ServerEntry) *transport.DockerConfig {
	if !e.IsDocker() {
		return nil
	}
	d := e.Docker
	// Workdir falls back to the entry's cwd: for a container that field names
	// a directory inside the image, and SpawnDocker applies it as --workdir.
	// Leaving it out here would still work (the spawner reads Spec.Cwd too)
	// but would make this config disagree with the one confops renders for
	// `server inspect`, and those two must stay identical.
	workdir := d.Workdir
	if workdir == "" {
		workdir = e.Cwd
	}
	cfg := &transport.DockerConfig{
		Image:        d.Image,
		Network:      d.Network,
		Memory:       d.Memory,
		CPUs:         d.CPUs,
		User:         d.User,
		Workdir:      workdir,
		ExtraRunArgs: slices.Clone(d.ExtraArgs),
	}
	for _, m := range d.Mounts {
		cfg.Mounts = append(cfg.Mounts, transport.Mount{
			Source: m.Source,
			Target: m.Target,
			Write:  m.Write,
		})
	}
	return cfg
}
