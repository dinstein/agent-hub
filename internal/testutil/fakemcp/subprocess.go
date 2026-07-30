package fakemcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dinstein/agent-hub/internal/mcp/transport"
)

// ScriptEnv is the environment variable carrying the JSON script to a
// re-executed fake server process. json.Marshal output contains no raw
// newlines, so one env var always suffices.
const ScriptEnv = "FAKEMCP_SCRIPT"

// MaybeServe turns the current process into a fake MCP server when
// ScriptEnv is set, and never returns in that case. Call it first thing in
// a package's TestMain:
//
//	func TestMain(m *testing.M) {
//		fakemcp.MaybeServe()
//		os.Exit(m.Run())
//	}
//
// SpawnStdio-based tests then use (*Script).StdioConfig(), which re-executes
// the test binary; MaybeServe intercepts the child before any test runs.
// When ScriptEnv is unset MaybeServe is a no-op.
//
// Exit codes: 0 clean serve (client EOF / scripted crash), 1 serve error,
// 2 unparseable script.
func MaybeServe() {
	raw := os.Getenv(ScriptEnv)
	if raw == "" {
		return
	}
	script, err := ParseScript([]byte(raw))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakemcp: %v\n", err)
		os.Exit(2)
	}
	if err := Serve(context.Background(), os.Stdin, os.Stdout, os.Stderr, script); err != nil {
		fmt.Fprintf(os.Stderr, "fakemcp: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// StdioConfig returns a transport.StdioConfig that re-executes the current
// binary with the script passed through ScriptEnv — a real child process
// for transport.SpawnStdio tests. The parent environment is
// inherited so the Go test runner's context survives.
func (s *Script) StdioConfig() (transport.StdioConfig, error) {
	exe, err := os.Executable()
	if err != nil {
		return transport.StdioConfig{}, fmt.Errorf("fakemcp: resolve executable: %w", err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return transport.StdioConfig{}, fmt.Errorf("fakemcp: encode script: %w", err)
	}
	return transport.StdioConfig{
		Command: exe,
		Env:     append(os.Environ(), ScriptEnv+"="+string(data)),
	}, nil
}
