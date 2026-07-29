package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/guard/spawnguard"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/registry"
)

func TestServerAddDockerRuntime(t *testing.T) {
	setDataDir(t)
	code, out, errOut := runCLI(t, "", "server", "add", "fs", "--json",
		"--cmd", "node", "--args", "/app/server.js",
		"--image", "ghcr.io/example/mcp-fs:1",
		"--mount", "/home/alice/proj:/work",
		"--mount", "/srv/out:/out:rw",
		"--memory", "512m", "--cpus", "1.5")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s\n%s", code, out, errOut)
	}
	var added struct {
		Added []ServerRow `json:"added"`
	}
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &added); err != nil {
		t.Fatal(err)
	}
	if len(added.Added) != 1 {
		t.Fatalf("added = %+v", added.Added)
	}
	row := added.Added[0]
	if row.Runtime != registry.RuntimeDocker {
		t.Fatalf("--image alone must select the docker runtime, got %q", row.Runtime)
	}
	if row.Docker == nil || row.Docker.Image != "ghcr.io/example/mcp-fs:1" {
		t.Fatalf("docker block = %+v", row.Docker)
	}
	if len(row.Docker.Mounts) != 2 {
		t.Fatalf("mounts = %+v", row.Docker.Mounts)
	}
	if row.Docker.Mounts[0].Write {
		t.Errorf("a mount without a mode must be read-only: %+v", row.Docker.Mounts[0])
	}
	if !row.Docker.Mounts[1].Write {
		t.Errorf(":rw not honored: %+v", row.Docker.Mounts[1])
	}
	if !strings.Contains(row.target(), "docker[ghcr.io/example/mcp-fs:1]") {
		t.Errorf("target column hides the container: %q", row.target())
	}
}

// TestServerAddHostRuntimeJSONUnchanged is the backward-compatibility half:
// an entry added the way every pre-M2 entry was added must not grow a
// runtime member anywhere.
func TestServerAddHostRuntimeJSONUnchanged(t *testing.T) {
	setDataDir(t)
	code, out, _ := runCLI(t, "", "server", "add", "plain", "--json", "--cmd", "npx", "--args", "-y,pkg")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if strings.Contains(out, "runtime") || strings.Contains(out, "docker") {
		t.Fatalf("host entry acquired a runtime member:\n%s", out)
	}
}

func TestServerAddDockerRejections(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			"unknown runtime",
			[]string{"--cmd", "node", "--runtime", "dcoker"},
			CodeUsage,
		},
		{
			"docker runtime without an image",
			[]string{"--cmd", "node", "--runtime", "docker"},
			CodeUsage,
		},
		{
			"container flags on an http entry",
			[]string{"--url", "https://mcp.example.com/mcp", "--image", "alpine"},
			CodeUsage,
		},
		{
			// An explicit "--runtime host" alongside an isolation flag is a
			// contradiction, and resolving it either way is worse than
			// refusing: honouring --image would ignore what the operator
			// typed, and honouring --runtime host would drop the isolation
			// they asked for while reporting success.
			"explicit host runtime together with container flags",
			[]string{"--cmd", "node", "--runtime", "host", "--image", "alpine"},
			CodeUsage,
		},
		{
			"explicit host runtime together with a mount",
			[]string{"--cmd", "node", "--runtime", "host", "--mount", "/srv:/srv:ro"},
			CodeUsage,
		},
		{
			"bad mount mode",
			[]string{"--cmd", "node", "--image", "alpine", "--mount", "/a:/b:rx"},
			CodeUsage,
		},
		{
			"bad memory limit",
			[]string{"--cmd", "node", "--image", "alpine", "--memory", "lots"},
			CodeUsage,
		},
		{
			"relative mount source",
			[]string{"--cmd", "node", "--image", "alpine", "--mount", "rel/path:/work"},
			CodeUsage,
		},
		{
			"docker-arg overriding the isolation default",
			[]string{"--cmd", "node", "--image", "alpine", "--docker-arg", "--network=host"},
			CodeUsage,
		},
		{
			// The spawn guard, not this package, is what recognises the
			// escape — see TestGeneratedRunLinePassesTheSpawnGuard.
			"mount of a sensitive host root",
			[]string{"--cmd", "node", "--image", "alpine", "--mount", "/:/host"},
			CodeDenied,
		},
		{
			"docker-arg smuggling a privileged container",
			[]string{"--cmd", "node", "--image", "alpine", "--docker-arg", "--privileged"},
			CodeDenied,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setDataDir(t)
			args := append([]string{"server", "add", "s", "--json"}, tt.args...)
			code, out, _ := runCLI(t, "", args...)
			if code == ExitOK {
				t.Fatalf("accepted, want rejection:\n%s", out)
			}
			env := decodeEnvelope(t, out)
			if env.Error == nil || env.Error.Code != tt.want {
				t.Fatalf("error = %+v, want code %s", env.Error, tt.want)
			}
		})
	}
}

// TestRejectedEntryIsNotPersisted: validation runs before the store is
// opened, so a refused `docker run` line must leave no registry behind it.
func TestRejectedEntryIsNotPersisted(t *testing.T) {
	setDataDir(t)
	if code, _, _ := runCLI(t, "", "server", "add", "bad", "--json",
		"--cmd", "node", "--image", "alpine", "--mount", "/:/host"); code == ExitOK {
		t.Fatal("expected rejection")
	}
	code, out, _ := runCLI(t, "", "server", "ls", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if strings.Contains(out, `"bad"`) {
		t.Fatalf("refused entry was persisted:\n%s", out)
	}
}

// TestGeneratedRunLinePassesTheSpawnGuard is the alignment test docs/modules/foundation.md
//
//	asks for: the argv the isolation spawner actually generates, screened
//
// by the real spawn guard. It lives here because internal/cli is the only
// package allowed to import both (internal/mcp is standard-library only and
// internal/guard/* is a zero-business-dependency foundation).
//
// Two directions matter equally:
//   - the spawner's own output must NEVER be blocked — a guard that refuses
//     agenthub's own isolation defaults is an outage, not a safeguard;
//   - an escape shape reached through the docker config must still be
//     caught, and caught by the guard rather than by a second copy of the
//     rules living in the transport.
func TestGeneratedRunLinePassesTheSpawnGuard(t *testing.T) {
	guard := spawnguard.New(spawnguard.Config{})

	allowed := registry.ServerEntry{
		Command: "node", Args: []string{"/app/server.js"},
		Env:     map[string]string{"API_BASE": "https://x"},
		Runtime: registry.RuntimeDocker,
		Docker: &registry.DockerRuntime{
			Image:   "ghcr.io/example/mcp:1",
			Network: "bridge",
			Memory:  "512m", CPUs: "1.5",
			User: "1000:1000", Workdir: "/work",
			Mounts: []registry.DockerMount{
				{Source: "/home/alice/proj", Target: "/work"},
				{Source: "/etc/myapp", Target: "/config"},
				{Source: "/srv/out", Target: "/out", Write: true},
			},
			ExtraArgs: []string{"--read-only", "--pids-limit", "128"},
		},
	}
	args, err := dockerRunLine("demo", allowed)
	if err != nil {
		t.Fatalf("dockerRunLine: %v", err)
	}
	if err := guard.Check("docker", args, nil); err != nil {
		t.Fatalf("the spawner's own run line is blocked by the guard: %v\nargv: %v", err, args)
	}

	escapes := map[string]registry.DockerRuntime{
		"root mount":       {Image: "a", Mounts: []registry.DockerMount{{Source: "/", Target: "/host"}}},
		"docker socket":    {Image: "a", Mounts: []registry.DockerMount{{Source: "/var/run/docker.sock", Target: "/s"}}},
		"proc mount":       {Image: "a", Mounts: []registry.DockerMount{{Source: "/proc", Target: "/p"}}},
		"privileged extra": {Image: "a", ExtraArgs: []string{"--privileged"}},
		"host pid extra":   {Image: "a", ExtraArgs: []string{"--pid=host"}},
		"cap sys_admin":    {Image: "a", ExtraArgs: []string{"--cap-add", "SYS_ADMIN"}},
	}
	for name, dr := range escapes {
		t.Run(name, func(t *testing.T) {
			e := registry.ServerEntry{Command: "node", Runtime: registry.RuntimeDocker, Docker: &dr}
			argv, err := dockerRunLine("demo", e)
			if err != nil {
				t.Fatalf("dockerRunLine: %v", err)
			}
			if err := guard.Check("docker", argv, nil); err == nil {
				t.Fatalf("guard did not catch %s\nargv: %v", name, argv)
			}
		})
	}
}

// TestDockerConfigForMapsEveryField guards against a field added to the
// registry type and forgotten in the mapping — the failure mode there is
// silent (a limit the operator set that is never applied).
func TestDockerConfigForMapsEveryField(t *testing.T) {
	e := registry.ServerEntry{
		Env:     map[string]string{"T": "1"},
		Runtime: registry.RuntimeDocker,
		Docker: &registry.DockerRuntime{
			Image: "img", Network: "bridge", Memory: "1g", CPUs: "2",
			User: "u", Workdir: "/w",
			Mounts:    []registry.DockerMount{{Source: "/s", Target: "/t", Write: true}},
			ExtraArgs: []string{"--read-only"},
		},
	}
	got := dockerConfigFor("id", e)
	want := transport.DockerConfig{
		Image: "img", Network: "bridge", Memory: "1g", CPUs: "2",
		User: "u", Workdir: "/w", ServerID: "id",
		Env:          map[string]string{"T": "1"},
		Mounts:       []transport.Mount{{Source: "/s", Target: "/t", Write: true}},
		ExtraRunArgs: []string{"--read-only"},
	}
	if got.Image != want.Image || got.Network != want.Network || got.Memory != want.Memory ||
		got.CPUs != want.CPUs || got.User != want.User || got.Workdir != want.Workdir ||
		got.ServerID != want.ServerID || len(got.Mounts) != 1 || got.Mounts[0] != want.Mounts[0] ||
		len(got.ExtraRunArgs) != 1 || got.Env["T"] != "1" {
		t.Fatalf("dockerConfigFor = %+v, want %+v", got, want)
	}
}

// TestServerTestRefusesDockerEntry pins the fail-closed direction: probing a
// containerized entry through the connection layer would spawn it on the
// host, silently discarding the isolation. Refusing is the only honest
// answer until the connection layer selects the runtime.
func TestServerTestRefusesDockerEntry(t *testing.T) {
	setDataDir(t)
	if code, out, _ := runCLI(t, "", "server", "add", "boxed", "--json",
		"--cmd", "node", "--image", "alpine"); code != ExitOK {
		t.Fatalf("add failed:\n%s", out)
	}
	code, out, _ := runCLI(t, "", "server", "test", "boxed", "--json")
	if code == ExitOK {
		t.Fatalf("server test probed a docker entry:\n%s", out)
	}
	env := decodeEnvelope(t, out)
	if env.Error == nil || env.Error.Code != CodeNotImplemented {
		t.Fatalf("error = %+v, want %s", env.Error, CodeNotImplemented)
	}
	if !strings.Contains(env.Error.Message, "refusing to probe it on the host") {
		t.Fatalf("message does not say what it refused: %q", env.Error.Message)
	}
}

func TestParseMountFlags(t *testing.T) {
	tests := []struct {
		in   string
		want registry.DockerMount
	}{
		{"/data", registry.DockerMount{Source: "/data"}},
		{"/data:/mnt", registry.DockerMount{Source: "/data", Target: "/mnt"}},
		{"/data:ro", registry.DockerMount{Source: "/data"}},
		{"/data:rw", registry.DockerMount{Source: "/data", Write: true}},
		{"/data:/mnt:rw", registry.DockerMount{Source: "/data", Target: "/mnt", Write: true}},
		{"/data:/mnt:ro", registry.DockerMount{Source: "/data", Target: "/mnt"}},
	}
	for _, tt := range tests {
		got, err := parseMountFlags([]string{tt.in})
		if err != nil {
			t.Fatalf("%q: %v", tt.in, err)
		}
		if got[0] != tt.want {
			t.Errorf("%q = %+v, want %+v", tt.in, got[0], tt.want)
		}
	}
	for _, bad := range []string{"", ":/mnt", "/a:/b:/c:/d", "/a:/b:maybe"} {
		if _, err := parseMountFlags([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestDoctorReportsDockerRuntimeHonestly covers the doctor half of the
// fail-closed rule: a docker-runtime entry is NEVER probed, because the
// connection layer has no runtime dimension and probing would spawn the
// command on the host — discarding the isolation while reporting a healthy
// handshake. Doctor must instead validate what it can and say plainly that no
// handshake was attempted.
//
// This path (doctor → validateDockerEntry) had no test at all: every existing
// docker case went through `server add`, which validates on the way IN. Doctor
// is what re-validates entries that are already on disk, which is where a
// registry edited by hand, restored from a backup, or written by an older
// build shows up.
func TestDoctorReportsDockerRuntimeHonestly(t *testing.T) {
	setDataDir(t)
	isolateHome(t)
	if code, _, stderr := runCLI(t, "", "server", "add", "boxed",
		"--cmd", "node", "--image", "alpine:3"); code != ExitOK {
		t.Fatalf("add failed: %s", stderr)
	}
	if code, _, stderr := runCLI(t, "", "server", "enable", "boxed", "--no-probe"); code != ExitOK {
		t.Fatalf("enable failed: %s", stderr)
	}

	code, out, _ := runCLI(t, "", "doctor", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	check := findCheck(t, decodeDoctor(t, decodeEnvelope(t, out)), "server:boxed")
	if check.Status != StatusWarn {
		t.Fatalf("status = %q, want warn (a valid config that cannot be handshaked): %+v",
			check.Status, check)
	}
	// The wording is the point: an operator must not read this as "healthy".
	for _, want := range []string{"docker runtime", "no handshake"} {
		if !strings.Contains(check.Detail, want) {
			t.Errorf("detail %q missing %q", check.Detail, want)
		}
	}
}

// TestDoctorFailsAnInvalidDockerEntryOnDisk is the other half: an entry whose
// docker block cannot produce a valid `docker run` line must FAIL, not warn.
//
// The entry is written straight into the registry rather than through
// `server add`, because add would have refused it — and an entry that only
// add can reject is an entry nothing re-checks once it is on disk.
func TestDoctorFailsAnInvalidDockerEntryOnDisk(t *testing.T) {
	dir := setDataDir(t)
	isolateHome(t)
	if code, _, stderr := runCLI(t, "", "server", "add", "boxed",
		"--cmd", "node", "--image", "alpine:3"); code != ExitOK {
		t.Fatalf("add failed: %s", stderr)
	}
	if code, _, stderr := runCLI(t, "", "server", "enable", "boxed", "--no-probe"); code != ExitOK {
		t.Fatalf("enable failed: %s", stderr)
	}

	// Drop the image: "runtime: docker" with nothing to run is precisely the
	// shape that must not degrade into a host spawn.
	path := filepath.Join(dir, "registry", "servers.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	servers, _ := doc["servers"].(map[string]any)
	entry, _ := servers["boxed"].(map[string]any)
	entry["docker"] = map[string]any{}
	patched, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, patched, 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, _ := runCLI(t, "", "doctor", "--json")
	if code == ExitOK {
		t.Fatalf("doctor passed an unrunnable docker entry:\n%s", out)
	}
	check := findCheck(t, decodeDoctor(t, decodeEnvelope(t, out)), "server:boxed")
	if check.Status != StatusFail {
		t.Fatalf("status = %q, want fail: %+v", check.Status, check)
	}
	if !strings.Contains(check.Detail, "docker runtime configuration is invalid") {
		t.Errorf("detail %q does not name the problem", check.Detail)
	}
}
