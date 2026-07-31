package transport

import (
	"slices"
	"strings"
	"testing"
)

// TestExtraRunArgsRejectsEverySpellingOfAnOwnedFlag is the regression for
// the isolation bypass the 2026-07-31 sweep confirmed.
//
// The check compared the token up to '=' against the owned set, so it saw
// `--user 0:0` and `--user=0:0` and missed `-u0:0` — the same flag with its
// value attached to the shorthand, which docker parses identically. The
// isolation defaults are emitted first, docker's last wins, so the config
// that said `user: 1000:1000` ran as root.
func TestExtraRunArgsRejectsEverySpellingOfAnOwnedFlag(t *testing.T) {
	refused := []string{
		// attached shorthand: the spelling that got through
		"-u0:0", "-m0", "-v/home/user/.ssh:/keys", "-eNAME=VALUE", "-w/", "-lk=v",
		// and the spellings that were already refused, which must stay so
		"--user", "--user=0:0", "-u", "-m", "-v", "-e", "-w", "-l", "-i",
		"--network=host", "--net", "--memory=0", "--volume", "--mount", "--rm",
		// an owned shorthand reached through a boolean cluster
		"-tu0:0", "-dv/etc:/etc",
	}
	for _, a := range refused {
		cfg := DockerConfig{Image: "img", User: "1000:1000", Memory: "512m", ExtraRunArgs: []string{a}}
		if err := validateDockerConfig(cfg); err == nil {
			t.Errorf("%q was accepted: it re-specifies a flag the isolation defaults own", a)
		}
	}

	// Over-refusing would make ExtraRunArgs useless, so the flags it exists
	// for must still pass.
	allowed := []string{
		"--cap-drop=ALL", "--read-only", "--security-opt=no-new-privileges",
		"--pids-limit=100", "-p8080:80", "--publish=8080:80", "--platform=linux/amd64",
		"-t", "--init",
	}
	for _, a := range allowed {
		cfg := DockerConfig{Image: "img", ExtraRunArgs: []string{a}}
		if err := validateDockerConfig(cfg); err != nil {
			t.Errorf("%q was refused, but the isolation defaults do not emit it: %v", a, err)
		}
	}
}

// TestBuildDockerRunArgsNeverEmitsTwoUserFlags is the property the check
// above exists to protect, asserted on the argv itself rather than on the
// validator: whatever survives validation, the run line must not contain a
// second setting of a flag the isolation defaults own.
func TestBuildDockerRunArgsNeverEmitsTwoUserFlags(t *testing.T) {
	cfg := DockerConfig{
		Image:        "img",
		User:         "1000:1000",
		Memory:       "512m",
		ExtraRunArgs: []string{"--cap-drop=ALL", "--read-only"},
	}
	if err := validateDockerConfig(cfg); err != nil {
		t.Fatalf("a config with only unowned extra args was refused: %v", err)
	}
	argv, err := BuildDockerRunArgs(cfg, "", nil)
	if err != nil {
		t.Fatalf("BuildDockerRunArgs: %v", err)
	}
	for _, owned := range []string{"--user", "-u", "--memory", "-m"} {
		if n := slices.Index(argv, owned); n >= 0 && slices.Index(argv[n+1:], owned) >= 0 {
			t.Errorf("%q appears twice in %v", owned, argv)
		}
	}
	// And nothing after the image, which is where the container's own
	// command lives, may be read as a run flag.
	img := slices.Index(argv, cfg.Image)
	if img < 0 {
		t.Fatalf("the image is not in the argv: %v", argv)
	}
	for _, a := range argv[:img] {
		if strings.HasPrefix(a, "-u") && a != "--user" {
			t.Errorf("an attached-shorthand user flag reached the argv: %q in %v", a, argv)
		}
	}
}
