package downstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// errNoSpawnInTest is what the scripted spawn screen refuses with, so a test
// can reach the log line without launching anything.
var errNoSpawnInTest = errors.New("spawn refused by the test")

// findLine returns the one serialized record carrying msg.
//
// Serialized, never decoded: the duplicate-key bug this file guards against
// is invisible after a decode, because the decode is what discards the
// duplicate (foundation.md, the client / client_name entry).
func findLine(t *testing.T, out, msg string) string {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, `"msg":"`+msg+`"`) {
			return line
		}
	}
	t.Fatalf("no %q record in:\n%s", msg, out)
	return ""
}

// The `connected` event says a connection exists and how many tools came with
// it. What the two ends AGREED to was recorded nowhere — and the negotiated
// version decides whether requests carry _meta, whether the session is
// stateless, and how resultType is normalized on the way out.
func TestHandshakeTermsAreRecorded(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	script := fakemcp.Minimal("echo")
	script.ServerInfo = mcp.Implementation{Name: "linear-mcp", Version: "2.1.0"}
	script.Capabilities = json.RawMessage(`{"tools":{"listChanged":true},"logging":{}}`)

	srv, err := Connect(context.Background(), Spec{ID: "linear"}, Deps{
		Log: log,
		Dial: func(context.Context, Spec) (transport.Transport, error) {
			return fakemcp.Connect(script)
		},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(srv.Close)

	// The buffer holds the whole connection's prose, so the record has to be
	// isolated before anything is counted on it.
	line := findLine(t, buf.String(), "downstream handshake complete")
	for _, want := range []string{
		`"level":"INFO"`,
		`"protocol":"` + mcp.ProtocolVersion + `"`,
		`"server_name":"linear-mcp"`,
		`"server_version":"2.1.0"`,
		// Sorted keys, so the same server reads the same way run to run.
		`"capabilities":"logging tools"`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("handshake record missing %s: %s", want, line)
		}
	}
	// `server` stays the registry id. slog's JSON handler does not
	// deduplicate, so spelling the peer's self-report under the same key would
	// put the field on the line twice and a reader taking the last one would
	// join on the wrong value — the bug `client` / `client_name` already
	// records. The assertion is on the SERIALIZED line for that reason: a
	// decode would discard the duplicate and see nothing wrong.
	if n := strings.Count(line, `"server":`); n != 1 {
		t.Fatalf("the bound server id appears %d times on one line: %s", n, line)
	}
}

// capabilityNames answers "which capabilities were declared", not "what is in
// them": InitializeResult keeps them raw because this facade does not
// interpret them, and a log line is not where that starts.
func TestCapabilityNamesListsKeysAndSurvivesJunk(t *testing.T) {
	t.Parallel()
	got := capabilityNames(json.RawMessage(`{"tools":{"listChanged":true},"logging":{}}`))
	if got != "logging tools" {
		t.Errorf("capabilityNames = %q, want sorted keys %q", got, "logging tools")
	}
	// Failure direction: a malformed capabilities document belongs to the
	// server and must not turn a live handshake into a failure at the logging
	// step.
	for _, raw := range []string{``, `not json`, `[1,2]`, `null`} {
		if got := capabilityNames(json.RawMessage(raw)); got != "" {
			t.Errorf("capabilityNames(%q) = %q, want empty", raw, got)
		}
	}
}

// The stdio path had no log call at all, so an entry that hung — the
// documented normal for an npx/uvx cold start — left nothing saying what had
// been launched. `runtime` is the load-bearing field: AGENTS.md requires that
// isolation a config claims is delivered or refused, and this is the only
// place recording which of the two a connection actually got.
func TestSpawningAStdioDownstreamIsRecorded(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	deps := Deps{
		Log: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
		// Refuse the spawn: the record is written before it, which is the
		// whole point — a command that never comes up is the case with
		// nothing else to read.
		Spawn: func(string, []string, []string) error { return errNoSpawnInTest },
	}
	spec := Spec{
		ID: "docs", Command: "npx", Args: []string{"-y", "@scope/server@latest"},
		Cwd: "/tmp/work",
		Env: map[string]string{"API_TOKEN": "sk-must-never-be-logged"},
	}

	_, err := deps.dialStdio(context.Background(), spec)
	if err == nil {
		t.Fatal("the scripted spawn refusal did not surface")
	}

	line := findLine(t, buf.String(), "spawning a stdio downstream")
	for _, want := range []string{
		`"level":"INFO"`,
		`"runtime":"host"`,
		`"command":"npx"`,
		`"args":"-y @scope/server@latest"`,
		`"cwd":"/tmp/work"`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("spawn record missing %s: %s", want, line)
		}
	}
	// The env is the one input carrying expanded secrets, and never writing
	// it is a stronger guarantee than redacting it.
	if strings.Contains(line, "API_TOKEN") || strings.Contains(line, "sk-must-never-be-logged") {
		t.Fatalf("the child environment reached the log: %s", line)
	}
}
