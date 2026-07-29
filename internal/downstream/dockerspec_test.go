package downstream

import (
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/registry"
)

// The whole point of these tests is that a config asking for container
// isolation can never quietly turn into a host spawn. Before this wiring
// existed, SpecFromEntry ignored the runtime field entirely: `runtime:
// docker` produced a perfectly ordinary host child, so an operator reading
// their config would believe a server was contained while it held the same
// privileges as the gateway itself.

func TestDockerRuntimeReachesTheSpec(t *testing.T) {
	entry := registry.ServerEntry{
		Command: "server",
		Args:    []string{"--stdio"},
		Runtime: registry.RuntimeDocker,
		Docker: &registry.DockerRuntime{
			Image:   "example/mcp:1",
			Network: "bridge",
			Memory:  "512m",
			CPUs:    "1.5",
			User:    "1000:1000",
			Workdir: "/work",
			Mounts: []registry.DockerMount{
				{Source: "/host/ro"},
				{Source: "/host/rw", Target: "/data", Write: true},
			},
			ExtraArgs: []string{"--pids-limit", "64"},
		},
	}
	spec, err := SpecFromEntry("contained", entry)
	if err != nil {
		t.Fatalf("SpecFromEntry: %v", err)
	}
	if spec.Docker == nil {
		t.Fatal("docker runtime produced a host spec: isolation was dropped silently")
	}
	if spec.Docker.Image != "example/mcp:1" || spec.Docker.Network != "bridge" {
		t.Errorf("image/network = %q/%q", spec.Docker.Image, spec.Docker.Network)
	}
	if spec.Docker.Memory != "512m" || spec.Docker.CPUs != "1.5" {
		t.Errorf("limits = %q/%q", spec.Docker.Memory, spec.Docker.CPUs)
	}
	if spec.Docker.User != "1000:1000" || spec.Docker.Workdir != "/work" {
		t.Errorf("user/workdir = %q/%q", spec.Docker.User, spec.Docker.Workdir)
	}
	if len(spec.Docker.Mounts) != 2 {
		t.Fatalf("mounts = %d, want 2", len(spec.Docker.Mounts))
	}
	if spec.Docker.Mounts[0].Write {
		t.Error("a mount without an explicit write flag must stay read-only")
	}
	if got := spec.Docker.Mounts[1]; got.Target != "/data" || !got.Write {
		t.Errorf("second mount = %+v", got)
	}
	if len(spec.Docker.ExtraRunArgs) != 2 {
		t.Errorf("extra args = %v", spec.Docker.ExtraRunArgs)
	}
}

func TestHostRuntimeProducesNoDockerConfig(t *testing.T) {
	spec, err := SpecFromEntry("plain", registry.ServerEntry{Command: "server"})
	if err != nil {
		t.Fatalf("SpecFromEntry: %v", err)
	}
	if spec.Docker != nil {
		t.Error("a host entry must not carry a container config")
	}
}

func TestUnusableIsolationIsRefusedNotDowngraded(t *testing.T) {
	cases := []struct {
		name  string
		entry registry.ServerEntry
		want  string
	}{{
		name:  "typo in the runtime name",
		entry: registry.ServerEntry{Command: "server", Runtime: "dcoker"},
		want:  "dcoker",
	}, {
		name:  "docker without an image",
		entry: registry.ServerEntry{Command: "server", Runtime: registry.RuntimeDocker},
		want:  "image",
	}, {
		name: "container block on a host entry",
		entry: registry.ServerEntry{
			Command: "server",
			Docker:  &registry.DockerRuntime{Image: "example/mcp:1"},
		},
		want: "docker block",
	}, {
		name: "container block on a transport that spawns nothing",
		entry: registry.ServerEntry{
			Transport: registry.TransportHTTP,
			URL:       "https://example.test/mcp",
			Runtime:   registry.RuntimeDocker,
			Docker:    &registry.DockerRuntime{Image: "example/mcp:1"},
		},
		want: "stdio",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SpecFromEntry("srv", tc.entry)
			if err == nil {
				t.Fatal("want an error; a config that cannot be honored must never dial")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
