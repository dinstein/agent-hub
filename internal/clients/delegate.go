package clients

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DELEGATION: agenthub still never re-encodes a TOML document. It asks the
// tool that owns the format to edit it, and then checks the result.
//
// The old refusal was correct about the danger and wrong about the
// conclusion. Rewriting codex's config ourselves would cost the user their
// comments and layout, but codex's own CLI writes that file every day
// without losing anything — so the safe move is not to refuse, it is to
// stop being the one holding the pen.
//
// Three properties make this delegation and not a shrug:
//
//   - the file is BACKED UP first, exactly as agenthub's own writes are, so
//     the operator's undo survives a delegate that does something unwanted;
//   - the result is VERIFIED by re-reading the file. A CLI that exits 0
//     without writing what was asked is reported as a failure, never as a
//     connect;
//   - a delegate that is not installed REFUSES, with the manual snippet. It
//     never silently leaves the client unconnected while reporting success.

// delegateTimeout bounds one delegate invocation. These CLIs edit a local
// file; one that has not finished in this long is wedged, and a config
// command must not hang on it.
const delegateTimeout = 20 * time.Second

// clientCLI describes a client's own configuration command: the one program
// that is allowed to rewrite a format agenthub will not.
type clientCLI struct {
	// bin is looked up on PATH. Its resolved absolute path goes into the
	// result, because "which program did you run on my behalf" must be
	// answerable after the fact.
	bin string
	// add and remove build the argument vectors. No shell is involved
	// anywhere: the arguments reach execve as written.
	add    func(name string, e Entry) []string
	remove func(name string) []string
}

// DelegateError reports that the client's own CLI was run and did not
// produce the requested state — it failed, or it exited cleanly and the
// file does not show the change.
//
// Failure direction: this is an error, never a degraded success. The whole
// value of delegating is that afterwards agenthub knows, rather than
// assumes, what is in the file.
type DelegateError struct {
	Client string
	Op     string
	// Command is the resolved program and its arguments, so the operator
	// can run exactly what agenthub ran.
	Command []string
	// Output is the delegate's combined output, trimmed.
	Output string
	// Snippet is the manual instruction to fall back on.
	Snippet string
	Err     error
}

func (e *DelegateError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s via %q", e.Client, e.Op, strings.Join(e.Command, " "))
	if e.Err != nil {
		fmt.Fprintf(&b, " failed: %v", e.Err)
	} else {
		b.WriteString(" reported success but the entry is not in the file")
	}
	if e.Output != "" {
		fmt.Fprintf(&b, "\n%s", e.Output)
	}
	return b.String()
}

func (e *DelegateError) Unwrap() error { return e.Err }

// runDelegate backs the file up, runs the client's CLI, and returns the
// backup path. Verification is the caller's, because only it knows what the
// file should say afterwards.
func (f *probeFormat) runDelegate(op, path string, args []string) (backup string, cmd []string, err error) {
	cli := f.spec.delegate
	bin, lookErr := exec.LookPath(cli.bin)
	if lookErr != nil {
		return "", nil, lookErr
	}
	cmd = append([]string{bin}, args...)

	// Back up before handing the file to anyone else. A delegate is still
	// something editing the user's configuration, and agenthub's undo does
	// not get weaker just because it is not the one writing.
	if data, readErr := os.ReadFile(path); readErr == nil {
		if backup, err = f.tbl.backup(f.spec.id, path, data); err != nil {
			return "", cmd, err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), delegateTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, bin, args...)
	// No stdin: a delegate that wants to ask a question must fail instead
	// of hanging on a terminal that may not be there.
	c.Stdin = nil
	out, runErr := c.CombinedOutput()
	if runErr != nil {
		return backup, cmd, &DelegateError{
			Client: f.spec.id, Op: op, Command: cmd,
			Output: strings.TrimSpace(string(out)),
			Err:    runErr,
		}
	}
	return backup, cmd, nil
}

// codexCLI drives `codex mcp add|remove`, which is how codex's own
// documentation tells a user to edit that file.
var codexCLI = &clientCLI{
	bin: "codex",
	add: func(name string, e Entry) []string {
		return append([]string{"mcp", "add", name, "--", e.Command}, e.Args...)
	},
	remove: func(name string) []string {
		return []string{"mcp", "remove", name}
	},
}
