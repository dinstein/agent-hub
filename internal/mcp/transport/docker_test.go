package transport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fullDockerConfig is the "everything set" configuration whose argv is
// pinned by the golden file. Determinism is a contract (ruling #27): the
// order and spelling of the run flags is what an operator reads in ps(1)
// and what the spawn guard screens.
func fullDockerConfig() DockerConfig {
	return DockerConfig{
		Image:    "ghcr.io/example/mcp-fs:1.4.0",
		ServerID: "filesystem",
		Network:  "bridge",
		Memory:   "512m",
		CPUs:     "1.5",
		User:     "1000:1000",
		Workdir:  "/work",
		// Deliberately unsorted: the argv must be a function of the config,
		// not of authoring order.
		Mounts: []Mount{
			{Source: "/srv/shared", Target: "/mnt/shared", Write: true},
			{Source: "/home/alice/project", Target: "/work"},
			{Source: "/etc/ssl/certs"},
		},
		Env:           map[string]string{"GITHUB_TOKEN": "s3cr3t", "API_BASE": "https://x"},
		ContainerName: "agenthub-filesystem-1",
		CIDFile:       "/tmp/agenthub-cid/cid",
		ExtraRunArgs:  []string{"--read-only", "--pids-limit", "128"},
	}
}

func TestBuildDockerRunArgsGolden(t *testing.T) {
	args, err := BuildDockerRunArgs(fullDockerConfig(), "node", []string{"/app/server.js", "--stdio"})
	if err != nil {
		t.Fatalf("BuildDockerRunArgs: %v", err)
	}
	checkGolden(t, "docker_run_args.txt", []byte(strings.Join(args, "\n")))
}

// TestBuildDockerRunArgsIsolationDefaults pins the three defaults that make
// this a sandbox rather than a rename of the host spawner: no network, no
// implicit mounts, read-only mounts.
func TestBuildDockerRunArgsIsolationDefaults(t *testing.T) {
	args, err := BuildDockerRunArgs(DockerConfig{
		Image:  "alpine",
		Mounts: []Mount{{Source: "/data"}},
	}, "", nil)
	if err != nil {
		t.Fatalf("BuildDockerRunArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--network none") {
		t.Errorf("network default is not none: %s", joined)
	}
	if !strings.Contains(joined, "-v /data:/data:ro") {
		t.Errorf("mount default is not read-only: %s", joined)
	}
	if strings.Contains(joined, "--privileged") {
		t.Errorf("privileged must never be emitted: %s", joined)
	}
	if !strings.HasPrefix(joined, "run -i --rm ") {
		t.Errorf("run flags drifted: %s", joined)
	}
	if args[len(args)-1] != "alpine" {
		t.Errorf("image must be the last operand when no command is given: %v", args)
	}
}

// TestSpawnDockerCwdBecomesWorkdir pins the container reading of
// StdioConfig.Cwd: "the directory the server runs in" is a path inside the
// image, so it must land as --workdir on the run line.
//
// It used to be applied to the docker CLI process (cmd.Dir), which the
// workload never sees: an entry asking for a working directory silently got
// none. The recorded argv is the assertion, and the directory named here
// does not exist on the host, so a regression to cmd.Dir cannot pass
// quietly either — the spawn itself would fail.
func TestSpawnDockerCwdBecomesWorkdir(t *testing.T) {
	argv := recordedDockerArgv(t, StdioConfig{
		Command: "server",
		Cwd:     "/workspace/inside/the/image", // exists in the image, not here
		Docker:  &DockerConfig{Image: "alpine:test"},
	})
	if !strings.Contains(argv, "--workdir /workspace/inside/the/image") {
		t.Errorf("cwd did not become --workdir: %s", argv)
	}
}

// TestSpawnDockerExplicitWorkdirWins: --workdir is the more specific
// statement, so an explicit one is not overwritten by the entry's cwd.
func TestSpawnDockerExplicitWorkdirWins(t *testing.T) {
	argv := recordedDockerArgv(t, StdioConfig{
		Command: "server",
		Cwd:     "/from-cwd",
		Docker:  &DockerConfig{Image: "alpine:test", Workdir: "/from-workdir"},
	})
	if !strings.Contains(argv, "--workdir /from-workdir") {
		t.Errorf("explicit --workdir was not honored: %s", argv)
	}
	if strings.Contains(argv, "/from-cwd") {
		t.Errorf("cwd overrode the explicit --workdir: %s", argv)
	}
}

// recordedDockerArgv spawns cfg against a stand-in docker CLI that records
// its argv, and returns that argv joined by spaces. cfg.Docker.Binary is
// filled in by this helper.
func recordedDockerArgv(t *testing.T, cfg StdioConfig) string {
	t.Helper()
	requirePOSIX(t)
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	bin := filepath.Join(dir, "docker")
	// Write to a temp file and rename: a reader polling for argvFile must
	// never observe a half-written argv, which reads exactly like a missing
	// flag and made this test flaky under the full suite's load.
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = rm ]; then exit 0; fi\n" +
		"printf '%s\\n' \"$@\" > " + argvFile + ".tmp\n" +
		"mv " + argvFile + ".tmp " + argvFile + "\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	dc := *cfg.Docker
	dc.Binary = bin
	cfg.Docker = &dc

	tr, err := SpawnStdio(cfg)
	if err != nil {
		t.Fatalf("SpawnStdio: %v", err)
	}
	defer func() { _ = tr.Close() }()

	// The spawn returns once the process starts; the argv file lands a moment
	// later.
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, readErr := os.ReadFile(argvFile)
		if readErr == nil && len(raw) > 0 {
			return strings.Join(strings.Split(strings.TrimSpace(string(raw)), "\n"), " ")
		}
		if time.Now().After(deadline) {
			t.Fatal(fmt.Errorf("the stand-in docker CLI never recorded an argv at %s", argvFile))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDockerEnvNeverInArgv is the secrets invariant: argv is world-readable
// through ps(1), so a forwarded variable appears by NAME only.
func TestDockerEnvNeverInArgv(t *testing.T) {
	args, err := BuildDockerRunArgs(DockerConfig{
		Image: "alpine",
		Env:   map[string]string{"TOKEN": "hunter2"},
	}, "", nil)
	if err != nil {
		t.Fatalf("BuildDockerRunArgs: %v", err)
	}
	for _, a := range args {
		if strings.Contains(a, "hunter2") {
			t.Fatalf("secret value leaked into argv: %v", args)
		}
	}
	if !strings.Contains(strings.Join(args, " "), "-e TOKEN") {
		t.Fatalf("env name not forwarded: %v", args)
	}
}

func TestValidateDockerConfigRejects(t *testing.T) {
	tests := []struct {
		name string
		cfg  DockerConfig
	}{
		{"no image", DockerConfig{}},
		{"flag-shaped image", DockerConfig{Image: "--privileged"}},
		{"image with space", DockerConfig{Image: "alpine latest"}},
		{"relative mount source", DockerConfig{Image: "a", Mounts: []Mount{{Source: "rel"}}}},
		{"colon in mount", DockerConfig{Image: "a", Mounts: []Mount{{Source: "/a:b", Target: "/t"}}}},
		{"bad memory", DockerConfig{Image: "a", Memory: "lots"}},
		{"bad cpus", DockerConfig{Image: "a", CPUs: "-1"}},
		{"bad network", DockerConfig{Image: "a", Network: "--host"}},
		{"bad env name", DockerConfig{Image: "a", Env: map[string]string{"BAD NAME": "x"}}},
		{"unprefixed container name", DockerConfig{Image: "a", ContainerName: "mine"}},
		{"relative cidfile", DockerConfig{Image: "a", CIDFile: "cid"}},
		{"extra arg re-specifies network", DockerConfig{Image: "a", ExtraRunArgs: []string{"--network=host"}}},
		{"extra arg re-specifies volume", DockerConfig{Image: "a", ExtraRunArgs: []string{"-v", "/:/host"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := BuildDockerRunArgs(tt.cfg, "", nil); !errors.Is(err, ErrDockerConfig) {
				t.Fatalf("err = %v, want ErrDockerConfig", err)
			}
		})
	}
}

func TestDiagnoseDockerStderr(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   error
	}{
		{
			"image missing",
			"Unable to find image 'ghcr.io/x/y:1' locally\ndocker: Error response from daemon: manifest unknown.",
			ErrDockerImage,
		},
		{"pull denied", "docker: Error response from daemon: pull access denied for x", ErrDockerImage},
		{
			"daemon down",
			"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
			ErrDockerDaemon,
		},
		{"socket perms", "permission denied while trying to connect to the Docker daemon socket", ErrDockerDaemon},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err, ok := DiagnoseDockerStderr("ghcr.io/x/y:1", tt.stderr)
			if !ok {
				t.Fatalf("not diagnosed: %q", tt.stderr)
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if _, ok := DiagnoseDockerStderr("x", "server started on stdio"); ok {
		t.Fatal("ordinary output must not be diagnosed as a runtime failure")
	}
}

// TestDiagnoseDecoratesStderrTail proves the diagnosis travels with the
// error the operator actually sees: internal/downstream embeds Stderr() in
// the initialization failure, which is what keeps "image not found" from
// arriving as a bare deadline-exceeded.
func TestDiagnoseDecoratesStderrTail(t *testing.T) {
	d := diagnoseDocker("ghcr.io/x/y:1")
	got := d("Unable to find image 'ghcr.io/x/y:1' locally")
	if !strings.Contains(got, "agenthub: ") || !strings.Contains(got, ErrDockerImage.Error()) {
		t.Fatalf("tail not decorated: %q", got)
	}
	if got := d("plain output"); got != "plain output" {
		t.Fatalf("undiagnosed tail must pass through unchanged: %q", got)
	}
}

func TestDockerBinaryMissingIsTyped(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	// The fallback list may legitimately hit a real docker install on a
	// developer machine; only assert the typed error when nothing is found.
	got, err := DockerBinary("")
	if err == nil {
		if !filepath.IsAbs(got) {
			t.Fatalf("DockerBinary returned a relative path %q", got)
		}
		return
	}
	if !errors.Is(err, ErrDockerUnavailable) {
		t.Fatalf("err = %v, want ErrDockerUnavailable", err)
	}
	if !strings.Contains(err.Error(), "install Docker") {
		t.Fatalf("error lacks remediation: %v", err)
	}
}

func TestDockerBinaryOverrideMustBeExecutable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DockerBinary(p); !errors.Is(err, ErrDockerUnavailable) {
		t.Fatalf("err = %v, want ErrDockerUnavailable", err)
	}
}

func TestSpawnDockerScreenRejectionIsFatal(t *testing.T) {
	blocked := errors.New("blocked by policy")
	_, err := SpawnStdio(StdioConfig{
		Command: "node",
		Docker: &DockerConfig{
			Image:  "alpine",
			Binary: fakeDockerBinary(t),
		},
		Screen: func(string, []string, []string) error { return blocked },
	})
	var terr *Error
	if !errors.As(err, &terr) {
		t.Fatalf("err = %v, want *transport.Error", err)
	}
	if terr.Class != ClassFatal {
		t.Fatalf("class = %v, want ClassFatal (a policy verdict must not feed the breaker)", terr.Class)
	}
	if !errors.Is(err, blocked) {
		t.Fatalf("err = %v, want the guard's error", err)
	}
}

func TestSpawnDockerConfigErrorIsFatal(t *testing.T) {
	_, err := SpawnStdio(StdioConfig{
		Command: "node",
		Docker:  &DockerConfig{Binary: fakeDockerBinary(t)}, // no image
	})
	var terr *Error
	if !errors.As(err, &terr) || terr.Class != ClassFatal {
		t.Fatalf("err = %v, want ClassFatal", err)
	}
	if !errors.Is(err, ErrDockerConfig) {
		t.Fatalf("err = %v, want ErrDockerConfig", err)
	}
}

func TestSpawnDockerUnavailableIsFatal(t *testing.T) {
	_, err := SpawnStdio(StdioConfig{
		Command: "node",
		Docker:  &DockerConfig{Image: "alpine", Binary: filepath.Join(t.TempDir(), "nope")},
	})
	var terr *Error
	if !errors.As(err, &terr) || terr.Class != ClassFatal {
		t.Fatalf("err = %v, want ClassFatal", err)
	}
	if !errors.Is(err, ErrDockerUnavailable) {
		t.Fatalf("err = %v, want ErrDockerUnavailable", err)
	}
}

// fakeDockerBinary returns a path to an executable stand-in so the tests
// above exercise the config/policy paths without a docker installation.
func fakeDockerBinary(t *testing.T) string {
	t.Helper()
	requirePOSIX(t)
	p := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSpawnDockerRunsTheCLI drives the whole path with a shell stand-in for
// docker: it records the argv, echoes a diagnosable stderr line and exits.
// The transport must then surface the container's failure as
// ClassUnavailable with the diagnosis attached — never as a hang.
func TestSpawnDockerRunsTheCLI(t *testing.T) {
	requirePOSIX(t)
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	bin := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = rm ]; then exit 0; fi\n" +
		"printf '%s\\n' \"$@\" > " + argvFile + "\n" +
		"echo \"Unable to find image 'alpine:test' locally\" >&2\n" +
		"exit 125\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	tr, err := SpawnStdio(StdioConfig{
		Command: "server",
		Args:    []string{"--stdio"},
		Env:     []string{"PATH=/usr/bin:/bin", "TOKEN=hunter2"},
		Docker: &DockerConfig{
			Image:    "alpine:test",
			ServerID: "demo",
			Binary:   bin,
			Env:      map[string]string{"TOKEN": "hunter2"},
			Mounts:   []Mount{{Source: "/tmp"}},
		},
	})
	if err != nil {
		t.Fatalf("SpawnStdio: %v", err)
	}
	defer func() { _ = tr.Close() }()

	// The child exits immediately; wait for the read loop to observe it.
	if _, callErr := tr.Call(t.Context(), "initialize", nil); callErr == nil {
		t.Fatal("expected the call to fail once the container exited")
	}
	tail := tr.Stderr()
	if !strings.Contains(tail, "agenthub: ") || !strings.Contains(tail, "docker image unavailable") {
		t.Fatalf("stderr tail lacks the diagnosis: %q", tail)
	}

	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	argv := strings.Split(strings.TrimSpace(string(raw)), "\n")
	joined := strings.Join(argv, " ")
	for _, want := range []string{"run", "-i", "--rm", "--network none", "-v /tmp:/tmp:ro", "-e TOKEN", "alpine:test", "server --stdio"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "hunter2") {
		t.Errorf("secret value reached argv: %s", joined)
	}
	if !strings.Contains(joined, "--label "+LabelManaged+"=true") {
		t.Errorf("managed label missing: %s", joined)
	}
	if !strings.Contains(joined, "--name "+containerNamePrefix) {
		t.Errorf("container name lacks the agenthub prefix: %s", joined)
	}
}

func TestDockerEnvPrependsBinaryDir(t *testing.T) {
	env := dockerEnv([]string{"PATH=/usr/bin", "HOME=/home/a"}, map[string]string{"TOKEN": "v"}, "/opt/docker/bin")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PATH=/opt/docker/bin"+string(os.PathListSeparator)+"/usr/bin") {
		t.Fatalf("binary dir not prepended to PATH: %v", env)
	}
	if !strings.Contains(joined, "TOKEN=v") {
		t.Fatalf("container env not exported to the CLI: %v", env)
	}
	// Idempotent: an already-present dir is not duplicated.
	again := dockerEnv(env, nil, "/opt/docker/bin")
	if strings.Count(strings.Join(again, "\n"), "/opt/docker/bin") != 1 {
		t.Fatalf("PATH entry duplicated: %v", again)
	}
}

func TestGenerateContainerNameIsLegalAndPrefixed(t *testing.T) {
	for _, id := range []string{"", "my server/1", "----", strings.Repeat("x", 80)} {
		name := generateContainerName(id)
		if !strings.HasPrefix(name, containerNamePrefix) {
			t.Fatalf("name %q lacks the prefix", name)
		}
		if !containerNameRe.MatchString(name) {
			t.Fatalf("name %q is not a legal docker name", name)
		}
	}
}
