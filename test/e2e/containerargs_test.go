package e2e_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The container flags — `--memory`, `--cpus`, `--network`, `--container-user`,
// `--container-workdir`, `--docker-arg` — had no end-to-end coverage at all.
// The one docker case in the suite proves a container runs, and skips itself
// wherever no Docker daemon does; nothing anywhere checked that the limits an
// operator wrote are the limits the container gets.
//
// They fail in the direction that does not announce itself. A dropped
// `--memory` is a container with no memory cap, which behaves exactly like one
// with a cap until the day it does not; a dropped `--network none` is a
// downstream with the network it was specifically denied. Both look perfect
// from every angle except the one nobody checks.
//
// These cases need no Docker. `transport.DockerBinary` searches PATH before
// its well-known locations, so a recording stand-in placed earlier on PATH is
// what the gateway executes — and what it records is the REAL command line the
// spawner built, not a second rendering of the same config written for a test.

// fakeDocker installs a stand-in `docker` on a PATH prefix and returns
// (pathPrefix, transcriptPath). The stand-in appends each invocation and
// exits non-zero: the container never starts, which is fine — what is under
// test is the command line, and a spawn that fails afterwards exercises the
// cleanup call as well.
func fakeDocker(t *testing.T) (string, string) {
	t.Helper()
	bin := t.TempDir()
	transcript := filepath.Join(t.TempDir(), "docker-argv.txt")
	body := "#!/bin/sh\n{ echo '=== invocation'; printf '%s\\n' \"$@\"; } >> " + transcript + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	return bin, transcript
}

// dockerRunArgv waits for the stand-in to record a `run` and returns its
// argument vector.
func dockerRunArgv(t *testing.T, c *gatewayClient, transcript string, budget time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		data, err := os.ReadFile(transcript)
		if err == nil {
			for _, block := range strings.Split(string(data), "=== invocation\n") {
				argv := strings.Split(strings.TrimSpace(block), "\n")
				if len(argv) > 0 && argv[0] == "run" {
					return argv
				}
			}
		}
		if !time.Now().Before(deadline) {
			c.fatalf("the docker stand-in never recorded a `run` within %s (transcript err %v)", budget, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// flagValue returns the argument following name, and whether name was present.
// Pair lookup rather than substring matching: "--memory" appearing somewhere
// in a command line says nothing about what it was set to.
func flagValue(argv []string, name string) (string, bool) {
	for i, a := range argv {
		if a == name && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

// startBoxedGateway registers a contained server plus a plain one, starts a
// gateway whose PATH finds the stand-in first, and returns the client and the
// transcript. The plain server is how a case knows the gateway is past its
// dialling phase.
func startBoxedGateway(t *testing.T, dataDir string, env []string, addArgs ...string) (*gatewayClient, string) {
	t.Helper()
	bin, transcript := fakeDocker(t)
	runAgenthubEnv(t, env, "", append([]string{"server", "add", "boxed"}, append(addArgs, "--json")...)...)
	runAgenthubEnv(t, env, "", "server", "enable", "boxed", "--no-probe")
	runAgenthubEnv(t, env, "", "server", "add", "plain", "--cmd", fakemcpBin, "--json")
	runAgenthubEnv(t, env, "", "server", "enable", "plain", "--no-probe")
	runAgenthubEnv(t, env, "", "config", "set", "discovery", "full")

	c := startGatewayEnv(t, envWith(t, env, "PATH="+bin+":"+os.Getenv("PATH")), "boxedclient")
	c.initialize()
	c.waitForTool("plain__echo", 45*time.Second)
	return c, transcript
}

// TestTheContainerLimitsReachTheDockerCommandLine is the delivery half of the
// isolation rule at the level an operator actually configures: not "a
// container ran" but "the container ran with what I asked for".
func TestTheContainerLimitsReachTheDockerCommandLine(t *testing.T) {
	dataDir := t.TempDir()
	mount := t.TempDir()

	c, transcript := startBoxedGateway(t, dataDir, testEnv(dataDir),
		"--cmd", "/opt/app/server", "--args", "--serve",
		"--image", "alpine:3",
		"--memory", "512m",
		"--cpus", "1.5",
		"--network", "none",
		"--container-user", "1000:1000",
		"--container-workdir", "/work",
		"--mount", mount+":/data:ro",
	)
	argv := dockerRunArgv(t, c, transcript, 45*time.Second)

	for _, want := range []struct{ flag, value string }{
		// The four resource and identity limits, none of which had any test.
		{"--memory", "512m"},
		{"--cpus", "1.5"},
		{"--network", "none"},
		{"--user", "1000:1000"},
		{"--workdir", "/work"},
		// The mount keeps its read-only mode. Losing the `:ro` would hand a
		// downstream write access to a host directory it was given to read.
		{"-v", mount + ":/data:ro"},
		// How `agenthub doctor` recognises a container of ours, and how a
		// stray one is attributed to the server that leaked it.
		{"--label", "agenthub.managed=true"},
		{"--label", "agenthub.server=boxed"},
	} {
		got, ok := flagValue(argv, want.flag)
		if !ok {
			c.fatalf("the docker command line carries no %s: %v", want.flag, argv)
		}
		if want.flag == "--label" {
			// Two labels share the flag name, so the pair lookup above finds
			// only the first; check membership for these.
			if !slices.Contains(argv, want.value) {
				c.fatalf("label %q is missing: %v", want.value, argv)
			}
			continue
		}
		if got != want.value {
			c.fatalf("%s = %q, want %q: %v", want.flag, got, want.value, argv)
		}
	}

	// --rm, so a failed or finished downstream leaves no container behind.
	if !slices.Contains(argv, "--rm") {
		c.fatalf("the container is not started with --rm: %v", argv)
	}

	// The image separates docker's own flags from the command run INSIDE it.
	// Order is the whole meaning here: an argument on the wrong side of the
	// image is either a flag docker rejects or a flag the container silently
	// receives as an argument.
	image := slices.Index(argv, "alpine:3")
	cmd := slices.Index(argv, "/opt/app/server")
	if image < 0 || cmd < 0 || image > cmd {
		c.fatalf("image and command are not in that order: %v", argv)
	}
	if cmd+1 >= len(argv) || argv[cmd+1] != "--serve" {
		c.fatalf("the container command's own args do not follow it: %v", argv)
	}
	c.close()
}

// TestASecretInAContainerEnvIsPassedByNameNotByValue is the security half,
// and it is the reason the docker path routes environment the way it does.
//
// A container's command line is world-readable in ps(1). Passing `-e KEY=value`
// would put every API key an operator configured on it — for any process on
// the machine to read, including the ones a contained downstream exists to be
// protected from. The spawner passes `-e KEY` alone and lets the value travel
// through the docker CLI's own environment instead.
//
// So the assertion is not that the flag is present but that the VALUE is
// absent, everywhere in the argv.
func TestASecretInAContainerEnvIsPassedByNameNotByValue(t *testing.T) {
	dataDir := t.TempDir()
	env := vaultEnv(dataDir)
	const secret = "s3cr3t-container-api-key"
	runAgenthubEnv(t, env, secret+"\n", "secret", "set", "boxed", "API_KEY", "--stdin")

	c, transcript := startBoxedGateway(t, dataDir, env,
		"--cmd", "/opt/app/server", "--image", "alpine:3",
		"--env", "SERVICE_API_KEY=${SECRET_API_KEY}",
	)
	argv := dockerRunArgv(t, c, transcript, 45*time.Second)

	// The variable is declared by name, so the container receives it.
	if !slices.Contains(argv, "SERVICE_API_KEY") {
		c.fatalf("the container is not given SERVICE_API_KEY at all: %v", argv)
	}
	// And its value is nowhere on the command line — not as a pair, not
	// anywhere. This is the assertion; the one above only establishes that
	// the variable was not simply dropped.
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, secret) {
		c.fatalf("the secret VALUE is on the container command line, where ps(1) can read it: %v", argv)
	}
	// The unresolved placeholder must not be there either: a resolver failing
	// open would hand the container the literal text as its key.
	if strings.Contains(joined, "${SECRET_") {
		c.fatalf("an unresolved placeholder reached the command line: %v", argv)
	}
	c.close()
}

// TestExtraDockerArgsLandBeforeTheImage covers `--docker-arg`, the escape
// hatch for whatever the typed flags do not cover.
//
// Where it lands is the whole of its contract: before the image it is an
// argument to docker, after it an argument to the contained program. An
// escape hatch that ended up on the wrong side would silently change what the
// downstream was invoked with rather than how it was contained.
func TestExtraDockerArgsLandBeforeTheImage(t *testing.T) {
	dataDir := t.TempDir()

	c, transcript := startBoxedGateway(t, dataDir, testEnv(dataDir),
		"--cmd", "/opt/app/server", "--image", "alpine:3",
		"--docker-arg", "--pids-limit=64",
	)
	argv := dockerRunArgv(t, c, transcript, 45*time.Second)

	extra := slices.Index(argv, "--pids-limit=64")
	image := slices.Index(argv, "alpine:3")
	if extra < 0 {
		c.fatalf("--docker-arg never reached the command line: %v", argv)
	}
	if image < 0 || extra > image {
		c.fatalf("--docker-arg landed after the image, where it configures the "+
			"contained program instead of the container: %v", argv)
	}
	c.close()
}
