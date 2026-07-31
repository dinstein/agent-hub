package spawnguard

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

// --- wrapper unwrapping ---------------------------------------------------

// Wrappers that forward to a real command. Unwrapping exists so that
// "env FOO=1 sh -c ..." is judged as "sh -c ..." (with the assignment
// env-checked), not as harmless "env".
// simpleWrappers describes the wrappers whose argv can be walked to find the
// command they run.
//
// BOTH tables are required, and a flag in neither is a parse failure rather
// than a guess. The reason is that guessing wrong in either direction moved
// the command position and the real command was then never checked:
//
//	sudo --prompt x sh -c 'evil'      was ALLOWED  (value read as the command)
//	timeout -d x 10 sh -c 'evil'      was ALLOWED
//	stdbuf --input L sh -c 'evil'     was ALLOWED
//
// These are coreutils and sudo: their option sets are documented and closed,
// unlike `docker run` (see containerBoolFlags for why that one is inverted
// instead). So the tables can be complete, and anything outside them is a
// shape this build does not understand — which is refused, not guessed.
//
// A flag omitted here therefore BLOCKS. That is the intended direction: the
// cost is a loud rejection of an unusual-but-legitimate command line, which an
// operator can see and fix, against a silent bypass, which nobody can.
var simpleWrappers = map[string]struct {
	valueFlags  map[string]bool // flags that consume the following argument
	boolFlags   map[string]bool // flags that stand alone
	positionals int             // leading non-flag operands to skip (timeout's duration)
}{
	"nohup":  {},
	"setsid": {boolFlags: map[string]bool{"-c": true, "--ctty": true, "-f": true, "--fork": true, "-w": true, "--wait": true}},
	"nice":   {valueFlags: map[string]bool{"-n": true, "--adjustment": true}},
	"stdbuf": {valueFlags: map[string]bool{
		"-i": true, "--input": true, "-o": true, "--output": true, "-e": true, "--error": true,
	}},
	"timeout": {
		valueFlags: map[string]bool{"-k": true, "--kill-after": true, "-s": true, "--signal": true},
		boolFlags: map[string]bool{
			"--preserve-status": true, "--foreground": true, "-v": true, "--verbose": true,
		},
		positionals: 1,
	},
	"sudo": {
		valueFlags: map[string]bool{
			"-u": true, "--user": true, "-g": true, "--group": true,
			"-p": true, "--prompt": true, "-h": true, "--host": true,
			"-U": true, "--other-user": true, "-C": true, "--close-from": true,
			"-D": true, "--chdir": true, "-R": true, "--chroot": true,
			"-r": true, "--role": true, "-t": true, "--type": true,
			"-T": true, "--command-timeout": true,
		},
		boolFlags: map[string]bool{
			"-A": true, "--askpass": true, "-b": true, "--background": true,
			"-E": true, "--preserve-env": true, "-e": true, "--edit": true,
			"-H": true, "--set-home": true, "-i": true, "--login": true,
			"-K": true, "--remove-timestamp": true, "-k": true, "--reset-timestamp": true,
			"-l": true, "--list": true, "-n": true, "--non-interactive": true,
			"-N": true, "--no-update": true, "-P": true, "--preserve-groups": true,
			"-S": true, "--stdin": true, "-s": true, "--shell": true,
			"-v": true, "--validate": true, "-B": true, "--bell": true,
		},
	},
	"doas": {
		valueFlags: map[string]bool{"-u": true, "-C": true, "-a": true},
		boolFlags:  map[string]bool{"-L": true, "-n": true, "-s": true},
	},
}

// unwrap resolves one wrapper layer. unwrapped=false means base is not a
// wrapper; next=="" with unwrapped=true means the wrapper had no discernible
// command.
//
// A wrapper this build cannot parse is REFUSED (err), not left alone. Passing
// it on meant checking the wrong command: the unparsed flag's value became the
// "command", and the real one — `sh -c ...` sitting right after it — was never
// examined. See simpleWrappers for why both flag tables have to be complete
// for that to be decidable at all.
func (g *Guard) unwrap(base string, args []string) (next string, nextArgs []string, unwrapped bool, err error) {
	if base == "env" {
		return g.unwrapEnv(args)
	}
	if base == "busybox" {
		if len(args) == 0 {
			return "", nil, true, nil
		}
		return args[0], args[1:], true, nil
	}
	w, ok := simpleWrappers[base]
	if !ok {
		return "", nil, false, nil
	}
	skip := w.positionals
	noMoreFlags := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !noMoreFlags && a == "--":
			noMoreFlags = true
		case !noMoreFlags && strings.HasPrefix(a, "-") && a != "-":
			switch {
			case w.valueFlags[a]:
				i++ // consume the flag's value
			case w.boolFlags[a]:
				// stands alone
			default:
				// Neither table knows it, so whether the NEXT argument is its
				// value or the command is unknowable — and both readings have
				// produced bypasses. Refuse rather than pick one.
				return "", nil, true, blockedf(CodeDenylisted,
					"%s flag %q is not one this build knows, so the command it wraps cannot be identified", base, a)
			}
		case skip > 0:
			skip--
		default:
			return a, args[i+1:], true, nil
		}
	}
	return "", nil, true, nil
}

// env's own options, split the same way as simpleWrappers and for the same
// reason: a value read as the command means the real command is never checked.
//
// GNU env and BSD env do not agree on this set (BSD has -P, GNU has the
// signal options), so neither table can be complete for both. Unknown flags
// are therefore refused rather than assumed — which is also what makes it safe
// not to know whether a given build's env has --argv0.
var (
	envValueFlags = map[string]bool{
		"-u": true, "--unset": true, "-C": true, "--chdir": true,
		"-P": true, "-a": true, "--argv0": true,
	}
	envBoolFlags = map[string]bool{
		"-i": true, "--ignore-environment": true, "-0": true, "--null": true,
		"-v": true, "--debug": true, "--list-signal-handling": true,
		// The signal options take an OPTIONAL value, which GNU requires to be
		// attached with "="; in bare form they stand alone and the next
		// argument really is the command.
		"--block-signal": true, "--default-signal": true, "--ignore-signal": true,
	}
)

// unwrapEnv parses `env [flags] [NAME=VALUE...] command args...`. The
// assignments run through the same dangerous-env check as the direct env
// slice — env(1) is the classic smuggling wrapper. `env -S` (split-string)
// re-tokenizes an opaque string into a command line; that is exactly the
// smuggling shape this guard exists for, so it blocks rather than guesses.
func (g *Guard) unwrapEnv(args []string) (next string, nextArgs []string, unwrapped bool, err error) {
	noMoreFlags := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !noMoreFlags && (a == "-S" || strings.HasPrefix(a, "-S") || a == "--split-string" || strings.HasPrefix(a, "--split-string=")):
			return "", nil, true, blockedf(CodeEnvSmuggling, "env %s re-tokenizes its argument into a command line", a)
		case !noMoreFlags && a == "--":
			noMoreFlags = true
		case !noMoreFlags && envValueFlags[a]:
			i++ // consume the flag's value
		case !noMoreFlags && envBoolFlags[a]:
			// stands alone
		case !noMoreFlags && strings.HasPrefix(a, "-") && a != "-":
			// Unknown: whether the next argument is its value or the command
			// is undecidable, and guessing "no value" made the value the
			// command — `env --argv0 foo sh -c evil` ran unchecked. env's
			// option set also differs between GNU and BSD, so refusing is the
			// only answer that does not depend on which one is installed.
			return "", nil, true, blockedf(CodeEnvSmuggling,
				"env flag %q is not one this build knows, so the command it wraps cannot be identified", a)
		case strings.Contains(a, "="):
			if err := g.checkEnvEntry(a); err != nil {
				return "", nil, true, err
			}
		default:
			return a, args[i+1:], true, nil
		}
	}
	return "", nil, true, nil
}

// --- inline eval ----------------------------------------------------------

var (
	shellNames = map[string]bool{
		"sh": true, "bash": true, "zsh": true, "dash": true,
		"ksh": true, "ksh93": true, "mksh": true, "fish": true,
	}
	pythonName = regexp.MustCompile(`^python[0-9.]*$`)
	// combinedShort matches single-dash combined short flags like -lc, -Ec.
	combinedShort = regexp.MustCompile(`^-[a-zA-Z]+$`)
)

// Flags whose VALUE is a SEPARATE argument. Skipping them is what keeps a
// flag's value from being mistaken for the first operand — which is what ends
// the scan. Getting this table wrong is not a cosmetic bug: an unskipped value
// stops the scan early and the eval flag AFTER it is never examined, so
// `bash --rcfile /tmp/x -c 'evil'` walked straight through this guard.
//
// The attached forms (-I/tmp, --rcfile=/tmp/x) need no entry: they are a
// single argument, so nothing is mistaken for an operand.
var (
	shellValueFlags = map[string]bool{
		"-o": true, "-O": true, "+o": true, "+O": true,
		"--rcfile": true, "--init-file": true,
	}
	pythonValueFlags = map[string]bool{
		"-W": true, "-X": true, "-Q": true, "--check-hash-based-pycs": true,
	}
	// node's value-taking options are open-ended and grow with each release,
	// so this table is the ones that exist today rather than a closed set —
	// see the residual noted on checkInlineEval.
	nodeValueFlags = map[string]bool{
		"-C": true, "--conditions": true, "--title": true, "--icu-data-dir": true,
		"--openssl-config": true, "--redirect-warnings": true, "--tls-keylog": true,
		"--max-old-space-size": true, "--stack-size": true, "--dns-result-order": true,
		"--unhandled-rejections": true, "--report-directory": true,
		"--report-filename": true, "--heap-prof-dir": true, "--cpu-prof-dir": true,
		"--env-file": true, "--watch-path": true, "--snapshot-blob": true,
		"--experimental-specifier-resolution": true, "--policy-integrity": true,
	}
	perlValueFlags = map[string]bool{"-I": true, "-x": true}
	rubyValueFlags = map[string]bool{
		"-I": true, "-r": true, "-C": true, "-E": true, "-F": true, "-K": true,
	}
	// php -c is the CONFIG path, not eval; php's eval flag is -r.
	phpValueFlags = map[string]bool{"-c": true, "-d": true, "-f": true}
)

// evalScan describes how one interpreter family exposes inline execution.
type evalScan struct {
	// valueFlags consume the following argument.
	valueFlags map[string]bool
	// stopFlags end the interpreter's own options without being eval flags
	// (python -m runs a module by name, not inline text).
	stopFlags map[string]bool
	// isEval reports whether a flag takes program text from the command line.
	isEval func(string) bool
}

// combinedShortWith reports whether a is a single-dash combined short-flag
// cluster (-lc, -Ec) containing any of the given letters.
func combinedShortWith(letters string) func(string) bool {
	return func(a string) bool {
		return combinedShort.MatchString(a) && strings.ContainsAny(a, letters)
	}
}

// scan walks the interpreter's OWN flags and returns the first inline-eval
// flag it finds. It stops at "--", at a stop flag, or at the first true
// operand, because flags after the script path belong to the script and
// blocking those would be a false positive.
func (e evalScan) scan(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || !strings.HasPrefix(a, "-") || e.stopFlags[a] {
			return "", false
		}
		if e.isEval(a) {
			return a, true
		}
		if e.valueFlags[a] {
			i++ // its value is not the operand
		}
	}
	return "", false
}

// evalScans maps an interpreter family to its scan. The families themselves
// are matched by checkInlineEval, which owns the name matching (shellNames,
// the python regexp) that a map cannot express.
var (
	shellScan = evalScan{
		valueFlags: shellValueFlags,
		isEval:     combinedShortWith("c"),
	}
	pythonScan = evalScan{
		valueFlags: pythonValueFlags,
		stopFlags:  map[string]bool{"-m": true},
		isEval:     combinedShortWith("c"),
	}
	nodeScan = evalScan{
		valueFlags: nodeValueFlags,
		isEval: func(a string) bool {
			for _, f := range []string{"-e", "--eval", "-p", "--print",
				"-r", "--require", "--import", "--experimental-loader"} {
				if a == f || strings.HasPrefix(a, f+"=") {
					return true
				}
			}
			return false
		},
	}
	perlScan = evalScan{valueFlags: perlValueFlags, isEval: combinedShortWith("eE")}
	rubyScan = evalScan{valueFlags: rubyValueFlags, isEval: combinedShortWith("e")}
	phpScan  = evalScan{valueFlags: phpValueFlags, isEval: combinedShortWith("r")}
	// osascript takes -e repeatedly and has no script-path form worth
	// modelling; it has no value flags.
	osascriptScan = evalScan{isEval: func(a string) bool { return a == "-e" }}
)

// checkInlineEval blocks interpreter invocations that take the program text
// from the command line (sh -c, python -c, node -e, ...).
//
// Two deliberate fail-open edges, in decreasing confidence:
//
//   - Scanning stops at the first operand: flags after the script path belong
//     to the script, and blocking them would be a false positive.
//   - A value-taking flag this build does not know about still stops the scan
//     at its value, hiding an eval flag behind it. The tables above close every
//     case that exists today for the closed-option interpreters (shells, perl,
//     ruby, php, python); node's option set grows every release, so an unknown
//     `node --new-flag value -e ...` remains possible. Closing that completely
//     means scanning past the operand and accepting false positives on scripts
//     that take -e or -c themselves, which is a policy change rather than a
//     bug fix.
func checkInlineEval(base string, args []string) error {
	blockFlag := func(a string) error {
		return blockedf(CodeInlineEval, "%s %s executes inline code from the command line", base, a)
	}

	var scan evalScan
	switch {
	case base == "eval":
		return blockedf(CodeInlineEval, "eval executes inline code")
	case base == "deno":
		if len(args) > 0 && args[0] == "eval" {
			return blockedf(CodeInlineEval, "deno eval executes inline code")
		}
		return nil
	case shellNames[base]:
		scan = shellScan
	case pythonName.MatchString(base):
		scan = pythonScan
	case base == "node" || base == "nodejs" || base == "bun":
		scan = nodeScan
	case base == "perl":
		scan = perlScan
	case base == "ruby":
		scan = rubyScan
	case base == "php":
		scan = phpScan
	case base == "osascript":
		scan = osascriptScan
	default:
		return nil
	}

	if flag, found := scan.scan(args); found {
		return blockFlag(flag)
	}
	return nil
}

// --- container escape -----------------------------------------------------

var containerCLIs = map[string]bool{"docker": true, "podman": true, "nerdctl": true}

// Global CLI flags (before the subcommand) that consume a value.
// Global CLI flags (before the subcommand). Both kinds are listed because
// misreading either moves the SUBCOMMAND position, and a subcommand that is
// not run/create/exec skips the container check entirely:
//
//	docker --tlscacert /tmp/ca run -v /:/host img   was ALLOWED
//
// docker's global set is small and closed, so an unknown global flag is
// refused rather than guessed — the same call made for the wrappers, and for
// the same reason: there is no safe direction to guess when the position of
// the thing being checked is what moves.
var containerGlobalValueFlags = map[string]bool{
	"-H": true, "--host": true, "-c": true, "--context": true, "--config": true,
	"-l": true, "--log-level": true,
	"--tlscacert": true, "--tlscert": true, "--tlskey": true,
}

var containerGlobalBoolFlags = map[string]bool{
	"-D": true, "--debug": true, "--tls": true, "--tlsverify": true,
	"-v": true, "--version": true, "--help": true,
}

// run/create/exec flags that consume a value and are inspected (or must be
// consumed so their value is not mistaken for the image operand).
// containerBoolFlags are the `docker run` flags that take NO value, so the
// argument after them is a real operand rather than their value.
//
// This table is deliberately the inverse of the obvious one. Listing the
// VALUE-taking flags instead means every flag missing from the list is treated
// as valueless, its value is mistaken for the image, and the scan stops there
// — hiding every policy-bearing flag behind it. docker has on the order of a
// hundred run flags and gains more each release, so that list could never be
// complete, and each gap was a silent container escape:
//
//	docker run --sysctl net.ipv4.ip_forward=1 -v /:/host img   was ALLOWED
//	docker run --storage-opt size=1G --privileged img           was ALLOWED
//
// Inverting it moves the incompleteness to the safe side. A boolean flag
// missing from THIS table is assumed to take a value, so the scan skips one
// argument and keeps going: at worst it walks past the image into the
// container's own command and judges an argument that was never docker's.
// That is a false positive, which is loud and fixable, rather than a bypass,
// which is neither.
var containerBoolFlags = map[string]bool{
	"--privileged": true, "-d": true, "--detach": true,
	"-i": true, "--interactive": true, "-t": true, "--tty": true,
	"--rm": true, "--init": true, "--read-only": true,
	"--no-healthcheck": true, "--oom-kill-disable": true,
	"-P": true, "--publish-all": true, "--sig-proxy": true,
	"--disable-content-trust": true, "-q": true, "--quiet": true,
	"--help": true, "--no-cgroupns": true,
}

// escapeDevices and escapeDevicePrefixes name the --device sources that hand
// a container the host itself. Everything else passes.
//
// This one is a DENY list, and it is the deliberate exception to the allow-list
// rule — which is about tool selectors, where the question is "what may this
// client reach" and an unknown answer must be no. Here the layer's own stated
// failure direction is the opposite: "deterministic checks always block; shape
// checks fail open" (docs/modules/security.md), because spawnguard is
// anti-smuggling over a command line the OPERATOR wrote, not a sandbox. An
// allow list of device nodes could never be complete — /dev/dri, /dev/kvm,
// /dev/fuse, /dev/net/tun, /dev/snd, /dev/ttyUSB0 are all ordinary things a
// server legitimately asks for — so it would refuse working configurations,
// which after the guard moved onto the spawn path means the server is dead at
// connect rather than rejected at `server add`.
//
// What is denied is the set whose grant is the host: raw block devices (the
// filesystem, bypassing every permission on it) and the memory/port devices
// (kernel memory, arbitrary I/O). Those are enumerable, they do not grow with
// each new driver, and none of them has a use inside an MCP server.
var escapeDevices = map[string]bool{
	"/dev":       true, // the whole tree
	"/dev/mem":   true, // physical memory
	"/dev/kmem":  true, // kernel virtual memory
	"/dev/kcore": true,
	"/dev/port":  true, // I/O ports
}

// escapeDevicePrefixes are the block-device families, matched on the node name
// so /dev/sda, /dev/sda1 and /dev/nvme0n1p2 are all covered.
var escapeDevicePrefixes = []string{
	"/dev/sd",   // SCSI/SATA disks
	"/dev/hd",   // legacy IDE disks
	"/dev/vd",   // virtio disks
	"/dev/xvd",  // Xen disks
	"/dev/nvme", // NVMe namespaces
	"/dev/dm-",  // device-mapper (LVM, LUKS)
	"/dev/md",   // software RAID
	"/dev/loop", // loop devices
	"/dev/mapper/",
	"/dev/disk/", // the by-id/by-uuid symlink trees
}

// deviceIsEscape reports whether a --device host source hands over the host.
//
// Failure direction: allow, matching this layer's shape checks. The path is
// cleaned first so /dev/null/../sda is judged as /dev/sda.
func deviceIsEscape(src string) bool {
	clean := path.Clean(src)
	if escapeDevices[clean] {
		return true
	}
	for _, p := range escapeDevicePrefixes {
		if strings.HasPrefix(clean, p) {
			return true
		}
	}
	return false
}

// privilegedIsOn reports whether a --privileged argument enables privileged
// mode. arg is the whole token, bare ("--privileged") or attached
// ("--privileged=true").
//
// Failure direction: ON. docker parses the attached value with
// strconv.ParseBool, so --privileged=1, =t and =TRUE all enable it, and a
// value neither this build nor docker can parse is not evidence that
// isolation is intact. Only an explicit, parseable false is treated as off.
func privilegedIsOn(arg string) bool {
	_, val, attached := strings.Cut(arg, "=")
	if !attached {
		return true
	}
	on, err := strconv.ParseBool(val)
	return err != nil || on
}

var dangerousCaps = map[string]bool{
	"ALL": true, "SYS_ADMIN": true, "SYS_PTRACE": true, "SYS_MODULE": true,
	"DAC_READ_SEARCH": true, "SYS_RAWIO": true,
}

// checkContainer blocks docker/podman/nerdctl run|create|exec invocations
// with container-escape shapes: --privileged, host namespaces, dangerous
// capabilities, disabled seccomp/apparmor, and bind mounts of sensitive host
// roots. Ordinary project mounts (-v $PWD:/work) pass. Scanning stops at the
// first operand (the image), so flags belonging to the containerized command
// cannot false-positive (fail-open).
func checkContainer(base string, args []string) error {
	if !containerCLIs[base] {
		return nil
	}
	// Locate the subcommand, skipping global flags.
	i := 0
	sub := ""
	for ; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			switch {
			case strings.Contains(a, "="), containerGlobalBoolFlags[a]:
				// self-contained
			case containerGlobalValueFlags[a]:
				i++
			default:
				return blockedf(CodeContainerEscape,
					"%s global flag %q is not one this build knows, so the subcommand cannot be identified", base, a)
			}
			continue
		}
		sub = a
		i++
		break
	}
	if sub == "container" && i < len(args) { // docker container run ...
		sub = args[i]
		i++
	}
	if sub != "run" && sub != "create" && sub != "exec" {
		return nil
	}
	for ; i < len(args); i++ {
		a := args[i]
		if k, _, _ := strings.Cut(a, "="); k == "--privileged" {
			if privilegedIsOn(a) {
				return blockedf(CodeContainerEscape, "%s %s %s disables container isolation", base, sub, a)
			}
			continue // an explicit --privileged=false is what it says
		}
		key, val := a, ""
		hasVal := false
		switch {
		case strings.HasPrefix(a, "--") && strings.Contains(a, "="):
			key, val, _ = strings.Cut(a, "=")
			hasVal = true
		case strings.HasPrefix(a, "-v") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			key, val, hasVal = "-v", a[2:], true // pflag attached shorthand
		case containerBoolFlags[a]:
			// Takes no value: the next argument is not its value.
		case strings.HasPrefix(a, "-") && i+1 < len(args):
			// Anything else that looks like a flag is ASSUMED to take a
			// value. That assumption is the fail-closed direction: guessing
			// "value" skips one argument and keeps scanning, so a policy flag
			// after it is still judged. Guessing "no value" would let the
			// value be mistaken for the image and end the scan — which is
			// exactly how `docker run --sysctl k=v -v /:/host img` used to
			// walk through this check.
			key, val, hasVal = a, args[i+1], true
			i++
		}
		if !strings.HasPrefix(key, "-") {
			break // first operand: the image / container name
		}
		if !hasVal {
			continue
		}
		if err := checkContainerFlag(key, val); err != nil {
			return err
		}
	}
	return nil
}

func checkContainerFlag(key, val string) error {
	switch key {
	case "--pid", "--ipc", "--userns", "--uts", "--cgroupns":
		if val == "host" {
			return blockedf(CodeContainerEscape, "%s=%s shares a host namespace", key, val)
		}
	case "--cap-add":
		// Case-fold BEFORE stripping the prefix, the way docker normalizes
		// it. Stripping "CAP_" from a value spelled "cap_sys_admin" removed
		// nothing, and upper-casing what was left produced "CAP_SYS_ADMIN",
		// which is not a key in dangerousCaps. So the lower-cased spelling
		// was allowed and the upper-cased one blocked, while docker grants
		// the identical capability for both.
		if dangerousCaps[strings.TrimPrefix(strings.ToUpper(val), "CAP_")] {
			return blockedf(CodeContainerEscape, "--cap-add=%s grants an escape-capable capability", val)
		}
	case "--security-opt":
		// The separator is normalized because moby still accepts the legacy
		// ':' form — the deprecation was never carried through to a
		// removal — so "seccomp:unconfined" turned seccomp off while
		// "seccomp=unconfined" was refused. A bare "disable" is moby's
		// shorthand for disabling SELinux labelling and was missed for the
		// same reason: this compared text where it had to compare meaning.
		v := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(val)), ":", "=")
		if strings.Contains(v, "seccomp=unconfined") ||
			strings.Contains(v, "apparmor=unconfined") ||
			v == "disable" || v == "label=disable" {
			return blockedf(CodeContainerEscape, "--security-opt=%s disables a confinement layer", val)
		}
	case "-v", "--volume":
		if src, ok := bindSource(val); ok && sensitiveHostPath(src) {
			return blockedf(CodeContainerEscape, "bind mount of sensitive host path %s", src)
		}
	case "--mount":
		for part := range strings.SplitSeq(val, ",") {
			k, v, _ := strings.Cut(part, "=")
			if (k == "source" || k == "src") && sensitiveHostPath(v) {
				return blockedf(CodeContainerEscape, "bind mount of sensitive host path %s", v)
			}
		}
	case "--device":
		// The spec is host-src[:container-dest[:permissions]], so the host
		// node is the first colon-separated field.
		src, _, _ := strings.Cut(val, ":")
		if deviceIsEscape(src) {
			return blockedf(CodeContainerEscape,
				"--device=%s exposes a host block or memory device", val)
		}
	}
	return nil
}

// bindSource extracts the host source of a -v/--volume spec. Anonymous
// volumes ("/data") and named volumes ("cache:/data") are not host binds.
func bindSource(spec string) (string, bool) {
	src, _, ok := strings.Cut(spec, ":")
	if !ok || src == "" {
		return "", false
	}
	if src[0] != '/' && src[0] != '.' && src[0] != '~' {
		return "", false // named volume
	}
	return src, true
}

// sensitiveHostPath reports whether a bind-mount source grants host control:
// the root itself, system configuration roots, the container runtime socket,
// or anything under /proc and /sys. Subdirectories of e.g. /etc are allowed —
// the guard targets whole-tree exposure, not every conceivable secret.
func sensitiveHostPath(p string) bool {
	c := path.Clean(p)
	switch c {
	case "/", "/etc", "/root", "/boot", "/dev", "/var", "/usr", "/home":
		return true
	}
	if c == "/proc" || strings.HasPrefix(c, "/proc/") || c == "/sys" || strings.HasPrefix(c, "/sys/") {
		return true
	}
	return strings.HasSuffix(c, "/docker.sock") || strings.HasSuffix(c, "/podman.sock") ||
		strings.HasSuffix(c, "/containerd.sock")
}
